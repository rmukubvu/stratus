package rds

import (
	"encoding/json"
	"encoding/xml"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/store"
)

const (
	namespace          = "http://rds.amazonaws.com/doc/2014-10-31/"
	instancesBucket    = "rds-instances"
	subnetGroupsBucket = "rds-subnet-groups"
)

type Service struct {
	metadata store.Store
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

type instanceRecord struct {
	AllocatedStorage int       `json:"allocated_storage"`
	Class            string    `json:"class"`
	Created          time.Time `json:"created"`
	Endpoint         string    `json:"endpoint"`
	Engine           string    `json:"engine"`
	Identifier       string    `json:"identifier"`
	Status           string    `json:"status"`
	SubnetGroup      string    `json:"subnet_group"`
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

func (s *Service) createDBInstance(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	identifier := form.Get("DBInstanceIdentifier")
	if identifier == "" {
		return validation("DBInstanceIdentifier is required")
	}
	storage, _ := strconv.Atoi(defaultString(form.Get("AllocatedStorage"), "20"))
	subnetGroup := form.Get("DBSubnetGroupName")
	if subnetGroup != "" {
		if _, err := s.loadSubnetGroup(subnetGroup); err != nil {
			return err
		}
	}
	record := instanceRecord{
		AllocatedStorage: storage,
		Class:            defaultString(form.Get("DBInstanceClass"), "db.t3.micro"),
		Created:          s.now().UTC(),
		Endpoint:         identifier + "." + "local." + "rds.amazonaws.com",
		Engine:           defaultString(form.Get("Engine"), "postgres"),
		Identifier:       identifier,
		Status:           "available",
		SubnetGroup:      subnetGroup,
	}
	if err := s.putRecord(instancesBucket, identifier, record); err != nil {
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

func (s *Service) describeDBInstances(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	identifier := form.Get("DBInstanceIdentifier")
	items := make([]instanceRecord, 0)
	if identifier != "" {
		record, err := s.loadInstance(identifier)
		if err != nil {
			return err
		}
		items = append(items, record)
	} else {
		if err := s.metadata.Scan(instancesBucket, "", func(_, v []byte) error {
			var record instanceRecord
			if err := json.Unmarshal(v, &record); err != nil {
				return nil
			}
			items = append(items, record)
			return nil
		}); err != nil {
			return internal(err)
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].Identifier < items[j].Identifier })
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

func (s *Service) deleteDBInstance(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	record, err := s.loadInstance(form.Get("DBInstanceIdentifier"))
	if err != nil {
		return err
	}
	if err := s.metadata.Delete(instancesBucket, record.Identifier); err != nil {
		return internal(err)
	}
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

type dbInstanceXML struct {
	AllocatedStorage     int    `xml:"AllocatedStorage"`
	DBInstanceClass      string `xml:"DBInstanceClass"`
	DBInstanceIdentifier string `xml:"DBInstanceIdentifier"`
	DBInstanceStatus     string `xml:"DBInstanceStatus"`
	Engine               string `xml:"Engine"`
	Endpoint             struct {
		Address string `xml:"Address"`
		Port    int    `xml:"Port"`
	} `xml:"Endpoint"`
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
		DBInstanceClass:      record.Class,
		DBInstanceIdentifier: record.Identifier,
		DBInstanceStatus:     record.Status,
		Engine:               record.Engine,
	}
	out.Endpoint.Address = record.Endpoint
	out.Endpoint.Port = 5432
	return out
}

func (s *Service) loadSubnetGroup(name string) (subnetGroupRecord, error) {
	return loadRecord[subnetGroupRecord](s.metadata, subnetGroupsBucket, name, "DBSubnetGroupNotFoundFault", "db subnet group not found")
}

func (s *Service) loadInstance(identifier string) (instanceRecord, error) {
	return loadRecord[instanceRecord](s.metadata, instancesBucket, identifier, "DBInstanceNotFound", "db instance not found")
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
		return zero, &apierror.Error{StatusCode: http.StatusBadRequest, Code: code, Message: msg}
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

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}
