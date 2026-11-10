// Package servicediscovery emulates AWS Cloud Map. HTTP namespaces answer
// DiscoverInstances; DNS namespaces also own a Route 53 hosted zone in which
// every registered instance gets its records, so the names Cloud Map
// publishes resolve the way they would on AWS.
package servicediscovery

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/stratus/internal/store"
)

const (
	namespacesBucket = "servicediscovery-namespaces"
	servicesBucket   = "servicediscovery-services"
	instancesBucket  = "servicediscovery-instances"
	operationsBucket = "servicediscovery-operations"
)

// Zones is the DNS side of Cloud Map: the hosted zones behind DNS namespaces
// and the records behind instances. The Route 53 service implements it.
type Zones interface {
	CreateManagedZone(name string, private bool, vpcID, comment string) (zoneID string, err error)
	DeleteManagedZone(zoneID string) error
	SetRecord(zoneID, name, rtype string, ttl int64, setID string, multiValue bool, values []string) error
	DeleteRecord(zoneID, name, rtype, setID string) error
}

// Options wires the service to its adapters.
type Options struct {
	Metadata store.Store
	Zones    Zones
	Now      func() time.Time
}

// Directory is the Cloud Map application service.
type Directory struct {
	metadata store.Store
	zones    Zones
	now      func() time.Time
	mu       sync.Mutex
}

// NewDirectory builds a Cloud Map service.
func NewDirectory(opts Options) *Directory {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Directory{metadata: opts.Metadata, zones: opts.Zones, now: now}
}

// NamespaceInput is what the CreateXxxNamespace operations accept.
type NamespaceInput struct {
	Type             string
	Name             string
	Description      string
	CreatorRequestID string
	VPC              string
	Tags             []Tag
}

// CreateNamespace creates a namespace; a DNS namespace gets its hosted zone.
func (d *Directory) CreateNamespace(in NamespaceInput) (Operation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	name := strings.TrimSuffix(strings.TrimSpace(in.Name), ".")
	if name == "" {
		return Operation{}, invalidInput("Name is required")
	}
	if in.Type == typeDNSPrivate && in.VPC == "" {
		return Operation{}, invalidInput("Vpc is required for a private DNS namespace")
	}
	existing, err := scan[Namespace](d.metadata, namespacesBucket)
	if err != nil {
		return Operation{}, err
	}
	for _, ns := range existing {
		if strings.EqualFold(ns.Name, name) && ns.Type == in.Type {
			return Operation{}, clientError("NamespaceAlreadyExists", "namespace %s already exists", name)
		}
	}
	ns := Namespace{
		ID: "ns-" + randomHex(8), Name: name, Type: in.Type, Description: in.Description,
		CreatorRequestID: in.CreatorRequestID, VPC: in.VPC, Created: d.now().UTC(), Tags: in.Tags,
	}
	if ns.isDNS() {
		if d.zones == nil {
			return Operation{}, invalidInput("DNS namespaces need Route 53, which is not available")
		}
		zoneID, err := d.zones.CreateManagedZone(name, ns.Type == typeDNSPrivate, in.VPC, "Created by AWS Cloud Map namespace with ARN "+ns.ARN())
		if err != nil {
			return Operation{}, err
		}
		ns.HostedZoneID = zoneID
	}
	if err := put(d.metadata, namespacesBucket, ns.ID, ns); err != nil {
		return Operation{}, err
	}
	return d.operation("CREATE_NAMESPACE", map[string]string{"NAMESPACE": ns.ID})
}

// Namespace finds a namespace by ID.
func (d *Directory) Namespace(id string) (Namespace, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.namespace(id)
}

// Namespaces lists namespaces sorted by name.
func (d *Directory) Namespaces() ([]Namespace, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out, err := scan[Namespace](d.metadata, namespacesBucket)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, err
}

