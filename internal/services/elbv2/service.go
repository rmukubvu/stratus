package elbv2

import (
	"encoding/json"
	"encoding/xml"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/store"
)

const (
	namespace           = "http://elasticloadbalancing.amazonaws.com/doc/2015-12-01/"
	loadBalancersBucket = "elbv2-load-balancers"
	targetGroupsBucket  = "elbv2-target-groups"
	listenersBucket     = "elbv2-listeners"
	targetsBucket       = "elbv2-targets"
	accountID           = "000000000000"
	region              = "us-east-1"
)

type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

type loadBalancerRecord struct {
	Arn     string    `json:"arn"`
	Created time.Time `json:"created"`
	DNSName string    `json:"dns_name"`
	Name    string    `json:"name"`
	Scheme  string    `json:"scheme"`
	State   string    `json:"state"`
	Type    string    `json:"type"`
}

type targetGroupRecord struct {
	Arn        string    `json:"arn"`
	Created    time.Time `json:"created"`
	Name       string    `json:"name"`
	Port       int       `json:"port"`
	Protocol   string    `json:"protocol"`
	TargetType string    `json:"target_type"`
	VpcID      string    `json:"vpc_id"`
}

type listenerRecord struct {
	Arn              string    `json:"arn"`
	Created          time.Time `json:"created"`
	DefaultTargetArn string    `json:"default_target_arn"`
	LoadBalancerArn  string    `json:"load_balancer_arn"`
	Port             int       `json:"port"`
	Protocol         string    `json:"protocol"`
}

type targetRecord struct {
	Health string `json:"health"`
	ID     string `json:"id"`
	Port   int    `json:"port"`
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
	case "CreateLoadBalancer":
		return s.createLoadBalancer(w, r, requestID)
	case "DescribeLoadBalancers":
		return s.describeLoadBalancers(w, r, requestID)
	case "CreateTargetGroup":
		return s.createTargetGroup(w, r, requestID)
	case "DescribeTargetGroups":
		return s.describeTargetGroups(w, r, requestID)
	case "CreateListener":
		return s.createListener(w, r, requestID)
	case "DescribeListeners":
		return s.describeListeners(w, r, requestID)
	case "RegisterTargets":
		return s.registerTargets(w, r, requestID)
	case "DescribeTargetHealth":
		return s.describeTargetHealth(w, r, requestID)
	default:
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "elbv2 operation is not implemented"}
	}
}

