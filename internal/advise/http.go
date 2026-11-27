package advise

import (
	"net/http"

	"github.com/stratus/internal/control"
)

// Router is where advise registers its endpoints; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes guided architecture decisions on the control API:
//
//	GET  /_stratus/control/v1/advise/families               decision families, their questions and options
//	POST /_stratus/control/v1/advise/decisions              start a decision {family, title?, answers?}
//	GET  /_stratus/control/v1/advise/decisions              list decisions, newest first
//	GET  /_stratus/control/v1/advise/decisions/{id}         a decision, its pending questions and ranking
//	POST /_stratus/control/v1/advise/decisions/{id}/answers record answers {answers: {...}}
//	POST /_stratus/control/v1/advise/decisions/{id}/assess  judge and rank the options
//	POST /_stratus/control/v1/advise/decisions/{id}/decide  choose an option {option, rationale?}
//	GET  /_stratus/control/v1/advise/decisions/{id}/adr     the ADR as Markdown (?format=json wraps it as {id, markdown})
func RegisterRoutes(r Router, s *Service) {
	base := control.PathPrefix + "/advise"
	r.HandleFunc("GET "+base+"/families", func(w http.ResponseWriter, _ *http.Request) {
		control.WriteJSON(w, http.StatusOK, map[string]any{"families": s.Families()})
	})
	r.HandleFunc("POST "+base+"/decisions", func(w http.ResponseWriter, req *http.Request) {
		var in StartInput
		if err := control.DecodeOptionalJSON(req, &in); err != nil {
			control.WriteError(w, err)
			return
		}
		respond(w, http.StatusCreated)(s.Start(in))
	})
	r.HandleFunc("GET "+base+"/decisions", func(w http.ResponseWriter, _ *http.Request) {
		ds, err := s.List()
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"decisions": ds})
	})
	r.HandleFunc("GET "+base+"/decisions/{id}", func(w http.ResponseWriter, req *http.Request) {
		respond(w, http.StatusOK)(s.Get(req.PathValue("id")))
	})
	r.HandleFunc("POST "+base+"/decisions/{id}/answers", func(w http.ResponseWriter, req *http.Request) {
		var in struct {
			Answers map[string]any `json:"answers"`
		}
		if err := control.DecodeOptionalJSON(req, &in); err != nil {
			control.WriteError(w, err)
			return
		}
		respond(w, http.StatusOK)(s.Answer(req.PathValue("id"), in.Answers))
	})
	r.HandleFunc("POST "+base+"/decisions/{id}/assess", func(w http.ResponseWriter, req *http.Request) {
		respond(w, http.StatusOK)(s.Assess(req.PathValue("id")))
	})
	r.HandleFunc("POST "+base+"/decisions/{id}/decide", func(w http.ResponseWriter, req *http.Request) {
		var in DecideInput
		if err := control.DecodeOptionalJSON(req, &in); err != nil {
			control.WriteError(w, err)
			return
		}
		respond(w, http.StatusOK)(s.Decide(req.PathValue("id"), in))
	})
	r.HandleFunc("GET "+base+"/decisions/{id}/adr", func(w http.ResponseWriter, req *http.Request) {
		md, err := s.ADR(req.PathValue("id"))
		if err != nil {
			control.WriteError(w, err)
			return
		}
		if req.URL.Query().Get("format") == "json" {
			control.WriteJSON(w, http.StatusOK, map[string]string{"id": req.PathValue("id"), "markdown": md})
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(md))
	})
}

func respond(w http.ResponseWriter, status int) func(View, error) {
	return func(v View, err error) {
		if err != nil {
			control.WriteError(w, err)
			return
		}
		control.WriteJSON(w, status, v)
	}
}
