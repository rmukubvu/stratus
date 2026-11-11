// Package elasticache emulates Amazon ElastiCache. Every cache cluster is a
// real cache engine running in a container, so clients connect to the node
// endpoint the API returns and use the real protocol.
package elasticache

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store"
)

const (
	namespace      = "http://elasticache.amazonaws.com/doc/2015-02-02/"
	clustersBucket = "elasticache-clusters"

	// kind labels the engine containers this service owns.
	kind = "elasticache"
)

// Cluster statuses, as ElastiCache reports them.
const (
	statusAvailable = "available"
	statusDeleting  = "deleting"
	statusFailed    = "failed"
)

// Options wires the service to its adapters.
type Options struct {
	Metadata store.Store
	// Engines runs the cache engines. Nil means no container engine is
	// available, so creating a cluster fails with a clear error.
	Engines backing.Runner
	// Catalog lists the supported engines. Zero means DefaultEngines without
	// image overrides.
	Catalog Engines
	Now     func() time.Time
}

type Service struct {
	scope    scope.Scope
	metadata store.Store
	engines  backing.Runner
	catalog  Engines
	now      func() time.Time
	mu       sync.Mutex
}

// cacheClusterRecord is the persisted state of one cluster. Stratus runs a
// single node per cluster, published at Address:Port.
type cacheClusterRecord struct {
	Address       string    `json:"address"`
	Created       time.Time `json:"created"`
	Engine        string    `json:"engine"`
	EngineVersion string    `json:"engine_version"`
	ID            string    `json:"id"`
	NodeType      string    `json:"node_type"`
	Nodes         int       `json:"nodes"`
	Port          int       `json:"port"`
	Status        string    `json:"status"`
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

// NewService builds an ElastiCache service.
func NewService(opts Options) *Service {
	engines := opts.Engines
	if engines == nil {
		engines = backing.Unavailable{}
	}
	catalog := opts.Catalog
	if catalog.byName == nil {
		catalog = DefaultEngines(nil)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{scope: scope.Default(), metadata: opts.Metadata, engines: engines, catalog: catalog, now: now}
}

// Service names this backend for the control plane.
func (s *Service) Service() string { return kind }

// Reset stops every engine. The control plane calls it after dropping the
// service's metadata, so no cluster is left running without a record. Without a
// container engine there is nothing reachable to stop, and resetting state
// must still work, so that case is not an error.
func (s *Service) Reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.engines.StopAll(ctx, kind); err != nil && !errors.Is(err, backing.ErrUnavailable) {
		return err
	}
	return nil
}

// Reconcile starts an engine for every recorded cluster that has none. It
// runs after a snapshot restore and at startup. Each engine keeps its recorded
// endpoint unless opts asks for fresh ones, and starts from its archive in
// opts.DataDir when there is one. A cluster whose engine cannot start is
// marked failed; the error names them all.
func (s *Service) Reconcile(ctx context.Context, opts backing.ReconcileOptions) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	records, err := s.listClusters()
	if err != nil {
		return err
	}
	var errs []error
	for _, record := range records {
		running, err := s.engines.Running(ctx, kind, record.ID)
		if err == nil && running {
			continue
		}
		if err == nil {
			hostPort := record.Port
			if opts.FreshEndpoints {
				hostPort = 0
			}
			err = s.startEngine(ctx, &record, hostPort, backing.SeedFrom(opts.DataDir, record.ID))
		}
		if err != nil {
			record.Status = statusFailed
			errs = append(errs, fmt.Errorf("elasticache: start engine for %s: %w", record.ID, err))
		}
		if err := s.putRecord(record.ID, record); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// Capture exports the data of every running cluster whose engine keeps any
// into dir, one archive per cluster, and returns the cluster IDs it captured.
func (s *Service) Capture(ctx context.Context, dir string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	records, err := s.listClusters()
	if err != nil {
		return nil, err
	}
	engines := make([]backing.Exportable, 0, len(records))
	for _, record := range records {
		engine, ok := s.catalog.Lookup(record.Engine)
		if !ok {
			continue
		}
		if data, keeps := engine.Data(); keeps {
			engines = append(engines, backing.Exportable{Name: record.ID, Data: data})
		}
	}
	return backing.ExportAll(ctx, s.engines, kind, dir, engines)
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

// createCacheCluster starts a real engine and returns once it answers, so
// the cluster is available as soon as the call returns.
func (s *Service) createCacheCluster(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.clusterFromForm(form)
	if err != nil {
		return err
	}
	if existing, _ := s.metadata.Get(clustersBucket, record.ID); existing != nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "CacheClusterAlreadyExists", Message: "cache cluster already exists: " + record.ID}
	}
	if err := s.startEngine(r.Context(), &record, 0, ""); err != nil {
		return backing.APIError(err, "cache cluster "+record.ID)
	}
	if err := s.putRecord(record.ID, record); err != nil {
		_ = s.engines.Stop(context.Background(), kind, record.ID)
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
	payload.Result.CacheCluster = s.cacheClusterXMLFromRecord(record)
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) clusterFromForm(form url.Values) (cacheClusterRecord, error) {
	id := form.Get("CacheClusterId")
	if id == "" {
		return cacheClusterRecord{}, validation("CacheClusterId is required")
	}
	if form.Get("NumCacheNodes") == "0" {
		return cacheClusterRecord{}, validation("NumCacheNodes must be greater than 0")
	}
	engineName := defaultString(form.Get("Engine"), "redis")
	engine, ok := s.catalog.Lookup(engineName)
	if !ok {
		return cacheClusterRecord{}, validation(s.catalog.unsupported(engineName))
	}
	return cacheClusterRecord{
		Created:       s.now().UTC(),
		Engine:        engine.Name,
		EngineVersion: defaultString(form.Get("EngineVersion"), engine.DefaultVersion),
		ID:            id,
		NodeType:      defaultString(form.Get("CacheNodeType"), "cache.t3.micro"),
		Nodes:         1,
	}, nil
}

// startEngine runs the cluster's engine on hostPort (zero picks one), from
// the data archive seed when it is set, and records where it listens.
func (s *Service) startEngine(ctx context.Context, record *cacheClusterRecord, hostPort int, seed string) error {
	engine, ok := s.catalog.Lookup(record.Engine)
	if !ok {
		return errors.New(s.catalog.unsupported(record.Engine))
	}
	endpoint, err := s.engines.Start(ctx, engine.Spec(record.ID, hostPort, seed))
	if err != nil {
		return err
	}
	record.Address = endpoint.Address
	record.Port = endpoint.Port
	record.Status = statusAvailable
	return nil
}

func (s *Service) describeCacheClusters(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	id := form.Get("CacheClusterId")
	var items []cacheClusterRecord
	if id != "" {
		record, err := s.loadRecord(id)
		if err != nil {
			return err
		}
		items = append(items, record)
	} else {
		items, err = s.listClusters()
		if err != nil {
			return err
		}
	}
	payload := struct {
		XMLName xml.Name `xml:"DescribeCacheClustersResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			CacheClusters []cacheClusterXML `xml:"CacheClusters>CacheCluster"`
		} `xml:"DescribeCacheClustersResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	for _, item := range items {
		payload.Result.CacheClusters = append(payload.Result.CacheClusters, s.cacheClusterXMLFromRecord(item))
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

// deleteCacheCluster stops the engine and forgets the cluster. The record is
// removed even when the container engine is unreachable, so a lost Docker
// never leaves a cluster that cannot be deleted.
func (s *Service) deleteCacheCluster(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadRecord(form.Get("CacheClusterId"))
	if err != nil {
		return err
	}
	if err := s.engines.Stop(r.Context(), kind, record.ID); err != nil && !errors.Is(err, backing.ErrUnavailable) {
		return backing.APIError(err, "cache cluster "+record.ID)
	}
	if err := s.metadata.Delete(clustersBucket, record.ID); err != nil {
		return internal(err)
	}
	record.Status = statusDeleting
	payload := struct {
		XMLName xml.Name `xml:"DeleteCacheClusterResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			CacheCluster cacheClusterXML `xml:"CacheCluster"`
		} `xml:"DeleteCacheClusterResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	payload.Result.CacheCluster = s.cacheClusterXMLFromRecord(record)
	writeXML(w, http.StatusOK, payload)
	return nil
}

type endpointXML struct {
	Address string `xml:"Address"`
	Port    int    `xml:"Port"`
}

type cacheNodeXML struct {
	CacheNodeId         string      `xml:"CacheNodeId"`
	CacheNodeStatus     string      `xml:"CacheNodeStatus"`
	CacheNodeCreateTime time.Time   `xml:"CacheNodeCreateTime"`
	Endpoint            endpointXML `xml:"Endpoint"`
}

type cacheClusterXML struct {
	ARN                    string         `xml:"ARN"`
	CacheClusterId         string         `xml:"CacheClusterId"`
	CacheClusterStatus     string         `xml:"CacheClusterStatus"`
	CacheClusterCreateTime time.Time      `xml:"CacheClusterCreateTime"`
	CacheNodeType          string         `xml:"CacheNodeType"`
	Engine                 string         `xml:"Engine"`
	EngineVersion          string         `xml:"EngineVersion"`
	NumCacheNodes          int            `xml:"NumCacheNodes"`
	ConfigurationEndpoint  *endpointXML   `xml:"ConfigurationEndpoint,omitempty"`
	CacheNodes             []cacheNodeXML `xml:"CacheNodes>CacheNode"`
}

func (s *Service) cacheClusterXMLFromRecord(record cacheClusterRecord) cacheClusterXML {
	endpoint := endpointXML{Address: record.Address, Port: record.Port}
	out := cacheClusterXML{
		ARN:                    fmt.Sprintf("arn:aws:elasticache:%s:%s:cluster:%s", s.scope.Region, s.scope.Account, record.ID),
		CacheClusterId:         record.ID,
		CacheClusterStatus:     record.Status,
		CacheClusterCreateTime: record.Created,
		CacheNodeType:          record.NodeType,
		Engine:                 record.Engine,
		EngineVersion:          record.EngineVersion,
		NumCacheNodes:          record.Nodes,
	}
	if record.Address != "" {
		out.CacheNodes = []cacheNodeXML{{
			CacheNodeId:         "0001",
			CacheNodeStatus:     record.Status,
			CacheNodeCreateTime: record.Created,
			Endpoint:            endpoint,
		}}
		// Memcached clients discover nodes through the configuration endpoint.
		if record.Engine == "memcached" {
			out.ConfigurationEndpoint = &endpoint
		}
	}
	return out
}

func (s *Service) loadRecord(id string) (cacheClusterRecord, error) {
	raw, err := s.metadata.Get(clustersBucket, id)
	if err != nil {
		return cacheClusterRecord{}, internal(err)
	}
	if raw == nil {
		return cacheClusterRecord{}, &apierror.Error{StatusCode: http.StatusNotFound, Code: "CacheClusterNotFound", Message: "cache cluster not found"}
	}
	var record cacheClusterRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return cacheClusterRecord{}, internal(err)
	}
	return record, nil
}

func (s *Service) listClusters() ([]cacheClusterRecord, error) {
	items := make([]cacheClusterRecord, 0)
	if err := s.metadata.Scan(clustersBucket, "", func(_, v []byte) error {
		var record cacheClusterRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return items, nil
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

// SetScope sets the account and region this service instance serves.
func (s *Service) SetScope(sc scope.Scope) {
	s.scope = sc
}
