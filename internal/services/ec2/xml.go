package ec2

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"strings"

	"github.com/stratus/internal/tags"
)

const namespace = "http://ec2.amazonaws.com/doc/2016-11-15/"

// writeXML writes body's fields inside <ActionResponse>, after requestId, as
// EC2's query protocol does.
func writeXML(w http.ResponseWriter, action, requestID string, body any) {
	var inner bytes.Buffer
	_ = xml.NewEncoder(&inner).EncodeElement(body, xml.StartElement{Name: xml.Name{Local: "x"}})
	fields := strings.TrimSuffix(strings.TrimPrefix(inner.String(), "<x>"), "</x>")

	var out bytes.Buffer
	out.WriteString(xml.Header)
	out.WriteString("<" + action + `Response xmlns="` + namespace + `"><requestId>`)
	_ = xml.EscapeText(&out, []byte(requestID))
	out.WriteString("</requestId>" + fields + "</" + action + "Response>")

	w.Header().Set("Content-Type", "text/xml;charset=UTF-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out.Bytes())
}

type returnXML struct {
	Return bool `xml:"return"`
}

type tagItem struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

func tagItems(list tags.List) []tagItem {
	out := make([]tagItem, 0, len(list))
	for _, t := range list {
		out = append(out, tagItem{t.Key, t.Value})
	}
	return out
}

type stateXML struct {
	State string `xml:"state"`
}

type cidrAssociationXML struct {
	AssociationID string   `xml:"associationId"`
	CidrBlock     string   `xml:"cidrBlock"`
	State         stateXML `xml:"cidrBlockState"`
}

type vpcXML struct {
	VpcID           string               `xml:"vpcId"`
	OwnerID         string               `xml:"ownerId"`
	State           string               `xml:"state"`
	CidrBlock       string               `xml:"cidrBlock"`
	Associations    []cidrAssociationXML `xml:"cidrBlockAssociationSet>item"`
	DhcpOptionsID   string               `xml:"dhcpOptionsId"`
	InstanceTenancy string               `xml:"instanceTenancy"`
	IsDefault       bool                 `xml:"isDefault"`
	Tags            []tagItem            `xml:"tagSet>item,omitempty"`
}

func (s *Service) vpcXML(v vpc) vpcXML {
	return vpcXML{
		VpcID: v.ID, OwnerID: s.scope.Account, State: "available", CidrBlock: v.CidrBlock,
		Associations:  []cidrAssociationXML{{AssociationID: "vpc-cidr-assoc-" + suffix(v.ID), CidrBlock: v.CidrBlock, State: stateXML{"associated"}}},
		DhcpOptionsID: "default", InstanceTenancy: v.InstanceTenancy, Tags: tagItems(v.Tags),
	}
}

type subnetXML struct {
	SubnetID                string    `xml:"subnetId"`
	SubnetArn               string    `xml:"subnetArn"`
	OwnerID                 string    `xml:"ownerId"`
	State                   string    `xml:"state"`
	VpcID                   string    `xml:"vpcId"`
	CidrBlock               string    `xml:"cidrBlock"`
	AvailableIPAddressCount int       `xml:"availableIpAddressCount"`
	AvailabilityZone        string    `xml:"availabilityZone"`
	AvailabilityZoneID      string    `xml:"availabilityZoneId"`
	DefaultForAz            bool      `xml:"defaultForAz"`
	MapPublicIPOnLaunch     bool      `xml:"mapPublicIpOnLaunch"`
	Tags                    []tagItem `xml:"tagSet>item,omitempty"`
}

func (s *Service) subnetXML(sn subnet) subnetXML {
	available := 0
	if _, network, err := parseCIDR(sn.CidrBlock); err == nil {
		ones, bits := network.Mask.Size()
		available = 1<<(bits-ones) - 5 // AWS reserves five addresses per subnet
	}
	return subnetXML{
		SubnetID: sn.ID, SubnetArn: s.scope.ARN("ec2", "subnet/"+sn.ID), OwnerID: s.scope.Account,
		State: "available", VpcID: sn.VpcID, CidrBlock: sn.CidrBlock, AvailableIPAddressCount: available,
		AvailabilityZone: sn.AvailabilityZone, AvailabilityZoneID: s.zoneID(sn.AvailabilityZone),
		MapPublicIPOnLaunch: sn.MapPublicIPOnLaunch, Tags: tagItems(sn.Tags),
	}
}

type attachmentXML struct {
	VpcID string `xml:"vpcId"`
	State string `xml:"state"`
}

type gatewayXML struct {
	InternetGatewayID string          `xml:"internetGatewayId"`
	OwnerID           string          `xml:"ownerId"`
	Attachments       []attachmentXML `xml:"attachmentSet>item"`
	Tags              []tagItem       `xml:"tagSet>item,omitempty"`
}

func (s *Service) gatewayXML(g internetGateway) gatewayXML {
	out := gatewayXML{InternetGatewayID: g.ID, OwnerID: s.scope.Account, Tags: tagItems(g.Tags)}
	if g.VpcID != "" {
		out.Attachments = []attachmentXML{{VpcID: g.VpcID, State: "available"}}
	}
	return out
}

type routeXML struct {
	DestinationCidrBlock string `xml:"destinationCidrBlock"`
	GatewayID            string `xml:"gatewayId"`
	State                string `xml:"state"`
	Origin               string `xml:"origin"`
}

type routeAssociationXML struct {
	AssociationID string   `xml:"routeTableAssociationId"`
	RouteTableID  string   `xml:"routeTableId"`
	Main          bool     `xml:"main"`
	State         stateXML `xml:"associationState"`
}

type routeTableXML struct {
	RouteTableID string                `xml:"routeTableId"`
	VpcID        string                `xml:"vpcId"`
	OwnerID      string                `xml:"ownerId"`
	Routes       []routeXML            `xml:"routeSet>item"`
	Associations []routeAssociationXML `xml:"associationSet>item"`
	Tags         []tagItem             `xml:"tagSet>item,omitempty"`
}

func (s *Service) routeTableXML(t routeTable) routeTableXML {
	out := routeTableXML{RouteTableID: t.ID, VpcID: t.VpcID, OwnerID: s.scope.Account, Tags: tagItems(t.Tags)}
	for _, r := range t.Routes {
		origin := "CreateRoute"
		if r.GatewayID == "local" {
			origin = "CreateRouteTable"
		}
		out.Routes = append(out.Routes, routeXML{DestinationCidrBlock: r.DestinationCidrBlock, GatewayID: r.GatewayID, State: "active", Origin: origin})
	}
	if t.Main {
		out.Associations = []routeAssociationXML{{AssociationID: "rtbassoc-" + suffix(t.ID), RouteTableID: t.ID, Main: true, State: stateXML{"associated"}}}
	}
	return out
}

type groupPairXML struct {
	UserID  string `xml:"userId"`
	GroupID string `xml:"groupId"`
}

type ipRangeXML struct {
	CidrIP string `xml:"cidrIp"`
}

type permissionXML struct {
	IPProtocol string         `xml:"ipProtocol"`
	FromPort   *int           `xml:"fromPort,omitempty"`
	ToPort     *int           `xml:"toPort,omitempty"`
	Groups     []groupPairXML `xml:"groups>item"`
	IPRanges   []ipRangeXML   `xml:"ipRanges>item"`
}

type securityGroupXML struct {
	OwnerID     string          `xml:"ownerId"`
	GroupID     string          `xml:"groupId"`
	GroupName   string          `xml:"groupName"`
	Description string          `xml:"groupDescription"`
	VpcID       string          `xml:"vpcId"`
	Ingress     []permissionXML `xml:"ipPermissions>item"`
	Egress      []permissionXML `xml:"ipPermissionsEgress>item"`
	Tags        []tagItem       `xml:"tagSet>item,omitempty"`
}

func (s *Service) securityGroupXML(g securityGroup) securityGroupXML {
	convert := func(rules []permission) []permissionXML {
		out := make([]permissionXML, 0, len(rules))
		for _, p := range rules {
			x := permissionXML{IPProtocol: p.IPProtocol, FromPort: p.FromPort, ToPort: p.ToPort}
			for _, id := range p.GroupIDs {
				x.Groups = append(x.Groups, groupPairXML{UserID: s.scope.Account, GroupID: id})
			}
			for _, cidr := range p.CidrIPs {
				x.IPRanges = append(x.IPRanges, ipRangeXML{cidr})
			}
			out = append(out, x)
		}
		return out
	}
	return securityGroupXML{
		OwnerID: s.scope.Account, GroupID: g.ID, GroupName: g.Name, Description: g.Description, VpcID: g.VpcID,
		Ingress: convert(g.Ingress), Egress: convert(g.Egress), Tags: tagItems(g.Tags),
	}
}

type zoneXML struct {
	ZoneName    string `xml:"zoneName"`
	State       string `xml:"zoneState"`
	RegionName  string `xml:"regionName"`
	ZoneID      string `xml:"zoneId"`
	ZoneType    string `xml:"zoneType"`
	OptInStatus string `xml:"optInStatus"`
	GroupName   string `xml:"groupName"`
}