func (s *Service) createLoadBalancer(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	name := form.Get("Name")
	if name == "" {
		return validation("Name is required")
	}
	lbType := form.Get("Type")
	if lbType == "" {
		lbType = "application"
	}
	if lbType != "application" {
		return notImplemented("only application load balancers are supported")
	}
	record := loadBalancerRecord{
		Arn:     loadBalancerARN(name),
		Created: s.now().UTC(),
		DNSName: name + "-" + uuid.NewString()[:8] + "." + region + ".elb.amazonaws.com",
		Name:    name,
		Scheme:  defaultString(form.Get("Scheme"), "internet-facing"),
		State:   "active",
		Type:    lbType,
	}
	if existing, err := s.loadLoadBalancer(name); err == nil {
		record = existing
	} else if err := s.putRecord(loadBalancersBucket, name, record); err != nil {
		return err
	}
	type loadBalancerDescription struct {
		LoadBalancerArn  string `xml:"LoadBalancerArn"`
		DNSName          string `xml:"DNSName"`
		LoadBalancerName string `xml:"LoadBalancerName"`
		Scheme           string `xml:"Scheme"`
		State            struct {
			Code string `xml:"Code"`
		} `xml:"State"`
		Type string `xml:"Type"`
	}
	payload := struct {
		XMLName xml.Name `xml:"CreateLoadBalancerResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			LoadBalancers []loadBalancerDescription `xml:"LoadBalancers>member"`
		} `xml:"CreateLoadBalancerResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	item := loadBalancerDescription{
		LoadBalancerArn:  record.Arn,
		DNSName:          record.DNSName,
		LoadBalancerName: record.Name,
		Scheme:           record.Scheme,
		Type:             record.Type,
	}
	item.State.Code = record.State
	payload.Result.LoadBalancers = []loadBalancerDescription{item}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) describeLoadBalancers(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	names := indexedValues(form, "Names.member.")
	items := make([]loadBalancerRecord, 0)
	if len(names) > 0 {
		for _, name := range names {
			record, err := s.loadLoadBalancer(name)
			if err != nil {
				return err
			}
			items = append(items, record)
		}
	} else {
		if err := s.metadata.Scan(loadBalancersBucket, "", func(_, v []byte) error {
			var record loadBalancerRecord
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
	type loadBalancerDescription struct {
		LoadBalancerArn  string `xml:"LoadBalancerArn"`
		DNSName          string `xml:"DNSName"`
		LoadBalancerName string `xml:"LoadBalancerName"`
		Scheme           string `xml:"Scheme"`
		State            struct {
			Code string `xml:"Code"`
		} `xml:"State"`
		Type string `xml:"Type"`
	}
	payload := struct {
		XMLName xml.Name `xml:"DescribeLoadBalancersResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			LoadBalancers []loadBalancerDescription `xml:"LoadBalancers>member"`
		} `xml:"DescribeLoadBalancersResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	for _, record := range items {
		item := loadBalancerDescription{
			LoadBalancerArn:  record.Arn,
			DNSName:          record.DNSName,
			LoadBalancerName: record.Name,
			Scheme:           record.Scheme,
			Type:             record.Type,
		}
		item.State.Code = record.State
		payload.Result.LoadBalancers = append(payload.Result.LoadBalancers, item)
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) createTargetGroup(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	name := form.Get("Name")
	if name == "" {
		return validation("Name is required")
	}
	port, _ := strconv.Atoi(defaultString(form.Get("Port"), "80"))
	record := targetGroupRecord{
		Arn:        targetGroupARN(name),
		Created:    s.now().UTC(),
		Name:       name,
		Port:       port,
		Protocol:   defaultString(form.Get("Protocol"), "HTTP"),
		TargetType: defaultString(form.Get("TargetType"), "ip"),
		VpcID:      defaultString(form.Get("VpcId"), "vpc-12345678"),
	}
	if existing, err := s.loadTargetGroup(name); err == nil {
		record = existing
	} else if err := s.putRecord(targetGroupsBucket, name, record); err != nil {
		return err
	}
	type targetGroupDescription struct {
		Port            int    `xml:"Port"`
		Protocol        string `xml:"Protocol"`
		TargetGroupArn  string `xml:"TargetGroupArn"`
		TargetGroupName string `xml:"TargetGroupName"`
		TargetType      string `xml:"TargetType"`
		VpcId           string `xml:"VpcId"`
	}
	payload := struct {
		XMLName xml.Name `xml:"CreateTargetGroupResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			TargetGroups []targetGroupDescription `xml:"TargetGroups>member"`
		} `xml:"CreateTargetGroupResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	payload.Result.TargetGroups = []targetGroupDescription{{
		Port:            record.Port,
		Protocol:        record.Protocol,
		TargetGroupArn:  record.Arn,
		TargetGroupName: record.Name,
		TargetType:      record.TargetType,
		VpcId:           record.VpcID,
	}}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) describeTargetGroups(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	names := indexedValues(form, "Names.member.")
	items := make([]targetGroupRecord, 0)
	if len(names) > 0 {
		for _, name := range names {
			record, err := s.loadTargetGroup(name)
			if err != nil {
				return err
			}
			items = append(items, record)
		}
	} else {
		if err := s.metadata.Scan(targetGroupsBucket, "", func(_, v []byte) error {
			var record targetGroupRecord
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
	type targetGroupDescription struct {
		Port            int    `xml:"Port"`
		Protocol        string `xml:"Protocol"`
		TargetGroupArn  string `xml:"TargetGroupArn"`
		TargetGroupName string `xml:"TargetGroupName"`
		TargetType      string `xml:"TargetType"`
		VpcId           string `xml:"VpcId"`
	}
	payload := struct {
		XMLName xml.Name `xml:"DescribeTargetGroupsResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			TargetGroups []targetGroupDescription `xml:"TargetGroups>member"`
		} `xml:"DescribeTargetGroupsResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	for _, record := range items {
		payload.Result.TargetGroups = append(payload.Result.TargetGroups, targetGroupDescription{
			Port:            record.Port,
			Protocol:        record.Protocol,
			TargetGroupArn:  record.Arn,
			TargetGroupName: record.Name,
			TargetType:      record.TargetType,
			VpcId:           record.VpcID,
		})
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) createListener(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	lbArn := form.Get("LoadBalancerArn")
	port, _ := strconv.Atoi(defaultString(form.Get("Port"), "80"))
	if lbArn == "" {
		return validation("LoadBalancerArn is required")
	}
	targetArn := form.Get("DefaultActions.member.1.TargetGroupArn")
	if form.Get("DefaultActions.member.1.Type") != "forward" {
		return notImplemented("only forward listeners are supported")
	}
	record := listenerRecord{
		Arn:              listenerARN(),
		Created:          s.now().UTC(),
		DefaultTargetArn: targetArn,
		LoadBalancerArn:  lbArn,
		Port:             port,
		Protocol:         defaultString(form.Get("Protocol"), "HTTP"),
	}
	if err := s.putRecord(listenersBucket, record.Arn, record); err != nil {
		return err
	}
	type listenerDescription struct {
		ListenerArn     string `xml:"ListenerArn"`
		LoadBalancerArn string `xml:"LoadBalancerArn"`
		Port            int    `xml:"Port"`
		Protocol        string `xml:"Protocol"`
		DefaultActions  []struct {
			TargetGroupArn string `xml:"TargetGroupArn"`
			Type           string `xml:"Type"`
		} `xml:"DefaultActions>member"`
	}
	payload := struct {
		XMLName xml.Name `xml:"CreateListenerResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			Listeners []listenerDescription `xml:"Listeners>member"`
		} `xml:"CreateListenerResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	item := listenerDescription{
		ListenerArn:     record.Arn,
		LoadBalancerArn: record.LoadBalancerArn,
		Port:            record.Port,
		Protocol:        record.Protocol,
	}
	item.DefaultActions = []struct {
		TargetGroupArn string `xml:"TargetGroupArn"`
		Type           string `xml:"Type"`
	}{{TargetGroupArn: record.DefaultTargetArn, Type: "forward"}}
	payload.Result.Listeners = []listenerDescription{item}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) describeListeners(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	lbArn := form.Get("LoadBalancerArn")
	type listenerDescription struct {
		ListenerArn     string `xml:"ListenerArn"`
		LoadBalancerArn string `xml:"LoadBalancerArn"`
		Port            int    `xml:"Port"`
		Protocol        string `xml:"Protocol"`
		DefaultActions  []struct {
			TargetGroupArn string `xml:"TargetGroupArn"`
			Type           string `xml:"Type"`
		} `xml:"DefaultActions>member"`
	}
	payload := struct {
		XMLName xml.Name `xml:"DescribeListenersResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			Listeners []listenerDescription `xml:"Listeners>member"`
		} `xml:"DescribeListenersResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	if err := s.metadata.Scan(listenersBucket, "", func(_, v []byte) error {
		var record listenerRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if lbArn != "" && record.LoadBalancerArn != lbArn {
			return nil
		}
		item := listenerDescription{
			ListenerArn:     record.Arn,
			LoadBalancerArn: record.LoadBalancerArn,
			Port:            record.Port,
			Protocol:        record.Protocol,
		}
		item.DefaultActions = []struct {
			TargetGroupArn string `xml:"TargetGroupArn"`
			Type           string `xml:"Type"`
		}{{TargetGroupArn: record.DefaultTargetArn, Type: "forward"}}
		payload.Result.Listeners = append(payload.Result.Listeners, item)
		return nil
	}); err != nil {
		return internal(err)
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) registerTargets(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	targetGroupArn := form.Get("TargetGroupArn")
	if targetGroupArn == "" {
		return validation("TargetGroupArn is required")
	}
	if _, err := s.loadTargetGroupByARN(targetGroupArn); err != nil {
		return err
	}
	for idx := 1; ; idx++ {
		id := form.Get(fmt.Sprintf("Targets.member.%d.Id", idx))
		if id == "" {
			break
		}
		port, _ := strconv.Atoi(defaultString(form.Get(fmt.Sprintf("Targets.member.%d.Port", idx)), "80"))
		record := targetRecord{Health: "healthy", ID: id, Port: port}
		if err := s.putRecord(targetsBucket, targetStoreKey(targetGroupArn, id), record); err != nil {
			return err
		}
	}
	payload := struct {
		XMLName          xml.Name         `xml:"RegisterTargetsResponse"`
		XMLNS            string           `xml:"xmlns,attr"`
		Result           struct{}         `xml:"RegisterTargetsResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) describeTargetHealth(w http.ResponseWriter, r *http.Request, requestID string) error {
	form, err := parseForm(r)
	if err != nil {
		return err
	}
	targetGroupArn := form.Get("TargetGroupArn")
	if targetGroupArn == "" {
		return validation("TargetGroupArn is required")
	}
	type targetHealthDescription struct {
		Target struct {
			ID   string `xml:"Id"`
			Port int    `xml:"Port"`
		} `xml:"Target"`
		TargetHealth struct {
			State string `xml:"State"`
		} `xml:"TargetHealth"`
	}
	payload := struct {
		XMLName xml.Name `xml:"DescribeTargetHealthResponse"`
		XMLNS   string   `xml:"xmlns,attr"`
		Result  struct {
			Descriptions []targetHealthDescription `xml:"TargetHealthDescriptions>member"`
		} `xml:"DescribeTargetHealthResult"`
		ResponseMetadata responseMetadata `xml:"ResponseMetadata"`
	}{XMLNS: namespace, ResponseMetadata: responseMetadata{RequestID: requestID}}
	if err := s.metadata.Scan(targetsBucket, targetGroupArn+"|", func(_, v []byte) error {
		var record targetRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		var item targetHealthDescription
		item.Target.ID = record.ID
		item.Target.Port = record.Port
		item.TargetHealth.State = record.Health
		payload.Result.Descriptions = append(payload.Result.Descriptions, item)
		return nil
	}); err != nil {
		return internal(err)
	}
	writeXML(w, http.StatusOK, payload)
	return nil
}

func (s *Service) loadLoadBalancer(name string) (loadBalancerRecord, error) {
	return loadRecord[loadBalancerRecord](s.metadata, loadBalancersBucket, name, "LoadBalancerNotFound", "load balancer not found")
}

func (s *Service) loadTargetGroup(name string) (targetGroupRecord, error) {
	return loadRecord[targetGroupRecord](s.metadata, targetGroupsBucket, name, "TargetGroupNotFound", "target group not found")
}

func (s *Service) loadTargetGroupByARN(arn string) (targetGroupRecord, error) {
	var found targetGroupRecord
	matched := false
	if err := s.metadata.Scan(targetGroupsBucket, "", func(_, v []byte) error {
		var record targetGroupRecord
		if err := json.Unmarshal(v, &record); err != nil {
			return nil
		}
		if record.Arn == arn {
			found = record
			matched = true
		}
		return nil
	}); err != nil {
		return targetGroupRecord{}, internal(err)
	}
	if !matched {
		return targetGroupRecord{}, &apierror.Error{StatusCode: http.StatusBadRequest, Code: "TargetGroupNotFound", Message: "target group not found"}
	}
	return found, nil
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

func targetStoreKey(groupARN, id string) string {
	return groupARN + "|" + id
}

func loadBalancerARN(name string) string {
	return "arn:aws:elasticloadbalancing:" + region + ":" + accountID + ":loadbalancer/app/" + name + "/" + uuid.NewString()[:8]
}

func targetGroupARN(name string) string {
	return "arn:aws:elasticloadbalancing:" + region + ":" + accountID + ":targetgroup/" + name + "/" + uuid.NewString()[:8]
}

func listenerARN() string {
	return "arn:aws:elasticloadbalancing:" + region + ":" + accountID + ":listener/app/" + uuid.NewString()
}

func validation(message string) error {
	return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "ValidationError", Message: message}
}

func notImplemented(message string) error {
	return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: message}
}

func internal(err error) error {
	return &apierror.Error{StatusCode: http.StatusInternalServerError, Code: "InternalFailure", Message: err.Error()}
}

func writeXML(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	_ = xml.NewEncoder(w).Encode(payload)
}
