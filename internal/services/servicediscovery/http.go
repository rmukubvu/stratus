package servicediscovery

import (
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/stratus/internal/apierror"
)

// TargetPrefix is the X-Amz-Target prefix of Cloud Map's JSON API.
const TargetPrefix = "Route53AutoNaming_v20170314"

type namespaceRequest struct {
	Name             string `json:"Name"`
	Description      string `json:"Description"`
	CreatorRequestID string `json:"CreatorRequestId"`
	VPC              string `json:"Vpc"`
	Tags             []Tag  `json:"Tags"`
}

// Handle serves one Cloud Map JSON request.
func (d *Directory) Handle(w http.ResponseWriter, r *http.Request, operation string) error {
	switch operation {
	case "CreateHttpNamespace":
		return d.createNamespace(w, r, typeHTTP)
	case "CreatePrivateDnsNamespace":
		return d.createNamespace(w, r, typeDNSPrivate)
	case "CreatePublicDnsNamespace":
		return d.createNamespace(w, r, typeDNSPublic)
	case "GetNamespace":
		var in struct {
			ID string `json:"Id"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		ns, err := d.Namespace(in.ID)
		if err != nil {
			return err
		}
		return respond(w, map[string]any{"Namespace": d.namespaceJSON(ns)})
	case "ListNamespaces":
		namespaces, err := d.Namespaces()
		if err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(namespaces))
		for _, ns := range namespaces {
			out = append(out, d.namespaceJSON(ns))
		}
		return respond(w, map[string]any{"Namespaces": out})
	case "DeleteNamespace":
		var in struct {
			ID string `json:"Id"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		op, err := d.DeleteNamespace(in.ID)
		return respondOperation(w, op, err)
	case "CreateService":
		return d.createService(w, r)
	case "GetService":
		var in struct {
			ID string `json:"Id"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		svc, err := d.Service(in.ID)
		if err != nil {
			return err
		}
		return respond(w, map[string]any{"Service": d.serviceJSON(svc)})
	case "ListServices":
		return d.listServices(w, r)
	case "DeleteService":
		var in struct {
			ID string `json:"Id"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := d.DeleteService(in.ID); err != nil {
			return err
		}
		return respond(w, map[string]any{})
	case "RegisterInstance":
		var in struct {
			ServiceID        string            `json:"ServiceId"`
			InstanceID       string            `json:"InstanceId"`
			CreatorRequestID string            `json:"CreatorRequestId"`
			Attributes       map[string]string `json:"Attributes"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		op, err := d.RegisterInstance(in.ServiceID, in.InstanceID, in.CreatorRequestID, in.Attributes)
		return respondOperation(w, op, err)
	case "DeregisterInstance":
		var in instanceRef
		if err := decode(r, &in); err != nil {
			return err
		}
		op, err := d.DeregisterInstance(in.ServiceID, in.InstanceID)
		return respondOperation(w, op, err)
	case "GetInstance":
		var in instanceRef
		if err := decode(r, &in); err != nil {
			return err
		}
		inst, err := d.Instance(in.ServiceID, in.InstanceID)
		if err != nil {
			return err
		}
		return respond(w, map[string]any{"Instance": map[string]any{"Id": inst.ID, "CreatorRequestId": inst.CreatorRequestID, "Attributes": inst.Attributes}})
	case "ListInstances":
		var in struct {
			ServiceID string `json:"ServiceId"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		instances, err := d.Instances(in.ServiceID)
		if err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(instances))
		for _, inst := range instances {
			out = append(out, map[string]any{"Id": inst.ID, "Attributes": inst.Attributes})
		}
		return respond(w, map[string]any{"Instances": out})
	case "DiscoverInstances":
		return d.discover(w, r)
	case "GetInstancesHealthStatus":
		return d.healthStatus(w, r)
	case "UpdateInstanceCustomHealthStatus":
		var in struct {
			instanceRef
			Status string `json:"Status"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := d.SetCustomHealth(in.ServiceID, in.InstanceID, in.Status); err != nil {
			return err
		}
		return respond(w, map[string]any{})
	case "GetOperation":
		var in struct {
			OperationID string `json:"OperationId"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		op, err := d.GetOperation(in.OperationID)
		if err != nil {
			return err
		}
		return respond(w, map[string]any{"Operation": operationJSON(op)})
	case "ListOperations":
		ops, err := d.Operations()
		if err != nil {
			return err
		}
		out := make([]map[string]any, 0, len(ops))
		for _, op := range ops {
			out = append(out, map[string]any{"Id": op.ID, "Status": "SUCCESS"})
		}
		return respond(w, map[string]any{"Operations": out})
	case "ListTagsForResource":
		var in struct {
			ResourceARN string `json:"ResourceARN"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		tags, err := d.Tags(in.ResourceARN)
		if err != nil {
			return err
		}
		if tags == nil {
			tags = []Tag{}
		}
		return respond(w, map[string]any{"Tags": tags})
	case "TagResource":
		var in struct {
			ResourceARN string `json:"ResourceARN"`
			Tags        []Tag  `json:"Tags"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := d.SetTags(in.ResourceARN, in.Tags, nil); err != nil {
			return err
		}
		return respond(w, map[string]any{})
	case "UntagResource":
		var in struct {
			ResourceARN string   `json:"ResourceARN"`
			TagKeys     []string `json:"TagKeys"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := d.SetTags(in.ResourceARN, nil, in.TagKeys); err != nil {
			return err
		}
		return respond(w, map[string]any{})
	default:
		return &apierror.Error{StatusCode: http.StatusBadRequest, Code: "UnknownOperationException", Message: "Cloud Map operation " + operation + " is not implemented"}
	}
}

type instanceRef struct {
	ServiceID  string `json:"ServiceId"`
	InstanceID string `json:"InstanceId"`
}

func (d *Directory) createNamespace(w http.ResponseWriter, r *http.Request, kind string) error {
	var in namespaceRequest
	if err := decode(r, &in); err != nil {
		return err
	}
	op, err := d.CreateNamespace(NamespaceInput{Type: kind, Name: in.Name, Description: in.Description, CreatorRequestID: in.CreatorRequestID, VPC: in.VPC, Tags: in.Tags})
	return respondOperation(w, op, err)
}

func (d *Directory) createService(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name                    string          `json:"Name"`
		NamespaceID             string          `json:"NamespaceId"`
		Type                    string          `json:"Type"`
		Description             string          `json:"Description"`
		CreatorRequestID        string          `json:"CreatorRequestId"`
		DNSConfig               *DNSConfig      `json:"DnsConfig"`
		HealthCheckConfig       json.RawMessage `json:"HealthCheckConfig"`
		HealthCheckCustomConfig json.RawMessage `json:"HealthCheckCustomConfig"`
		Tags                    []Tag           `json:"Tags"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	svc, err := d.CreateService(ServiceInput{
		Name: in.Name, NamespaceID: in.NamespaceID, Type: in.Type, Description: in.Description,
		CreatorRequestID: in.CreatorRequestID, DNSConfig: in.DNSConfig, HealthCheckConfig: in.HealthCheckConfig,
		CustomHealth: in.HealthCheckCustomConfig, Tags: in.Tags,
	})
	if err != nil {
		return err
	}
	return respond(w, map[string]any{"Service": d.serviceJSON(svc)})
}

func (d *Directory) listServices(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Filters []struct {
			Name   string   `json:"Name"`
			Values []string `json:"Values"`
		} `json:"Filters"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	namespaceID := ""
	for _, f := range in.Filters {
		if f.Name == "NAMESPACE_ID" && len(f.Values) > 0 {
			namespaceID = f.Values[0]
		}
	}
	services, err := d.Services(namespaceID)
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(services))
	for _, svc := range services {
		out = append(out, d.serviceJSON(svc))
	}
	return respond(w, map[string]any{"Services": out})
}

func (d *Directory) discover(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		NamespaceName      string            `json:"NamespaceName"`
		ServiceName        string            `json:"ServiceName"`
		QueryParameters    map[string]string `json:"QueryParameters"`
		OptionalParameters map[string]string `json:"OptionalParameters"`
		HealthStatus       string            `json:"HealthStatus"`
		MaxResults         int               `json:"MaxResults"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	instances, err := d.Discover(DiscoverQuery{
		NamespaceName: in.NamespaceName, ServiceName: in.ServiceName, QueryParameters: in.QueryParameters,
		OptionalParameters: in.OptionalParameters, HealthStatus: in.HealthStatus, MaxResults: in.MaxResults,
	})
	if err != nil {
		return err
	}
	out := make([]map[string]any, 0, len(instances))
	for _, inst := range instances {
		out = append(out, map[string]any{
			"InstanceId": inst.ID, "NamespaceName": in.NamespaceName, "ServiceName": in.ServiceName,
			"HealthStatus": inst.Health, "Attributes": inst.Attributes,
		})
	}
	return respond(w, map[string]any{"Instances": out, "InstancesRevision": len(instances)})
}

func (d *Directory) healthStatus(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		ServiceID string   `json:"ServiceId"`
		Instances []string `json:"Instances"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	instances, err := d.Instances(in.ServiceID)
	if err != nil {
		return err
	}
	wanted := map[string]bool{}
	for _, id := range in.Instances {
		wanted[id] = true
	}
	status := map[string]string{}
	for _, inst := range instances {
		if len(wanted) == 0 || wanted[inst.ID] {
			status[inst.ID] = inst.Health
		}
	}
	return respond(w, map[string]any{"Status": status})
}

func (d *Directory) namespaceJSON(ns Namespace) map[string]any {
	props := map[string]any{"HttpProperties": map[string]any{"HttpName": ns.Name}}
	if ns.isDNS() {
		props["DnsProperties"] = map[string]any{"HostedZoneId": ns.HostedZoneID, "SOA": map[string]any{"TTL": 15}}
	}
	return map[string]any{
		"Id": ns.ID, "Arn": ns.ARN(), "Name": ns.Name, "Type": ns.Type, "Description": ns.Description,
		"ServiceCount": d.ServiceCount(ns.ID), "Properties": props, "CreateDate": epoch(ns.Created),
		"CreatorRequestId": ns.CreatorRequestID,
	}
}

func (d *Directory) serviceJSON(svc Service) map[string]any {
	out := map[string]any{
		"Id": svc.ID, "Arn": svc.ARN(), "Name": svc.Name, "NamespaceId": svc.NamespaceID, "Type": svc.Type,
		"Description": svc.Description, "InstanceCount": d.InstanceCount(svc.ID), "CreateDate": epoch(svc.Created),
		"CreatorRequestId": svc.CreatorRequestID,
	}
	if svc.DNSConfig != nil {
		out["DnsConfig"] = svc.DNSConfig
	}
	if len(svc.HealthCheckConfig) > 0 {
		out["HealthCheckConfig"] = svc.HealthCheckConfig
	}
	if len(svc.CustomHealth) > 0 {
		out["HealthCheckCustomConfig"] = svc.CustomHealth
	}
	return out
}

func operationJSON(op Operation) map[string]any {
	return map[string]any{
		"Id": op.ID, "Type": op.Type, "Status": "SUCCESS", "CreateDate": epoch(op.Created),
		"UpdateDate": epoch(op.Created), "Targets": op.Targets,
	}
}

func respondOperation(w http.ResponseWriter, op Operation, err error) error {
	if err != nil {
		return err
	}
	return respond(w, map[string]any{"OperationId": op.ID})
}

// epoch renders a time as AWS JSON does: fractional epoch seconds.
func epoch(t time.Time) float64 {
	return float64(t.UnixNano()) / float64(time.Second)
}

func decode(r *http.Request, into any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return invalidInput("read request body: %v", err)
	}
	if len(body) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, into); err != nil {
		return invalidInput("invalid request body: %v", err)
	}
	return nil
}

func respond(w http.ResponseWriter, payload any) error {
	w.Header().Set("Content-Type", "application/x-amz-json-1.1")
	w.WriteHeader(http.StatusOK)
	return json.NewEncoder(w).Encode(payload)
}
