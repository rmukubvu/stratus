package ec2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/stratus/internal/apierror"
	"github.com/stratus/internal/awscompat"
	"github.com/stratus/internal/scope"
	"github.com/stratus/internal/store"
	"github.com/stratus/internal/tags"
)

// Service is the EC2 networking API of one account and region.
type Service struct {
	scope    scope.Scope
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

// NewService returns the EC2 networking service backed by metadata.
func NewService(metadata store.Store) *Service {
	return &Service{scope: scope.Default(), metadata: metadata, now: time.Now}
}

// SetScope sets the account and region the service answers for.
func (s *Service) SetScope(sc scope.Scope) { s.scope = sc }

// Operations lists the EC2 actions served, for request classification.
var Operations = []string{
	"CreateVpc", "DescribeVpcs", "DeleteVpc", "ModifyVpcAttribute", "DescribeVpcAttribute",
	"CreateSubnet", "DescribeSubnets", "DeleteSubnet", "ModifySubnetAttribute",
	"CreateInternetGateway", "DescribeInternetGateways", "DeleteInternetGateway",
	"AttachInternetGateway", "DetachInternetGateway",
	"DescribeRouteTables", "CreateRoute", "DeleteRoute",
	"CreateSecurityGroup", "DescribeSecurityGroups", "DeleteSecurityGroup",
	"AuthorizeSecurityGroupIngress", "AuthorizeSecurityGroupEgress",
	"RevokeSecurityGroupIngress", "RevokeSecurityGroupEgress",
	"DescribeAvailabilityZones", "DescribeAccountAttributes",
	"CreateTags", "DeleteTags", "DescribeTags",
}

// Handle serves one EC2 query-protocol action.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request, operation, requestID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	form, err := awscompat.ParseQueryForm(r)
	if err != nil {
		return invalidParameter("request body is not a valid query form")
	}
	c := call{svc: s, form: form, requestID: requestID, w: w}
	handlers := map[string]func(*call) error{
		"CreateVpc":                     (*call).createVpc,
		"DescribeVpcs":                  (*call).describeVpcs,
		"DeleteVpc":                     (*call).deleteVpc,
		"ModifyVpcAttribute":            (*call).modifyVpcAttribute,
		"DescribeVpcAttribute":          (*call).describeVpcAttribute,
		"CreateSubnet":                  (*call).createSubnet,
		"DescribeSubnets":               (*call).describeSubnets,
		"DeleteSubnet":                  (*call).deleteSubnet,
		"ModifySubnetAttribute":         (*call).modifySubnetAttribute,
		"CreateInternetGateway":         (*call).createInternetGateway,
		"DescribeInternetGateways":      (*call).describeInternetGateways,
		"DeleteInternetGateway":         (*call).deleteInternetGateway,
		"AttachInternetGateway":         (*call).attachInternetGateway,
		"DetachInternetGateway":         (*call).detachInternetGateway,
		"DescribeRouteTables":           (*call).describeRouteTables,
		"CreateRoute":                   (*call).createRoute,
		"DeleteRoute":                   (*call).deleteRoute,
		"CreateSecurityGroup":           (*call).createSecurityGroup,
		"DescribeSecurityGroups":        (*call).describeSecurityGroups,
		"DeleteSecurityGroup":           (*call).deleteSecurityGroup,
		"AuthorizeSecurityGroupIngress": func(c *call) error { return c.changeRules(true, true) },
		"AuthorizeSecurityGroupEgress":  func(c *call) error { return c.changeRules(false, true) },
		"RevokeSecurityGroupIngress":    func(c *call) error { return c.changeRules(true, false) },
		"RevokeSecurityGroupEgress":     func(c *call) error { return c.changeRules(false, false) },
		"DescribeAvailabilityZones":     (*call).describeAvailabilityZones,
		"DescribeAccountAttributes":     (*call).describeAccountAttributes,
		"CreateTags":                    func(c *call) error { return c.changeTags(true) },
		"DeleteTags":                    func(c *call) error { return c.changeTags(false) },
		"DescribeTags":                  (*call).describeTags,
	}
	handler, ok := handlers[operation]
	if !ok {
		return &apierror.Error{StatusCode: http.StatusNotImplemented, Code: "NotImplemented", Message: "ec2 operation is not implemented: " + operation}
	}
	return handler(&c)
}

