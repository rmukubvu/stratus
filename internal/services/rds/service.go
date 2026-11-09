// Package rds emulates Amazon RDS. Every DB instance is a real database
// engine running in a container, so clients connect to the endpoint the API
// returns and run real SQL against it.
package rds

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/backing"
	"github.com/stratus/internal/store"
)

const (
	namespace          = "http://rds.amazonaws.com/doc/2014-10-31/"
	instancesBucket    = "rds-instances"
	subnetGroupsBucket = "rds-subnet-groups"

	// kind labels the engine containers this service owns.
	kind = "rds"

	accountID = "000000000000"
	region    = "us-east-1"
)

// Instance statuses, as RDS reports them.
const (
	statusAvailable = "available"
	statusDeleting  = "deleting"
	statusFailed    = "failed"
)

// Options wires the service to its adapters.
type Options struct {
	Metadata store.Store
	// Engines runs the database engines. Nil means no container engine is
	// available, so creating an instance fails with a clear error.
	Engines backing.Runner
	// Catalog lists the supported engines. Zero means DefaultEngines without
	// image overrides.
	Catalog Engines
	Now     func() time.Time
}

type Service struct {
	metadata store.Store
	engines  backing.Runner
	catalog  Engines
	now      func() time.Time
	mu       sync.Mutex
}

type subnetGroupRecord struct {
	Created     time.Time `json:"created"`
	Description string    `json:"description"`
	Name        string    `json:"name"`
	Subnets     []string  `json:"subnets"`
	VpcID       string    `json:"vpc_id"`
}

// instanceRecord is the persisted state of one DB instance. The master
// password is kept so the engine can be recreated with the same credentials
// after a restart or a snapshot restore.
type instanceRecord struct {
	Address            string    `json:"address"`
	AllocatedStorage   int       `json:"allocated_storage"`
	Class              string    `json:"class"`
	Created            time.Time `json:"created"`
	DBName             string    `json:"db_name"`
	Engine             string    `json:"engine"`
	EngineVersion      string    `json:"engine_version"`
	Identifier         string    `json:"identifier"`
	MasterUsername     string    `json:"master_username"`
	MasterUserPassword string    `json:"master_user_password"`
	Port               int       `json:"port"`
	Status             string    `json:"status"`
	SubnetGroup        string    `json:"subnet_group"`
}

type responseMetadata struct {
	RequestID string `xml:"RequestId"`
}

// NewService builds an RDS service.
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
	return &Service{metadata: opts.Metadata, engines: engines, catalog: catalog, now: now}
}

// Service names this backend for the control plane.
func (s *Service) Service() string { return kind }

// Reset stops every engine. The control plane calls it after dropping the
// service's metadata, so no instance is left running without a record. Without a
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

// Reconcile starts an engine for every recorded instance that has none,
// keeping each instance's endpoint. It runs after a snapshot restore and at
// startup, when the records exist but the containers do not. An instance
// whose engine cannot start is marked failed; the error names them all.
func (s *Service) Reconcile(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	records, err := s.listInstances()
	if err != nil {
		return err
	}
	var errs []error
	for _, record := range records {
		running, err := s.engines.Running(ctx, kind, record.Identifier)
		if err == nil && running {
			continue
		}
		if err == nil {
			err = s.startEngine(ctx, &record, record.Port)
		}
		if err != nil {
			record.Status = statusFailed
			errs = append(errs, fmt.Errorf("rds: start engine for %s: %w", record.Identifier, err))
		}
		if err := s.putRecord(instancesBucket, record.Identifier, record); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	switch operation {
	case "CreateDBSubnetGroup":
		return s.createDBSubnetGroup(w, r, requestID)
	case "DescribeDBSubnetGroups":
		return s.describeDBSubnetGroups(w, r, requestID)
	case "CreateDBInstance":
		return s.createDBInstance(w, r, requestID)
	case "DescribeDBInstances":
		return s.describeDBInstances(w, r, requestID)
	case "DeleteDBInstance":
		return s.deleteDBInstance(w, r, requestID)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "rds operation is not implemented"}
	}
}

