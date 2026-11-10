package route53

import (
	"sort"
	"strings"
	"time"
)

// Change actions, as ChangeResourceRecordSets accepts them.
const (
	ActionCreate = "CREATE"
	ActionDelete = "DELETE"
	ActionUpsert = "UPSERT"
)

// defaultNameServers are the delegation set every zone reports.
var defaultNameServers = []string{
	"ns-2048.awsdns-64.com.",
	"ns-2049.awsdns-65.net.",
	"ns-2050.awsdns-66.org.",
	"ns-2051.awsdns-67.co.uk.",
}

// VPC is a VPC associated with a private hosted zone.
type VPC struct {
	Region string `json:"region"`
	ID     string `json:"id"`
}

// AliasTarget points a record set at another DNS name, usually a load
// balancer, distribution or another record in a hosted zone.
type AliasTarget struct {
	HostedZoneID         string `json:"hosted_zone_id"`
	DNSName              string `json:"dns_name"`
	EvaluateTargetHealth bool   `json:"evaluate_target_health"`
}

// RecordSet is one resource record set. Name, Type and SetIdentifier
// identify it within a zone; routing fields other than those are kept
// verbatim so describe calls echo them.
type RecordSet struct {
	Name             string       `json:"name"`
	Type             string       `json:"type"`
	SetIdentifier    string       `json:"set_identifier,omitempty"`
	TTL              *int64       `json:"ttl,omitempty"`
	Values           []string     `json:"values,omitempty"`
	Alias            *AliasTarget `json:"alias,omitempty"`
	Weight           *int64       `json:"weight,omitempty"`
	Region           string       `json:"region,omitempty"`
	Failover         string       `json:"failover,omitempty"`
	MultiValueAnswer *bool        `json:"multi_value_answer,omitempty"`
	HealthCheckID    string       `json:"health_check_id,omitempty"`
}

func (r RecordSet) key() string {
	return r.Name + "|" + r.Type + "|" + r.SetIdentifier
}

// sameAs reports whether r matches other exactly, as a DELETE requires.
func (r RecordSet) sameAs(other RecordSet) bool {
	if r.key() != other.key() || !equalInt(r.TTL, other.TTL) || len(r.Values) != len(other.Values) {
		return false
	}
	for i := range r.Values {
		if r.Values[i] != other.Values[i] {
			return false
		}
	}
	if (r.Alias == nil) != (other.Alias == nil) {
		return false
	}
	return r.Alias == nil || strings.EqualFold(r.Alias.DNSName, other.Alias.DNSName)
}

// Change is one action in a change batch.
type Change struct {
	Action    string
	RecordSet RecordSet
}

// HostedZone is a DNS zone and the record sets in it.
type HostedZone struct {
	ID              string            `json:"id"`
	Name            string            `json:"name"`
	CallerReference string            `json:"caller_reference"`
	Comment         string            `json:"comment,omitempty"`
	Private         bool              `json:"private"`
	VPCs            []VPC             `json:"vpcs,omitempty"`
	Created         time.Time         `json:"created"`
	Tags            map[string]string `json:"tags,omitempty"`
	// ManagedBy names the service that created the zone, such as Cloud Map,
	// or is empty for zones created through the Route 53 API.
	ManagedBy  string      `json:"managed_by,omitempty"`
	RecordSets []RecordSet `json:"record_sets"`
}

// newHostedZone builds a zone with the SOA and NS records Route 53 creates
// for every zone.
func newHostedZone(id, name, callerReference string, now time.Time) (HostedZone, error) {
	fqdn, err := normalizeName(name)
	if err != nil {
		return HostedZone{}, err
	}
	if callerReference == "" {
		return HostedZone{}, invalidInput("CallerReference is required")
	}
	nsTTL, soaTTL := int64(172800), int64(900)
	return HostedZone{
		ID:              id,
		Name:            fqdn,
		CallerReference: callerReference,
		Created:         now.UTC(),
		RecordSets: []RecordSet{
			{Name: fqdn, Type: "NS", TTL: &nsTTL, Values: append([]string(nil), defaultNameServers...)},
			{Name: fqdn, Type: "SOA", TTL: &soaTTL, Values: []string{defaultNameServers[0] + " awsdns-hostmaster.amazon.com. 1 7200 900 1209600 86400"}},
		},
	}, nil
}

