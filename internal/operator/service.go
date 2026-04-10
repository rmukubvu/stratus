package operator

import (
	"net/http"
	"strconv"

	"github.com/stratus/internal/services/logs"
)

type Service struct {
	store *Store
	logs  *logs.Service
}

func NewService(store *Store, logs *logs.Service) *Service {
	return &Service{store: store, logs: logs}
}

func (s *Service) Record(record RequestRecord) {
	if s == nil || s.store == nil {
		return
	}
	s.store.Record(record)
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request) (int, any) {
	switch r.URL.Path {
	case "/_stratus/operator/bootstrap":
		return http.StatusOK, s.Bootstrap()
	case "/_stratus/operator/overview":
		return http.StatusOK, s.store.Overview()
	case "/_stratus/operator/activity":
		return http.StatusOK, map[string]any{
			"items": s.store.Activity(ActivityFilter{
				Service:     r.URL.Query().Get("service"),
				StatusClass: r.URL.Query().Get("status"),
				Query:       r.URL.Query().Get("q"),
				Limit:       parseLimit(r.URL.Query().Get("limit"), 100),
			}),
		}
	case "/_stratus/operator/errors":
		return http.StatusOK, map[string]any{
			"items": s.store.Errors(parseLimit(r.URL.Query().Get("limit"), 100)),
		}
	case "/_stratus/operator/logs/groups":
		if s.logs == nil {
			return http.StatusNotImplemented, map[string]string{"error": "logs service is not configured"}
		}
		groups, err := s.logs.ListGroups()
		if err != nil {
			return http.StatusInternalServerError, map[string]string{"error": err.Error()}
		}
		return http.StatusOK, map[string]any{"items": groups}
	case "/_stratus/operator/logs/streams":
		if s.logs == nil {
			return http.StatusNotImplemented, map[string]string{"error": "logs service is not configured"}
		}
		group := r.URL.Query().Get("group")
		streams, err := s.logs.ListStreams(group)
		if err != nil {
			return http.StatusInternalServerError, map[string]string{"error": err.Error()}
		}
		return http.StatusOK, map[string]any{"items": streams}
	case "/_stratus/operator/logs/events":
		if s.logs == nil {
			return http.StatusNotImplemented, map[string]string{"error": "logs service is not configured"}
		}
		group := r.URL.Query().Get("group")
		stream := r.URL.Query().Get("stream")
		events, err := s.logs.ListEvents(group, stream, parseLimit(r.URL.Query().Get("limit"), 200))
		if err != nil {
			return http.StatusInternalServerError, map[string]string{"error": err.Error()}
		}
		return http.StatusOK, map[string]any{"items": events}
	default:
		return http.StatusNotFound, map[string]string{"error": "operator endpoint not found"}
	}
}

func (s *Service) PortalHTML() ([]byte, error) {
	return s.PortalPage()
}

func parseLimit(raw string, fallback int) int {
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return fallback
	}
	return value
}