// call is one request in flight.
type call struct {
	svc       *Service
	form      url.Values
	requestID string
	w         http.ResponseWriter
}

func (c *call) respond(action string, body any) error {
	writeXML(c.w, action, c.requestID, body)
	return nil
}

func (c *call) returnTrue(action string) error {
	return c.respond(action, returnXML{Return: true})
}

// ---- VPCs ----

func (c *call) createVpc() error {
	block := c.form.Get("CidrBlock")
	if block == "" {
		return missingParameter("CidrBlock")
	}
	if _, ok := validCidr(block); !ok {
		return badRequest("InvalidVpc.Range", fmt.Sprintf("The CIDR '%s' is invalid.", block))
	}
	list, err := tagSpecification(c.form, "vpc")
	if err != nil {
		return err
	}
	tenancy := c.form.Get("InstanceTenancy")
	if tenancy == "" {
		tenancy = "default"
	}
	v := vpc{ID: newID("vpc"), CidrBlock: block, EnableDNSSupport: true, InstanceTenancy: tenancy, Tags: list, Created: c.svc.now().UTC()}
	main := routeTable{ID: newID("rtb"), VpcID: v.ID, Main: true, Routes: []route{{DestinationCidrBlock: block, GatewayID: "local"}}}
	group := securityGroup{
		ID: newID("sg"), VpcID: v.ID, Name: "default", Description: "default VPC security group",
		Egress: []permission{{IPProtocol: "-1", CidrIPs: []string{"0.0.0.0/0"}}},
	}
	group.Ingress = []permission{{IPProtocol: "-1", GroupIDs: []string{group.ID}}}
	for _, put := range []func() error{
		func() error { return c.svc.put(vpcsBucket, v.ID, v) },
		func() error { return c.svc.put(routeTablesBucket, main.ID, main) },
		func() error { return c.svc.put(securityGroupsBucket, group.ID, group) },
	} {
		if err := put(); err != nil {
			return internal(err)
		}
	}
	return c.respond("CreateVpc", struct {
		Vpc vpcXML `xml:"vpc"`
	}{c.svc.vpcXML(v)})
}

func (c *call) describeVpcs() error {
	ids := listParam(c.form, "VpcId.")
	var out []vpcXML
	err := describe(c, vpcsBucket, ids, "InvalidVpcID.NotFound", "vpc", func(v vpc) { out = append(out, c.svc.vpcXML(v)) })
	if err != nil {
		return err
	}
	return c.respond("DescribeVpcs", struct {
		Vpcs []vpcXML `xml:"vpcSet>item"`
	}{out})
}

func (c *call) deleteVpc() error {
	id := c.form.Get("VpcId")
	var v vpc
	if err := c.svc.load(vpcsBucket, id, &v, "InvalidVpcID.NotFound", "vpc"); err != nil {
		return err
	}
	if anyIn(c.svc, subnetsBucket, func(s subnet) bool { return s.VpcID == id }) ||
		anyIn(c.svc, gatewaysBucket, func(g internetGateway) bool { return g.VpcID == id }) ||
		anyIn(c.svc, securityGroupsBucket, func(g securityGroup) bool { return g.VpcID == id && g.Name != "default" }) ||
		anyIn(c.svc, routeTablesBucket, func(t routeTable) bool { return t.VpcID == id && !t.Main }) {
		return badRequest("DependencyViolation", fmt.Sprintf("The vpc '%s' has dependencies and cannot be deleted.", id))
	}
	for _, bucket := range []string{routeTablesBucket, securityGroupsBucket} {
		if err := c.svc.deleteWhere(bucket, id); err != nil {
			return internal(err)
		}
	}
	if err := c.svc.metadata.Delete(vpcsBucket, id); err != nil {
		return internal(err)
	}
	return c.returnTrue("DeleteVpc")
}

func (c *call) modifyVpcAttribute() error {
	id := c.form.Get("VpcId")
	var v vpc
	if err := c.svc.load(vpcsBucket, id, &v, "InvalidVpcID.NotFound", "vpc"); err != nil {
		return err
	}
	if raw := c.form.Get("EnableDnsSupport.Value"); raw != "" {
		v.EnableDNSSupport = raw == "true"
	}
	if raw := c.form.Get("EnableDnsHostnames.Value"); raw != "" {
		if raw == "true" && !v.EnableDNSSupport {
			return invalidParameter("enableDnsHostnames requires enableDnsSupport")
		}
		v.EnableDNSHostnames = raw == "true"
	}
	if err := c.svc.put(vpcsBucket, id, v); err != nil {
		return internal(err)
	}
	return c.returnTrue("ModifyVpcAttribute")
}

