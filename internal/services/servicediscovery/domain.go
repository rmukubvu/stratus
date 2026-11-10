package servicediscovery

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	accountID = "000000000000"
	region    = "us-east-1"

	// Namespace types.
	typeHTTP       = "HTTP"
	typeDNSPrivate = "DNS_PRIVATE"
	typeDNSPublic  = "DNS_PUBLIC"

	// Service types.
	serviceHTTP    = "HTTP"
	serviceDNSHTTP = "DNS_HTTP"

	routingMultivalue = "MULTIVALUE"
	routingWeighted   = "WEIGHTED"

	healthy   = "HEALTHY"
	unhealthy = "UNHEALTHY"

	// Instance attributes Cloud Map turns into DNS records.
	attrIPv4  = "AWS_INSTANCE_IPV4"
	attrIPv6  = "AWS_INSTANCE_IPV6"
	attrPort  = "AWS_INSTANCE_PORT"
	attrCNAME = "AWS_INSTANCE_CNAME"
)

// Tag is a resource tag.
type Tag struct {
	Key   string `json:"Key"`
	Value string `json:"Value"`
}

// Namespace groups services under a name. DNS namespaces own a Route 53
// hosted zone of the same name.
type Namespace struct {
	ID               string    `json:"id"`
	Name             string    `json:"name"`
	Type             string    `json:"type"`
	Description      string    `json:"description,omitempty"`
	CreatorRequestID string    `json:"creator_request_id,omitempty"`
	HostedZoneID     string    `json:"hosted_zone_id,omitempty"`
	VPC              string    `json:"vpc,omitempty"`
	Created          time.Time `json:"created"`
	Tags             []Tag     `json:"tags,omitempty"`
}

// ARN is the namespace's Amazon Resource Name.
func (n Namespace) ARN() string {
	return fmt.Sprintf("arn:aws:servicediscovery:%s:%s:namespace/%s", region, accountID, n.ID)
}

// isDNS reports whether the namespace has a hosted zone.
func (n Namespace) isDNS() bool { return n.Type == typeDNSPrivate || n.Type == typeDNSPublic }

// DNSRecordTemplate is one record type a service publishes per instance.
type DNSRecordTemplate struct {
	Type string `json:"Type"`
	TTL  int64  `json:"TTL"`
}

// DNSConfig says which records a service publishes for each instance.
type DNSConfig struct {
	NamespaceID   string              `json:"NamespaceId,omitempty"`
	RoutingPolicy string              `json:"RoutingPolicy,omitempty"`
	DNSRecords    []DNSRecordTemplate `json:"DnsRecords"`
}

// Service is a named group of instances in a namespace.
type Service struct {
	ID                string          `json:"id"`
	Name              string          `json:"name"`
	NamespaceID       string          `json:"namespace_id"`
	Type              string          `json:"type"`
	Description       string          `json:"description,omitempty"`
	CreatorRequestID  string          `json:"creator_request_id,omitempty"`
	DNSConfig         *DNSConfig      `json:"dns_config,omitempty"`
	HealthCheckConfig json.RawMessage `json:"health_check_config,omitempty"`
	// CustomHealth is the HealthCheckCustomConfig. When set, the application
	// reports instance health with UpdateInstanceCustomHealthStatus.
	CustomHealth json.RawMessage `json:"custom_health,omitempty"`
	Created      time.Time       `json:"created"`
	Tags         []Tag           `json:"tags,omitempty"`
}

// ARN is the service's Amazon Resource Name.
func (s Service) ARN() string {
	return fmt.Sprintf("arn:aws:servicediscovery:%s:%s:service/%s", region, accountID, s.ID)
}

// Instance is one registered endpoint of a service.
type Instance struct {
	ID               string            `json:"id"`
	ServiceID        string            `json:"service_id"`
	CreatorRequestID string            `json:"creator_request_id,omitempty"`
	Attributes       map[string]string `json:"attributes"`
	Health           string            `json:"health"`
}

// DNSRecord is a record set Cloud Map keeps in the namespace's hosted zone
// for one instance.
type DNSRecord struct {
	Name          string
	Type          string
	TTL           int64
	SetIdentifier string
	MultiValue    bool
	Value         string
}

// records derives the DNS records a service publishes for an instance, as
// Cloud Map does: one record set per instance and record type, named
// "<service>.<namespace>", told apart by the instance ID. An SRV record also
// gets an A record for the host it names.
func (s Service) records(ns Namespace, inst Instance) ([]DNSRecord, error) {
	if s.DNSConfig == nil || !ns.isDNS() {
		return nil, nil
	}
	name := s.Name + "." + strings.TrimSuffix(ns.Name, ".") + "."
	multi := s.DNSConfig.RoutingPolicy != routingWeighted
	base := DNSRecord{Name: name, SetIdentifier: inst.ID, MultiValue: multi}
	var out []DNSRecord
	for _, tmpl := range s.DNSConfig.DNSRecords {
		rec := base
		rec.Type, rec.TTL = tmpl.Type, tmpl.TTL
		switch tmpl.Type {
		case "A":
			rec.Value = inst.Attributes[attrIPv4]
		case "AAAA":
			rec.Value = inst.Attributes[attrIPv6]
		case "CNAME":
			rec.Value = inst.Attributes[attrCNAME]
		case "SRV":
			port := inst.Attributes[attrPort]
			if port == "" {
				return nil, invalidInput("%s is required for a service with an SRV record", attrPort)
			}
			host := inst.ID + "." + name
			rec.Value = fmt.Sprintf("1 1 %s %s", port, host)
			if ip := inst.Attributes[attrIPv4]; ip != "" {
				out = append(out, DNSRecord{Name: host, Type: "A", TTL: tmpl.TTL, SetIdentifier: inst.ID, MultiValue: multi, Value: ip})
			}
		default:
			return nil, invalidInput("unsupported DNS record type %q", tmpl.Type)
		}
		if rec.Value == "" {
			return nil, invalidInput("instance %s needs the %s attribute for the service's %s record", inst.ID, attributeFor(tmpl.Type), tmpl.Type)
		}
		out = append(out, rec)
	}
	return out, nil
}

func attributeFor(rtype string) string {
	switch rtype {
	case "AAAA":
		return attrIPv6
	case "CNAME":
		return attrCNAME
	default:
		return attrIPv4
	}
}

// Operation records an asynchronous Cloud Map operation. Stratus finishes
// every operation before answering, so each one has succeeded.
type Operation struct {
	ID      string            `json:"id"`
	Type    string            `json:"type"`
	Created time.Time         `json:"created"`
	Targets map[string]string `json:"targets"`
}
