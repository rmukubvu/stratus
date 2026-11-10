package route53

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	// PathPrefix is where every Route 53 operation lives.
	PathPrefix = "/2013-04-01/"
	xmlns      = "https://route53.amazonaws.com/doc/2013-04-01/"

	defaultMaxItems = 100
)

type route struct {
	method    string
	pattern   []string
	operation string
}

var routes = []route{
	{http.MethodPost, []string{"hostedzone"}, "CreateHostedZone"},
	{http.MethodGet, []string{"hostedzone"}, "ListHostedZones"},
	{http.MethodGet, []string{"hostedzone", "{id}"}, "GetHostedZone"},
	{http.MethodPost, []string{"hostedzone", "{id}"}, "UpdateHostedZoneComment"},
	{http.MethodDelete, []string{"hostedzone", "{id}"}, "DeleteHostedZone"},
	{http.MethodPost, []string{"hostedzone", "{id}", "rrset"}, "ChangeResourceRecordSets"},
	{http.MethodGet, []string{"hostedzone", "{id}", "rrset"}, "ListResourceRecordSets"},
	{http.MethodGet, []string{"hostedzonesbyname"}, "ListHostedZonesByName"},
	{http.MethodGet, []string{"hostedzonecount"}, "GetHostedZoneCount"},
	{http.MethodGet, []string{"change", "{id}"}, "GetChange"},
	{http.MethodGet, []string{"tags", "hostedzone", "{id}"}, "ListTagsForResource"},
	{http.MethodPost, []string{"tags", "hostedzone", "{id}"}, "ChangeTagsForResource"},
}

func match(method, path string) (operation, id string) {
	if !strings.HasPrefix(path, PathPrefix) {
		return "", ""
	}
	segments := strings.Split(strings.Trim(strings.TrimPrefix(path, PathPrefix), "/"), "/")
	for _, r := range routes {
		if r.method != method || len(r.pattern) != len(segments) {
			continue
		}
		id, ok := "", true
		for i, part := range r.pattern {
			switch {
			case part == "{id}":
				id = segments[i]
			case part != segments[i]:
				ok = false
			}
		}
		if ok {
			return r.operation, id
		}
	}
	return "", ""
}

// Operation names the Route 53 operation for a request, or "" when the
// request is not one this service serves.
func Operation(method, path string) string {
	op, _ := match(method, path)
	return op
}