func (c *call) describeVpcAttribute() error {
	id := c.form.Get("VpcId")
	var v vpc
	if err := c.svc.load(vpcsBucket, id, &v, "InvalidVpcID.NotFound", "vpc"); err != nil {
		return err
	}
	type boolValue struct {
		Value bool `xml:"value"`
	}
	body := struct {
		VpcID              string     `xml:"vpcId"`
		EnableDNSSupport   *boolValue `xml:"enableDnsSupport,omitempty"`
		EnableDNSHostnames *boolValue `xml:"enableDnsHostnames,omitempty"`
	}{VpcID: id}
	switch c.form.Get("Attribute") {
	case "enableDnsSupport":
		body.EnableDNSSupport = &boolValue{v.EnableDNSSupport}
	case "enableDnsHostnames":
		body.EnableDNSHostnames = &boolValue{v.EnableDNSHostnames}
	default:
		return invalidParameter("Attribute must be enableDnsSupport or enableDnsHostnames")
	}
	return c.respond("DescribeVpcAttribute", body)
}

// ---- Subnets ----

func (c *call) createSubnet() error {
	vpcID := c.form.Get("VpcId")
	var v vpc
	if err := c.svc.load(vpcsBucket, vpcID, &v, "InvalidVpcID.NotFound", "vpc"); err != nil {
		return err
	}
	block := c.form.Get("CidrBlock")
	network, ok := validCidr(block)
	_, vpcNet, _ := parseCIDR(v.CidrBlock)
	if !ok || !within(network, vpcNet) {
		return badRequest("InvalidSubnet.Range", fmt.Sprintf("The CIDR '%s' is invalid.", block))
	}
	conflict := anyIn(c.svc, subnetsBucket, func(s subnet) bool {
		_, other, err := parseCIDR(s.CidrBlock)
		return s.VpcID == vpcID && err == nil && overlaps(network, other)
	})
	if conflict {
		return badRequest("InvalidSubnet.Conflict", fmt.Sprintf("The CIDR '%s' conflicts with another subnet", block))
	}
	zone := c.form.Get("AvailabilityZone")
	if zone == "" {
		zone = c.svc.zones()[0]
	} else if !contains(c.svc.zones(), zone) {
		return invalidParameter(fmt.Sprintf("Value (%s) for parameter availabilityZone is invalid.", zone))
	}
	list, err := tagSpecification(c.form, "subnet")
	if err != nil {
		return err
	}
	sn := subnet{ID: newID("subnet"), VpcID: vpcID, CidrBlock: block, AvailabilityZone: zone, Tags: list}
	if err := c.svc.put(subnetsBucket, sn.ID, sn); err != nil {
		return internal(err)
	}
	return c.respond("CreateSubnet", struct {
		Subnet subnetXML `xml:"subnet"`
	}{c.svc.subnetXML(sn)})
}

func (c *call) describeSubnets() error {
	var out []subnetXML
	err := describe(c, subnetsBucket, listParam(c.form, "SubnetId."), "InvalidSubnetID.NotFound", "subnet",
		func(sn subnet) { out = append(out, c.svc.subnetXML(sn)) })
	if err != nil {
		return err
	}
	return c.respond("DescribeSubnets", struct {
		Subnets []subnetXML `xml:"subnetSet>item"`
	}{out})
}

func (c *call) deleteSubnet() error {
	id := c.form.Get("SubnetId")
	var sn subnet
	if err := c.svc.load(subnetsBucket, id, &sn, "InvalidSubnetID.NotFound", "subnet"); err != nil {
		return err
	}
	if err := c.svc.metadata.Delete(subnetsBucket, id); err != nil {
		return internal(err)
	}
	return c.returnTrue("DeleteSubnet")
}

