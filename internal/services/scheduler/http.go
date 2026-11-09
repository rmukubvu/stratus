package scheduler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/stratus/internal/apierror"
)

// route is one EventBridge Scheduler REST operation.
type route struct {
	method    string
	resource  string
	withName  bool
	operation string
}

var routes = []route{
	{http.MethodPost, "schedules", true, "CreateSchedule"},
	{http.MethodGet, "schedules", true, "GetSchedule"},
	{http.MethodPut, "schedules", true, "UpdateSchedule"},
	{http.MethodDelete, "schedules", true, "DeleteSchedule"},
	{http.MethodGet, "schedules", false, "ListSchedules"},
	{http.MethodPost, "schedule-groups", true, "CreateScheduleGroup"},
	{http.MethodGet, "schedule-groups", true, "GetScheduleGroup"},
	{http.MethodDelete, "schedule-groups", true, "DeleteScheduleGroup"},
	{http.MethodGet, "schedule-groups", false, "ListScheduleGroups"},
	{http.MethodGet, "tags", true, "ListTagsForResource"},
	{http.MethodPost, "tags", true, "TagResource"},
	{http.MethodDelete, "tags", true, "UntagResource"},
}

// splitPath returns the resource and name of a path such as
// /schedules/nightly or /tags/arn:aws:scheduler:...:schedule-group/x.
func splitPath(path string) (resource, name string, ok bool) {
	trimmed := strings.Trim(path, "/")
	resource, rest, hasRest := strings.Cut(trimmed, "/")
	if resource == "" {
		return "", "", false
	}
	if !hasRest {
		return resource, "", true
	}
	if resource != "tags" && strings.Contains(rest, "/") {
		return "", "", false
	}
	name, err := url.PathUnescape(rest)
	return resource, name, err == nil && name != ""
}

// Operation names the Scheduler operation for a request, or "" when the
// request is not one this service serves.
func Operation(method, path string) string {
	resource, name, ok := splitPath(path)
	if !ok {
		return ""
	}
	for _, r := range routes {
		if r.method == method && r.resource == resource && r.withName == (name != "") {
			return r.operation
		}
	}
	return ""
}

// epoch is an AWS JSON timestamp: epoch seconds, possibly fractional. It
// also accepts RFC 3339 strings for hand-written requests.
type epoch time.Time

func (e *epoch) UnmarshalJSON(raw []byte) error {
	text := strings.TrimSpace(string(raw))
	if text == "null" {
		return nil
	}
	if strings.HasPrefix(text, "\"") {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			return err
		}
		*e = epoch(t)
		return nil
	}
	seconds, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return err
	}
	*e = epoch(time.Unix(0, int64(seconds*float64(time.Second))).UTC())
	return nil
}

func (e epoch) MarshalJSON() ([]byte, error) {
	return json.Marshal(epochSeconds(time.Time(e)))
}

func epochSeconds(t time.Time) float64 {
	return float64(t.UnixNano()) / float64(time.Second)
}

func (e *epoch) timePtr() *time.Time {
	if e == nil {
		return nil
	}
	t := time.Time(*e).UTC()
	return &t
}

type scheduleRequest struct {
	ActionAfterCompletion      string              `json:"ActionAfterCompletion"`
	ClientToken                string              `json:"ClientToken"`
	Description                string              `json:"Description"`
	EndDate                    *epoch              `json:"EndDate"`
	FlexibleTimeWindow         *FlexibleTimeWindow `json:"FlexibleTimeWindow"`
	GroupName                  string              `json:"GroupName"`
	KmsKeyArn                  string              `json:"KmsKeyArn"`
	ScheduleExpression         string              `json:"ScheduleExpression"`
	ScheduleExpressionTimezone string              `json:"ScheduleExpressionTimezone"`
	StartDate                  *epoch              `json:"StartDate"`
	State                      string              `json:"State"`
	Target                     *Target             `json:"Target"`
}

func (req scheduleRequest) input(name string) ScheduleInput {
	in := ScheduleInput{
		Name:                       name,
		GroupName:                  req.GroupName,
		Description:                req.Description,
		State:                      req.State,
		ScheduleExpression:         req.ScheduleExpression,
		ScheduleExpressionTimezone: req.ScheduleExpressionTimezone,
		StartDate:                  req.StartDate.timePtr(),
		EndDate:                    req.EndDate.timePtr(),
		ActionAfterCompletion:      req.ActionAfterCompletion,
		KmsKeyArn:                  req.KmsKeyArn,
	}
	if req.FlexibleTimeWindow != nil {
		in.FlexibleTimeWindow = *req.FlexibleTimeWindow
	}
	if req.Target != nil {
		in.Target = *req.Target
	}
	return in
}

type tag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

