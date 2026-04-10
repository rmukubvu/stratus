package monitoring

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/store"
)

const (
	namespace     = "http://monitoring.amazonaws.com/doc/2010-08-01/"
	metricsBucket = "cloudwatch-metrics"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

type metricDatum struct {
	Dimensions []dimension `json:"dimensions"`
	MetricName string      `json:"metric_name"`
	Namespace  string      `json:"namespace"`
	Timestamp  time.Time   `json:"timestamp"`
	Unit       string      `json:"unit,omitempty"`
	Value      float64     `json:"value"`
}

type dimension struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type metricDatumJSON struct {
	Dimensions []dimension `json:"Dimensions"`
	MetricName string      `json:"MetricName"`
	Timestamp  flexTime    `json:"Timestamp"`
	Unit       string      `json:"Unit"`
	Value      float64     `json:"Value"`
}

type putMetricDataRequestJSON struct {
	Namespace  string            `json:"Namespace"`
	MetricData []metricDatumJSON `json:"MetricData"`
}

type listMetricsRequestJSON struct {
	Namespace  string      `json:"Namespace"`
	MetricName string      `json:"MetricName"`
	Dimensions []dimension `json:"Dimensions"`
}

type getMetricStatisticsRequestJSON struct {
	Namespace  string      `json:"Namespace"`
	MetricName string      `json:"MetricName"`
	StartTime  flexTime    `json:"StartTime"`
	EndTime    flexTime    `json:"EndTime"`
	Statistics []string    `json:"Statistics"`
	Dimensions []dimension `json:"Dimensions"`
}

type flexTime struct {
	raw string
}

func (f *flexTime) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		f.raw = ""
		return nil
	}

	if trimmed[0] == '"' {
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return err
		}
		f.raw = strings.TrimSpace(s)
		return nil
	}

	var number float64
	if err := json.Unmarshal(trimmed, &number); err != nil {
		return err
	}
	if number > 1e12 {
		secs := int64(number) / 1000
		nanos := (int64(number) % 1000) * int64(time.Millisecond)
		f.raw = time.Unix(secs, nanos).UTC().Format(time.RFC3339Nano)
		return nil
	}
	secs, frac := math.Modf(number)
	f.raw = time.Unix(int64(secs), int64(frac*float64(time.Second))).UTC().Format(time.RFC3339Nano)
	return nil
}

