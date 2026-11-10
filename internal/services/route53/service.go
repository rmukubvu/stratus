// Package route53 emulates Amazon Route 53 hosted zones and record sets, and
// resolves names against them so tests can check what a DNS lookup would
// return.
package route53

import (
	"crypto/rand"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/store"
)

const (
	zonesBucket   = "route53-zones"
	changesBucket = "route53-changes"

	changeInSync = "INSYNC"

	// idAlphabet is what Route 53 builds zone and change IDs from.
	idAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
)

// ChangeInfo describes a submitted change. Stratus applies changes at once,
// so every change is INSYNC.
type ChangeInfo struct {
	ID          string    `json:"id"`
	Status      string    `json:"status"`
	SubmittedAt time.Time `json:"submitted_at"`
	Comment     string    `json:"comment,omitempty"`
}

// Options wires the service to its adapters.
type Options struct {
	Metadata store.Store
	Now      func() time.Time
}

// Service is the Route 53 application service.
type Service struct {
	metadata store.Store
	now      func() time.Time
	mu       sync.Mutex
}

// NewService builds a Route 53 service.
func NewService(opts Options) *Service {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{metadata: opts.Metadata, now: now}
}

// CreateZone creates a hosted zone with its default SOA and NS records. A
// private zone needs a VPC. Reusing a caller reference fails, as on AWS.
func (s *Service) CreateZone(name, callerReference, comment string, private bool, vpc *VPC) (HostedZone, ChangeInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createZone(name, callerReference, comment, private, vpc, "")
}

func (s *Service) createZone(name, callerReference, comment string, private bool, vpc *VPC, managedBy string) (HostedZone, ChangeInfo, error) {
	zones, err := s.zones()
	if err != nil {
		return HostedZone{}, ChangeInfo{}, err
	}
	for _, z := range zones {
		if z.CallerReference == callerReference && callerReference != "" {
			return HostedZone{}, ChangeInfo{}, hostedZoneAlreadyExists(callerReference)
		}
	}
	zone, err := newHostedZone("Z"+randomID(20), name, callerReference, s.now())
	if err != nil {
		return HostedZone{}, ChangeInfo{}, err
	}
	zone.Comment, zone.Private, zone.ManagedBy = comment, private || vpc != nil, managedBy
	if vpc != nil {
		zone.VPCs = []VPC{*vpc}
	}
	if err := s.put(zone); err != nil {
		return HostedZone{}, ChangeInfo{}, err
	}
	change, err := s.recordChange("")
	return zone, change, err
}

// Zone finds a hosted zone by ID, with or without the /hostedzone/ prefix.
func (s *Service) Zone(id string) (HostedZone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.load(id)
}

// Zones lists hosted zones sorted by name, then ID. With dnsName set, only
// zones whose name sorts at or after it are returned, as
// ListHostedZonesByName does.
func (s *Service) Zones(dnsName string) ([]HostedZone, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	zones, err := s.zones()
	if err != nil || dnsName == "" {
		return zones, err
	}
	start, err := normalizeName(dnsName)
	if err != nil {
		return nil, err
	}
	out := zones[:0]
	for _, z := range zones {
		if reversedLabels(z.Name) >= reversedLabels(start) {
			out = append(out, z)
		}
	}
	return out, nil
}

// DeleteZone deletes a zone that holds only its SOA and NS records.
func (s *Service) DeleteZone(id string) (ChangeInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	zone, err := s.load(id)
	if err != nil {
		return ChangeInfo{}, err
	}
	if zone.hasUserRecords() {
		return ChangeInfo{}, hostedZoneNotEmpty(zone.ID)
	}
	if err := s.metadata.Delete(zonesBucket, zone.ID); err != nil {
		return ChangeInfo{}, internal(err)
	}
	return s.recordChange("")
}

// ChangeRecordSets applies a change batch atomically.
func (s *Service) ChangeRecordSets(zoneID, comment string, changes []Change) (ChangeInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	zone, err := s.load(zoneID)
	if err != nil {
		return ChangeInfo{}, err
	}
	if err := zone.apply(changes); err != nil {
		return ChangeInfo{}, err
	}
	if err := s.put(zone); err != nil {
		return ChangeInfo{}, err
	}
	return s.recordChange(comment)
}

// Change finds a submitted change by ID, with or without the /change/ prefix.
func (s *Service) Change(id string) (ChangeInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id = strings.TrimPrefix(id, "/change/")
	raw, err := s.metadata.Get(changesBucket, id)
	if err != nil {
		return ChangeInfo{}, internal(err)
	}
	var change ChangeInfo
	if raw == nil || json.Unmarshal(raw, &change) != nil {
		return ChangeInfo{}, noSuchChange(id)
	}
	return change, nil
}

// SetTags adds or replaces tags and removes the keys in remove.
func (s *Service) SetTags(zoneID string, add map[string]string, remove []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	zone, err := s.load(zoneID)
	if err != nil {
		return err
	}
	if zone.Tags == nil {
		zone.Tags = map[string]string{}
	}
	for k, v := range add {
		zone.Tags[k] = v
	}
	for _, k := range remove {
		delete(zone.Tags, k)
	}
	return s.put(zone)
}

// Resolve answers a DNS query against every hosted zone.
func (s *Service) Resolve(name, qtype string) (Resolution, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	zones, err := s.zones()
	if err != nil {
		return Resolution{}, err
	}
	return resolve(zones, name, qtype)
}

func (s *Service) recordChange(comment string) (ChangeInfo, error) {
	change := ChangeInfo{ID: "C" + randomID(20), Status: changeInSync, SubmittedAt: s.now().UTC(), Comment: comment}
	raw, err := json.Marshal(change)
	if err != nil {
		return ChangeInfo{}, internal(err)
	}
	if err := s.metadata.Put(changesBucket, change.ID, raw); err != nil {
		return ChangeInfo{}, internal(err)
	}
	return change, nil
}

func (s *Service) load(id string) (HostedZone, error) {
	id = strings.TrimPrefix(id, "/hostedzone/")
	raw, err := s.metadata.Get(zonesBucket, id)
	if err != nil {
		return HostedZone{}, internal(err)
	}
	var zone HostedZone
	if raw == nil || json.Unmarshal(raw, &zone) != nil {
		return HostedZone{}, noSuchHostedZone(id)
	}
	return zone, nil
}

func (s *Service) zones() ([]HostedZone, error) {
	out := make([]HostedZone, 0)
	err := s.metadata.Scan(zonesBucket, "", func(_, v []byte) error {
		var zone HostedZone
		if json.Unmarshal(v, &zone) == nil {
			out = append(out, zone)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := reversedLabels(out[i].Name), reversedLabels(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (s *Service) put(zone HostedZone) error {
	raw, err := json.Marshal(zone)
	if err != nil {
		return internal(err)
	}
	if err := s.metadata.Put(zonesBucket, zone.ID, raw); err != nil {
		return internal(err)
	}
	return nil
}

func randomID(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = idAlphabet[int(b[i])%len(idAlphabet)]
	}
	return string(b)
}
