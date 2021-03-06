package provider

import (
	"net/http"

	"github.com/asaskevich/govalidator"
	"github.com/bleenco/abstruse/pkg/lib"
	"github.com/bleenco/abstruse/server/api/middlewares"
	"github.com/bleenco/abstruse/server/api/render"
	"github.com/bleenco/abstruse/server/core"
)

// HandleCreate returns http.HandlerFunc which writes JSON encoded
// result about creating provider to the http response body.
func HandleCreate(providers core.ProviderStore) http.HandlerFunc {
	type form struct {
		Name        string `json:"name" valid:"stringlength(4|12),required"`
		URL         string `json:"url" valid:"url,required"`
		Host        string `json:"host" valid:"url,required"`
		AccessToken string `json:"accessToken" valid:"stringlength(12|50),required"`
		Secret      string `json:"secret" valid:"stringlength(5|50),required"`
	}

	return func(w http.ResponseWriter, r *http.Request) {
		claims := middlewares.ClaimsFromCtx(r.Context())
		var f form
		var err error
		defer r.Body.Close()

		if err = lib.DecodeJSON(r.Body, &f); err != nil {
			render.InternalServerError(w, err.Error())
			return
		}

		if valid, err := govalidator.ValidateStruct(f); err != nil || !valid {
			render.BadRequestError(w, err.Error())
			return
		}

		provider := core.Provider{
			Name:        f.Name,
			URL:         f.URL,
			Host:        f.Host,
			AccessToken: f.AccessToken,
			Secret:      f.Secret,
			UserID:      claims.ID,
		}

		if err := providers.Create(provider); err != nil {
			render.InternalServerError(w, err.Error())
			return
		}

		render.JSON(w, http.StatusOK, provider)
	}
}
