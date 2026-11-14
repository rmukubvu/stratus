package loadrun

import (
	"net/http"

	"github.com/stratus/internal/control"
)

// Router is where runs registers its endpoints; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes load runs on the control API:
//
//	POST /_stratus/control/v1/runs            start a run (optionally with limits and chaos)
//	GET  /_stratus/control/v1/runs            list runs, newest first
//	GET  /_stratus/control/v1/runs/{id}       a run's summary and timeline ("current", "latest")
//	POST /_stratus/control/v1/runs/{id}/stop  stop a run and undo what it set up
func RegisterRoutes(r Router, rec *Recorder) {
	base := control.PathPrefix + "/runs"
	r.HandleFunc("POST "+base, func(w http.ResponseWriter, req *http.Request) {
		var in StartInput
		if err := control.DecodeOptionalJSON(req, &in); err != nil {
			control.WriteError(w, err)
			return
		}
		s, err := rec.Start(in)
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusCreated, s)
	})
	r.HandleFunc("GET "+base, func(w http.ResponseWriter, _ *http.Request) {
		runs := rec.List()
		if runs == nil {
			runs = []RunInfo{}
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"runs": runs})
	})
	r.HandleFunc("GET "+base+"/{id}", func(w http.ResponseWriter, req *http.Request) {
		s, err := rec.Get(req.PathValue("id"))
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, s)
	})
	r.HandleFunc("POST "+base+"/{id}/stop", func(w http.ResponseWriter, req *http.Request) {
		s, err := rec.Stop(req.PathValue("id"))
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, s)
	})
}