// DeleteNamespace deletes a namespace that holds no services, with its zone.
func (d *Directory) DeleteNamespace(id string) (Operation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	ns, err := d.namespace(id)
	if err != nil {
		return Operation{}, err
	}
	if count, err := d.serviceCount(ns.ID); err != nil {
		return Operation{}, err
	} else if count > 0 {
		return Operation{}, clientError("ResourceInUse", "namespace %s has %d services; delete them first", ns.ID, count)
	}
	if ns.HostedZoneID != "" && d.zones != nil {
		if err := d.zones.DeleteManagedZone(ns.HostedZoneID); err != nil {
			return Operation{}, err
		}
	}
	if err := d.metadata.Delete(namespacesBucket, ns.ID); err != nil {
		return Operation{}, internal(err)
	}
	return d.operation("DELETE_NAMESPACE", map[string]string{"NAMESPACE": ns.ID})
}

// ServiceInput is what CreateService accepts.
type ServiceInput struct {
	Name              string
	NamespaceID       string
	Type              string
	Description       string
	CreatorRequestID  string
	DNSConfig         *DNSConfig
	HealthCheckConfig json.RawMessage
	CustomHealth      json.RawMessage
	Tags              []Tag
}

// CreateService creates a service in a namespace.
func (d *Directory) CreateService(in ServiceInput) (Service, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if in.Name == "" {
		return Service{}, invalidInput("Name is required")
	}
	nsID := in.NamespaceID
	if nsID == "" && in.DNSConfig != nil {
		nsID = in.DNSConfig.NamespaceID
	}
	ns, err := d.namespace(nsID)
	if err != nil {
		return Service{}, err
	}
	svc := Service{
		ID: "srv-" + randomHex(8), Name: in.Name, NamespaceID: ns.ID, Description: in.Description,
		CreatorRequestID: in.CreatorRequestID, HealthCheckConfig: in.HealthCheckConfig, CustomHealth: in.CustomHealth,
		Created: d.now().UTC(), Tags: in.Tags,
	}
	switch {
	case in.DNSConfig != nil && !ns.isDNS():
		return Service{}, invalidInput("DnsConfig is not allowed in HTTP namespace %s", ns.ID)
	case in.DNSConfig != nil && in.Type != serviceHTTP:
		cfg := *in.DNSConfig
		cfg.NamespaceID = ""
		if cfg.RoutingPolicy == "" {
			cfg.RoutingPolicy = routingMultivalue
		}
		if len(cfg.DNSRecords) == 0 {
			return Service{}, invalidInput("DnsConfig needs at least one DnsRecord")
		}
		svc.DNSConfig, svc.Type = &cfg, serviceDNSHTTP
	default:
		svc.Type = serviceHTTP
	}
	services, err := scan[Service](d.metadata, servicesBucket)
	if err != nil {
		return Service{}, err
	}
	for _, other := range services {
		if other.NamespaceID == ns.ID && strings.EqualFold(other.Name, svc.Name) {
			return Service{}, clientError("ServiceAlreadyExists", "service %s already exists in namespace %s", svc.Name, ns.ID)
		}
	}
	return svc, put(d.metadata, servicesBucket, svc.ID, svc)
}

// Service finds a service by ID.
func (d *Directory) Service(id string) (Service, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.service(id)
}

