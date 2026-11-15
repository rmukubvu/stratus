package container

import (
	"strings"
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"

	"github.com/stratus/internal/backing"
)

func envOf(vars map[string]string) func(string) string {
	return func(k string) string { return vars[k] }
}

func TestParseReachSettings(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want ReachSettings
	}{
		{"nothing set detects", nil, ReachSettings{}},
		{"auto detects", map[string]string{EnvReach: "AUTO", EnvFiles: "auto"}, ReachSettings{}},
		{"explicit loopback", map[string]string{EnvReach: "loopback"}, ReachSettings{Mode: ReachLoopback}},
		{"network name implies network", map[string]string{EnvNetwork: " ci-net "}, ReachSettings{Mode: ReachNetwork, Network: "ci-net"}},
		{"host address implies host", map[string]string{EnvHostAddress: "10.0.0.7"}, ReachSettings{Mode: ReachHost, HostAddress: "10.0.0.7"}},
		{"network without name", map[string]string{EnvReach: "network"}, ReachSettings{Mode: ReachNetwork}},
		{"files copy", map[string]string{EnvFiles: "Copy"}, ReachSettings{Files: DeliverCopy}},
		{"files bind", map[string]string{EnvFiles: "bind"}, ReachSettings{Files: DeliverBind}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseReachSettings(envOf(tc.env))
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestParseReachSettingsRejectsContradictions(t *testing.T) {
	for name, env := range map[string]map[string]string{
		"unknown mode":            {EnvReach: "bridge"},
		"unknown files":           {EnvFiles: "rsync"},
		"network and host":        {EnvNetwork: "n", EnvHostAddress: "10.0.0.1"},
		"host without an address": {EnvReach: "host"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseReachSettings(envOf(env)); err == nil {
				t.Fatal("want an error")
			}
		})
	}
}

func TestReachPublishesOnTheRightInterface(t *testing.T) {
	port := nat.Port("5432/tcp")
	if got := LoopbackReach().PortBindings(port, "")[port][0].HostIP; got != "127.0.0.1" {
		t.Errorf("loopback publishes on %q", got)
	}
	host := Reach{Mode: ReachHost, HostAddress: "10.0.0.7"}
	if got := host.PortBindings(port, "6000")[port][0]; got.HostIP != "0.0.0.0" || got.HostPort != "6000" {
		t.Errorf("host publishes %+v", got)
	}
	shared := Reach{Mode: ReachNetwork, Network: "ci"}
	if shared.PortBindings(port, "") != nil || shared.Publishes() {
		t.Error("a shared network must not publish host ports")
	}
}

func TestReachEndpoint(t *testing.T) {
	cases := []struct {
		reach Reach
		want  backing.Endpoint
	}{
		{LoopbackReach(), backing.Endpoint{Address: "127.0.0.1", Port: 49153}},
		{Reach{Mode: ReachHost, HostAddress: "10.0.0.7"}, backing.Endpoint{Address: "10.0.0.7", Port: 49153}},
		{Reach{Mode: ReachNetwork, Network: "ci"}, backing.Endpoint{Address: "stratus-abc-rds-db", Port: 5432}},
	}
	for _, tc := range cases {
		if got := tc.reach.Endpoint("stratus-abc-rds-db", 5432, 49153); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.reach, got, tc.want)
		}
	}
}

func TestReachNetworkingConfigJoinsOnlyTheSharedNetwork(t *testing.T) {
	if cfg := LoopbackReach().NetworkingConfig(); len(cfg.EndpointsConfig) != 0 {
		t.Errorf("loopback joins %v", cfg.EndpointsConfig)
	}
	cfg := Reach{Mode: ReachNetwork, Network: "ci"}.NetworkingConfig()
	if _, ok := cfg.EndpointsConfig["ci"]; !ok || len(cfg.EndpointsConfig) != 1 {
		t.Errorf("network reach joins %v", cfg.EndpointsConfig)
	}
}

func TestReachValidate(t *testing.T) {
	for _, bad := range []Reach{{Mode: ReachNetwork}, {Mode: ReachHost}, {Mode: "carrier-pigeon"}} {
		if err := bad.Validate(); err == nil {
			t.Errorf("%+v validated", bad)
		}
	}
	for _, good := range []Reach{LoopbackReach(), {Mode: ReachNetwork, Network: "n"}, {Mode: ReachHost, HostAddress: "h"}} {
		if err := good.Validate(); err != nil {
			t.Errorf("%+v: %v", good, err)
		}
	}
	if s := (Reach{Mode: ReachNetwork, Network: "n"}).String(); !strings.Contains(s, "n") {
		t.Errorf("String() = %q", s)
	}
}

func TestEndpointOfAListedEngine(t *testing.T) {
	c := container.Summary{
		Names:  []string{"/stratus-abc-elasticache-cache"},
		Labels: map[string]string{portLabel: "6379"},
		Ports:  []container.Port{{PrivatePort: 6379, PublicPort: 49200}},
	}
	cases := []struct {
		reach Reach
		want  backing.Endpoint
	}{
		{LoopbackReach(), backing.Endpoint{Address: "127.0.0.1", Port: 49200}},
		{Reach{Mode: ReachHost, HostAddress: "10.0.4.17"}, backing.Endpoint{Address: "10.0.4.17", Port: 49200}},
		{Reach{Mode: ReachNetwork, Network: "ci"}, backing.Endpoint{Address: "stratus-abc-elasticache-cache", Port: 6379}},
	}
	for _, tc := range cases {
		got, err := endpointOf(c, tc.reach)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v %v, want %v", tc.reach, got, err, tc.want)
		}
	}
	if _, err := endpointOf(container.Summary{Names: []string{"/x"}}, Reach{Mode: ReachNetwork, Network: "ci"}); err == nil {
		t.Error("a container without a port label resolved on a shared network")
	}
}
