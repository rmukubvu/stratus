package container

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/errdefs"
)

const selfID = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestPlacementFindsTheContainerID(t *testing.T) {
	mountinfo := "1203 1180 0:57 /var/lib/docker/containers/" + selfID + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n" +
		"1204 1180 0:57 /var/lib/docker/containers/" + selfID + "/hosts /etc/hosts rw - ext4 /dev/vda1 rw\n"
	p := placementFrom(mountinfo, "", "runner", never, envOf(nil))
	if !p.InContainer || !reflect.DeepEqual(p.ContainerIDs, []string{selfID}) {
		t.Fatalf("placement = %+v", p)
	}

	p = placementFrom("", "12:pids:/docker/"+selfID+"\n", "", never, envOf(nil))
	if !reflect.DeepEqual(p.ContainerIDs, []string{selfID}) {
		t.Fatalf("cgroup placement = %+v", p)
	}
}

func TestPlacementRecognisesContainersWithoutAnID(t *testing.T) {
	cases := map[string]Placement{
		"dockerenv": placementFrom("", "", "", func(p string) bool { return p == "/.dockerenv" }, envOf(nil)),
		"podman":    placementFrom("", "", "", func(p string) bool { return p == "/run/.containerenv" }, envOf(nil)),
		"kubernetes": placementFrom("", "", "", never,
			envOf(map[string]string{"KUBERNETES_SERVICE_HOST": "10.96.0.1"})),
	}
	for name, p := range cases {
		if !p.InContainer || len(p.ContainerIDs) != 0 {
			t.Errorf("%s: %+v", name, p)
		}
	}
	if p := placementFrom("", "0::/user.slice", "laptop", never, envOf(nil)); p.InContainer {
		t.Errorf("a host process counted as in a container: %+v", p)
	}
}

func never(string) bool { return false }

// fakeDaemon is an in-memory daemonAPI.
type fakeDaemon struct {
	containers   map[string]container.InspectResponse
	networks     map[string]bool
	unreachable  bool
	created      []string
	connected    []string
	removed      []string
	disconnected []string
}

func newFakeDaemon() *fakeDaemon {
	return &fakeDaemon{containers: map[string]container.InspectResponse{}, networks: map[string]bool{}}
}

func (f *fakeDaemon) addSelf(mode string, networks map[string]string) {
	eps := map[string]*network.EndpointSettings{}
	for name, ip := range networks {
		eps[name] = &network.EndpointSettings{IPAddress: ip}
	}
	f.containers[selfID] = container.InspectResponse{
		ContainerJSONBase: &container.ContainerJSONBase{ID: selfID, HostConfig: &container.HostConfig{NetworkMode: container.NetworkMode(mode)}},
		Config:            &container.Config{Hostname: selfID[:12]},
		NetworkSettings:   &container.NetworkSettings{Networks: eps},
	}
}

func (f *fakeDaemon) ContainerInspect(_ context.Context, id string) (container.InspectResponse, error) {
	if f.unreachable {
		return container.InspectResponse{}, errors.New("connection refused")
	}
	if c, ok := f.containers[id]; ok {
		return c, nil
	}
	return container.InspectResponse{}, errdefs.NotFound(errors.New("no such container"))
}

func (f *fakeDaemon) NetworkInspect(_ context.Context, id string, _ network.InspectOptions) (network.Inspect, error) {
	if f.networks[id] {
		return network.Inspect{Name: id}, nil
	}
	return network.Inspect{}, errdefs.NotFound(errors.New("no such network"))
}

func (f *fakeDaemon) NetworkCreate(_ context.Context, name string, _ network.CreateOptions) (network.CreateResponse, error) {
	f.networks[name] = true
	f.created = append(f.created, name)
	return network.CreateResponse{ID: name}, nil
}

func (f *fakeDaemon) NetworkConnect(_ context.Context, networkID, containerID string, _ *network.EndpointSettings) error {
	c := f.containers[containerID]
	c.NetworkSettings.Networks[networkID] = &network.EndpointSettings{IPAddress: "172.30.0.2"}
	f.connected = append(f.connected, networkID)
	return nil
}

func (f *fakeDaemon) NetworkDisconnect(_ context.Context, networkID, _ string, _ bool) error {
	f.disconnected = append(f.disconnected, networkID)
	return nil
}

func (f *fakeDaemon) NetworkRemove(_ context.Context, networkID string) error {
	delete(f.networks, networkID)
	f.removed = append(f.removed, networkID)
	return nil
}

func resolverFor(d *fakeDaemon, p Placement) resolver {
	return resolver{api: d, placement: p, instance: "abc123", localAddressFor: func(remote string) (string, error) {
		return "10.244.1.5", nil
	}}
}

var (
	onHost           = Placement{Hostname: "laptop"}
	inKnownContainer = Placement{InContainer: true, ContainerIDs: []string{selfID}, Hostname: selfID[:12]}
	inUnknownPod     = Placement{InContainer: true, Hostname: "runner-x7k2p"}
)

