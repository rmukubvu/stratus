package ec2

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store"
	"github.com/stratus/internal/store/bbolt"
)

type fixture struct {
	t   *testing.T
	svc *Service
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	meta, err := bbolt.Open(filepath.Join(t.TempDir(), "meta.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = meta.Close() })
	svc := NewService(store.Scoped(meta, "@111111111111/eu-west-1/"))
	svc.SetScope(scope.Scope{Account: "111111111111", Region: "eu-west-1"})
	return &fixture{t: t, svc: svc}
}

func (f *fixture) call(action string, params ...string) (string, error) {
	f.t.Helper()
	form := url.Values{"Action": {action}, "Version": {"2016-11-15"}}
	for i := 0; i+1 < len(params); i += 2 {
		form.Set(params[i], params[i+1])
	}
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	if err := f.svc.Handle(w, r, action, "req-1"); err != nil {
		return "", err
	}
	return w.Body.String(), nil
}

func (f *fixture) must(action string, params ...string) string {
	f.t.Helper()
	body, err := f.call(action, params...)
	if err != nil {
		f.t.Fatalf("%s: %v", action, err)
	}
	return body
}

func (f *fixture) mustFail(code, action string, params ...string) {
	f.t.Helper()
	_, err := f.call(action, params...)
	var apiErr *apierror.Error
	if !errors.As(err, &apiErr) || apiErr.Code != code {
		f.t.Fatalf("%s: want %s, got %v", action, code, err)
	}
}

// first returns the text of the first <element> in body.
func first(t *testing.T, body, element string) string {
	t.Helper()
	m := regexp.MustCompile("<" + element + ">([^<]*)</" + element + ">").FindStringSubmatch(body)
	if m == nil {
		t.Fatalf("no <%s> in %s", element, body)
	}
	return m[1]
}

func count(body, element string) int { return strings.Count(body, "<"+element+">") }

// TestNetworkLifecycle follows the default-VPC-shaped network the fidelity
// suite builds before ELB, RDS and ECS tests, then tears it down.
func TestNetworkLifecycle(t *testing.T) {
	f := newFixture(t)
	created := f.must("CreateVpc", "CidrBlock", "10.0.0.0/16",
		"TagSpecification.1.ResourceType", "vpc", "TagSpecification.1.Tag.1.Key", "Name", "TagSpecification.1.Tag.1.Value", "fidelity")
	if !strings.Contains(created, `<CreateVpcResponse xmlns="`+namespace+`"><requestId>req-1</requestId><vpc>`) {
		t.Fatalf("CreateVpc envelope: %s", created)
	}
	vpcID := first(t, created, "vpcId")
	if !regexp.MustCompile(`^vpc-[0-9a-f]{17}$`).MatchString(vpcID) {
		t.Fatalf("vpc id %q", vpcID)
	}

	described := f.must("DescribeVpcs", "Filter.1.Name", "tag:Name", "Filter.1.Value.1", "fid*")
	if first(t, described, "vpcId") != vpcID || first(t, described, "state") != "available" {
		t.Fatalf("DescribeVpcs: %s", described)
	}
	f.must("ModifyVpcAttribute", "VpcId", vpcID, "EnableDnsHostnames.Value", "true")
	attr := f.must("DescribeVpcAttribute", "VpcId", vpcID, "Attribute", "enableDnsHostnames")
	if !strings.Contains(attr, "<enableDnsHostnames><value>true</value></enableDnsHostnames>") {
		t.Fatalf("DescribeVpcAttribute: %s", attr)
	}

	igwID := first(t, f.must("CreateInternetGateway"), "internetGatewayId")
	f.must("AttachInternetGateway", "InternetGatewayId", igwID, "VpcId", vpcID)
	f.mustFail("Resource.AlreadyAssociated", "AttachInternetGateway", "InternetGatewayId", igwID, "VpcId", vpcID)

	tables := f.must("DescribeRouteTables", "Filter.1.Name", "vpc-id", "Filter.1.Value.1", vpcID,
		"Filter.2.Name", "association.main", "Filter.2.Value.1", "true")
	if count(tables, "routeTableId") != 2 { // the table's own ID and its association's
		t.Fatalf("want the main route table: %s", tables)
	}
	f.must("CreateRoute", "RouteTableId", first(t, tables, "routeTableId"), "DestinationCidrBlock", "0.0.0.0/0", "GatewayId", igwID)
	f.mustFail("RouteAlreadyExists", "CreateRoute", "RouteTableId", first(t, tables, "routeTableId"), "DestinationCidrBlock", "0.0.0.0/0", "GatewayId", igwID)

	zones := f.must("DescribeAvailabilityZones")
	if count(zones, "zoneName") != 3 || first(t, zones, "zoneName") != "eu-west-1a" {
		t.Fatalf("DescribeAvailabilityZones: %s", zones)
	}
	subnet := f.must("CreateSubnet", "VpcId", vpcID, "CidrBlock", "10.0.1.0/24", "AvailabilityZone", "eu-west-1b")
	subnetID := first(t, subnet, "subnetId")
	if first(t, subnet, "availableIpAddressCount") != "251" || first(t, subnet, "availabilityZoneId") != "eu-west-1-az2" {
		t.Fatalf("CreateSubnet: %s", subnet)
	}
	f.must("ModifySubnetAttribute", "SubnetId", subnetID, "MapPublicIpOnLaunch.Value", "true")

	groups := f.must("DescribeSecurityGroups", "Filter.1.Name", "vpc-id", "Filter.1.Value.1", vpcID,
		"Filter.2.Name", "group-name", "Filter.2.Value.1", "default")
	if count(groups, "groupName") != 1 || !strings.Contains(groups, "<ipProtocol>-1</ipProtocol>") {
		t.Fatalf("default security group: %s", groups)
	}

	f.mustFail("DependencyViolation", "DeleteVpc", "VpcId", vpcID)
	f.mustFail("DependencyViolation", "DeleteInternetGateway", "InternetGatewayId", igwID)
	f.must("DetachInternetGateway", "InternetGatewayId", igwID, "VpcId", vpcID)
	f.mustFail("Gateway.NotAttached", "DetachInternetGateway", "InternetGatewayId", igwID, "VpcId", vpcID)
	f.must("DeleteInternetGateway", "InternetGatewayId", igwID)
	f.must("DeleteSubnet", "SubnetId", subnetID)
	f.must("DeleteVpc", "VpcId", vpcID)

	f.mustFail("InvalidVpcID.NotFound", "DescribeVpcs", "VpcId.1", vpcID)
	f.mustFail("InvalidSubnetID.NotFound", "DeleteSubnet", "SubnetId", subnetID)
	if body := f.must("DescribeSecurityGroups"); count(body, "groupId") != 0 {
		t.Fatalf("default group outlived its VPC: %s", body)
	}
}

func TestCreateVpcAndSubnetValidation(t *testing.T) {
	f := newFixture(t)
	f.mustFail("MissingParameter", "CreateVpc")
	f.mustFail("InvalidVpc.Range", "CreateVpc", "CidrBlock", "10.0.0.0/8")
	f.mustFail("InvalidVpc.Range", "CreateVpc", "CidrBlock", "10.0.0.1/16")
	f.mustFail("InvalidParameterValue", "CreateVpc", "CidrBlock", "10.0.0.0/16",
		"TagSpecification.1.ResourceType", "subnet", "TagSpecification.1.Tag.1.Key", "k")

	vpcID := first(t, f.must("CreateVpc", "CidrBlock", "10.1.0.0/16"), "vpcId")
	f.must("CreateSubnet", "VpcId", vpcID, "CidrBlock", "10.1.0.0/24")
	f.mustFail("InvalidSubnet.Conflict", "CreateSubnet", "VpcId", vpcID, "CidrBlock", "10.1.0.128/25")
	f.mustFail("InvalidSubnet.Range", "CreateSubnet", "VpcId", vpcID, "CidrBlock", "10.2.0.0/24")
	f.mustFail("InvalidParameterValue", "CreateSubnet", "VpcId", vpcID, "CidrBlock", "10.1.5.0/24", "AvailabilityZone", "us-east-1a")
	f.mustFail("InvalidVpcID.NotFound", "CreateSubnet", "VpcId", "vpc-0000", "CidrBlock", "10.1.6.0/24")
}

func TestSecurityGroupRules(t *testing.T) {
	f := newFixture(t)
	vpcID := first(t, f.must("CreateVpc", "CidrBlock", "10.0.0.0/16"), "vpcId")
	f.mustFail("InvalidGroup.Duplicate", "CreateSecurityGroup", "VpcId", vpcID, "GroupName", "default", "GroupDescription", "d")
	web := first(t, f.must("CreateSecurityGroup", "VpcId", vpcID, "GroupName", "web", "GroupDescription", "web tier"), "groupId")
	db := first(t, f.must("CreateSecurityGroup", "VpcId", vpcID, "GroupName", "db", "GroupDescription", "db tier"), "groupId")
	f.mustFail("InvalidGroup.Duplicate", "CreateSecurityGroup", "VpcId", vpcID, "GroupName", "web", "GroupDescription", "again")

	https := []string{"GroupId", web, "IpPermissions.1.IpProtocol", "tcp", "IpPermissions.1.FromPort", "443",
		"IpPermissions.1.ToPort", "443", "IpPermissions.1.IpRanges.1.CidrIp", "0.0.0.0/0"}
	f.must("AuthorizeSecurityGroupIngress", https...)
	f.mustFail("InvalidPermission.Duplicate", "AuthorizeSecurityGroupIngress", https...)
	f.must("AuthorizeSecurityGroupIngress", "GroupId", db, "IpPermissions.1.IpProtocol", "tcp",
		"IpPermissions.1.FromPort", "5432", "IpPermissions.1.ToPort", "5432", "IpPermissions.1.Groups.1.GroupId", web)

	described := f.must("DescribeSecurityGroups", "GroupId.1", web)
	if !strings.Contains(described, "<item><ipProtocol>tcp</ipProtocol><fromPort>443</fromPort><toPort>443</toPort><groups></groups><ipRanges><item><cidrIp>0.0.0.0/0</cidrIp></item></ipRanges></item>") {
		t.Fatalf("ingress rule: %s", described)
	}

	f.mustFail("DependencyViolation", "DeleteSecurityGroup", "GroupId", web)
	f.must("RevokeSecurityGroupIngress", https...)
	f.mustFail("InvalidPermission.NotFound", "RevokeSecurityGroupIngress", https...)
	f.must("DeleteSecurityGroup", "GroupId", db)
	f.must("DeleteSecurityGroup", "GroupId", web)

	defaultID := first(t, f.must("DescribeSecurityGroups", "Filter.1.Name", "group-name", "Filter.1.Value.1", "default"), "groupId")
	f.mustFail("CannotDelete", "DeleteSecurityGroup", "GroupId", defaultID)
	f.mustFail("InvalidGroup.NotFound", "DeleteSecurityGroup", "GroupId", web)
}

func TestTags(t *testing.T) {
	f := newFixture(t)
	vpcID := first(t, f.must("CreateVpc", "CidrBlock", "10.0.0.0/16"), "vpcId")
	f.must("CreateTags", "ResourceId.1", vpcID, "Tag.1.Key", "env", "Tag.1.Value", "dev", "Tag.2.Key", "team", "Tag.2.Value", "core")
	f.must("CreateTags", "ResourceId.1", vpcID, "Tag.1.Key", "env", "Tag.1.Value", "prod")
	f.must("DeleteTags", "ResourceId.1", vpcID, "Tag.1.Key", "team")

	described := f.must("DescribeVpcs", "VpcId.1", vpcID)
	if !strings.Contains(described, "<tagSet><item><key>env</key><value>prod</value></item></tagSet>") {
		t.Fatalf("vpc tags: %s", described)
	}
	listed := f.must("DescribeTags", "Filter.1.Name", "resource-type", "Filter.1.Value.1", "vpc")
	if count(listed, "resourceId") != 1 || first(t, listed, "value") != "prod" {
		t.Fatalf("DescribeTags: %s", listed)
	}
	f.mustFail("InvalidID", "CreateTags", "ResourceId.1", "vpc-0000", "Tag.1.Key", "k")
	f.mustFail("InvalidID", "CreateTags", "ResourceId.1", "i-0000", "Tag.1.Key", "k")
}

func TestUnknownActionIsNotImplemented(t *testing.T) {
	f := newFixture(t)
	f.mustFail("NotImplemented", "RunInstances")
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"abc", "abc", true}, {"abc", "abd", false}, {"a*", "abc", true}, {"*c", "abc", true},
		{"a?c", "abc", true}, {"a?c", "ac", false}, {"*", "", true}, {"a*b*c", "aXbYc", true}, {"a*b", "ac", false},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pattern, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v", c.pattern, c.s, got)
		}
	}
}

func TestCidrRules(t *testing.T) {
	for block, want := range map[string]bool{
		"10.0.0.0/16": true, "10.0.0.0/28": true, "10.0.0.0/15": false, "10.0.0.0/29": false,
		"10.0.0.5/16": false, "::/64": false, "nonsense": false,
	} {
		if _, got := validCidr(block); got != want {
			t.Errorf("validCidr(%q) = %v", block, got)
		}
	}
	_, vpcNet, _ := parseCIDR("10.0.0.0/16")
	_, inside, _ := parseCIDR("10.0.3.0/24")
	_, outside, _ := parseCIDR("10.1.0.0/24")
	_, half, _ := parseCIDR("10.0.3.128/25")
	if !within(inside, vpcNet) || within(outside, vpcNet) || within(vpcNet, inside) {
		t.Error("within")
	}
	if !overlaps(inside, half) || overlaps(inside, outside) {
		t.Error("overlaps")
	}
}
