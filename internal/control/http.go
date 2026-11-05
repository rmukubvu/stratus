package control

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// PathPrefix is where the control API is mounted.
const PathPrefix = "/_stratus/control/v1"

const maxRequestBody = 64 << 10

// Handler exposes the control Service over HTTP/JSON.
//
//	GET    /_stratus/control/v1/health                   full health report (always 200)
//	GET    /_stratus/control/v1/ready                    200 when ready, 503 otherwise
//	POST   /_stratus/control/v1/reset                    {"services": ["s3"]}; empty resets all
//	GET    /_stratus/control/v1/snapshots                list snapshots
//	POST   /_stratus/control/v1/snapshots                {"name": "baseline"}; name optional
//	GET    /_stratus/control/v1/snapshots/{id}           describe a snapshot
//	DELETE /_stratus/control/v1/snapshots/{id}           delete a snapshot
//	POST   /_stratus/control/v1/snapshots/{id}/restore   restore a snapshot
//
// Errors are returned as {"error": {"code", "message", "hint"}} so callers,
// including coding agents, can branch on a stable code and act on the hint.
type Handler struct {
	svc *Service
	mux *http.ServeMux
}

// NewHandler builds the HTTP adapter for svc.
func NewHandler(svc *Service) *Handler {
	h := &Handler{svc: svc, mux: http.NewServeMux()}
	h.mux.HandleFunc("GET "+PathPrefix+"/health", h.health)
	h.mux.HandleFunc("GET "+PathPrefix+"/ready", h.ready)
	h.mux.HandleFunc("POST "+PathPrefix+"/reset", h.reset)
	h.mux.HandleFunc("GET "+PathPrefix+"/snapshots", h.listSnapshots)
	h.mux.HandleFunc("POST "+PathPrefix+"/snapshots", h.createSnapshot)
	h.mux.HandleFunc("GET "+PathPrefix+"/snapshots/{id}", h.getSnapshot)
	h.mux.HandleFunc("DELETE "+PathPrefix+"/snapshots/{id}", h.deleteSnapshot)
	h.mux.HandleFunc("POST "+PathPrefix+"/snapshots/{id}/restore", h.restoreSnapshot)
	return h
}

// HandleFunc registers an extra route on the control API, so other bounded
// contexts (such as chaos) can expose endpoints under the same prefix and
// share its error format. Patterns use net/http ServeMux syntax.
func (h *Handler) HandleFunc(pattern string, fn http.HandlerFunc) {
	h.mux.HandleFunc(pattern, fn)
}

// Matches reports whether a request path belongs to the control API.
func Matches(path string) bool {
	return path == PathPrefix || strings.HasPrefix(path, PathPrefix+"/")
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, pattern := h.mux.Handler(r); pattern == "" {
		WriteError(w, &Error{
			Kind:    KindNotFound,
			Code:    "UnknownControlEndpoint",
			Message: r.Method + " " + r.URL.Path + " is not a control API endpoint",
			Hint:    "see GET " + PathPrefix + "/health; endpoints: health, ready, reset, snapshots, chaos",
		})
		return
	}
	h.mux.ServeHTTP(w, r)
}

func (h *Handler) health(w http.ResponseWriter, r *http.Request) {
	WriteJSON(w, http.StatusOK, h.svc.Health(r.Context()))
}

func (h *Handler) ready(w http.ResponseWriter, r *http.Request) {
	health := h.svc.Health(r.Context())
	status := http.StatusOK
	if !health.Ready {
		status = http.StatusServiceUnavailable
	}
	WriteJSON(w, status, map[string]any{"ready": health.Ready, "status": health.Status, "checks": health.Checks})
}

type resetRequest struct {
	Services []string `json:"services"`
}

func (h *Handler) reset(w http.ResponseWriter, r *http.Request) {
	var req resetRequest
	if err := DecodeOptionalJSON(r, &req); err != nil {
		WriteError(w, err)
		return
	}
	result, err := h.svc.Reset(r.Context(), req.Services)
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, result)
}

func (h *Handler) listSnapshots(w http.ResponseWriter, _ *http.Request) {
	snaps, err := h.svc.ListSnapshots()
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"items": snaps})
}

type createSnapshotRequest struct {
	Name string `json:"name"`
}

func (h *Handler) createSnapshot(w http.ResponseWriter, r *http.Request) {
	var req createSnapshotRequest
	if err := DecodeOptionalJSON(r, &req); err != nil {
		WriteError(w, err)
		return
	}
	snap, err := h.svc.CreateSnapshot(r.Context(), req.Name)
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusCreated, snap)
}

func (h *Handler) getSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := h.svc.GetSnapshot(r.PathValue("id"))
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, snap)
}

func (h *Handler) deleteSnapshot(w http.ResponseWriter, r *http.Request) {
	if err := h.svc.DeleteSnapshot(r.PathValue("id")); err != nil {
		WriteError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) restoreSnapshot(w http.ResponseWriter, r *http.Request) {
	snap, err := h.svc.RestoreSnapshot(r.Context(), r.PathValue("id"))
	if err != nil {
		WriteError(w, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"restored": snap})
}

// DecodeOptionalJSON decodes a JSON request body, rejecting unknown fields. It accepts an empty body as the zero value.
func DecodeOptionalJSON(r *http.Request, dst any) error {
	if r.Body == nil {
		return nil
	}
	dec := json.NewDecoder(io.LimitReader(r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil && !errors.Is(err, io.EOF) {
		return &Error{
			Kind:    KindInvalid,
			Code:    "InvalidRequestBody",
			Message: "request body is not valid JSON for this endpoint: " + err.Error(),
			Hint:    "send a JSON object, or an empty body to use the defaults",
		}
	}
	return nil
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

// WriteError writes err in the control API error format.
func WriteError(w http.ResponseWriter, err error) {
	ce := AsError(err)
	body := errorBody{Code: ce.Code, Message: ce.Message, Hint: ce.Hint}
	if ce.Kind == KindInternal && ce.Err != nil {
		body.Message = ce.Message + ": " + ce.Err.Error()
	}
	WriteJSON(w, statusFor(ce.Kind), map[string]errorBody{"error": body})
}

func statusFor(kind Kind) int {
	switch kind {
	case KindInvalid:
		return http.StatusBadRequest
	case KindNotFound:
		return http.StatusNotFound
	case KindConflict:
		return http.StatusConflict
	case KindUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// WriteJSON writes payload as indented JSON.
func WriteJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(payload)
}