// Services lists services sorted by name, only those in namespaceID when it
// is set.
func (d *Directory) Services(namespaceID string) ([]Service, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	all, err := scan[Service](d.metadata, servicesBucket)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, svc := range all {
		if namespaceID == "" || svc.NamespaceID == namespaceID {
			out = append(out, svc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// DeleteService deletes a service that has no registered instances.
func (d *Directory) DeleteService(id string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	svc, err := d.service(id)
	if err != nil {
		return err
	}
	instances, err := d.instances(svc.ID)
	if err != nil {
		return err
	}
	if len(instances) > 0 {
		return clientError("ResourceInUse", "service %s has %d registered instances; deregister them first", svc.ID, len(instances))
	}
	if err := d.metadata.Delete(servicesBucket, svc.ID); err != nil {
		return internal(err)
	}
	return nil
}

// RegisterInstance adds or replaces an instance and publishes its records.
func (d *Directory) RegisterInstance(serviceID, instanceID, creatorRequestID string, attributes map[string]string) (Operation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if instanceID == "" {
		return Operation{}, invalidInput("InstanceId is required")
	}
	svc, err := d.service(serviceID)
	if err != nil {
		return Operation{}, err
	}
	ns, err := d.namespace(svc.NamespaceID)
	if err != nil {
		return Operation{}, err
	}
	inst := Instance{ID: instanceID, ServiceID: svc.ID, CreatorRequestID: creatorRequestID, Attributes: attributes, Health: healthy}
	if previous, err := d.instance(svc.ID, instanceID); err == nil {
		inst.Health = previous.Health
		if err := d.unpublish(svc, ns, previous); err != nil {
			return Operation{}, err
		}
	}
	if inst.Attributes == nil {
		inst.Attributes = map[string]string{}
	}
	if inst.Health == healthy {
		if err := d.publish(svc, ns, inst); err != nil {
			return Operation{}, err
		}
	}
	if err := put(d.metadata, instancesBucket, instanceKey(svc.ID, inst.ID), inst); err != nil {
		return Operation{}, err
	}
	return d.operation("REGISTER_INSTANCE", map[string]string{"SERVICE": svc.ID, "INSTANCE": inst.ID})
}

// DeregisterInstance removes an instance and its records.
func (d *Directory) DeregisterInstance(serviceID, instanceID string) (Operation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	svc, err := d.service(serviceID)
	if err != nil {
		return Operation{}, err
	}
	inst, err := d.instance(svc.ID, instanceID)
	if err != nil {
		return Operation{}, err
	}
	if ns, err := d.namespace(svc.NamespaceID); err == nil {
		if err := d.unpublish(svc, ns, inst); err != nil {
			return Operation{}, err
		}
	}
	if err := d.metadata.Delete(instancesBucket, instanceKey(svc.ID, inst.ID)); err != nil {
		return Operation{}, internal(err)
	}
	return d.operation("DEREGISTER_INSTANCE", map[string]string{"SERVICE": svc.ID, "INSTANCE": inst.ID})
}

// Instance finds one registered instance.
func (d *Directory) Instance(serviceID, instanceID string) (Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.service(serviceID); err != nil {
		return Instance{}, err
	}
	return d.instance(serviceID, instanceID)
}

// Instances lists a service's instances sorted by ID.
func (d *Directory) Instances(serviceID string) ([]Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, err := d.service(serviceID); err != nil {
		return nil, err
	}
	return d.instances(serviceID)
}

// SetCustomHealth records the health the application reports for an
// instance. An unhealthy instance drops out of DNS answers and out of
// DiscoverInstances calls that ask for healthy instances.
func (d *Directory) SetCustomHealth(serviceID, instanceID, status string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if status != healthy && status != unhealthy {
		return invalidInput("Status must be HEALTHY or UNHEALTHY")
	}
	svc, err := d.service(serviceID)
	if err != nil {
		return err
	}
	if len(svc.CustomHealth) == 0 {
		return clientError("CustomHealthNotFound", "service %s has no HealthCheckCustomConfig", svc.ID)
	}
	inst, err := d.instance(svc.ID, instanceID)
	if err != nil {
		return err
	}
	if inst.Health == status {
		return nil
	}
	ns, err := d.namespace(svc.NamespaceID)
	if err != nil {
		return err
	}
	if status == healthy {
		err = d.publish(svc, ns, inst)
	} else {
		err = d.unpublish(svc, ns, inst)
	}
	if err != nil {
		return err
	}
	inst.Health = status
	return put(d.metadata, instancesBucket, instanceKey(svc.ID, inst.ID), inst)
}

// DiscoverQuery is what DiscoverInstances accepts.
type DiscoverQuery struct {
	NamespaceName      string
	ServiceName        string
	QueryParameters    map[string]string
	OptionalParameters map[string]string
	HealthStatus       string
	MaxResults         int
}

// Discover finds instances by namespace and service name. Unless the query
// asks for a health status, every instance is returned.
func (d *Directory) Discover(q DiscoverQuery) ([]Instance, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	namespaces, err := scan[Namespace](d.metadata, namespacesBucket)
	if err != nil {
		return nil, err
	}
	var ns *Namespace
	for i := range namespaces {
		if strings.EqualFold(namespaces[i].Name, strings.TrimSuffix(q.NamespaceName, ".")) {
			ns = &namespaces[i]
		}
	}
	if ns == nil {
		return nil, clientError("NamespaceNotFound", "namespace %s does not exist", q.NamespaceName)
	}
	services, err := scan[Service](d.metadata, servicesBucket)
	if err != nil {
		return nil, err
	}
	var svc *Service
	for i := range services {
		if services[i].NamespaceID == ns.ID && strings.EqualFold(services[i].Name, q.ServiceName) {
			svc = &services[i]
		}
	}
	if svc == nil {
		return nil, clientError("ServiceNotFound", "service %s does not exist in namespace %s", q.ServiceName, q.NamespaceName)
	}
	instances, err := d.instances(svc.ID)
	if err != nil {
		return nil, err
	}
	out := filterInstances(instances, q)
	if q.MaxResults > 0 && len(out) > q.MaxResults {
		out = out[:q.MaxResults]
	}
	return out, nil
}

func filterInstances(instances []Instance, q DiscoverQuery) []Instance {
	var matched []Instance
	for _, inst := range instances {
		if hasAttributes(inst, q.QueryParameters) {
			matched = append(matched, inst)
		}
	}
	if len(q.OptionalParameters) > 0 {
		var preferred []Instance
		for _, inst := range matched {
			if hasAttributes(inst, q.OptionalParameters) {
				preferred = append(preferred, inst)
			}
		}
		if len(preferred) > 0 {
			matched = preferred
		}
	}
	byHealth := func(status string) []Instance {
		var out []Instance
		for _, inst := range matched {
			if inst.Health == status {
				out = append(out, inst)
			}
		}
		return out
	}
	switch q.HealthStatus {
	case healthy, unhealthy:
		return byHealth(q.HealthStatus)
	case "HEALTHY_OR_ELSE_ALL":
		if h := byHealth(healthy); len(h) > 0 {
			return h
		}
	}
	return matched
}

func hasAttributes(inst Instance, want map[string]string) bool {
	for k, v := range want {
		if inst.Attributes[k] != v {
			return false
		}
	}
	return true
}

// GetOperation finds an operation by ID.
func (d *Directory) GetOperation(id string) (Operation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	op, err := get[Operation](d.metadata, operationsBucket, id)
	if errors.Is(err, errNotFound) {
		return Operation{}, clientError("OperationNotFound", "operation %s does not exist", id)
	}
	return op, err
}

// Operations lists operations, oldest first.
func (d *Directory) Operations() ([]Operation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	out, err := scan[Operation](d.metadata, operationsBucket)
	sort.Slice(out, func(i, j int) bool {
		return out[i].Created.Before(out[j].Created) || (out[i].Created.Equal(out[j].Created) && out[i].ID < out[j].ID)
	})
	return out, err
}

// Tags returns the tags of the namespace or service with the ARN.
func (d *Directory) Tags(arn string) ([]Tag, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var tags []Tag
	err := d.withTagged(arn, func(t *[]Tag) bool { tags = append(tags, *t...); return false })
	return tags, err
}

// SetTags adds or replaces tags and removes the keys in remove.
func (d *Directory) SetTags(arn string, add []Tag, remove []string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.withTagged(arn, func(t *[]Tag) bool {
		drop := map[string]bool{}
		for _, k := range remove {
			drop[k] = true
		}
		for _, tag := range add {
			drop[tag.Key] = true
		}
		kept := (*t)[:0]
		for _, tag := range *t {
			if !drop[tag.Key] {
				kept = append(kept, tag)
			}
		}
		*t = append(kept, add...)
		return true
	})
}

// withTagged finds the resource with arn, lets change edit its tags, and
// saves it when change returns true.
func (d *Directory) withTagged(arn string, change func(*[]Tag) bool) error {
	switch {
	case strings.Contains(arn, ":namespace/"):
		ns, err := d.namespace(arn[strings.LastIndex(arn, "/")+1:])
		if err != nil {
			return clientError("ResourceNotFoundException", "resource %s does not exist", arn)
		}
		if change(&ns.Tags) {
			return put(d.metadata, namespacesBucket, ns.ID, ns)
		}
		return nil
	case strings.Contains(arn, ":service/"):
		svc, err := d.service(arn[strings.LastIndex(arn, "/")+1:])
		if err != nil {
			return clientError("ResourceNotFoundException", "resource %s does not exist", arn)
		}
		if change(&svc.Tags) {
			return put(d.metadata, servicesBucket, svc.ID, svc)
		}
		return nil
	default:
		return clientError("ResourceNotFoundException", "resource %s does not exist", arn)
	}
}

// ServiceCount reports how many services a namespace holds.
func (d *Directory) ServiceCount(namespaceID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, _ := d.serviceCount(namespaceID)
	return n
}

// InstanceCount reports how many instances a service has.
func (d *Directory) InstanceCount(serviceID string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	instances, _ := d.instances(serviceID)
	return len(instances)
}

func (d *Directory) publish(svc Service, ns Namespace, inst Instance) error {
	records, err := svc.records(ns, inst)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if err := d.zones.SetRecord(ns.HostedZoneID, rec.Name, rec.Type, rec.TTL, rec.SetIdentifier, rec.MultiValue, []string{rec.Value}); err != nil {
			return err
		}
	}
	return nil
}

func (d *Directory) unpublish(svc Service, ns Namespace, inst Instance) error {
	records, err := svc.records(ns, inst)
	if err != nil {
		// The instance was registered, so its records were valid then.
		return nil
	}
	for _, rec := range records {
		if err := d.zones.DeleteRecord(ns.HostedZoneID, rec.Name, rec.Type, rec.SetIdentifier); err != nil {
			return err
		}
	}
	return nil
}

func (d *Directory) operation(kind string, targets map[string]string) (Operation, error) {
	op := Operation{ID: randomHex(16), Type: kind, Created: d.now().UTC(), Targets: targets}
	return op, put(d.metadata, operationsBucket, op.ID, op)
}

func (d *Directory) namespace(id string) (Namespace, error) {
	ns, err := get[Namespace](d.metadata, namespacesBucket, id)
	if errors.Is(err, errNotFound) {
		return Namespace{}, clientError("NamespaceNotFound", "namespace %s does not exist", id)
	}
	return ns, err
}

func (d *Directory) service(id string) (Service, error) {
	svc, err := get[Service](d.metadata, servicesBucket, id)
	if errors.Is(err, errNotFound) {
		return Service{}, clientError("ServiceNotFound", "service %s does not exist", id)
	}
	return svc, err
}

func (d *Directory) instance(serviceID, instanceID string) (Instance, error) {
	inst, err := get[Instance](d.metadata, instancesBucket, instanceKey(serviceID, instanceID))
	if errors.Is(err, errNotFound) {
		return Instance{}, clientError("InstanceNotFound", "instance %s is not registered with service %s", instanceID, serviceID)
	}
	return inst, err
}

func (d *Directory) instances(serviceID string) ([]Instance, error) {
	var out []Instance
	err := d.metadata.Scan(instancesBucket, serviceID+"/", func(_, v []byte) error {
		var inst Instance
		if json.Unmarshal(v, &inst) == nil {
			out = append(out, inst)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (d *Directory) serviceCount(namespaceID string) (int, error) {
	services, err := scan[Service](d.metadata, servicesBucket)
	n := 0
	for _, svc := range services {
		if svc.NamespaceID == namespaceID {
			n++
		}
	}
	return n, err
}

func instanceKey(serviceID, instanceID string) string { return serviceID + "/" + instanceID }

var errNotFound = errors.New("not found")

func get[T any](m store.Store, bucket, key string) (T, error) {
	var out T
	raw, err := m.Get(bucket, key)
	if err != nil {
		return out, internal(err)
	}
	if raw == nil || json.Unmarshal(raw, &out) != nil {
		return out, errNotFound
	}
	return out, nil
}

func scan[T any](m store.Store, bucket string) ([]T, error) {
	out := make([]T, 0)
	err := m.Scan(bucket, "", func(_, v []byte) error {
		var item T
		if json.Unmarshal(v, &item) == nil {
			out = append(out, item)
		}
		return nil
	})
	if err != nil {
		return nil, internal(err)
	}
	return out, nil
}

func put(m store.Store, bucket, key string, value any) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return internal(err)
	}
	if err := m.Put(bucket, key, raw); err != nil {
		return internal(err)
	}
	return nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
