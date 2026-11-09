package ses

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/control"
)

// Router is where the inbox registers its endpoints; *control.Handler fits.
type Router interface {
	HandleFunc(pattern string, fn http.HandlerFunc)
}

// RegisterRoutes exposes the captured inbox on the control API:
//
//	GET    /_stratus/control/v1/ses/messages        list captured emails, oldest first
//	       ?to=&from=&subject=&since=&limit=         filters; since is RFC 3339 or a duration such as 5m
//	GET    /_stratus/control/v1/ses/messages/{id}   one email by MessageId
//	DELETE /_stratus/control/v1/ses/messages        empty the inbox (identities and templates stay)
func RegisterRoutes(r Router, svc *Service) {
	path := control.PathPrefix + "/ses/messages"
	r.HandleFunc("GET "+path, func(w http.ResponseWriter, req *http.Request) {
		filter, err := filterFromQuery(req, time.Now())
		if err != nil {
			control.WriteError(w, err)
			return
		}
		items, err := svc.Messages(filter)
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"items": items, "count": len(items)})
	})
	r.HandleFunc("GET "+path+"/{id}", func(w http.ResponseWriter, req *http.Request) {
		msg, err := svc.Message(req.PathValue("id"))
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, msg)
	})
	r.HandleFunc("DELETE "+path, func(w http.ResponseWriter, _ *http.Request) {
		count, err := svc.ClearMessages()
		if err != nil {
			control.WriteError(w, controlError(err))
			return
		}
		control.WriteJSON(w, http.StatusOK, map[string]any{"cleared": count})
	})
}

func filterFromQuery(r *http.Request, now time.Time) (MessageFilter, error) {
	q := r.URL.Query()
	filter := MessageFilter{To: q.Get("to"), From: q.Get("from"), Subject: q.Get("subject")}
	if raw := q.Get("since"); raw != "" {
		since, err := parseSince(raw, now)
		if err != nil {
			return MessageFilter{}, &control.Error{Kind: control.KindInvalid, Code: "InvalidQuery", Message: "since must be an RFC 3339 time or a duration such as 5m", Hint: "example: since=2026-11-09T09:00:00Z or since=10m"}
		}
		filter.Since = since
	}
	if raw := q.Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil || limit < 0 {
			return MessageFilter{}, &control.Error{Kind: control.KindInvalid, Code: "InvalidQuery", Message: "limit must be a non-negative integer"}
		}
		filter.Limit = limit
	}
	return filter, nil
}

func parseSince(raw string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(raw); err == nil {
		return now.Add(-d), nil
	}
	return time.Parse(time.RFC3339, raw)
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
	}
	return &control.Error{Kind: kind, Code: apiErr.Code, Message: apiErr.Message}
}
