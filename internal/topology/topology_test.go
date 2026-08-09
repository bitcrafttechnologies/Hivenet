package topology

import (
	"strings"
	"testing"
)

// twoHosts is a minimal valid topology: two hosts joined by one link.
func twoHosts() Topology {
	return Topology{
		Nodes: []Node{
			{ID: "h1", Type: TypeHost, Image: "alpine", Owner: DefaultOwner,
				Interfaces: []Interface{{ID: "eth0", Name: "eth0", Config: Config{"ip": "10.0.0.1/24"}}}},
			{ID: "h2", Type: TypeHost, Image: "alpine", Owner: DefaultOwner,
				Interfaces: []Interface{{ID: "eth0", Name: "eth0", Config: Config{"ip": "10.0.0.2/24"}}}},
		},
		Links: []Link{
			{ID: "l1", Endpoints: [2]Endpoint{{NodeID: "h1", IfaceID: "eth0"}, {NodeID: "h2", IfaceID: "eth0"}}},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Topology)
		wantErr string // substring; empty means the topology must validate
	}{
		{name: "valid", mutate: func(*Topology) {}},
		{
			name:    "duplicate node id",
			mutate:  func(tp *Topology) { tp.Nodes[1].ID = "h1" },
			wantErr: "duplicate node id h1",
		},
		{
			name:    "empty node id",
			mutate:  func(tp *Topology) { tp.Nodes[0].ID = "" },
			wantErr: "nodes[0].id: must not be empty",
		},
		{
			name:    "node id with unsafe characters",
			mutate:  func(tp *Topology) { tp.Nodes[0].ID = "../etc/passwd" },
			wantErr: "nodes[0].id",
		},
		{
			name:    "unknown node type",
			mutate:  func(tp *Topology) { tp.Nodes[0].Type = "firewall" },
			wantErr: `unknown node type "firewall"`,
		},
		{
			name:    "empty image",
			mutate:  func(tp *Topology) { tp.Nodes[0].Image = "" },
			wantErr: "nodes[0].image: must not be empty",
		},
		{
			name: "duplicate interface id within a node",
			mutate: func(tp *Topology) {
				tp.Nodes[0].Interfaces = append(tp.Nodes[0].Interfaces, Interface{ID: "eth0", Name: "eth1"})
			},
			wantErr: "duplicate interface id eth0",
		},
		{
			name:    "interface name exceeds IFNAMSIZ",
			mutate:  func(tp *Topology) { tp.Nodes[0].Interfaces[0].Name = "an-extremely-long-interface-name" },
			wantErr: "at most 15 chars",
		},
		{
			name:    "link references unknown node",
			mutate:  func(tp *Topology) { tp.Links[0].Endpoints[1].NodeID = "ghost" },
			wantErr: "references unknown node ghost",
		},
		{
			name:    "link references unknown interface",
			mutate:  func(tp *Topology) { tp.Links[0].Endpoints[1].IfaceID = "eth9" },
			wantErr: "node h2 has no interface eth9",
		},
		{
			name:    "duplicate link id",
			mutate:  func(tp *Topology) { tp.Links = append(tp.Links, tp.Links[0]) },
			wantErr: "duplicate link id l1",
		},
		{
			name: "interface claimed by two links",
			mutate: func(tp *Topology) {
				tp.Nodes[0].Interfaces = append(tp.Nodes[0].Interfaces, Interface{ID: "eth1", Name: "eth1"})
				tp.Links = append(tp.Links, Link{ID: "l2", Endpoints: [2]Endpoint{
					{NodeID: "h1", IfaceID: "eth0"}, // already used by l1
					{NodeID: "h1", IfaceID: "eth1"},
				}})
			},
			wantErr: "already used by link l1",
		},
		{
			name: "link loops an interface back to itself",
			mutate: func(tp *Topology) {
				tp.Links[0].Endpoints[1] = tp.Links[0].Endpoints[0]
			},
			wantErr: "both endpoints are the same interface",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			topo := twoHosts()
			tc.mutate(&topo)
			err := topo.Validate()

			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected topology to validate, got: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantErr, err)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	topo := twoHosts()
	topo.Nodes[0].Image = ""
	topo.Nodes[1].Type = "bogus"

	err := topo.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("expected a joined error so the API can report per-field details, got %T", err)
	}
	if got := len(joined.Unwrap()); got != 2 {
		t.Fatalf("expected 2 validation errors, got %d: %v", got, err)
	}
}