// Handle serves one Route 53 REST request.
func (s *Service) Handle(w http.ResponseWriter, r *http.Request) error {
	op, id := match(r.Method, r.URL.Path)
	switch op {
	case "CreateHostedZone":
		return s.handleCreateHostedZone(w, r)
	case "ListHostedZones":
		return s.handleListHostedZones(w, r)
	case "ListHostedZonesByName":
		return s.handleListHostedZonesByName(w, r)
	case "GetHostedZoneCount":
		zones, err := s.Zones("")
		if err != nil {
			return err
		}
		return writeXML(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"GetHostedZoneCountResponse"`
			XMLNS   string   `xml:"xmlns,attr"`
			Count   int      `xml:"HostedZoneCount"`
		}{XMLNS: xmlns, Count: len(zones)})
	case "GetHostedZone":
		zone, err := s.Zone(id)
		if err != nil {
			return err
		}
		return writeXML(w, http.StatusOK, getHostedZoneResponse{XMLNS: xmlns, zoneDetails: detailsXML(zone)})
	case "UpdateHostedZoneComment":
		return s.handleUpdateComment(w, r, id)
	case "DeleteHostedZone":
		change, err := s.DeleteZone(id)
		if err != nil {
			return err
		}
		return writeXML(w, http.StatusOK, changeResponse{XMLName: xml.Name{Local: "DeleteHostedZoneResponse"}, XMLNS: xmlns, ChangeInfo: changeXML(change)})
	case "ChangeResourceRecordSets":
		return s.handleChangeRecordSets(w, r, id)
	case "ListResourceRecordSets":
		return s.handleListRecordSets(w, r, id)
	case "GetChange":
		change, err := s.Change(id)
		if err != nil {
			return err
		}
		return writeXML(w, http.StatusOK, changeResponse{XMLName: xml.Name{Local: "GetChangeResponse"}, XMLNS: xmlns, ChangeInfo: changeXML(change)})
	case "ListTagsForResource":
		zone, err := s.Zone(id)
		if err != nil {
			return err
		}
		return writeXML(w, http.StatusOK, listTagsResponse{XMLNS: xmlns, TagSet: tagSetXML{ResourceType: "hostedzone", ResourceID: zone.ID, Tags: tagsXML(zone.Tags)}})
	case "ChangeTagsForResource":
		var in changeTagsRequest
		if err := decode(r, &in); err != nil {
			return err
		}
		add := map[string]string{}
		for _, t := range in.AddTags {
			add[t.Key] = t.Value
		}
		if err := s.SetTags(id, add, in.RemoveTagKeys); err != nil {
			return err
		}
		return writeXML(w, http.StatusOK, struct {
			XMLName xml.Name `xml:"ChangeTagsForResourceResponse"`
			XMLNS   string   `xml:"xmlns,attr"`
		}{XMLNS: xmlns})
	default:
		return invalidInput("no Route 53 operation matches %s %s", r.Method, r.URL.Path)
	}
}

func (s *Service) handleCreateHostedZone(w http.ResponseWriter, r *http.Request) error {
	var in struct {
		Name             string  `xml:"Name"`
		CallerReference  string  `xml:"CallerReference"`
		VPC              *vpcXML `xml:"VPC"`
		HostedZoneConfig struct {
			Comment     string `xml:"Comment"`
			PrivateZone bool   `xml:"PrivateZone"`
		} `xml:"HostedZoneConfig"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	var vpc *VPC
	if in.VPC != nil {
		vpc = &VPC{Region: in.VPC.Region, ID: in.VPC.ID}
	}
	if in.HostedZoneConfig.PrivateZone && vpc == nil {
		return invalidInput("a private hosted zone needs a VPC")
	}
	zone, change, err := s.CreateZone(in.Name, in.CallerReference, in.HostedZoneConfig.Comment, in.HostedZoneConfig.PrivateZone, vpc)
	if err != nil {
		return err
	}
	w.Header().Set("Location", "https://route53.amazonaws.com/2013-04-01/hostedzone/"+zone.ID)
	details := detailsXML(zone)
	resp := struct {
		XMLName    xml.Name `xml:"CreateHostedZoneResponse"`
		XMLNS      string   `xml:"xmlns,attr"`
		HostedZone zoneXML  `xml:"HostedZone"`
		ChangeInfo changeInfoXML
		Delegation *delegationXML `xml:"DelegationSet,omitempty"`
		VPC        *vpcXML        `xml:"VPC,omitempty"`
	}{XMLNS: xmlns, HostedZone: details.HostedZone, ChangeInfo: changeXML(change), Delegation: details.Delegation}
	if len(details.VPCs) > 0 {
		resp.VPC = &details.VPCs[0]
	}
	return writeXML(w, http.StatusCreated, resp)
}

func (s *Service) handleListHostedZones(w http.ResponseWriter, r *http.Request) error {
	zones, err := s.Zones("")
	if err != nil {
		return err
	}
	q := r.URL.Query()
	maxItems := maxItemsParam(q)
	start := 0
	if marker := q.Get("marker"); marker != "" {
		for i, z := range zones {
			if z.ID == marker {
				start = i
			}
		}
	}
	page, next := pageZones(zones[start:], maxItems)
	resp := struct {
		XMLName     xml.Name  `xml:"ListHostedZonesResponse"`
		XMLNS       string    `xml:"xmlns,attr"`
		HostedZones []zoneXML `xml:"HostedZones>HostedZone"`
		Marker      string    `xml:"Marker"`
		IsTruncated bool      `xml:"IsTruncated"`
		NextMarker  string    `xml:"NextMarker,omitempty"`
		MaxItems    int       `xml:"MaxItems"`
	}{XMLNS: xmlns, HostedZones: zonesXML(page), Marker: q.Get("marker"), IsTruncated: next != nil, MaxItems: maxItems}
	if next != nil {
		resp.NextMarker = next.ID
	}
	return writeXML(w, http.StatusOK, resp)
}

func (s *Service) handleListHostedZonesByName(w http.ResponseWriter, r *http.Request) error {
	q := r.URL.Query()
	zones, err := s.Zones(q.Get("dnsname"))
	if err != nil {
		return err
	}
	maxItems := maxItemsParam(q)
	page, next := pageZones(zones, maxItems)
	resp := struct {
		XMLName          xml.Name  `xml:"ListHostedZonesByNameResponse"`
		XMLNS            string    `xml:"xmlns,attr"`
		HostedZones      []zoneXML `xml:"HostedZones>HostedZone"`
		DNSName          string    `xml:"DNSName,omitempty"`
		IsTruncated      bool      `xml:"IsTruncated"`
		NextDNSName      string    `xml:"NextDNSName,omitempty"`
		NextHostedZoneID string    `xml:"NextHostedZoneId,omitempty"`
		MaxItems         int       `xml:"MaxItems"`
	}{XMLNS: xmlns, HostedZones: zonesXML(page), DNSName: q.Get("dnsname"), IsTruncated: next != nil, MaxItems: maxItems}
	if next != nil {
		resp.NextDNSName, resp.NextHostedZoneID = next.Name, next.ID
	}
	return writeXML(w, http.StatusOK, resp)
}

func (s *Service) handleUpdateComment(w http.ResponseWriter, r *http.Request, id string) error {
	var in struct {
		Comment string `xml:"Comment"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	s.mu.Lock()
	zone, err := s.load(id)
	if err == nil {
		zone.Comment = in.Comment
		err = s.put(zone)
	}
	s.mu.Unlock()
	if err != nil {
		return err
	}
	return writeXML(w, http.StatusOK, struct {
		XMLName    xml.Name `xml:"UpdateHostedZoneCommentResponse"`
		XMLNS      string   `xml:"xmlns,attr"`
		HostedZone zoneXML  `xml:"HostedZone"`
	}{XMLNS: xmlns, HostedZone: detailsXML(zone).HostedZone})
}

func (s *Service) handleChangeRecordSets(w http.ResponseWriter, r *http.Request, id string) error {
	var in struct {
		ChangeBatch struct {
			Comment string `xml:"Comment"`
			Changes []struct {
				Action    string       `xml:"Action"`
				RecordSet recordSetXML `xml:"ResourceRecordSet"`
			} `xml:"Changes>Change"`
		} `xml:"ChangeBatch"`
	}
	if err := decode(r, &in); err != nil {
		return err
	}
	changes := make([]Change, 0, len(in.ChangeBatch.Changes))
	for _, c := range in.ChangeBatch.Changes {
		changes = append(changes, Change{Action: strings.ToUpper(c.Action), RecordSet: c.RecordSet.domain()})
	}
	change, err := s.ChangeRecordSets(id, in.ChangeBatch.Comment, changes)
	if err != nil {
		return err
	}
	return writeXML(w, http.StatusOK, changeResponse{XMLName: xml.Name{Local: "ChangeResourceRecordSetsResponse"}, XMLNS: xmlns, ChangeInfo: changeXML(change)})
}

func (s *Service) handleListRecordSets(w http.ResponseWriter, r *http.Request, id string) error {
	zone, err := s.Zone(id)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	maxItems := maxItemsParam(q)
	sets := zone.RecordSets
	if name := q.Get("name"); name != "" {
		start, err := normalizeName(name)
		if err != nil {
			return err
		}
		rtype, setID := strings.ToUpper(q.Get("type")), q.Get("identifier")
		first := sort.Search(len(sets), func(i int) bool {
			return !lessRecord(sets[i].Name, sets[i].Type, sets[i].SetIdentifier, RecordSet{Name: start, Type: rtype, SetIdentifier: setID})
		})
		sets = sets[first:]
	}
	resp := struct {
		XMLName              xml.Name       `xml:"ListResourceRecordSetsResponse"`
		XMLNS                string         `xml:"xmlns,attr"`
		RecordSets           []recordSetXML `xml:"ResourceRecordSets>ResourceRecordSet"`
		IsTruncated          bool           `xml:"IsTruncated"`
		NextRecordName       string         `xml:"NextRecordName,omitempty"`
		NextRecordType       string         `xml:"NextRecordType,omitempty"`
		NextRecordIdentifier string         `xml:"NextRecordIdentifier,omitempty"`
		MaxItems             int            `xml:"MaxItems"`
	}{XMLNS: xmlns, MaxItems: maxItems, RecordSets: []recordSetXML{}}
	for i, rs := range sets {
		if i == maxItems {
			resp.IsTruncated = true
			resp.NextRecordName, resp.NextRecordType, resp.NextRecordIdentifier = display(rs.Name), rs.Type, rs.SetIdentifier
			break
		}
		resp.RecordSets = append(resp.RecordSets, toRecordSetXML(rs))
	}
	return writeXML(w, http.StatusOK, resp)
}

// XML shapes.

type vpcXML struct {
	Region string `xml:"VPCRegion"`
	ID     string `xml:"VPCId"`
}

type zoneXML struct {
	ID              string `xml:"Id"`
	Name            string `xml:"Name"`
	CallerReference string `xml:"CallerReference"`
	Config          struct {
		Comment     string `xml:"Comment,omitempty"`
		PrivateZone bool   `xml:"PrivateZone"`
	} `xml:"Config"`
	RecordCount   int `xml:"ResourceRecordSetCount"`
	LinkedService *struct {
		ServicePrincipal string `xml:"ServicePrincipal"`
	} `xml:"LinkedService,omitempty"`
}

type delegationXML struct {
	NameServers []string `xml:"NameServers>NameServer"`
}

type zoneDetails struct {
	HostedZone zoneXML        `xml:"HostedZone"`
	Delegation *delegationXML `xml:"DelegationSet,omitempty"`
	VPCs       []vpcXML       `xml:"VPCs>VPC,omitempty"`
}

type getHostedZoneResponse struct {
	XMLName xml.Name `xml:"GetHostedZoneResponse"`
	XMLNS   string   `xml:"xmlns,attr"`
	zoneDetails
}

type changeInfoXML struct {
	XMLName     xml.Name `xml:"ChangeInfo"`
	ID          string   `xml:"Id"`
	Status      string   `xml:"Status"`
	SubmittedAt string   `xml:"SubmittedAt"`
	Comment     string   `xml:"Comment,omitempty"`
}

type changeResponse struct {
	XMLName    xml.Name
	XMLNS      string `xml:"xmlns,attr"`
	ChangeInfo changeInfoXML
}

type resourceRecordXML struct {
	Value string `xml:"Value"`
}

type aliasXML struct {
	HostedZoneID         string `xml:"HostedZoneId"`
	DNSName              string `xml:"DNSName"`
	EvaluateTargetHealth bool   `xml:"EvaluateTargetHealth"`
}

type recordSetXML struct {
	Name             string              `xml:"Name"`
	Type             string              `xml:"Type"`
	SetIdentifier    string              `xml:"SetIdentifier,omitempty"`
	Weight           *int64              `xml:"Weight,omitempty"`
	Region           string              `xml:"Region,omitempty"`
	Failover         string              `xml:"Failover,omitempty"`
	MultiValueAnswer *bool               `xml:"MultiValueAnswer,omitempty"`
	TTL              *int64              `xml:"TTL,omitempty"`
	ResourceRecords  []resourceRecordXML `xml:"ResourceRecords>ResourceRecord,omitempty"`
	AliasTarget      *aliasXML           `xml:"AliasTarget,omitempty"`
	HealthCheckID    string              `xml:"HealthCheckId,omitempty"`
}

func (x recordSetXML) domain() RecordSet {
	rs := RecordSet{
		Name: x.Name, Type: x.Type, SetIdentifier: x.SetIdentifier, TTL: x.TTL, Weight: x.Weight,
		Region: x.Region, Failover: x.Failover, MultiValueAnswer: x.MultiValueAnswer, HealthCheckID: x.HealthCheckID,
	}
	for _, rr := range x.ResourceRecords {
		rs.Values = append(rs.Values, rr.Value)
	}
	if a := x.AliasTarget; a != nil {
		rs.Alias = &AliasTarget{HostedZoneID: a.HostedZoneID, DNSName: a.DNSName, EvaluateTargetHealth: a.EvaluateTargetHealth}
	}
	return rs
}

func toRecordSetXML(rs RecordSet) recordSetXML {
	x := recordSetXML{
		Name: display(rs.Name), Type: rs.Type, SetIdentifier: rs.SetIdentifier, TTL: rs.TTL, Weight: rs.Weight,
		Region: rs.Region, Failover: rs.Failover, MultiValueAnswer: rs.MultiValueAnswer, HealthCheckID: rs.HealthCheckID,
	}
	for _, v := range rs.Values {
		x.ResourceRecords = append(x.ResourceRecords, resourceRecordXML{Value: v})
	}
	if a := rs.Alias; a != nil {
		x.AliasTarget = &aliasXML{HostedZoneID: a.HostedZoneID, DNSName: a.DNSName, EvaluateTargetHealth: a.EvaluateTargetHealth}
	}
	return x
}

type tagXML struct {
	Key   string `xml:"Key"`
	Value string `xml:"Value"`
}

type tagSetXML struct {
	ResourceType string   `xml:"ResourceType"`
	ResourceID   string   `xml:"ResourceId"`
	Tags         []tagXML `xml:"Tags>Tag"`
}

type listTagsResponse struct {
	XMLName xml.Name  `xml:"ListTagsForResourceResponse"`
	XMLNS   string    `xml:"xmlns,attr"`
	TagSet  tagSetXML `xml:"ResourceTagSet"`
}

type changeTagsRequest struct {
	AddTags       []tagXML `xml:"AddTags>Tag"`
	RemoveTagKeys []string `xml:"RemoveTagKeys>Key"`
}

func detailsXML(zone HostedZone) zoneDetails {
	var z zoneXML
	z.ID, z.Name, z.CallerReference = "/hostedzone/"+zone.ID, zone.Name, zone.CallerReference
	z.Config.Comment, z.Config.PrivateZone = zone.Comment, zone.Private
	z.RecordCount = len(zone.RecordSets)
	if zone.ManagedBy != "" {
		z.LinkedService = &struct {
			ServicePrincipal string `xml:"ServicePrincipal"`
		}{ServicePrincipal: zone.ManagedBy}
	}
	out := zoneDetails{HostedZone: z}
	if zone.Private {
		for _, v := range zone.VPCs {
			out.VPCs = append(out.VPCs, vpcXML{Region: v.Region, ID: v.ID})
		}
	} else {
		out.Delegation = &delegationXML{NameServers: defaultNameServers}
	}
	return out
}

func zonesXML(zones []HostedZone) []zoneXML {
	out := make([]zoneXML, 0, len(zones))
	for _, z := range zones {
		out = append(out, detailsXML(z).HostedZone)
	}
	return out
}

func changeXML(c ChangeInfo) changeInfoXML {
	return changeInfoXML{ID: "/change/" + c.ID, Status: c.Status, SubmittedAt: c.SubmittedAt.UTC().Format(time.RFC3339Nano), Comment: c.Comment}
}

func tagsXML(tags map[string]string) []tagXML {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]tagXML, 0, len(keys))
	for _, k := range keys {
		out = append(out, tagXML{Key: k, Value: tags[k]})
	}
	return out
}

func pageZones(zones []HostedZone, maxItems int) (page []HostedZone, next *HostedZone) {
	if len(zones) <= maxItems {
		return zones, nil
	}
	return zones[:maxItems], &zones[maxItems]
}

func maxItemsParam(q url.Values) int {
	if n, err := strconv.Atoi(q.Get("maxitems")); err == nil && n > 0 {
		return n
	}
	return defaultMaxItems
}

func decode(r *http.Request, into any) error {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return invalidInput("read request body: %v", err)
	}
	if len(body) == 0 {
		return invalidInput("request body is required")
	}
	if err := xml.Unmarshal(body, into); err != nil {
		return invalidInput("request body is not valid XML: %v", err)
	}
	return nil
}

func writeXML(w http.ResponseWriter, status int, payload any) error {
	w.Header().Set("Content-Type", "text/xml")
	w.WriteHeader(status)
	if _, err := io.WriteString(w, xml.Header); err != nil {
		return err
	}
	return xml.NewEncoder(w).Encode(payload)
}