func (s *Service) createDBSubnetGroup(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	name := form.Get("DBSubnetGroupName")
	if name == "" {
		return validation("DBSubnetGroupName is required")
	}
	record := subnetGroupRecord{
		Created:     s.now().UTC(),
		Description: form.Get("DBSubnetGroupDescription"),
		Name:        name,
		Subnets:     indexedValues(form, "SubnetIds.member."),
		VpcID:       "vpc-12345678",
	}
	if err := s.putRecord(subnetGroupsBucket, name, record); err != nil {
		return err
	}
	payload := struct {
		XMLName xml.Name `xml:"CreateDBSubnetGroupResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			DBSubnetGroup dbSubnetGroupXML `xml:"DBSubnetGroup"`
		} `xml:"CreateDBSubnetGroupResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	payload.Result.DBSubnetGroup = subnetGroupXML(record)
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) describeDBSubnetGroups(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	name := form.Get("DBSubnetGroupName")
	items := make([]subnetGroupRecord, 0)
	if name != "" {
		record, err := s.loadSubnetGroup(name)
		if err != nil {
			return err
		}
		items = append(items, record)
	} else {
		if err := s.metadata.Scan(subnetGroupsBucket, "", func(_, v []byte) error {
			var record subnetGroupRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return nil
			}
			items = append(items, record)
			return nil
		}); err != nil {
			return internal(err)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
	payload := struct {
		XMLName xml.Name `xml:"DescribeDBSubnetGroupsResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			DBSubnetGroups []dbSubnetGroupXML `xml:"DBSubnetGroups>DBSubnetGroup"`
		} `xml:"DescribeDBSubnetGroupsResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	for _, item := range items {
		payload.Result.DBSubnetGroups = append(payload.Result.DBSubnetGroups, subnetGroupXML(item))
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

// createDBInstance starts a real engine and returns once it accepts
// connections, so the instance is available as soon as the call returns.
func (s *Service) createDBInstance(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.instanceFromForm(form)
	if err != nil {
		return err
	}
	if existing, _ := s.metadata.Get(instancesBucket, record.Identifier); existing != nil {
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "DBInstanceAlreadyExists", Message: "DB instance already exists: " + record.Identifier}
	}
	if record.SubnetGroup != "" {
		if _, err := s.loadSubnetGroup(record.SubnetGroup); err != nil {
			return err
		}
	}
	if err := s.startEngine(r.Context(), &record, 0); err != nil {
		return backing.APIError(err, "DB instance "+record.Identifier)
	}
	if err := s.putRecord(instancesBucket, record.Identifier, record); err != nil {
		_ = s.engines.Stop(context.Background(), kind, record.Identifier)
		return err
	}
	payload := struct {
		XMLName xml.Name `xml:"CreateDBInstanceResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			DBInstance dbInstanceXML `xml:"DBInstance"`
		} `xml:"CreateDBInstanceResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	payload.Result.DBInstance = instanceXML(record)
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) instanceFromForm(form url.Values) (instanceRecord, error) {
	identifier := form.Get("DBInstanceIdentifier")
	if identifier == "" {
		return instanceRecord{}, validation("DBInstanceIdentifier is required")
	}
	engineName := defaultString(form.Get("Engine"), "postgres")
	engine, ok := s.catalog.Lookup(engineName)
	if !ok {
		return instanceRecord{}, invalidParameter(s.catalog.unsupported(engineName))
	}
	username := form.Get("MasterUsername")
	if username == "" {
		return instanceRecord{}, invalidParameter("The parameter MasterUsername must be provided and must not be blank.")
	}
	password := form.Get("MasterUserPassword")
	if password == "" {
		return instanceRecord{}, invalidParameter("The parameter MasterUserPassword must be provided and must not be blank.")
	}
	storage, _ := strconv.Atoi(defaultString(form.Get("AllocatedStorage"), "20"))
	creds := Credentials{Username: username, Password: password, Database: form.Get("DBName")}
	return instanceRecord{
		AllocatedStorage:   storage,
		Class:              defaultString(form.Get("DBInstanceClass"), "db.t3.micro"),
		Created:            s.now().UTC(),
		DBName:             engine.DatabaseName(creds),
		Engine:             engine.Name,
		EngineVersion:      defaultString(form.Get("EngineVersion"), engine.DefaultVersion),
		Identifier:         identifier,
		MasterUsername:     username,
		MasterUserPassword: password,
		SubnetGroup:        form.Get("DBSubnetGroupName"),
	}, nil
}

// startEngine runs the instance's engine on hostPort (zero picks one) and
// records where it listens.
func (s *Service) startEngine(ctx context.Context, record *instanceRecord, hostPort int) error {
	engine, ok := s.catalog.Lookup(record.Engine)
	if !ok {
		return errors.New(s.catalog.unsupported(record.Engine))
	}
	creds := Credentials{Username: record.MasterUsername, Password: record.MasterUserPassword, Database: record.DBName}
	endpoint, err := s.engines.Start(ctx, engine.Spec(record.Identifier, creds, hostPort))
	if err != nil {
		return err
	}
	record.Address = endpoint.Address
	record.Port = endpoint.Port
	record.Status = statusAvailable
	return nil
}

func (s *Service) describeDBInstances(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	identifier := form.Get("DBInstanceIdentifier")
	var items []instanceRecord
	if identifier != "" {
		record, err := s.loadInstance(identifier)
		if err != nil {
			return err
		}
		items = append(items, record)
	} else {
		items, err = s.listInstances()
		if err != nil {
			return err
		}
	}
	payload := struct {
		XMLName xml.Name `xml:"DescribeDBInstancesResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			DBInstances []dbInstanceXML `xml:"DBInstances>DBInstance"`
		} `xml:"DescribeDBInstancesResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	for _, item := range items {
		payload.Result.DBInstances = append(payload.Result.DBInstances, instanceXML(item))
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

// deleteDBInstance stops the engine and forgets the instance. The record is
// removed even when the container engine is unreachable, so a lost Docker
// never leaves an instance that cannot be deleted.
func (s *Service) deleteDBInstance(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadInstance(form.Get("DBInstanceIdentifier"))
	if err != nil {
		return err
	}
	if err := s.engines.Stop(r.Context(), kind, record.Identifier); err != nil && !errors.Is(err, backing.ErrUnavailable) {
		return backing.APIError(err, "DB instance "+record.Identifier)
	}
	if err := s.metadata.Delete(instancesBucket, record.Identifier); err != nil {
		return internal(err)
	}
	record.Status = statusDeleting
	payload := struct {
		XMLName xml.Name `xml:"DeleteDBInstanceResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			DBInstance dbInstanceXML `xml:"DBInstance"`
		} `xml:"DeleteDBInstanceResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	payload.Result.DBInstance = instanceXML(record)
	writeXML(w, http.StatusOK, payload)
	return nil
}

type dbSubnetGroupXML struct {
	DBSubnetGroupDescription string `xml:"DBSubnetGroupDescription"`
	DBSubnetGroupName        string `xml:"DBSubnetGroupName"`
	SubnetGroupStatus        string `xml:"SubnetGroupStatus"`
	VpcId                    string `xml:"VpcId"`
}

type endpointXML struct {
	Address      string `xml:"Address"`
	Port         int    `xml:"Port"`
	HostedZoneId string `xml:"HostedZoneId"`
}

type dbInstanceXML struct {
	AllocatedStorage     int         `xml:"AllocatedStorage"`
	DBInstanceArn        string      `xml:"DBInstanceArn"`
	DBInstanceClass      string      `xml:"DBInstanceClass"`
	DBInstanceIdentifier string      `xml:"DBInstanceIdentifier"`
	DBInstanceStatus     string      `xml:"DBInstanceStatus"`
	DBName               string      `xml:"DBName,omitempty"`
	Engine               string      `xml:"Engine"`
	EngineVersion        string      `xml:"EngineVersion"`
	Endpoint             endpointXML `xml:"Endpoint"`
	InstanceCreateTime   time.Time   `xml:"InstanceCreateTime"`
	MasterUsername       string      `xml:"MasterUsername"`
	PubliclyAccessible   bool        `xml:"PubliclyAccessible"`
	StorageEncrypted     bool        `xml:"StorageEncrypted"`
	DBSubnetGroup        *struct {
		DBSubnetGroupName string `xml:"DBSubnetGroupName"`
	} `xml:"DBSubnetGroup,omitempty"`
}

func subnetGroupXML(record subnetGroupRecord) dbSubnetGroupXML {
	return dbSubnetGroupXML{
		DBSubnetGroupDescription: record.Description,
		DBSubnetGroupName:        record.Name,
		SubnetGroupStatus:        "Complete",
		VpcId:                    record.VpcID,
	}
}

func instanceXML(record instanceRecord) dbInstanceXML {
	out := dbInstanceXML{
		AllocatedStorage:     record.AllocatedStorage,
		DBInstanceArn:        fmt.Sprintf("arn:aws:rds:%s:%s:db:%s", region, accountID, record.Identifier),
		DBInstanceClass:      record.Class,
		DBInstanceIdentifier: record.Identifier,
		DBInstanceStatus:     record.Status,
		DBName:               record.DBName,
		Engine:               record.Engine,
		EngineVersion:        record.EngineVersion,
		Endpoint:             endpointXML{Address: record.Address, Port: record.Port, HostedZoneId: "Z2R2ITUGPM61AM"},
		InstanceCreateTime:   record.Created,
		MasterUsername:       record.MasterUsername,
		PubliclyAccessible:   true,
	}
	if record.SubnetGroup != "" {
		out.DBSubnetGroup = &struct {
			DBSubnetGroupName string `xml:"DBSubnetGroupName"`
		}{DBSubnetGroupName: record.SubnetGroup}
	}
	return out
}

func (s *Service) loadSubnetGroup(name string) (subnetGroupRecord, error) {
	return loadRecord[subnetGroupRecord](s.metadata, subnetGroupsBucket, name, "DBSubnetGroupNotFoundFault", "db subnet group not found")
}

func (s *Service) loadInstance(identifier string) (instanceRecord, error) {
	return loadRecord[instanceRecord](s.metadata, instancesBucket, identifier, "DBInstanceNotFound", "db instance not found")
}

func (s *Service) listInstances() ([]instanceRecord, error) {
	items := make([]instanceRecord, 0)
	if err := s.metadata.Scan(instancesBucket, "", func(_, v []byte) error {
		var record instanceRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		items = append(items, record)
		return nil
	}); err != nil {
		return nil, internal(err)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Identifier < items[j].Identifier })
	return items, nil
}

func (s *Service) putRecord(bucket, key string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(bucket, key, raw); err != nil {
		return internal(err)
	}
	return nil
}

func loadRecord[T any](metadata store.Store, bucket, key, code, msg string) (T, error) {
	var zero T
	raw, err := metadata.Get(bucket, key)
	if err != nil {
		return zero, internal(err)
	}
	if raw == nil {
		return zero, &apierror.Error{StatusCode: http.StatusNotFound, Code: code, Message: msg}
	}
	var record T
	if err := json.Unmarshal(raw, &record); err != nil {
		return zero, internal(err)
	}
	return record, nil
}

func parseForm(r *http.Request) (url.Values, error) {
	form, err := awscompat.ParseQueryForm(r)
	if err != nil {
		return nil, validation("request body is not valid form data")
	}
	return form, nil
}

func indexedValues(form url.Values, prefix string) []string {
	items := make([]string, 0)
	for idx := 1; ; idx++ {
		value := form.Get(prefix + strconv.Itoa(idx))
		if value == "" {
			break
		}
		items = append(items, value)
	}
	return items
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationError", Message: message}
}

func invalidParameter(message string) error {
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
