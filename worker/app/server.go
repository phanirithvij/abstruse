package app

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	pb "github.com/bleenco/abstruse/pb"
	"github.com/bleenco/abstruse/pkg/fs"
	"github.com/bleenco/abstruse/pkg/stats"
	"github.com/bleenco/abstruse/worker/config"
	"github.com/bleenco/abstruse/worker/docker"
	"github.com/bleenco/abstruse/worker/git"
	"github.com/golang/protobuf/ptypes/empty"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

// Server represents gRPC server.
type Server struct {
	mu       sync.Mutex
	config   *config.Config
	id       string
	addr     string
	listener net.Listener
	server   *grpc.Server
	app      *App
	logger   *zap.SugaredLogger
	jobs     map[uint64]*pb.Job
	errch    chan error
}

// NewServer returns new gRPC server.
func NewServer(config *config.Config, logger *zap.Logger, app *App) *Server {
	return &Server{
		config: config,
		id:     config.ID,
		addr:   config.GRPC.Addr,
		app:    app,
		logger: logger.With(zap.String("type", "server")).Sugar(),
		jobs:   make(map[uint64]*pb.Job),
		errch:  make(chan error),
	}
}

// Run starts the gRPC server.
func (s *Server) Run() error {
	var err error
	grpcOpts := []grpc.ServerOption{}
	certificate, err := tls.LoadX509KeyPair(s.config.TLS.Cert, s.config.TLS.Key)
	if err != nil {
		return err
	}
	s.listener, err = net.Listen("tcp", s.config.GRPC.Addr)
	if err != nil {
		return err
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates:       []tls.Certificate{certificate},
		InsecureSkipVerify: true,
	})

	grpcOpts = append(grpcOpts, grpc.Creds(creds))
	grpcOpts = append(grpcOpts, grpc.UnaryInterceptor(s.unaryInterceptor))
	grpcOpts = append(grpcOpts, grpc.StreamInterceptor(s.streamInterceptor))
	s.server = grpc.NewServer(grpcOpts...)
	pb.RegisterAPIServer(s.server, s)
	s.logger.Infof("grpc server listening on %s", s.config.GRPC.Addr)

	return s.server.Serve(s.listener)
}

// Connect returns worker host information.
func (s *Server) Connect(ctx context.Context, in *empty.Empty) (*pb.HostInfo, error) {
	info, err := stats.GetHostStats()
	if err != nil {
		return nil, err
	}

	return &pb.HostInfo{
		Id:                   s.id,
		Addr:                 s.addr,
		Hostname:             info.Hostname,
		Uptime:               info.Uptime,
		BootTime:             info.BootTime,
		Procs:                info.Procs,
		Os:                   info.OS,
		Platform:             info.Platform,
		PlatformFamily:       info.PlatformFamily,
		PlatformVersion:      info.PlatformVersion,
		KernelVersion:        info.KernelVersion,
		KernelArch:           info.KernelArch,
		VirtualizationSystem: info.VirtualizationRole,
		VirtualizationRole:   info.VirtualizationRole,
		HostID:               info.HostID,
		MaxParallel:          uint64(s.config.Scheduler.MaxParallel),
	}, nil
}

// Usage returns stream of health data.
func (s *Server) Usage(stream pb.API_UsageServer) error {
	errch := make(chan error)
	s.logger.Infof("connection with server successfully initialized")

	send := func(stream pb.API_UsageServer) error {
		cpu, mem := stats.GetUsageStats()
		if err := stream.Send(&pb.UsageStats{
			Cpu: cpu,
			Mem: mem,
		}); err != nil {
			return err
		}
		return nil
	}

	go func() {
		_, err := stream.Recv()
		if err != nil {
			errch <- err
		}
	}()

	go func() {
		if err := send(stream); err != nil {
			errch <- err
		}
		ticker := time.NewTicker(5 * time.Second)
		for range ticker.C {
			if err := send(stream); err != nil {
				ticker.Stop()
				errch <- err
			}
		}
	}()

	err := <-errch
	s.logger.Errorf("lost connection with server: %s", err.Error())
	s.errch <- err
	return err
}

// StartJob gRPC method.
func (s *Server) StartJob(job *pb.Job, stream pb.API_StartJobServer) error {
	name := fmt.Sprintf("abstruse-job-%d", job.GetId())

	s.mu.Lock()
	if _, ok := s.jobs[job.Id]; ok {
		docker.StopContainer(name)
		delete(s.jobs, job.Id)
	}
	s.jobs[job.Id] = job
	s.mu.Unlock()

	defer func() {
		s.mu.Lock()
		delete(s.jobs, job.Id)
		s.mu.Unlock()
	}()

	logch := make(chan []byte, 1024)
	image := job.Image
	env := strings.Split(job.Env, " ")
	var cmds []string
	if err := json.Unmarshal([]byte(job.Commands), &cmds); err != nil {
		return err
	}
	var commands [][]string
	for _, c := range cmds {
		commands = append(commands, strings.Split(c, " "))
	}

	dir, err := fs.TempDir()
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	if err := git.CloneRepository(job.GetUrl(), job.GetRef(), job.GetCommitSHA(), job.GetProviderToken(), dir); err != nil {
		return err
	}

	go func() {
		for output := range logch {
			log := &pb.JobResp{Id: job.GetId(), Content: output, Type: pb.JobResp_Log}
			if err := stream.Send(log); err != nil {
				break
			}
		}
	}()

	if err := docker.RunContainer(name, image, commands, env, dir, logch); err != nil {
		stream.Send(&pb.JobResp{Id: job.GetId(), Type: pb.JobResp_Done, Status: pb.JobResp_StatusFailing})
		return err
	}

	stream.Send(&pb.JobResp{Id: job.GetId(), Type: pb.JobResp_Done, Status: pb.JobResp_StatusPassing})

	return nil
}

// StopJob gRPC method.
func (s *Server) StopJob(ctx context.Context, job *pb.Job) (*pb.JobStopResp, error) {
	defer func() {
		s.mu.Lock()
		delete(s.jobs, job.Id)
		s.mu.Unlock()
	}()

	name := fmt.Sprintf("abstruse-job-%d", job.GetId())
	if err := docker.StopContainer(name); err != nil {
		return &pb.JobStopResp{Stopped: false}, nil
	}

	return &pb.JobStopResp{Stopped: true}, nil
}

func (s *Server) Error() chan error {
	return s.errch
}