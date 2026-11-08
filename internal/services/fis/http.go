package fis

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/stratus/internal/apierror"
)

// route is one FIS REST operation.
type route struct {
	method    string
	resource  string // first path segment
	withID    bool   // whether the path has an {id} segment
	operation string
}

var routes = []route{
	{http.MethodPost, "experimentTemplates", false, "CreateExperimentTemplate"},
	{http.MethodGet, "experimentTemplates", false, "ListExperimentTemplates"},
	{http.MethodGet, "experimentTemplates", true, "GetExperimentTemplate"},
	{http.MethodPatch, "experimentTemplates", true, "UpdateExperimentTemplate"},
	{http.MethodDelete, "experimentTemplates", true, "DeleteExperimentTemplate"},
	{http.MethodPost, "experiments", false, "StartExperiment"},
	{http.MethodGet, "experiments", false, "ListExperiments"},
	{http.MethodGet, "experiments", true, "GetExperiment"},
	{http.MethodDelete, "experiments", true, "StopExperiment"},
	{http.MethodGet, "actions", false, "ListActions"},
	{http.MethodGet, "actions", true, "GetAction"},
	{http.MethodGet, "targetResourceTypes", false, "ListTargetResourceTypes"},
}

// splitPath returns the resource and ID of an FIS path such as
// /experimentTemplates/EXT123.
func splitPath(path string) (resource, id string, ok bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	switch len(parts) {
	case 1:
		return parts[0], "", true
	case 2:
		id, err := url.PathUnescape(parts[1])
		return parts[0], id, err == nil && id != ""
	default:
		return "", "", false
	}
}

// Operation names the FIS operation for a request, or "" if the path is
// not an FIS one.
func Operation(method, path string) string {
	resource, id, ok := splitPath(path)
	if !ok {
		return ""
	}
	for _, r := range routes {
		if r.method == method && r.resource == resource && r.withID == (id != "") {
			return r.operation
		}
	}
	return ""
}

// Handle serves an FIS REST request.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request) error {
	_, id, _ := splitPath(r.URL.Path)
	switch Operation(r.Method, r.URL.Path) {
	case "CreateExperimentTemplate":
		var in TemplateInput
		if err := decode(r, &in); err != nil {
			return err
		}
		t, err := s.CreateTemplate(in)
		return respond(w, map[string]any{"experimentTemplate": t}, err)
	case "ListExperimentTemplates":
		ts, err := s.Templates()
		return respond(w, map[string]any{"experimentTemplates": ts}, err)
	case "GetExperimentTemplate":
		t, err := s.Template(id)
		return respond(w, map[string]any{"experimentTemplate": t}, err)
	case "UpdateExperimentTemplate":
		var in TemplateInput
		if err := decode(r, &in); err != nil {
			return err
		}
		t, err := s.UpdateTemplate(id, in)
		return respond(w, map[string]any{"experimentTemplate": t}, err)
	case "DeleteExperimentTemplate":
		t, err := s.DeleteTemplate(id)
		return respond(w, map[string]any{"experimentTemplate": t}, err)
	case "StartExperiment":
		var in StartInput
		if err := decode(r, &in); err != nil {
			return err
		}
		e, err := s.StartExperiment(in)
		return respond(w, map[string]any{"experiment": e}, err)
	case "ListExperiments":
		es, err := s.Experiments()
		return respond(w, map[string]any{"experiments": es}, err)
	case "GetExperiment":
		e, err := s.Experiment(id)
		return respond(w, map[string]any{"experiment": e}, err)
	case "StopExperiment":
		e, err := s.StopExperiment(id)
		return respond(w, map[string]any{"experiment": e}, err)
	case "ListActions":
		return respond(w, map[string]any{"actions": s.Actions()}, nil)
	case "GetAction":
		a, err := s.Action(id)
		return respond(w, map[string]any{"action": a}, err)
	case "ListTargetResourceTypes":
		return respond(w, map[string]any{"targetResourceTypes": s.TargetResourceTypes()}, nil)
	default:
		return &apierror.Error{
			StatusCode: http.StatusNotImplemented,
			Code:       "NotImplementedException",
			Message:    "Stratus does not implement " + r.Method + " " + r.URL.Path + " for FIS",
		}
	}
}

func decode(r *http.Request, v any) error {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return validation("could not read the request body: %s", err.Error())
	}
	if len(strings.TrimSpace(string(body))) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, v); err != nil {
		return validation("request body is not valid JSON: %s", err.Error())
	}
	return nil
}

func respond(w http.ResponseWriter, payload any, err error) error {
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(payload)
}
