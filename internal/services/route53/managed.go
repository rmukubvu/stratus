package route53

// Methods in this file let another service own zones and records, the way
// AWS Cloud Map creates a hosted zone for a DNS namespace and records for
// each registered instance. They take plain values so that callers depend on
// a port of their own, not on this package.

const managedByCloudMap = "servicediscovery.amazonaws.com"

// CreateManagedZone creates a zone on behalf of Cloud Map and returns its ID.
// vpcID is required for a private zone.
func (s *Service) CreateManagedZone(name string, private bool, vpcID, comment string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var vpc *VPC
	if private {
		vpc = &VPC{Region: "us-east-1", ID: vpcID}
	}
	zone, _, err := s.createZone(name, "cloudmap-"+randomID(16), comment, private, vpc, managedByCloudMap)
	return zone.ID, err
}

// DeleteManagedZone removes a zone and whatever records it still holds.
// Deleting a zone that no longer exists is not an error.
func (s *Service) DeleteManagedZone(zoneID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.load(zoneID); err != nil {
		return nil
	}
	if err := s.metadata.Delete(zonesBucket, zoneID); err != nil {
		return internal(err)
	}
	return nil
}

// SetRecord creates or replaces one record set. A non-empty setID with
// multiValue makes it a multivalue answer record; with !multiValue it is a
// weighted record of weight 1, as Cloud Map's WEIGHTED routing creates.
func (s *Service) SetRecord(zoneID, name, rtype string, ttl int64, setID string, multiValue bool, values []string) error {
	rs := RecordSet{Name: name, Type: rtype, SetIdentifier: setID, TTL: &ttl, Values: values}
	if setID != "" {
		if multiValue {
			rs.MultiValueAnswer = &multiValue
		} else {
			weight := int64(1)
			rs.Weight = &weight
		}
	}
	_, err := s.ChangeRecordSets(zoneID, "", []Change{{Action: ActionUpsert, RecordSet: rs}})
	return err
}

// DeleteRecord removes one record set if it exists.
func (s *Service) DeleteRecord(zoneID, name, rtype, setID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	zone, err := s.load(zoneID)
	if err != nil {
		return nil
	}
	fqdn, err := normalizeName(name)
	if err != nil {
		return err
	}
	key := (RecordSet{Name: fqdn, Type: rtype, SetIdentifier: setID}).key()
	kept := zone.RecordSets[:0]
	for _, rs := range zone.RecordSets {
		if rs.key() != key {
			kept = append(kept, rs)
		}
	}
	zone.RecordSets = kept
	return s.put(zone)
}