func (c *call) modifySubnetAttribute() error {
	id := c.form.Get("SubnetId")
	var sn subnet
	if err := c.svc.load(subnetsBucket, id, &sn, "InvalidSubnetID.NotFound", "subnet"); err != nil {
		return err
	}
	if raw := c.form.Get("MapPublicIpOnLaunch.Value"); raw != "" {
		sn.MapPublicIPOnLaunch = raw == "true"
	}
	if err := c.svc.put(subnetsBucket, id, sn); err != nil {
		return internal(err)
	}
	return c.returnTrue("ModifySubnetAttribute")
}

// ---- Internet gateways ----

func (c *call) createInternetGateway() error {
	list, err := tagSpecification(c.form, "internet-gateway")
	if err != nil {
		return err
	}
	g := internetGateway{ID: newID("igw"), Tags: list}
	if err := c.svc.put(gatewaysBucket, g.ID, g); err != nil {
		return internal(err)
	}
	return c.respond("CreateInternetGateway", struct {
		Gateway gatewayXML `xml:"internetGateway"`
	}{c.svc.gatewayXML(g)})
}

func (c *call) describeInternetGateways() error {
	var out []gatewayXML
	err := describe(c, gatewaysBucket, listParam(c.form, "InternetGatewayId."), "InvalidInternetGatewayID.NotFound", "internet gateway",
		func(g internetGateway) { out = append(out, c.svc.gatewayXML(g)) })
	if err != nil {
		return err
	}
	return c.respond("DescribeInternetGateways", struct {
		Gateways []gatewayXML `xml:"internetGatewaySet>item"`
	}{out})
}

func (c *call) deleteInternetGateway() error {
	id := c.form.Get("InternetGatewayId")
	var g internetGateway
	if err := c.svc.load(gatewaysBucket, id, &g, "InvalidInternetGatewayID.NotFound", "internet gateway"); err != nil {
		return err
	}
	if g.VpcID != "" {
		return badRequest("DependencyViolation", fmt.Sprintf("The internetGateway '%s' has dependencies and cannot be deleted.", id))
	}
	if err := c.svc.metadata.Delete(gatewaysBucket, id); err != nil {
		return internal(err)
	}
	return c.returnTrue("DeleteInternetGateway")
}

func (c *call) attachInternetGateway() error {
	id, vpcID := c.form.Get("InternetGatewayId"), c.form.Get("VpcId")
	var g internetGateway
	if err := c.svc.load(gatewaysBucket, id, &g, "InvalidInternetGatewayID.NotFound", "internet gateway"); err != nil {
		return err
	}
	var v vpc
	if err := c.svc.load(vpcsBucket, vpcID, &v, "InvalidVpcID.NotFound", "vpc"); err != nil {
		return err
	}
	if g.VpcID != "" {
		return badRequest("Resource.AlreadyAssociated", fmt.Sprintf("resource %s is already attached to network %s", id, g.VpcID))
	}
	g.VpcID = vpcID
	if err := c.svc.put(gatewaysBucket, id, g); err != nil {
		return internal(err)
	}
	return c.returnTrue("AttachInternetGateway")
}

func (c *call) detachInternetGateway() error {
	id, vpcID := c.form.Get("InternetGatewayId"), c.form.Get("VpcId")
	var g internetGateway
	if err := c.svc.load(gatewaysBucket, id, &g, "InvalidInternetGatewayID.NotFound", "internet gateway"); err != nil {
		return err
	}
	if g.VpcID == "" || g.VpcID != vpcID {
		return badRequest("Gateway.NotAttached", fmt.Sprintf("resource %s is not attached to network %s", id, vpcID))
	}
	g.VpcID = ""
	if err := c.svc.put(gatewaysBucket, id, g); err != nil {
		return internal(err)
	}
	return c.returnTrue("DetachInternetGateway")
}

// ---- Route tables ----

func (c *call) describeRouteTables() error {
	var out []routeTableXML
	err := describe(c, routeTablesBucket, listParam(c.form, "RouteTableId."), "InvalidRouteTableID.NotFound", "route table",
		func(t routeTable) { out = append(out, c.svc.routeTableXML(t)) })
	if err != nil {
		return err
	}
	return c.respond("DescribeRouteTables", struct {
		RouteTables []routeTableXML `xml:"routeTableSet>item"`
	}{out})
}