// Config values arrive both from JSON decoding (numbers become float64) and
// from Go literals in driver code. If those compared unequal, every reconcile
// cycle would see a phantom diff and rewrite config forever.
func TestConfigEqualIgnoresNumericRepresentation(t *testing.T) {
	fromGo := Config{"mtu": 1500}
	fromJSON := Config{"mtu": float64(1500)}

	if !fromGo.Equal(fromJSON) {
		t.Error("int 1500 and float64 1500 must compare equal")
	}
	if fromGo.Equal(Config{"mtu": 9000}) {
		t.Error("different values must not compare equal")
	}
	if !Config(nil).Equal(Config{}) {
		t.Error("nil and empty config must compare equal")
	}
	if (Config{"a": 1}).Equal(nil) {
		t.Error("populated and nil config must not compare equal")
	}
}

func TestNormalizeFillsDefaults(t *testing.T) {
	topo := Topology{Nodes: []Node{{
		ID: "h1", Type: TypeHost, Image: "alpine",
		Interfaces: []Interface{{ID: "a"}, {ID: "b"}},
	}}}
	topo.Normalize()

	if topo.Nodes[0].Owner != DefaultOwner {
		t.Errorf("owner = %q, want %q", topo.Nodes[0].Owner, DefaultOwner)
	}
	if got := topo.Nodes[0].Interfaces[0].Name; got != "eth0" {
		t.Errorf("interfaces[0].name = %q, want eth0", got)
	}
	if got := topo.Nodes[0].Interfaces[1].Name; got != "eth1" {
		t.Errorf("interfaces[1].name = %q, want eth1", got)
	}
}

func TestNormalizeKeepsExplicitValues(t *testing.T) {
	topo := Topology{Nodes: []Node{{
		ID: "h1", Type: TypeHost, Image: "alpine", Owner: "someone-else",
		Interfaces: []Interface{{ID: "a", Name: "wan0"}},
	}}}
	topo.Normalize()

	if topo.Nodes[0].Owner != "someone-else" {
		t.Error("Normalize overwrote an explicit owner")
	}
	if topo.Nodes[0].Interfaces[0].Name != "wan0" {
		t.Error("Normalize overwrote an explicit interface name")
	}
}

func TestTopologyEqualIgnoresOrdering(t *testing.T) {
	a := twoHosts()
	b := twoHosts()
	b.Nodes[0], b.Nodes[1] = b.Nodes[1], b.Nodes[0]

	if !a.Equal(b) {
		t.Error("topologies differing only in node order must compare equal")
	}

	b.Nodes[0].Image = "debian"
	if a.Equal(b) {
		t.Error("a real difference must not compare equal")
	}
}

// Every read from the store returns a clone. If Clone were shallow, a caller
// mutating a config map would silently corrupt stored desired state.
func TestCloneIsDeep(t *testing.T) {
	original := twoHosts()
	clone := original.Clone()

	clone.Nodes[0].Config = Config{"hostname": "changed"}
	clone.Nodes[0].Interfaces[0].Config["ip"] = "192.168.1.1/24"
	clone.Nodes[0].Interfaces[0].Name = "wan0"

	if got := original.Nodes[0].Interfaces[0].Config["ip"]; got != "10.0.0.1/24" {
		t.Errorf("mutating the clone changed the original interface config: %v", got)
	}
	if original.Nodes[0].Config != nil {
		t.Error("mutating the clone changed the original node config")
	}
	if original.Nodes[0].Interfaces[0].Name != "eth0" {
		t.Error("mutating the clone changed the original interface name")
	}
}
