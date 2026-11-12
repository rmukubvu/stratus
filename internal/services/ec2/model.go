// Package ec2 emulates the EC2 networking API that other services' tests
// build on: VPCs, subnets, internet gateways, route tables, security groups
// and availability zones. It holds no instances; it exists so that a test can
// create the default-VPC-shaped network that ELB, RDS, ElastiCache, MSK and
// ECS resources are placed in.
package ec2

import (
	"net"
	"strings"
	"time"

	"github.com/stratus/internal/tags"
)

// Store buckets, one per resource type.
const (
	vpcsBucket           = "ec2-vpcs"
	subnetsBucket        = "ec2-subnets"
	gatewaysBucket       = "ec2-internet-gateways"
	routeTablesBucket    = "ec2-route-tables"
	securityGroupsBucket = "ec2-security-groups"
)

type vpc struct {
	ID                 string    `json:"id"`
	CidrBlock          string    `json:"cidr_block"`
	EnableDNSSupport   bool      `json:"enable_dns_support"`
	EnableDNSHostnames bool      `json:"enable_dns_hostnames"`
	InstanceTenancy    string    `json:"instance_tenancy"`
	Tags               tags.List `json:"tags,omitempty"`
	Created            time.Time `json:"created"`
}

type subnet struct {
	ID                  string    `json:"id"`
	VpcID               string    `json:"vpc_id"`
	CidrBlock           string    `json:"cidr_block"`
	AvailabilityZone    string    `json:"availability_zone"`
	MapPublicIPOnLaunch bool      `json:"map_public_ip_on_launch"`
	Tags                tags.List `json:"tags,omitempty"`
}

type internetGateway struct {
	ID    string    `json:"id"`
	VpcID string    `json:"vpc_id,omitempty"`
	Tags  tags.List `json:"tags,omitempty"`
}

type route struct {
	DestinationCidrBlock string `json:"destination_cidr_block"`
	GatewayID            string `json:"gateway_id"`
}

type routeTable struct {
	ID     string    `json:"id"`
	VpcID  string    `json:"vpc_id"`
	Main   bool      `json:"main"`
	Routes []route   `json:"routes"`
	Tags   tags.List `json:"tags,omitempty"`
}

type permission struct {
	IPProtocol string   `json:"ip_protocol"`
	FromPort   *int     `json:"from_port,omitempty"`
	ToPort     *int     `json:"to_port,omitempty"`
	CidrIPs    []string `json:"cidr_ips,omitempty"`
	GroupIDs   []string `json:"group_ids,omitempty"`
}

type securityGroup struct {
	ID          string       `json:"id"`
	VpcID       string       `json:"vpc_id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Ingress     []permission `json:"ingress,omitempty"`
	Egress      []permission `json:"egress,omitempty"`
	Tags        tags.List    `json:"tags,omitempty"`
}

// filterable is a resource DescribeX can filter: the values each filter name
// matches against.
type filterable interface {
	filterValues(name string) []string
}

func tagFilterValues(list tags.List, name string) ([]string, bool) {
	switch {
	case name == "tag-key":
		keys := make([]string, 0, len(list))
		for _, t := range list {
			keys = append(keys, t.Key)
		}
		return keys, true
	case strings.HasPrefix(name, "tag:"):
		key := strings.TrimPrefix(name, "tag:")
		for _, t := range list {
			if t.Key == key {
				return []string{t.Value}, true
			}
		}
		return nil, true
	}
	return nil, false
}

func (v vpc) filterValues(name string) []string {
	if values, ok := tagFilterValues(v.Tags, name); ok {
		return values
	}
	switch name {
	case "vpc-id":
		return []string{v.ID}
	case "cidr", "cidr-block-association.cidr-block":
		return []string{v.CidrBlock}
	case "state":
		return []string{"available"}
	case "is-default":
		return []string{"false"}
	}
	return nil
}

func (s subnet) filterValues(name string) []string {
	if values, ok := tagFilterValues(s.Tags, name); ok {
		return values
	}
	switch name {
	case "subnet-id":
		return []string{s.ID}
	case "vpc-id":
		return []string{s.VpcID}
	case "availability-zone":
		return []string{s.AvailabilityZone}
	case "cidr-block", "cidr":
		return []string{s.CidrBlock}
	case "state":
		return []string{"available"}
	case "default-for-az":
		return []string{"false"}
	}
	return nil
}

func (g internetGateway) filterValues(name string) []string {
	if values, ok := tagFilterValues(g.Tags, name); ok {
		return values
	}
	switch name {
	case "internet-gateway-id":
		return []string{g.ID}
	case "attachment.vpc-id":
		if g.VpcID != "" {
			return []string{g.VpcID}
		}
	case "attachment.state":
		if g.VpcID != "" {
			return []string{"available"}
		}
	}
	return nil
}

func (t routeTable) filterValues(name string) []string {
	if values, ok := tagFilterValues(t.Tags, name); ok {
		return values
	}
	switch name {
	case "route-table-id":
		return []string{t.ID}
	case "vpc-id":
		return []string{t.VpcID}
	case "association.main":
		if t.Main {
			return []string{"true"}
		}
		return []string{"false"}
	}
	return nil
}

func (g securityGroup) filterValues(name string) []string {
	if values, ok := tagFilterValues(g.Tags, name); ok {
		return values
	}
	switch name {
	case "group-id":
		return []string{g.ID}
	case "group-name":
		return []string{g.Name}
	case "vpc-id":
		return []string{g.VpcID}
	case "description":
		return []string{g.Description}
	}
	return nil
}

// matches reports whether r satisfies every filter (values within one filter
// are ORed; "*" and "?" wildcards are honoured as EC2 does).
func matches(r filterable, filters map[string][]string) bool {
	for name, wanted := range filters {
		have := r.filterValues(name)
		if !anyMatch(have, wanted) {
			return false
		}
	}
	return true
}

func anyMatch(have, wanted []string) bool {
	for _, h := range have {
		for _, w := range wanted {
			if wildcardMatch(w, h) {
				return true
			}
		}
	}
	return false
}

func wildcardMatch(pattern, s string) bool {
	if !strings.ContainsAny(pattern, "*?") {
		return pattern == s
	}
	if pattern == "" {
		return s == ""
	}
	switch pattern[0] {
	case '*':
		for i := 0; i <= len(s); i++ {
			if wildcardMatch(pattern[1:], s[i:]) {
				return true
			}
		}
		return false
	case '?':
		return s != "" && wildcardMatch(pattern[1:], s[1:])
	default:
		return s != "" && s[0] == pattern[0] && wildcardMatch(pattern[1:], s[1:])
	}
}

// validCidr reports whether block is an IPv4 CIDR whose prefix length is
// within EC2's VPC and subnet limits (/16 to /28).
func validCidr(block string) (*net.IPNet, bool) {
	ip, network, err := net.ParseCIDR(block)
	if err != nil || ip.To4() == nil || !ip.Equal(network.IP) {
		return nil, false
	}
	ones, _ := network.Mask.Size()
	return network, ones >= 16 && ones <= 28
}

// within reports whether inner lies inside outer.
func within(inner, outer *net.IPNet) bool {
	innerOnes, _ := inner.Mask.Size()
	outerOnes, _ := outer.Mask.Size()
	return innerOnes >= outerOnes && outer.Contains(inner.IP)
}

// overlaps reports whether two networks share any address.
func overlaps(a, b *net.IPNet) bool {
	return a.Contains(b.IP) || b.Contains(a.IP)
}
