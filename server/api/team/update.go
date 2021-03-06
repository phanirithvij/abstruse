package team

import (
	"net/http"

	"github.com/asaskevich/govalidator"
	"github.com/bleenco/abstruse/pkg/lib"
	"github.com/bleenco/abstruse/server/api/middlewares"
	"github.com/bleenco/abstruse/server/api/render"
	"github.com/bleenco/abstruse/server/core"
)

// HandleUpdate returns an http.HandlerFunc that writes JSON encoded
// result about updating team to the http response body.
func HandleUpdate(teams core.TeamStore, users core.UserStore, permissions core.PermissionStore) http.HandlerFunc {
	type repoPerm struct {
		ID    uint `json:"id"`
		Read  bool `json:"read"`
		Write bool `json:"write"`
		Exec  bool `json:"exec"`
	}

	type form struct {
		ID      uint       `json:"id" valid:"required"`
		Name    string     `json:"name" valid:"required"`
		About   string     `json:"about" valid:"required"`
		Color   string     `json:"color" valid:"required"`
		Members []uint     `json:"members"`
		Repos   []repoPerm `json:"repos"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		claims := middlewares.ClaimsFromCtx(r.Context())
		var f form
		defer r.Body.Close()

		u, err := users.Find(claims.ID)
		if err != nil || u.Role != "admin" {
			render.UnathorizedError(w, err.Error())
			return
		}

		if err := lib.DecodeJSON(r.Body, &f); err != nil {
			render.InternalServerError(w, err.Error())
			return
		}

		if valid, err := govalidator.ValidateStruct(f); err != nil || !valid {
			render.BadRequestError(w, err.Error())
			return
		}

		team, err := teams.Find(f.ID)
		if err != nil {
			render.NotFoundError(w, err.Error())
			return
		}

		team.Name = f.Name
		team.About = f.About
		team.Color = f.Color

		if err := teams.Update(team); err != nil {
			render.InternalServerError(w, err.Error())
			return
		}

		var members []*core.User
		for _, id := range f.Members {
			if user, err := users.Find(id); err == nil {
				members = append(members, user)
			}
		}

		if err := teams.UpdateUsers(team.ID, members); err != nil {
			render.InternalServerError(w, err.Error())
			return
		}

		perms, err := permissions.List(team.ID)
		if err != nil {
			render.InternalServerError(w, err.Error())
			return
		}

		var updated []uint

		for _, perm := range perms {
			update := false
			for _, repo := range f.Repos {
				if repo.ID == perm.RepositoryID {
					update = true
					if p, err := permissions.Find(team.ID, perm.RepositoryID); err == nil {
						p.Read = repo.Read
						p.Write = repo.Write
						p.Exec = repo.Exec

						if err := permissions.Update(p); err != nil {
							render.InternalServerError(w, err.Error())
							return
						}
						updated = append(updated, perm.RepositoryID)
					}
				}
			}

			if !update {
				if err := permissions.Delete(perm); err != nil {
					render.InternalServerError(w, err.Error())
					return
				}
			}
		}

		for _, perm := range f.Repos {
			isUpdated := false
			for _, id := range updated {
				if id == perm.ID {
					isUpdated = true
				}
			}

			if !isUpdated {
				if err := permissions.Create(&core.Permission{
					TeamID:       team.ID,
					RepositoryID: perm.ID,
					Read:         perm.Read,
					Write:        perm.Write,
					Exec:         perm.Exec,
				}); err != nil {
					render.InternalServerError(w, err.Error())
					return
				}
			}
		}

		render.JSON(w, http.StatusOK, team)
	}
}
