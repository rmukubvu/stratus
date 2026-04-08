package elasticache

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/store"
)

const (
	namespace      = "http://elasticache.amazonaws.com/doc/2015-02-02/"
	clustersBucket = "elasticache-clusters"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

type cacheClusterRecord struct {
	Created  time.Time `json:"created"`
	Engine   string    `json:"engine"`
	ID       string    `json:"id"`
	NodeType string    `json:"node_type"`
	Nodes    int       `json:"nodes"`
	Status   string    `json:"status"`
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

func NewService(metadata store.Store) *Service {
	return &Service{metadata: metadata, now: time.Now}
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateCacheCluster":
		return s.createCacheCluster(w, r, requestID)
	case "DescribeCacheClusters":
		return s.describeCacheClusters(w, r, requestID)
	case "DeleteCacheCluster":
		return s.deleteCacheCluster(w, r, requestID)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "elasticache operation is not implemented"}
	}
}

func (s *Service) createCacheCluster(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	id := form.Get("CacheClusterId")
	if id == "" {
		return validation("CacheClusterId is required")
	}
	nodes := 1
	if raw := form.Get("NumCacheNodes"); raw != "" {
		if raw == "0" {
			return validation("NumCacheNodes must be greater than 0")
		}
	}
	record := cacheClusterRecord{
		Created:  s.now().UTC(),
		Engine:   defaultString(form.Get("Engine"), "redis"),
		ID:       id,
		NodeType: defaultString(form.Get("CacheNodeType"), "cache.t3.micro"),
		Nodes:    nodes,
		Status:   "available",
	}
	if err := s.putRecord(id, record); err != nil {
		return err
	}
	payload := struct {
		XMLName xml.Name `xml:"CreateCacheClusterResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			CacheCluster cacheClusterXML `xml:"CacheCluster"`
		} `xml:"CreateCacheClusterResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	payload.Result.CacheCluster = cacheClusterXMLFromRecord(record)
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) describeCacheClusters(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	id := form.Get("CacheClusterId")
	items := make([]cacheClusterRecord, 0)
	if id != "" {
		record, err := s.loadRecord(id)
		if err != nil {
			return err
		}
		items = append(items, record)
	} else {
		if err := s.metadata.Scan(clustersBucket, "", func(_, v []byte) error {
			var record cacheClusterRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return nil
			}
			items = append(items, record)
			return nil
		}); err != nil {
			return internal(err)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	payload := struct {
		XMLName xml.Name `xml:"DescribeCacheClustersResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			CacheClusters []cacheClusterXML `xml:"CacheClusters>CacheCluster"`
		} `xml:"DescribeCacheClustersResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	for _, item := range items {
		payload.Result.CacheClusters = append(payload.Result.CacheClusters, cacheClusterXMLFromRecord(item))
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) deleteCacheCluster(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadRecord(form.Get("CacheClusterId"))
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(clustersBucket, record.ID); err != nil {
		return internal(err)
	}
	payload := struct {
		XMLName xml.Name `xml:"DeleteCacheClusterResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			CacheCluster cacheClusterXML `xml:"CacheCluster"`
		} `xml:"DeleteCacheClusterResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	payload.Result.CacheCluster = cacheClusterXMLFromRecord(record)
	writeXML(w, http.StatusOK, payload)
	return nil
}

type cacheClusterXML struct {
	CacheClusterId     string `xml:"CacheClusterId"`
	CacheClusterStatus string `xml:"CacheClusterStatus"`
	CacheNodeType      string `xml:"CacheNodeType"`
	Engine             string `xml:"Engine"`
	NumCacheNodes      int    `xml:"NumCacheNodes"`
}

func cacheClusterXMLFromRecord(record cacheClusterRecord) cacheClusterXML {
	return cacheClusterXML{
		CacheClusterId:     record.ID,
		CacheClusterStatus: record.Status,
		CacheNodeType:      record.NodeType,
		Engine:             record.Engine,
		NumCacheNodes:      record.Nodes,
	}
}

func (s *Service) loadRecord(id string) (cacheClusterRecord, error) {
	raw, err := s.metadata.Get(clustersBucket, id)
	if err != nil {
		return cacheClusterRecord{}, internal(err)
	}
	if raw == nil {
		return cacheClusterRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "CacheClusterNotFound", Message: "cache cluster not found"}
	}
	var record cacheClusterRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return cacheClusterRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) putRecord(id string, record cacheClusterRecord) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(clustersBucket, id, raw); err != nil {
		return internal(err)
	}
	return nil
}

func parseForm(r *http.Request) (url.Values, error) {
	form, err := awscompat.ParseQueryForm(r)
	if err != nil {
		return nil, validation("request body is not valid form data")
	}
	return form, nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "InvalidParameterValue", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}