func (c *call) createRoute() error {
	id := c.form.Get("RouteTableId")
	var t routeTable
	if err := c.svc.load(routeTablesBucket, id, &t, "InvalidRouteTableID.NotFound", "route table"); err != nil {
		return err
	}
	dest := c.form.Get("DestinationCidrBlock")
	if _, _, err := parseCIDR(dest); err != nil {
		return invalidParameter(fmt.Sprintf("Value (%s) for parameter destinationCidrBlock is invalid.", dest))
	}
	gateway := c.form.Get("GatewayId")
	var g internetGateway
	if err := c.svc.load(gatewaysBucket, gateway, &g, "InvalidGatewayID.NotFound", "gateway"); err != nil {
		return err
	}
	for _, existing := range t.Routes {
		if existing.DestinationCidrBlock == dest {
			return badRequest("RouteAlreadyExists", fmt.Sprintf("The route identified by %s already exists.", dest))
		}
	}
	t.Routes = append(t.Routes, route{DestinationCidrBlock: dest, GatewayID: gateway})
	if err := c.svc.put(routeTablesBucket, id, t); err != nil {
		return internal(err)
	}
	return c.returnTrue("CreateRoute")
}

func (c *call) deleteRoute() error {
	id := c.form.Get("RouteTableId")
	var t routeTable
	if err := c.svc.load(routeTablesBucket, id, &t, "InvalidRouteTableID.NotFound", "route table"); err != nil {
		return err
	}
	dest := c.form.Get("DestinationCidrBlock")
	kept := t.Routes[:0]
	found := false
	for _, r := range t.Routes {
		if r.DestinationCidrBlock == dest && r.GatewayID != "local" {
			found = true
			continue
		}
		kept = append(kept, r)
	}
	if !found {
		return badRequest("InvalidRoute.NotFound", fmt.Sprintf("no route with destination-cidr-block %s in route table %s", dest, id))
	}
	t.Routes = kept
	if err := c.svc.put(routeTablesBucket, id, t); err != nil {
		return internal(err)
	}
	return c.returnTrue("DeleteRoute")
}

// ---- Security groups ----

func (c *call) createSecurityGroup() error {
	name, description, vpcID := c.form.Get("GroupName"), c.form.Get("GroupDescription"), c.form.Get("VpcId")
	if name == "" {
		return missingParameter("GroupName")
	}
	if description == "" {
		return missingParameter("GroupDescription")
	}
	if vpcID == "" {
		return badRequest("VPCIdNotSpecified", "No default VPC for this user")
	}
	var v vpc
	if err := c.svc.load(vpcsBucket, vpcID, &v, "InvalidVpcID.NotFound", "vpc"); err != nil {
		return err
	}
	if name == "default" || anyIn(c.svc, securityGroupsBucket, func(g securityGroup) bool { return g.VpcID == vpcID && g.Name == name }) {
		return badRequest("InvalidGroup.Duplicate", fmt.Sprintf("The security group '%s' already exists for VPC '%s'", name, vpcID))
	}
	list, err := tagSpecification(c.form, "security-group")
	if err != nil {
		return err
	}
	g := securityGroup{
		ID: newID("sg"), VpcID: vpcID, Name: name, Description: description, Tags: list,
		Egress: []permission{{IPProtocol: "-1", CidrIPs: []string{"0.0.0.0/0"}}},
	}
	if err := c.svc.put(securityGroupsBucket, g.ID, g); err != nil {
		return internal(err)
	}
	return c.respond("CreateSecurityGroup", struct {
		GroupID string    `xml:"groupId"`
		Tags    []tagItem `xml:"tagSet>item,omitempty"`
	}{g.ID, tagItems(g.Tags)})
}

func (c *call) describeSecurityGroups() error {
	var out []securityGroupXML
	ids := listParam(c.form, "GroupId.")
	names := listParam(c.form, "GroupName.")
	err := describe(c, securityGroupsBucket, ids, "InvalidGroup.NotFound", "security group", func(g securityGroup) {
		if len(names) == 0 || contains(names, g.Name) {
			out = append(out, c.svc.securityGroupXML(g))
		}
	})
	if err != nil {
		return err
	}
	return c.respond("DescribeSecurityGroups", struct {
		Groups []securityGroupXML `xml:"securityGroupInfo>item"`
	}{out})
}