func (f flexTime) String() string {
	return f.raw
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

type putMetricDataResponse struct {
	XMLName          xml.Name         `xml:"PutMetricDataResponse"`
	XMLNS            string           `xml:"xmlns,attr"`
	ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
}

type putMetricDataResponseJSON struct{}

type listMetricsResponse struct {
	XMLName          xml.Name          `xml:"ListMetricsResponse"`
	XMLNS            string            `xml:"xmlns,attr"`
	Result           listMetricsResult `xml:"ListMetricsResult"`
	ResponseMetadata responseMetadata  `xml:"ResponseMetadata"`
}

type listMetricsResponseJSON struct {
	Metrics   []metricJSON `json:"Metrics"`
	NextToken string       `json:"NextToken,omitempty"`
}

type listMetricsResult struct {
	Metrics   []metricXML `xml:"Metrics>member"`
	NextToken string      `xml:"NextToken,omitempty"`
}

type metricXML struct {
	Dimensions []dimensionXML `xml:"Dimensions>member,omitempty"`
	MetricName string         `xml:"MetricName"`
	Namespace  string         `xml:"Namespace"`
}

type metricJSON struct {
	Dimensions []dimension `json:"Dimensions,omitempty"`
	MetricName string      `json:"MetricName"`
	Namespace  string      `json:"Namespace"`
}

type dimensionXML struct {
	Name  string `xml:"Name"`
	Value string `xml:"Value"`
}

type getMetricStatisticsResponse struct {
	XMLName          xml.Name                  `xml:"GetMetricStatisticsResponse"`
	XMLNS            string                    `xml:"xmlns,attr"`
	Result           getMetricStatisticsResult `xml:"GetMetricStatisticsResult"`
	ResponseMetadata responseMetadata          `xml:"ResponseMetadata"`
}

type getMetricStatisticsResponseJSON struct {
	Datapoints []datapointJSON `json:"Datapoints"`
	Label      string          `json:"Label"`
}

type getMetricStatisticsResult struct {
	Datapoints []datapointXML `xml:"Datapoints>member"`
	Label      string         `xml:"Label"`
}

type datapointXML struct {
	Average     float64 `xml:"Average,omitempty"`
	Maximum     float64 `xml:"Maximum,omitempty"`
	Minimum     float64 `xml:"Minimum,omitempty"`
	SampleCount float64 `xml:"SampleCount,omitempty"`
	Sum         float64 `xml:"Sum,omitempty"`
	Timestamp   string  `xml:"Timestamp"`
	Unit        string  `xml:"Unit,omitempty"`
}

type datapointJSON struct {
	Average     float64 `json:"Average,omitempty"`
	Maximum     float64 `json:"Maximum,omitempty"`
	Minimum     float64 `json:"Minimum,omitempty"`
	SampleCount float64 `json:"SampleCount,omitempty"`
	Sum         float64 `json:"Sum,omitempty"`
	Timestamp   string  `json:"Timestamp"`
	Unit        string  `json:"Unit,omitempty"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch normalizeOperation(operation, r) {
	case "PutMetricData":
		return s.putMetricData(w, r, requestID)
	case "ListMetrics":
		return s.listMetrics(w, r, requestID)
	case "GetMetricStatistics":
		return s.getMetricStatistics(w, r, requestID)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "monitoring operation is not implemented"}
	}
}

func normalizeOperation(operation string, r *http.Request) string {
	op := strings.TrimSpace(operation)
	switch op {
	case "PutMetricData", "ListMetrics", "GetMetricStatistics":
		return op
	}

	if target := strings.TrimSpace(r.Header.Get("X-Amz-Target")); target != "" {
		if _, suffix, found := strings.Cut(target, "."); found {
			switch suffix {
			case "PutMetricData", "ListMetrics", "GetMetricStatistics":
				return suffix
			}
		}
	}

	form, err := parseForm(r)
	if err != nil {
		return op
	}
	switch action := strings.TrimSpace(form.Get("Action")); action {
	case "PutMetricData", "ListMetrics", "GetMetricStatistics":
		return action
	default:
		return op
	}
}

func (s *Service) putMetricData(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	ns := form.Get("Namespace")
	if ns != "" {
		metrics, err := parseMetricData(form, ns, s.now().UTC())
		if err != nil {
			return err
		}
		return s.storeMetrics(w, r, requestID, metrics)
	}

	payload, ok, err := parseJSONBody[putMetricDataRequestJSON](r)
	if err != nil {
		return err
	}
	if !ok || strings.TrimSpace(payload.Namespace) == "" {
		return validation("Namespace is required")
	}

	metrics, err := parseMetricDataJSON(payload, s.now().UTC())
	if err != nil {
		return err
	}
	return s.storeMetrics(w, r, requestID, metrics)
}

func (s *Service) storeMetrics(w http.ResponseWriter, r *http.Request, requestID string, metrics []metricDatum) error {
	for _, metric := range metrics {
		raw, err := json.Marshal(metric)
		if err != nil {
			return internal(err)
		}
		key := metric.Namespace + "|" + metric.MetricName + "|" + metric.Timestamp.Format(time.RFC3339Nano) + "|" + uuid.NewString()
		if err := s.metadata.Put(metricsBucket, key, raw); err != nil {
			return internal(err)
		}
	}
	writeResponse(w, r, http.StatusOK, putMetricDataResponse{
		XMLNS:            namespace,
		ResponseMetadata: responseMetadata{RequestID: requestID},
	}, putMetricDataResponseJSON{})
	return nil
}

func (s *Service) listMetrics(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	namespaceFilter := form.Get("Namespace")
	nameFilter := form.Get("MetricName")
	dimensionFilters := parseDimensions(form, "Dimensions.member.")
	if namespaceFilter == "" && nameFilter == "" && len(dimensionFilters) == 0 {
		payload, ok, err := parseJSONBody[listMetricsRequestJSON](r)
		if err != nil {
			return err
		}
		if ok {
			namespaceFilter = strings.TrimSpace(payload.Namespace)
			nameFilter = strings.TrimSpace(payload.MetricName)
			dimensionFilters = append([]dimension(nil), payload.Dimensions...)
			sort.Slice(dimensionFilters, func(i, j int) bool { return dimensionFilters[i].Name < dimensionFilters[j].Name })
		}
	}

	seen := map[string]metricDatum{}
	if err := s.metadata.Scan(metricsBucket, "", func(_, v []byte) error {
		var item metricDatum
		if err := json.Unmarshal(v, &item); err != nil {
			return nil
		}
		if namespaceFilter != "" && item.Namespace != namespaceFilter {
			return nil
		}
		if nameFilter != "" && item.MetricName != nameFilter {
			return nil
		}
		if !containsDimensions(item.Dimensions, dimensionFilters) {
			return nil
		}
		seen[metricIdentity(item)] = item
		return nil
	}); err != nil {
		return internal(err)
	}

	items := make([]metricXML, 0, len(seen))
	jsonItems := make([]metricJSON, 0, len(seen))
	for _, item := range seen {
		dims := make([]dimensionXML, 0, len(item.Dimensions))
		for _, dim := range item.Dimensions {
			dims = append(dims, dimensionXML{Name: dim.Name, Value: dim.Value})
		}
		sort.Slice(dims, func(i, j int) bool { return dims[i].Name < dims[j].Name })
		items = append(items, metricXML{Dimensions: dims, MetricName: item.MetricName, Namespace: item.Namespace})
		jsonItems = append(jsonItems, metricJSON{
			Dimensions: append([]dimension(nil), item.Dimensions...),
			MetricName: item.MetricName,
			Namespace:  item.Namespace,
		})
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].Namespace != items[j].Namespace {
			return items[i].Namespace < items[j].Namespace
		}
		return items[i].MetricName < items[j].MetricName
	})
	sort.Slice(jsonItems, func(i, j int) bool {
		if jsonItems[i].Namespace != jsonItems[j].Namespace {
			return jsonItems[i].Namespace < jsonItems[j].Namespace
		}
		return jsonItems[i].MetricName < jsonItems[j].MetricName
	})

	writeResponse(w, r, http.StatusOK, listMetricsResponse{
		XMLNS:            namespace,
		Result:           listMetricsResult{Metrics: items},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	}, listMetricsResponseJSON{Metrics: jsonItems})
	return nil
}

func (s *Service) getMetricStatistics(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	ns := form.Get("Namespace")
	name := form.Get("MetricName")
	startRaw := form.Get("StartTime")
	endRaw := form.Get("EndTime")
	stats := requestedStatistics(form)
	dimensions := parseDimensions(form, "Dimensions.member.")
	if ns == "" || name == "" {
		payload, ok, err := parseJSONBody[getMetricStatisticsRequestJSON](r)
		if err != nil {
			return err
		}
		if ok {
			ns = strings.TrimSpace(payload.Namespace)
			name = strings.TrimSpace(payload.MetricName)
			startRaw = payload.StartTime.String()
			endRaw = payload.EndTime.String()
			if len(payload.Statistics) > 0 {
				stats = append([]string(nil), payload.Statistics...)
			}
			dimensions = append([]dimension(nil), payload.Dimensions...)
			sort.Slice(dimensions, func(i, j int) bool { return dimensions[i].Name < dimensions[j].Name })
		}
	}
	if ns == "" || name == "" {
		return validation("Namespace and MetricName are required")
	}
	start, err := parseTime(startRaw)
	if err != nil {
		return validation("StartTime is invalid")
	}
	end, err := parseTime(endRaw)
	if err != nil {
		return validation("EndTime is invalid")
	}

	points := make([]metricDatum, 0)
	if err := s.metadata.Scan(metricsBucket, "", func(_, v []byte) error {
		var item metricDatum
		if err := json.Unmarshal(v, &item); err != nil {
			return nil
		}
		if item.Namespace != ns || item.MetricName != name {
			return nil
		}
		if item.Timestamp.Before(start) || item.Timestamp.After(end) {
			return nil
		}
		if !sameDimensions(item.Dimensions, dimensions) {
			return nil
		}
		points = append(points, item)
		return nil
	}); err != nil {
		return internal(err)
	}
	sort.Slice(points, func(i, j int) bool { return points[i].Timestamp.Before(points[j].Timestamp) })

	datapoints := make([]datapointXML, 0, len(points))
	jsonPoints := make([]datapointJSON, 0, len(points))
	for _, point := range points {
		entry := datapointXML{
			Timestamp: point.Timestamp.UTC().Format(time.RFC3339),
			Unit:      point.Unit,
		}
		for _, stat := range stats {
			switch stat {
			case "Average":
				entry.Average = point.Value
			case "Sum":
				entry.Sum = point.Value
			case "Minimum":
				entry.Minimum = point.Value
			case "Maximum":
				entry.Maximum = point.Value
			case "SampleCount":
				entry.SampleCount = 1
			}
		}
		datapoints = append(datapoints, entry)
		jsonPoints = append(jsonPoints, datapointJSON{
			Average:     entry.Average,
			Maximum:     entry.Maximum,
			Minimum:     entry.Minimum,
			SampleCount: entry.SampleCount,
			Sum:         entry.Sum,
			Timestamp:   entry.Timestamp,
			Unit:        entry.Unit,
		})
	}

	writeResponse(w, r, http.StatusOK, getMetricStatisticsResponse{
		XMLNS: namespace,
		Result: getMetricStatisticsResult{
			Datapoints: datapoints,
			Label:      name,
		},
		ResponseMetadata: responseMetadata{RequestID: requestID},
	}, getMetricStatisticsResponseJSON{
		Datapoints: jsonPoints,
		Label:      name,
	})
	return nil
}

func parseMetricData(form url.Values, namespace string, now time.Time) ([]metricDatum, error) {
	out := make([]metricDatum, 0)
	for idx := 1; ; idx++ {
		prefix := "MetricData.member." + strconv.Itoa(idx) + "."
		name := form.Get(prefix + "MetricName")
		if name == "" {
			if idx == 1 {
				break
			}
			break
		}
		value, err := strconv.ParseFloat(form.Get(prefix+"Value"), 64)
		if err != nil {
			return nil, validation("metric Value is invalid")
		}
		timestamp := now
		if raw := form.Get(prefix + "Timestamp"); raw != "" {
			parsed, err := parseTime(raw)
			if err != nil {
				return nil, validation("metric Timestamp is invalid")
			}
			timestamp = parsed
		}
		out = append(out, metricDatum{
			Dimensions: parseDimensions(form, prefix+"Dimensions.member."),
			MetricName: name,
			Namespace:  namespace,
			Timestamp:  timestamp.UTC(),
			Unit:       form.Get(prefix + "Unit"),
			Value:      value,
		})
	}
	if len(out) == 0 {
		return nil, validation("MetricData is required")
	}
	return out, nil
}

func parseMetricDataJSON(payload putMetricDataRequestJSON, now time.Time) ([]metricDatum, error) {
	out := make([]metricDatum, 0, len(payload.MetricData))
	for _, item := range payload.MetricData {
		name := strings.TrimSpace(item.MetricName)
		if name == "" {
			continue
		}
		timestamp := now
		if raw := item.Timestamp.String(); raw != "" {
			parsed, err := parseTime(raw)
			if err != nil {
				return nil, validation("metric Timestamp is invalid")
			}
			timestamp = parsed
		}
		dims := append([]dimension(nil), item.Dimensions...)
		sort.Slice(dims, func(i, j int) bool { return dims[i].Name < dims[j].Name })
		out = append(out, metricDatum{
			Dimensions: dims,
			MetricName: name,
			Namespace:  strings.TrimSpace(payload.Namespace),
			Timestamp:  timestamp.UTC(),
			Unit:       strings.TrimSpace(item.Unit),
			Value:      item.Value,
		})
	}
	if len(out) == 0 {
		return nil, validation("MetricData is required")
	}
	return out, nil
}

func parseDimensions(form url.Values, prefix string) []dimension {
	out := make([]dimension, 0)
	for idx := 1; ; idx++ {
		base := prefix + strconv.Itoa(idx) + "."
		name := form.Get(base + "Name")
		value := form.Get(base + "Value")
		if name == "" && value == "" {
			break
		}
		out = append(out, dimension{Name: name, Value: value})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func requestedStatistics(form url.Values) []string {
	out := make([]string, 0)
	for idx := 1; ; idx++ {
		value := form.Get("Statistics.member." + strconv.Itoa(idx))
		if value == "" {
			break
		}
		out = append(out, value)
	}
	if len(out) == 0 {
		return []string{"Average"}
	}
	return out
}

func metricIdentity(item metricDatum) string {
	parts := []string{item.Namespace, item.MetricName}
	for _, dim := range item.Dimensions {
		parts = append(parts, dim.Name+"="+dim.Value)
	}
	return strings.Join(parts, "|")
}

func containsDimensions(metric, filters []dimension) bool {
	if len(filters) == 0 {
		return true
	}
	lookup := map[string]string{}
	for _, dim := range metric {
		lookup[dim.Name] = dim.Value
	}
	for _, filter := range filters {
		if lookup[filter.Name] != filter.Value {
			return false
		}
	}
	return true
}

func sameDimensions(left, right []dimension) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func parseTime(raw string) (time.Time, error) {
	if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
		return parsed, nil
	}
	return time.Parse(time.RFC3339Nano, raw)
}

func parseForm(r *http.Request) (url.Values, error) {
	form, err := awscompat.ParseQueryForm(r)
	if err != nil {
		return nil, validation("request body is not valid form data")
	}
	return form, nil
}

func parseJSONBody[T any](r *http.Request) (T, bool, error) {
	var zero T
	if r == nil || r.Body == nil {
		return zero, false, nil
	}
	if !strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json") && strings.TrimSpace(r.Header.Get("X-Amz-Target")) == "" {
		return zero, false, nil
	}

	raw, err := io.ReadAll(r.Body)
	if err != nil {
		return zero, false, validation("request body is not valid json")
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return zero, false, nil
	}
	// Some AWS clients send query-style bodies even when headers suggest a
	// JSON target flow. Treat that as "not JSON" instead of surfacing a hard
	// JSON parse error so the caller can continue on the query-form path.
	if first := trimmed[0]; first != '{' && first != '[' {
		return zero, false, nil
	}

	var out T
	if err := json.Unmarshal(trimmed, &out); err != nil {
		return zero, false, validation("request body is not valid json")
	}
	return out, true, nil
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterValue", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalServiceError", Message: err.Error()}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeResponse(w http.ResponseWriter, r *http.Request, status int, xmlPayload, jsonPayload any) {
	if expectsJSON(r) {
		writeJSON(w, status, jsonPayload)
		return
	}
	writeXML(w, status, xmlPayload)
}

func expectsJSON(r *http.Request) bool {
	if r == nil {
		return false
	}
	if strings.TrimSpace(r.Header.Get("X-Amz-Target")) != "" {
		return true
	}
	return strings.Contains(strings.ToLower(r.Header.Get("Content-Type")), "json")
}
