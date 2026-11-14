package authn

import (
	"net/http"

	"github.com/stratus/internal/control"
)

// Router is where authn registers its endpoints; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// setRequest is the body of PUT /_stratus/control/v1/sigv4.
type setRequest struct {
	Mode string `json:"mode"`
}

// RegisterRoutes exposes the authenticator on the control API:
//
//	PUT    /_stratus/control/v1/sigv4   {"mode":"strict"|"audit"|"off"}; an empty body means strict
//	GET    /_stratus/control/v1/sigv4   mode, counters and recent failures with canonical requests
//	DELETE /_stratus/control/v1/sigv4   turn verification off and return the final status
func RegisterRoutes(r Router, a *Authenticator) {
	path := control.PathPrefix + "/sigv4"
	r.HandleFunc("PUT "+path, func(w http.ResponseWriter, req *http.Request) {
		var body setRequest
		if err := control.DecodeOptionalJSON(req, &body); err != nil {
			control.WriteError(w, err)
			return
		}
		if body.Mode == "" {
			body.Mode = string(Strict)
		}
		mode, err := ParseMode(body.Mode)
		if err != nil {
			control.WriteError(w, &control.Error{
				Kind:    control.KindInvalid,
				Code:    "InvalidParameter",
				Message: "invalid value for mode",
				Hint:    "use strict, audit or off",
			})
			return
		}
		control.WriteJSON(w, http.StatusOK, a.Set(mode))
	})
	r.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		control.WriteJSON(w, http.StatusOK, a.Status())
	})
	r.HandleFunc("DELETE "+path, func(w http.ResponseWriter, _ *http.Request) {
		control.WriteJSON(w, http.StatusOK, a.Clear())
	})
}