func TestResolveOnTheHostKeepsLoopbackAndBindMounts(t *testing.T) {
	res, err := resolverFor(newFakeDaemon(), onHost).Resolve(context.Background(), ReachSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach != LoopbackReach() || res.Files != DeliverBind || res.CallbackHost != hostDockerInternal {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveInAContainerTheDaemonDoesNotKnowCopiesFiles(t *testing.T) {
	// A Docker-in-Docker sidecar shares the pod's loopback but not its
	// filesystem.
	res, err := resolverFor(newFakeDaemon(), inUnknownPod).Resolve(context.Background(), ReachSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach != LoopbackReach() || res.Files != DeliverCopy {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveSiblingContainerUsesItsUserNetwork(t *testing.T) {
	d := newFakeDaemon()
	d.addSelf("github_network_1f2e", map[string]string{"github_network_1f2e": "172.18.0.2"})
	res, err := resolverFor(d, inKnownContainer).Resolve(context.Background(), ReachSettings{})
	if err != nil {
		t.Fatal(err)
	}
	want := Reach{Mode: ReachNetwork, Network: "github_network_1f2e"}
	if res.Reach != want || res.CallbackHost != "172.18.0.2" || res.Files != DeliverCopy {
		t.Fatalf("got %+v", res)
	}
	if len(d.created)+len(d.connected) != 0 {
		t.Fatalf("touched networks: created %v connected %v", d.created, d.connected)
	}
}

func TestResolveSiblingOnTheDefaultBridgeGetsItsOwnNetwork(t *testing.T) {
	d := newFakeDaemon()
	d.addSelf("default", map[string]string{"bridge": "172.17.0.3"})
	res, err := resolverFor(d, inKnownContainer).Resolve(context.Background(), ReachSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach != (Reach{Mode: ReachNetwork, Network: "stratus-abc123"}) || res.CallbackHost != "172.30.0.2" {
		t.Fatalf("got %+v", res)
	}
	if !reflect.DeepEqual(d.created, []string{"stratus-abc123"}) || !reflect.DeepEqual(d.connected, []string{"stratus-abc123"}) {
		t.Fatalf("created %v connected %v", d.created, d.connected)
	}
	if err := res.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.removed, []string{"stratus-abc123"}) || !reflect.DeepEqual(d.disconnected, []string{"stratus-abc123"}) {
		t.Fatalf("removed %v disconnected %v", d.removed, d.disconnected)
	}
}

func TestResolveHostNetworkContainerKeepsLoopback(t *testing.T) {
	d := newFakeDaemon()
	d.addSelf("host", map[string]string{"host": ""})
	res, err := resolverFor(d, inKnownContainer).Resolve(context.Background(), ReachSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach != LoopbackReach() || res.Files != DeliverCopy {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveFindsSelfByHostname(t *testing.T) {
	d := newFakeDaemon()
	d.addSelf("ci", map[string]string{"ci": "172.19.0.4"})
	d.containers[selfID[:12]] = d.containers[selfID]
	p := Placement{InContainer: true, Hostname: selfID[:12]}
	res, err := resolverFor(d, p).Resolve(context.Background(), ReachSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach.Mode != ReachNetwork || res.Reach.Network != "ci" {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveIgnoresAHostnameThatNamesAnotherContainer(t *testing.T) {
	d := newFakeDaemon()
	d.addSelf("ci", map[string]string{"ci": "172.19.0.4"})
	d.containers["runner"] = d.containers[selfID] // reports hostname selfID[:12]
	p := Placement{InContainer: true, Hostname: "runner"}
	res, err := resolverFor(d, p).Resolve(context.Background(), ReachSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach != LoopbackReach() {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveUnreachableDaemonFallsBackToLoopback(t *testing.T) {
	d := newFakeDaemon()
	d.unreachable = true
	res, err := resolverFor(d, inKnownContainer).Resolve(context.Background(), ReachSettings{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach != LoopbackReach() {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveExplicitHostForKubernetesPods(t *testing.T) {
	res, err := resolverFor(newFakeDaemon(), inUnknownPod).Resolve(context.Background(),
		ReachSettings{Mode: ReachHost, HostAddress: "10.0.4.17"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach != (Reach{Mode: ReachHost, HostAddress: "10.0.4.17"}) || res.CallbackHost != "10.244.1.5" || res.Files != DeliverCopy {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveExplicitNetworkJoinsIt(t *testing.T) {
	d := newFakeDaemon()
	d.networks["ci-net"] = true
	d.addSelf("default", map[string]string{"bridge": "172.17.0.3"})
	res, err := resolverFor(d, inKnownContainer).Resolve(context.Background(), ReachSettings{Mode: ReachNetwork, Network: "ci-net"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Reach.Network != "ci-net" || res.CallbackHost != "172.30.0.2" || !reflect.DeepEqual(d.connected, []string{"ci-net"}) {
		t.Fatalf("got %+v, connected %v", res, d.connected)
	}
	if err := res.Close(context.Background()); err != nil || len(d.removed) != 0 {
		t.Fatalf("an operator's network must not be removed: %v %v", err, d.removed)
	}
}

func TestResolveExplicitNetworkOutsideAKnownContainerUsesTheHostname(t *testing.T) {
	d := newFakeDaemon()
	d.networks["ci-net"] = true
	res, err := resolverFor(d, inUnknownPod).Resolve(context.Background(), ReachSettings{Mode: ReachNetwork, Network: "ci-net"})
	if err != nil {
		t.Fatal(err)
	}
	if res.CallbackHost != "runner-x7k2p" || len(d.connected) != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestResolveRejectsUnusableNetworkSettings(t *testing.T) {
	if _, err := resolverFor(newFakeDaemon(), inKnownContainer).Resolve(context.Background(),
		ReachSettings{Mode: ReachNetwork, Network: "missing"}); err == nil {
		t.Error("a missing network resolved")
	}
	if _, err := resolverFor(newFakeDaemon(), onHost).Resolve(context.Background(),
		ReachSettings{Mode: ReachNetwork}); err == nil {
		t.Error("network mode with nothing to join resolved")
	}
}

func TestResolveExplicitFilesWin(t *testing.T) {
	res, err := resolverFor(newFakeDaemon(), inUnknownPod).Resolve(context.Background(), ReachSettings{Files: DeliverBind})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != DeliverBind {
		t.Fatalf("got %+v", res)
	}
}
