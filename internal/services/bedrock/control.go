package bedrock

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/control"
)

// Router is where the control endpoints register; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes fixtures, the invocation log and the mode on the
// control API:
//
//	GET    /_stratus/control/v1/bedrock                  mode, fixture directory and upstream
//	PUT    /_stratus/control/v1/bedrock/mode             {"mode":"record"|"replay"}
//	GET    /_stratus/control/v1/bedrock/fixtures         every fixture, in match order
//	POST   /_stratus/control/v1/bedrock/fixtures         add a runtime fixture
//	DELETE /_stratus/control/v1/bedrock/fixtures         remove runtime fixtures (files stay)
//	DELETE /_stratus/control/v1/bedrock/fixtures/{id}    remove one runtime fixture
//	GET    /_stratus/control/v1/bedrock/invocations      model calls, oldest first
//	       ?model=&contains=&outcome=&limit=
//	DELETE /_stratus/control/v1/bedrock/invocations      empty the log
func RegisterRoutes(r Router, svc *Service) {
	base := control.PathPrefix + "/bedrock"
	r.HandleFunc("GET "+base, func(w http.ResponseWriter, _ *http.Request) {
		control.WriteJSON(w, http.StatusOK, svc.Status())
	})
	r.HandleFunc("PUT "+base+"/mode", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Mode string `json:"mode"`
		}
		if err := control.DecodeOptionalJSON(req, &body); err != nil {
			control.WriteError(w, err)
			return
		}
		mode, err := ParseMode(body.Mode)
		if err != nil || body.Mode == "" {
			control.WriteError(w, &control.Error{Kind: control.KindInvalid, Code: "InvalidMode", Message: `mode must be "replay" or "record"`})
			return
		}
		if err := svc.SetMode(mode); err != nil {
			control.WriteError(w, controlError(internal(err.Error())))
			return
		}
		control.WriteJSON(w, http.StatusOK, svc.Status())
	})

	r.HandleFunc("GET "+base+"/fixtures", func(w http.ResponseWriter, _ *http.Request) {
		fixtures, err := svc.Fixtures()
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		if fixtures == nil {
			fixtures = []Fixture{}
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"items": fixtures, "count": len(fixtures)})
	})
	r.HandleFunc("POST "+base+"/fixtures", func(w http.ResponseWriter, req *http.Request) {
		var f Fixture
		if err := control.DecodeOptionalJSON(req, &f); err != nil {
			control.WriteError(w, err)
			return
		}
		added, err := svc.AddFixture(f)
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusCreated, added)
	})
	r.HandleFunc("DELETE "+base+"/fixtures", func(w http.ResponseWriter, _ *http.Request) {
		n, err := svc.ClearFixtures()
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"cleared": n})
	})
	r.HandleFunc("DELETE "+base+"/fixtures/{id}", func(w http.ResponseWriter, req *http.Request) {
		if err := svc.DeleteFixture(req.PathValue("id")); err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"deleted": req.PathValue("id")})
	})

	r.HandleFunc("GET "+base+"/invocations", func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		filter := InvocationFilter{Model: q.Get("model"), Contains: q.Get("contains"), Outcome: q.Get("outcome")}
		if raw := q.Get("limit"); raw != "" {
			limit, err := strconv.Atoi(raw)
			if err != nil || limit < 0 {
				control.WriteError(w, &control.Error{Kind: control.KindInvalid, Code: "InvalidQuery", Message: "limit must be a non-negative integer"})
				return
			}
			filter.Limit = limit
		}
		items, err := svc.Invocations(filter)
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
	})
	r.HandleFunc("DELETE "+base+"/invocations", func(w http.ResponseWriter, _ *http.Request) {
		n, err := svc.ClearInvocations()
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"cleared": n})
	})
}

// controlError maps the service's AWS-shaped errors onto the control API's.
func controlError(err error) error {
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) {
		return err
	}
	kind := control.KindInternal
	switch apiErr.StatusCode {
	case http.StatusNotFound:
		kind = control.KindNotFound
	case http.StatusBadRequest:
		kind = control.KindInvalid
	case http.StatusConflict:
		kind = control.KindConflict
	}
	return &control.Error{Kind: kind, Code: apiErr.Code, Message: apiErr.Message}
}