func (c *call) deleteSecurityGroup() error {
	id := c.form.Get("GroupId")
	var g securityGroup
	if err := c.svc.load(securityGroupsBucket, id, &g, "InvalidGroup.NotFound", "security group"); err != nil {
		return err
	}
	if g.Name == "default" {
		return badRequest("CannotDelete", "the specified group: \""+id+"\" name: \"default\" cannot be deleted by a user")
	}
	referenced := anyIn(c.svc, securityGroupsBucket, func(other securityGroup) bool {
		return other.ID != id && references(other, id)
	})
	if referenced {
		return badRequest("DependencyViolation", fmt.Sprintf("resource %s has a dependent object", id))
	}
	if err := c.svc.metadata.Delete(securityGroupsBucket, id); err != nil {
		return internal(err)
	}
	return c.returnTrue("DeleteSecurityGroup")
}

func references(g securityGroup, id string) bool {
	for _, rules := range [][]permission{g.Ingress, g.Egress} {
		for _, p := range rules {
			if contains(p.GroupIDs, id) {
				return true
			}
		}
	}
	return false
}

// changeRules authorizes (add) or revokes (!add) ingress or egress rules.
func (c *call) changeRules(ingress, add bool) error {
	id := c.form.Get("GroupId")
	var g securityGroup
	if err := c.svc.load(securityGroupsBucket, id, &g, "InvalidGroup.NotFound", "security group"); err != nil {
		return err
	}
	perms, err := permissionsFromForm(c.form)
	if err != nil {
		return err
	}
	rules := &g.Egress
	if ingress {
		rules = &g.Ingress
	}
	for _, p := range perms {
		i := indexOfPermission(*rules, p)
		switch {
		case add && i >= 0:
			return badRequest("InvalidPermission.Duplicate", "the specified rule already exists")
		case add:
			*rules = append(*rules, p)
		case i < 0:
			return badRequest("InvalidPermission.NotFound", "The specified rule does not exist in this security group.")
		default:
			*rules = append((*rules)[:i], (*rules)[i+1:]...)
		}
	}
	if err := c.svc.put(securityGroupsBucket, id, g); err != nil {
		return internal(err)
	}
	action := map[[2]bool]string{
		{true, true}: "AuthorizeSecurityGroupIngress", {false, true}: "AuthorizeSecurityGroupEgress",
		{true, false}: "RevokeSecurityGroupIngress", {false, false}: "RevokeSecurityGroupEgress",
	}[[2]bool{ingress, add}]
	return c.returnTrue(action)
}

