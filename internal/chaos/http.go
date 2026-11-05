package chaos

import (
	"net/http"

	"github.com/stratus/internal/control"
)

// Router is where chaos registers its endpoints; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes the engine on the control API:
//
//	PUT    /_stratus/control/v1/chaos   start an experiment (replaces any current one)
//	GET    /_stratus/control/v1/chaos   report: per-rule counts and recent hits
//	DELETE /_stratus/control/v1/chaos   stop and return the final report
func RegisterRoutes(r Router, e *Engine) {
	path := control.PathPrefix + "/chaos"
	r.HandleFunc("PUT "+path, func(w http.ResponseWriter, req *http.Request) {
		var spec Spec
		if err := control.DecodeOptionalJSON(req, &spec); err != nil {
			control.WriteError(w, err)
			return
		}
		report, err := e.Start(spec)
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusCreated, report)
	})
	r.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		report, err := e.Report()
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, report)
	})
	r.HandleFunc("DELETE "+path, func(w http.ResponseWriter, _ *http.Request) {
		report, err := e.Stop()
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, report)
	})
}
