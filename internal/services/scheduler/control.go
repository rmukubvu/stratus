package scheduler

import (
	"errors"
	"net/http"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/control"
)

// Router is where the clock endpoints register; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes the scheduler clock on the control API:
//
//	GET  /_stratus/control/v1/scheduler/clock     {"now", "offset"}
//	POST /_stratus/control/v1/scheduler/advance   {"by": "30m"} or {"to": "2026-11-10T09:00:00Z"}; fires due schedules
//	POST /_stratus/control/v1/scheduler/run       {"name": "nightly", "group": "default"}; fires one schedule now
func RegisterRoutes(r Router, svc *Service) {
	path := control.PathPrefix + "/scheduler"
	r.HandleFunc("GET "+path+"/clock", func(w http.ResponseWriter, _ *http.Request) {
		control.WriteJSON(w, http.StatusOK, map[string]any{"now": svc.Now(), "offset": svc.Offset().String()})
	})
	r.HandleFunc("POST "+path+"/advance", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			By string `json:"by"`
			To string `json:"to"`
		}
		if err := control.DecodeOptionalJSON(req, &body); err != nil {
			control.WriteError(w, err)
			return
		}
		result, err := advance(req, svc, body.By, body.To)
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, result)
	})
	r.HandleFunc("POST "+path+"/run", func(w http.ResponseWriter, req *http.Request) {
		var body struct {
			Name  string `json:"name"`
			Group string `json:"group"`
		}
		if err := control.DecodeOptionalJSON(req, &body); err != nil {
			control.WriteError(w, err)
			return
		}
		if body.Name == "" {
			control.WriteError(w, &control.Error{Kind: control.KindInvalid, Code: "InvalidRequest", Message: "name is required", Hint: `{"name": "nightly", "group": "default"}`})
			return
		}
		inv, err := svc.Run(req.Context(), body.Group, body.Name)
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, inv)
	})
}

func advance(req *http.Request, svc *Service, by, to string) (AdvanceResult, error) {
	switch {
	case by != "" && to != "":
		return AdvanceResult{}, &control.Error{Kind: control.KindInvalid, Code: "InvalidRequest", Message: "give either by or to, not both"}
	case by != "":
		d, err := time.ParseDuration(by)
		if err != nil {
			return AdvanceResult{}, &control.Error{Kind: control.KindInvalid, Code: "InvalidRequest", Message: "by must be a duration such as 30m or 2h", Hint: `{"by": "30m"}`}
		}
		return svc.Advance(req.Context(), d)
	case to != "":
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return AdvanceResult{}, &control.Error{Kind: control.KindInvalid, Code: "InvalidRequest", Message: "to must be an RFC 3339 time", Hint: `{"to": "2026-11-10T09:00:00Z"}`}
		}
		return svc.AdvanceTo(req.Context(), t)
	default:
		return AdvanceResult{}, &control.Error{Kind: control.KindInvalid, Code: "InvalidRequest", Message: "by or to is required", Hint: `{"by": "30m"} or {"to": "2026-11-10T09:00:00Z"}`}
	}
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