// Handle serves an EventBridge Scheduler REST request.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request) error {
	_, name, _ := splitPath(r.URL.Path)
	query := r.URL.Query()
	switch Operation(r.Method, r.URL.Path) {
	case "CreateSchedule":
		var req scheduleRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		sch, err := s.CreateSchedule(req.input(name))
		return respond(w, map[string]any{"ScheduleArn": sch.Arn}, err)
	case "UpdateSchedule":
		var req scheduleRequest
		if err := decode(r, &req); err != nil {
			return err
		}
		sch, err := s.UpdateSchedule(req.input(name))
		return respond(w, map[string]any{"ScheduleArn": sch.Arn}, err)
	case "GetSchedule":
		sch, err := s.GetSchedule(param(query, "groupName", "GroupName"), name)
		return respond(w, scheduleJSON(sch), err)
	case "DeleteSchedule":
		return respond(w, map[string]any{}, s.DeleteSchedule(param(query, "groupName", "GroupName"), name))
	case "ListSchedules":
		schedules, err := s.ListSchedules(ListFilter{
			GroupName:  param(query, "GroupName", "groupName"),
			NamePrefix: param(query, "NamePrefix", "namePrefix"),
			State:      param(query, "State", "state"),
		})
		if err != nil {
			return err
		}
		items := make([]map[string]any, 0, len(schedules))
		for _, sch := range schedules {
			items = append(items, scheduleSummaryJSON(sch))
		}
		return respond(w, map[string]any{"Schedules": items}, nil)
	case "CreateScheduleGroup":
		var req struct {
			Tags []tag `json:"Tags"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		g, err := s.CreateGroup(name, tagMap(req.Tags))
		return respond(w, map[string]any{"ScheduleGroupArn": g.Arn}, err)
	case "GetScheduleGroup":
		g, err := s.GetGroup(name)
		return respond(w, groupJSON(g), err)
	case "DeleteScheduleGroup":
		return respond(w, map[string]any{}, s.DeleteGroup(name))
	case "ListScheduleGroups":
		groups, err := s.ListGroups(param(query, "NamePrefix", "namePrefix"))
		if err != nil {
			return err
		}
		items := make([]map[string]any, 0, len(groups))
		for _, g := range groups {
			items = append(items, groupJSON(g))
		}
		return respond(w, map[string]any{"ScheduleGroups": items}, nil)
	case "ListTagsForResource":
		g, err := s.GetGroup(groupNameFromARN(name))
		return respond(w, map[string]any{"Tags": tagList(g.Tags)}, err)
	case "TagResource":
		var req struct {
			Tags []tag `json:"Tags"`
		}
		if err := decode(r, &req); err != nil {
			return err
		}
		return respond(w, map[string]any{}, s.TagGroup(groupNameFromARN(name), tagMap(req.Tags)))
	case "UntagResource":
		return respond(w, map[string]any{}, s.UntagGroup(groupNameFromARN(name), query["TagKeys"]))
	default:
		return &apierror.Error{StatusCode: http.StatusNotFound, Code: "ResourceNotFoundException", Message: "Stratus does not implement " + r.Method + " " + r.URL.Path + " for EventBridge Scheduler"}
	}
}

func param(query url.Values, names ...string) string {
	for _, n := range names {
		if v := query.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// groupNameFromARN accepts a schedule-group ARN or a bare group name.
func groupNameFromARN(arn string) string {
	_, name, ok := strings.Cut(arn, ":schedule-group/")
	if !ok {
		return arn
	}
	return name
}

func tagMap(tags []tag) map[string]string {
	if len(tags) == 0 {
		return nil
	}
	out := make(map[string]string, len(tags))
	for _, t := range tags {
		out[t.Key] = t.Value
	}
	return out
}

func tagList(tags map[string]string) []tag {
	out := make([]tag, 0, len(tags))
	for k, v := range tags {
		out = append(out, tag{Key: k, Value: v})
	}
	return out
}

func scheduleJSON(sch Schedule) map[string]any {
	out := map[string]any{
		"Arn":                        sch.Arn,
		"Name":                       sch.Name,
		"GroupName":                  sch.GroupName,
		"State":                      sch.State,
		"ScheduleExpression":         sch.ScheduleExpression,
		"ScheduleExpressionTimezone": sch.ScheduleExpressionTimezone,
		"FlexibleTimeWindow":         sch.FlexibleTimeWindow,
		"ActionAfterCompletion":      sch.ActionAfterCompletion,
		"Target":                     sch.Target,
		"CreationDate":               epochSeconds(sch.CreationDate),
		"LastModificationDate":       epochSeconds(sch.LastModificationDate),
	}
	if sch.Description != "" {
		out["Description"] = sch.Description
	}
	if sch.KmsKeyArn != "" {
		out["KmsKeyArn"] = sch.KmsKeyArn
	}
	if sch.StartDate != nil {
		out["StartDate"] = epochSeconds(*sch.StartDate)
	}
	if sch.EndDate != nil {
		out["EndDate"] = epochSeconds(*sch.EndDate)
	}
	return out
}

func scheduleSummaryJSON(sch Schedule) map[string]any {
	return map[string]any{
		"Arn":                  sch.Arn,
		"Name":                 sch.Name,
		"GroupName":            sch.GroupName,
		"State":                sch.State,
		"Target":               map[string]any{"Arn": sch.Target.Arn},
		"CreationDate":         epochSeconds(sch.CreationDate),
		"LastModificationDate": epochSeconds(sch.LastModificationDate),
	}
}

func groupJSON(g Group) map[string]any {
	return map[string]any{
		"Arn":                  g.Arn,
		"Name":                 g.Name,
		"State":                g.State,
		"CreationDate":         epochSeconds(g.CreationDate),
		"LastModificationDate": epochSeconds(g.LastModificationDate),
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
