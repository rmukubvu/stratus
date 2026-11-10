package route53

import (
	"strings"
)

// maxHops bounds how many CNAME and alias steps a lookup follows, so a loop
// between records ends instead of spinning.
const maxHops = 8

// Answer is one record a lookup returned.
type Answer struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Value  string `json:"value"`
	TTL    int64  `json:"ttl"`
	ZoneID string `json:"zone_id"`
}

// Resolution is what a DNS lookup against the emulated zones returns.
type Resolution struct {
	Name string `json:"name"`
	Type string `json:"type"`
	// Answers hold CNAME steps followed by the records of the requested
	// type, in the order a resolver would see them.
	Answers []Answer `json:"answers"`
	// Found is false when no zone holds a record for the name (NXDOMAIN) or
	// the name has no records of the requested type (NODATA).
	Found bool `json:"found"`
}

// resolve answers a query from zones the way Route 53 would: it picks the
// most specific zone for the name, matches exact names before wildcards,
// returns every value of every record set of the type (weighted and
// multivalue sets alike), and follows CNAME records and alias targets
// between zones. Health checks and routing policies are not evaluated.
func resolve(zones []HostedZone, name, qtype string) (Resolution, error) {
	fqdn, err := normalizeName(name)
	if err != nil {
		return Resolution{}, err
	}
	qtype = strings.ToUpper(strings.TrimSpace(qtype))
	if qtype == "" {
		qtype = "A"
	}
	res := Resolution{Name: fqdn, Type: qtype, Answers: []Answer{}}

	current := fqdn
	for hop := 0; hop < maxHops; hop++ {
		zone, ok := zoneFor(zones, current)
		if !ok {
			return res, nil
		}
		sets := matching(zone, current)
		if wanted := ofType(sets, qtype); len(wanted) > 0 {
			if alias := wanted[0].Alias; alias != nil {
				current = alias.DNSName
				continue
			}
			for _, rs := range wanted {
				for _, v := range rs.Values {
					res.Answers = append(res.Answers, Answer{Name: current, Type: qtype, Value: v, TTL: ttl(rs), ZoneID: zone.ID})
				}
			}
			res.Found = true
			return res, nil
		}
		cname := ofType(sets, "CNAME")
		if len(cname) == 0 || len(cname[0].Values) == 0 {
			return res, nil
		}
		target := cname[0].Values[0]
		res.Answers = append(res.Answers, Answer{Name: current, Type: "CNAME", Value: target, TTL: ttl(cname[0]), ZoneID: zone.ID})
		if current, err = normalizeName(target); err != nil {
			return res, nil
		}
	}
	return res, nil
}

// zoneFor picks the zone whose name is the longest suffix of name. When a
// private and a public zone share a name, the private one wins, as it does
// for a client inside the VPC.
func zoneFor(zones []HostedZone, name string) (HostedZone, bool) {
	var best HostedZone
	found := false
	for _, z := range zones {
		if name != z.Name && !strings.HasSuffix(name, "."+z.Name) {
			continue
		}
		if !found || len(z.Name) > len(best.Name) || (len(z.Name) == len(best.Name) && z.Private && !best.Private) {
			best, found = z, true
		}
	}
	return best, found
}

// matching returns the record sets for name, falling back to the closest
// wildcard ("*.example.com.") when no record has the exact name.
func matching(zone HostedZone, name string) []RecordSet {
	if exact := withName(zone, name); len(exact) > 0 {
		return exact
	}
	labels := strings.Split(strings.TrimSuffix(name, "."), ".")
	for i := 1; i < len(labels); i++ {
		wildcard := "*." + strings.Join(labels[i:], ".") + "."
		if len(wildcard) < len(zone.Name) {
			break
		}
		if sets := withName(zone, wildcard); len(sets) > 0 {
			return sets
		}
	}
	return nil
}

func withName(zone HostedZone, name string) []RecordSet {
	var out []RecordSet
	for _, rs := range zone.RecordSets {
		if rs.Name == name {
			out = append(out, rs)
		}
	}
	return out
}

func ofType(sets []RecordSet, rtype string) []RecordSet {
	var out []RecordSet
	for _, rs := range sets {
		if rs.Type == rtype {
			out = append(out, rs)
		}
	}
	return out
}

func ttl(rs RecordSet) int64 {
	if rs.TTL == nil {
		return 60 // alias records carry the target's TTL; 60s is Route 53's usual
	}
	return *rs.TTL
}
