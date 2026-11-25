package clock

import (
	"errors"
	"net/http"
	"time"

	"github.com/stratus/internal/control"
)

// Router is where the clock endpoints register; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes the clock on the control API:
//
//	GET  /_stratus/control/v1/clock           {"now", "real_now", "offset", "frozen"}
//	POST /_stratus/control/v1/clock/advance   {"by": "30m"} or {"to": "2026-11-23T09:00:00Z"}; runs what became due
//	POST /_stratus/control/v1/clock/freeze    hold the clock where it stands
//	POST /_stratus/control/v1/clock/resume    let it run again from where it was held
func RegisterRoutes(r Router, c *Clock) {
	path := control.PathPrefix + "/clock"
	r.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) {
		control.WriteJSON(w, http.StatusOK, c.Status())
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
		result, err := advance(req, c, body.By, body.To)
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, result)
	})
	r.HandleFunc("POST "+path+"/freeze", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, c.Freeze)
	})
	r.HandleFunc("POST "+path+"/resume", func(w http.ResponseWriter, _ *http.Request) {
		writeStatus(w, c.Resume)
	})
}

func writeStatus(w http.ResponseWriter, change func() (Status, error)) {
	st, err := change()
	if err != nil {
		control.WriteError(w, controlError(err))
		return
	}
	control.WriteJSON(w, http.StatusOK, st)
}

const advanceHint = `{"by": "30m"} or {"to": "2026-11-23T09:00:00Z"}`

func advance(req *http.Request, c *Clock, by, to string) (MoveResult, error) {
	switch {
	case by != "" && to != "":
		return MoveResult{}, invalid("give either by or to, not both", advanceHint)
	case by != "":
		d, err := time.ParseDuration(by)
		if err != nil {
			return MoveResult{}, invalid("by must be a duration such as 30m or 2h", `{"by": "30m"}`)
		}
		return c.Advance(req.Context(), d)
	case to != "":
		t, err := time.Parse(time.RFC3339, to)
		if err != nil {
			return MoveResult{}, invalid("to must be an RFC 3339 time", `{"to": "2026-11-23T09:00:00Z"}`)
		}
		return c.AdvanceTo(req.Context(), t)
	default:
		return MoveResult{}, invalid("by or to is required", advanceHint)
	}
}

func invalid(message, hint string) error {
	return &control.Error{Kind: control.KindInvalid, Code: "InvalidRequest", Message: message, Hint: hint}
}

func controlError(err error) error {
	if errors.Is(err, ErrNotForward) {
		return &control.Error{Kind: control.KindInvalid, Code: "ClockNotForward",
			Message: "the clock only moves forward: by must be positive, and to must be later than the clock's current time",
			Hint:    "GET " + control.PathPrefix + "/clock shows the current time"}
	}
	var ce *control.Error
	if errors.As(err, &ce) {
		return ce
	}
	return &control.Error{Kind: control.KindInternal, Code: "InternalError", Message: "move the clock", Err: err}
}