// apply validates every change against the zone and then applies them all,
// so a batch either succeeds whole or leaves the zone untouched.
func (z *HostedZone) apply(changes []Change) error {
	if len(changes) == 0 {
		return invalidInput("ChangeBatch must contain at least one change")
	}
	sets := make(map[string]RecordSet, len(z.RecordSets))
	for _, rs := range z.RecordSets {
		sets[rs.key()] = rs
	}
	for _, c := range changes {
		rs, err := z.normalize(c.RecordSet)
		if err != nil {
			return err
		}
		existing, exists := sets[rs.key()]
		switch c.Action {
		case ActionCreate:
			if exists {
				return invalidChangeBatch("Tried to create resource record set [name='%s', type='%s'] but it already exists", display(rs.Name), rs.Type)
			}
			sets[rs.key()] = rs
		case ActionUpsert:
			sets[rs.key()] = rs
		case ActionDelete:
			if !exists || !existing.sameAs(rs) {
				return invalidChangeBatch("Tried to delete resource record set [name='%s', type='%s'] but it was not found", display(rs.Name), rs.Type)
			}
			if z.isApexDefault(rs) {
				return invalidChangeBatch("A HostedZone must contain at least one NS record and one SOA record for the zone itself")
			}
			delete(sets, rs.key())
		default:
			return invalidInput("unknown change action %q", c.Action)
		}
	}
	if err := checkCNAMEConflicts(sets); err != nil {
		return err
	}
	z.RecordSets = z.RecordSets[:0]
	for _, rs := range sets {
		z.RecordSets = append(z.RecordSets, rs)
	}
	sortRecordSets(z.RecordSets)
	return nil
}

// normalize checks a record set belongs in the zone and puts its name in
// canonical form.
func (z *HostedZone) normalize(rs RecordSet) (RecordSet, error) {
	name, err := normalizeName(rs.Name)
	if err != nil {
		return RecordSet{}, err
	}
	if name != z.Name && !strings.HasSuffix(name, "."+z.Name) {
		return RecordSet{}, invalidChangeBatch("RRSet with DNS name %s is not permitted in zone %s", display(name), z.Name)
	}
	rs.Name, rs.Type = name, strings.ToUpper(rs.Type)
	if rs.Type == "" {
		return RecordSet{}, invalidInput("Type is required")
	}
	if rs.Alias == nil && len(rs.Values) == 0 {
		return RecordSet{}, invalidChangeBatch("ResourceRecords or AliasTarget is required for %s", display(name))
	}
	if rs.Alias != nil {
		target, err := normalizeName(rs.Alias.DNSName)
		if err != nil {
			return RecordSet{}, err
		}
		alias := *rs.Alias
		alias.DNSName = target
		rs.Alias = &alias
	}
	return rs, nil
}

func (z *HostedZone) isApexDefault(rs RecordSet) bool {
	return rs.Name == z.Name && (rs.Type == "SOA" || rs.Type == "NS")
}

// hasUserRecords reports whether the zone holds records besides its apex SOA
// and NS, which DeleteHostedZone refuses.
func (z *HostedZone) hasUserRecords() bool {
	for _, rs := range z.RecordSets {
		if !z.isApexDefault(rs) {
			return true
		}
	}
	return false
}

func checkCNAMEConflicts(sets map[string]RecordSet) error {
	types := map[string]map[string]bool{}
	for _, rs := range sets {
		if types[rs.Name] == nil {
			types[rs.Name] = map[string]bool{}
		}
		types[rs.Name][rs.Type] = true
	}
	for name, seen := range types {
		if seen["CNAME"] && len(seen) > 1 {
			return invalidChangeBatch("RRSet of type CNAME with DNS name %s is not permitted as it conflicts with other records with the same DNS name", display(name))
		}
	}
	return nil
}

// sortRecordSets orders record sets the way ListResourceRecordSets does: by
// name with its labels reversed, then by type, then by set identifier.
func sortRecordSets(sets []RecordSet) {
	sort.Slice(sets, func(i, j int) bool { return lessRecord(sets[i].Name, sets[i].Type, sets[i].SetIdentifier, sets[j]) })
}

func lessRecord(name, rtype, setID string, other RecordSet) bool {
	a, b := reversedLabels(name), reversedLabels(other.Name)
	if a != b {
		return a < b
	}
	if rtype != other.Type {
		return rtype < other.Type
	}
	return setID < other.SetIdentifier
}

func reversedLabels(name string) string {
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i, j := 0, len(labels)-1; i < j; i, j = i+1, j-1 {
		labels[i], labels[j] = labels[j], labels[i]
	}
	return strings.Join(labels, ".")
}

// normalizeName lowercases a DNS name, adds the trailing dot and turns the
// octal escape Route 53 uses for a wildcard back into "*".
func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	name = strings.ReplaceAll(name, `\052`, "*")
	if name == "" || name == "." {
		return "", invalidInput("a DNS name is required")
	}
	name = strings.ToLower(name)
	if !strings.HasSuffix(name, ".") {
		name += "."
	}
	if len(name) > 255 || strings.Contains(name, "..") {
		return "", invalidInput("%q is not a valid DNS name", name)
	}
	return name, nil
}

// display renders a name the way Route 53 returns it, escaping a wildcard.
func display(name string) string {
	if strings.HasPrefix(name, "*.") || name == "*" {
		return `\052` + name[1:]
	}
	return name
}

func equalInt(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}
