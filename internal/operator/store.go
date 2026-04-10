package operator

import (
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	defaultActivityLimit = 1000
	defaultErrorLimit    = 250
)

type RequestRecord struct {
	Time         time.Time `json:"time"`
	RequestID    string    `json:"request_id"`
	Service      string    `json:"service"`
	Operation    string    `json:"operation"`
	Method       string    `json:"method"`
	Path         string    `json:"path"`
	Status       int       `json:"status"`
	DurationMS   int64     `json:"duration_ms"`
	ErrorCode    string    `json:"error_code,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
}

type ServiceCount struct {
	Service string `json:"service"`
	Count   int    `json:"count"`
}

type Overview struct {
	Endpoint      string          `json:"endpoint"`
	DataDir       string          `json:"data_dir"`
	LogLevel      string          `json:"log_level"`
	LogFormat     string          `json:"log_format"`
	StartedAt     time.Time       `json:"started_at"`
	UptimeSeconds int64           `json:"uptime_seconds"`
	TotalRequests int             `json:"total_requests"`
	Status2xx     int             `json:"status_2xx"`
	Status4xx     int             `json:"status_4xx"`
	Status5xx     int             `json:"status_5xx"`
	TopServices   []ServiceCount  `json:"top_services"`
	RecentErrors  []RequestRecord `json:"recent_errors"`
}

type Store struct {
	mu            sync.RWMutex
	started       time.Time
	endpoint      string
	dataDir       string
	logLevel      string
	logFormat     string
	activityLimit int
	errorLimit    int
	activity      []RequestRecord
	errors        []RequestRecord
}

func NewStore(endpoint, dataDir, logLevel, logFormat string) *Store {
	now := time.Now().UTC()
	return &Store{
		started:       now,
		endpoint:      endpoint,
		dataDir:       dataDir,
		logLevel:      logLevel,
		logFormat:     logFormat,
		activityLimit: defaultActivityLimit,
		errorLimit:    defaultErrorLimit,
	}
}

func (s *Store) Record(record RequestRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.activity = append([]RequestRecord{record}, s.activity...)
	if len(s.activity) > s.activityLimit {
		s.activity = s.activity[:s.activityLimit]
	}
	if record.Status >= 400 {
		s.errors = append([]RequestRecord{record}, s.errors...)
		if len(s.errors) > s.errorLimit {
			s.errors = s.errors[:s.errorLimit]
		}
	}
}

func (s *Store) Overview() Overview {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var status2xx, status4xx, status5xx int
	serviceCounts := map[string]int{}
	for _, record := range s.activity {
		switch {
		case record.Status >= 500:
			status5xx++
		case record.Status >= 400:
			status4xx++
		case record.Status >= 200:
			status2xx++
		}
		service := record.Service
		if service == "" {
			service = "unknown"
		}
		serviceCounts[service]++
	}

	topServices := make([]ServiceCount, 0, len(serviceCounts))
	for service, count := range serviceCounts {
		topServices = append(topServices, ServiceCount{Service: service, Count: count})
	}
	sort.Slice(topServices, func(i, j int) bool {
		if topServices[i].Count == topServices[j].Count {
			return topServices[i].Service < topServices[j].Service
		}
		return topServices[i].Count > topServices[j].Count
	})
	if len(topServices) > 8 {
		topServices = topServices[:8]
	}

	recentErrors := append(make([]RequestRecord, 0, len(s.errors)), s.errors...)
	if len(recentErrors) > 10 {
		recentErrors = recentErrors[:10]
	}

	return Overview{
		Endpoint:      s.endpoint,
		DataDir:       s.dataDir,
		LogLevel:      s.logLevel,
		LogFormat:     s.logFormat,
		StartedAt:     s.started,
		UptimeSeconds: int64(time.Since(s.started).Seconds()),
		TotalRequests: len(s.activity),
		Status2xx:     status2xx,
		Status4xx:     status4xx,
		Status5xx:     status5xx,
		TopServices:   topServices,
		RecentErrors:  recentErrors,
	}
}

type ActivityFilter struct {
	Service     string
	StatusClass string
	Query       string
	Limit       int
}

func (s *Store) Activity(filter ActivityFilter) []RequestRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	limit := filter.Limit
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	query := strings.ToLower(strings.TrimSpace(filter.Query))
	service := strings.TrimSpace(filter.Service)
	statusClass := strings.TrimSpace(filter.StatusClass)

	out := make([]RequestRecord, 0, limit)
	for _, record := range s.activity {
		if service != "" && record.Service != service {
			continue
		}
		if statusClass != "" {
			switch statusClass {
			case "2xx":
				if record.Status < 200 || record.Status >= 300 {
					continue
				}
			case "4xx":
				if record.Status < 400 || record.Status >= 500 {
					continue
				}
			case "5xx":
				if record.Status < 500 || record.Status >= 600 {
					continue
				}
			}
		}
		if query != "" {
			haystack := strings.ToLower(record.Service + " " + record.Operation + " " + record.Method + " " + record.Path + " " + record.RequestID + " " + record.ErrorCode + " " + record.ErrorMessage)
			if !strings.Contains(haystack, query) {
				continue
			}
		}
		out = append(out, record)
		if len(out) >= limit {
			break
		}
	}
	return out
}

func (s *Store) Errors(limit int) []RequestRecord {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 || limit > 200 {
		limit = 100
	}
	out := append(make([]RequestRecord, 0, len(s.errors)), s.errors...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}