func permissionsFromForm(form url.Values) ([]permission, error) {
	var out []permission
	for n := 1; ; n++ {
		base := "IpPermissions." + strconv.Itoa(n) + "."
		protocol := form.Get(base + "IpProtocol")
		if protocol == "" {
			break
		}
		if protocol == "all" {
			protocol = "-1"
		}
		p := permission{IPProtocol: protocol}
		for field, dst := range map[string]**int{"FromPort": &p.FromPort, "ToPort": &p.ToPort} {
			if raw := form.Get(base + field); raw != "" {
				v, err := strconv.Atoi(raw)
				if err != nil {
					return nil, invalidParameter("Invalid value '" + raw + "' for " + field)
				}
				*dst = &v
			}
		}
		for i := 1; ; i++ {
			cidr := form.Get(base + "IpRanges." + strconv.Itoa(i) + ".CidrIp")
			if cidr == "" {
				break
			}
			if _, _, err := parseCIDR(cidr); err != nil {
				return nil, invalidParameter("CIDR block " + cidr + " is malformed")
			}
			p.CidrIPs = append(p.CidrIPs, cidr)
		}
		for i := 1; ; i++ {
			group := form.Get(base + "Groups." + strconv.Itoa(i) + ".GroupId")
			if group == "" {
				break
			}
			p.GroupIDs = append(p.GroupIDs, group)
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil, missingParameter("IpPermissions")
	}
	return out, nil
}

func indexOfPermission(rules []permission, p permission) int {
	key := func(p permission) string {
		raw, _ := json.Marshal(p)
		return string(raw)
	}
	want := key(p)
	for i, r := range rules {
		if key(r) == want {
			return i
		}
	}
	return -1
}

// ---- Zones, account, tags ----

func (s *Service) zones() []string {
	region := s.scope.Region
	return []string{region + "a", region + "b", region + "c"}
}

func (c *call) describeAvailabilityZones() error {
	var out []zoneXML
	for i, name := range c.svc.zones() {
		out = append(out, zoneXML{
			ZoneName: name, ZoneID: fmt.Sprintf("%s-az%d", c.svc.scope.Region, i+1), State: "available",
			RegionName: c.svc.scope.Region, ZoneType: "availability-zone", OptInStatus: "opt-in-not-required",
			GroupName: c.svc.scope.Region,
		})
	}
	return c.respond("DescribeAvailabilityZones", struct {
		Zones []zoneXML `xml:"availabilityZoneInfo>item"`
	}{out})
}

func (c *call) describeAccountAttributes() error {
	type value struct {
		Value string `xml:"attributeValue"`
	}
	type attribute struct {
		Name   string  `xml:"attributeName"`
		Values []value `xml:"attributeValueSet>item"`
	}
	return c.respond("DescribeAccountAttributes", struct {
		Attributes []attribute `xml:"accountAttributeSet>item"`
	}{[]attribute{
		{Name: "supported-platforms", Values: []value{{"VPC"}}},
		{Name: "default-vpc", Values: []value{{"none"}}},
	}})
}

// taggable resource buckets, by ID prefix.
var taggableBuckets = map[string]string{
	"vpc": vpcsBucket, "subnet": subnetsBucket, "igw": gatewaysBucket,
	"rtb": routeTablesBucket, "sg": securityGroupsBucket,
}

func (c *call) changeTags(add bool) error {
	ids := listParam(c.form, "ResourceId.")
	list, err := tags.FromQuery(c.form, "Tag.")
	if err != nil {
		return invalidParameter(err.Error())
	}
	for _, id := range ids {
		bucket, ok := taggableBuckets[prefixOf(id)]
		if !ok {
			return badRequest("InvalidID", fmt.Sprintf("The ID '%s' is not valid", id))
		}
		raw, err := c.svc.metadata.Get(bucket, id)
		if err != nil {
			return internal(err)
		}
		if raw == nil {
			return badRequest("InvalidID", fmt.Sprintf("The ID '%s' does not exist", id))
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			return internal(err)
		}
		var current tags.List
		if t, ok := doc["tags"]; ok {
			_ = json.Unmarshal(t, &current)
		}
		if add {
			current = current.Merge(list)
			if err := current.Validate(tags.MaxPerResource); err != nil {
				return badRequest("TagLimitExceeded", err.Error())
			}
		} else {
			keys := make([]string, 0, len(list))
			for _, t := range list {
				keys = append(keys, t.Key)
			}
			current = current.Without(keys)
		}
		encoded, _ := json.Marshal(current)
		doc["tags"] = encoded
		updated, _ := json.Marshal(doc)
		if err := c.svc.metadata.Put(bucket, id, updated); err != nil {
			return internal(err)
		}
	}
	if add {
		return c.returnTrue("CreateTags")
	}
	return c.returnTrue("DeleteTags")
}

func (c *call) describeTags() error {
	filters := filtersFromForm(c.form)
	type tagDescription struct {
		ResourceID   string `xml:"resourceId"`
		ResourceType string `xml:"resourceType"`
		Key          string `xml:"key"`
		Value        string `xml:"value"`
	}
	resourceTypes := map[string]string{"vpc": "vpc", "subnet": "subnet", "igw": "internet-gateway", "rtb": "route-table", "sg": "security-group"}
	var out []tagDescription
	prefixes := make([]string, 0, len(taggableBuckets))
	for p := range taggableBuckets {
		prefixes = append(prefixes, p)
	}
	sort.Strings(prefixes)
	for _, prefix := range prefixes {
		_ = c.svc.metadata.Scan(taggableBuckets[prefix], "", func(k, v []byte) error {
			var doc struct {
				Tags tags.List `json:"tags"`
			}
			if json.Unmarshal(v, &doc) != nil {
				return nil
			}
			for _, t := range doc.Tags {
				d := tagDescription{ResourceID: string(k), ResourceType: resourceTypes[prefix], Key: t.Key, Value: t.Value}
				fields := map[string][]string{"resource-id": {d.ResourceID}, "resource-type": {d.ResourceType}, "key": {d.Key}, "value": {d.Value}}
				if matches(staticFields(fields), filters) {
					out = append(out, d)
				}
			}
			return nil
		})
	}
	return c.respond("DescribeTags", struct {
		Tags []tagDescription `xml:"tagSet>item"`
	}{out})
}

type staticFields map[string][]string

func (f staticFields) filterValues(name string) []string { return f[name] }
