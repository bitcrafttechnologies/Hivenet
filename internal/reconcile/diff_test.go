package reconcile

import (
	"reflect"
	"testing"

	"github.com/bitcrafttech/hivenet/internal/topology"
)

func node(id string, opts ...func(*topology.Node)) topology.Node {
	n := topology.Node{
		ID: id, Type: topology.TypeHost, Image: "alpine", Owner: topology.DefaultOwner,
		Interfaces: []topology.Interface{{ID: "eth0", Name: "eth0"}},
	}
	for _, o := range opts {
		o(&n)
	}
	return n
}

func withImage(image string) func(*topology.Node) {
	return func(n *topology.Node) { n.Image = image }
}

func withConfig(cfg topology.Config) func(*topology.Node) {
	return func(n *topology.Node) { n.Config = cfg }
}

func link(id, aNode, bNode string) topology.Link {
	return topology.Link{ID: id, Endpoints: [2]topology.Endpoint{
		{NodeID: aNode, IfaceID: "eth0"}, {NodeID: bNode, IfaceID: "eth0"},
	}}
}

// summarize renders ops as "Kind:target" so tests assert on both content and
// order without depending on Reason wording.
func summarize(ops []Op) []string {
	out := make([]string, len(ops))
	for i, op := range ops {
		out[i] = string(op.Kind) + ":" + op.Target()
	}
	return out
}

func TestDiff(t *testing.T) {
	tests := []struct {
		name    string
		desired topology.Topology
		actual  topology.Topology
		want    []string
	}{
		{
			name: "both empty",
			want: nil,
		},
		{
			name:    "converged topology produces no ops",
			desired: topology.Topology{Nodes: []topology.Node{node("h1"), node("h2")}, Links: []topology.Link{link("l1", "h1", "h2")}},
			actual:  topology.Topology{Nodes: []topology.Node{node("h1"), node("h2")}, Links: []topology.Link{link("l1", "h1", "h2")}},
			want:    nil,
		},
		{
			name:    "new node",
			desired: topology.Topology{Nodes: []topology.Node{node("h1")}},
			want:    []string{"CreateNode:h1"},
		},
		{
			name:    "new node with day-one hot config gets it applied, not just created bare",
			desired: topology.Topology{Nodes: []topology.Node{node("h1", withConfig(topology.Config{"hostname": "web"}))}},
			want:    []string{"CreateNode:h1", "UpdateNodeConfig:h1"},
		},
		{
			name:   "node no longer wanted",
			actual: topology.Topology{Nodes: []topology.Node{node("h1")}},
			want:   []string{"DestroyNode:h1"},
		},
		{
			name:    "hot config change updates in place",
			desired: topology.Topology{Nodes: []topology.Node{node("h1", withConfig(topology.Config{"hostname": "web"}))}},
			actual:  topology.Topology{Nodes: []topology.Node{node("h1")}},
			want:    []string{"UpdateNodeConfig:h1"},
		},
		{
			name:    "cold image change forces recreate",
			desired: topology.Topology{Nodes: []topology.Node{node("h1", withImage("debian"))}},
			actual:  topology.Topology{Nodes: []topology.Node{node("h1")}},
			want:    []string{"DestroyNode:h1", "CreateNode:h1"},
		},
		{
			name:    "recreated node carries its hot config over, not just structural fields",
			desired: topology.Topology{Nodes: []topology.Node{node("h1", withImage("debian"), withConfig(topology.Config{"hostname": "web"}))}},
			actual:  topology.Topology{Nodes: []topology.Node{node("h1")}},
			want:    []string{"DestroyNode:h1", "CreateNode:h1", "UpdateNodeConfig:h1"},
		},
		{
			name: "cold type change forces recreate",
			desired: topology.Topology{Nodes: []topology.Node{node("h1", func(n *topology.Node) {
				n.Type = topology.TypeRouter
			})}},
			actual: topology.Topology{Nodes: []topology.Node{node("h1")}},
			want:   []string{"DestroyNode:h1", "CreateNode:h1"},
		},
		{
			name:    "new link",
			desired: topology.Topology{Nodes: []topology.Node{node("h1"), node("h2")}, Links: []topology.Link{link("l1", "h1", "h2")}},
			actual:  topology.Topology{Nodes: []topology.Node{node("h1"), node("h2")}},
			want:    []string{"CreateLink:l1"},
		},
		{
			name:    "link no longer wanted",
			desired: topology.Topology{Nodes: []topology.Node{node("h1"), node("h2")}},
			actual:  topology.Topology{Nodes: []topology.Node{node("h1"), node("h2")}, Links: []topology.Link{link("l1", "h1", "h2")}},
			want:    []string{"DestroyLink:l1"},
		},
		{
			name:    "rewired link is torn down and rebuilt",
			desired: topology.Topology{Nodes: []topology.Node{node("h1"), node("h2"), node("h3")}, Links: []topology.Link{link("l1", "h1", "h3")}},
			actual:  topology.Topology{Nodes: []topology.Node{node("h1"), node("h2"), node("h3")}, Links: []topology.Link{link("l1", "h1", "h2")}},
			want:    []string{"DestroyLink:l1", "CreateLink:l1"},
		},
		{
			// A veth end lives in the node's netns and dies with the container,
			// so a recreated node takes its links with it even when the link
			// itself is unchanged. Missing this leaves a half-wired node.
			name:    "recreating a node rebuilds its untouched links",
			desired: topology.Topology{Nodes: []topology.Node{node("h1", withImage("debian")), node("h2")}, Links: []topology.Link{link("l1", "h1", "h2")}},
			actual:  topology.Topology{Nodes: []topology.Node{node("h1"), node("h2")}, Links: []topology.Link{link("l1", "h1", "h2")}},
			want:    []string{"DestroyLink:l1", "DestroyNode:h1", "CreateNode:h1", "CreateLink:l1"},
		},
		{
			name: "ops are emitted in the fixed phase order",
			desired: topology.Topology{
				Nodes: []topology.Node{node("keep", withConfig(topology.Config{"hostname": "keep"})), node("add")},
				Links: []topology.Link{link("newlink", "keep", "add")},
			},
			actual: topology.Topology{
				Nodes: []topology.Node{node("keep"), node("drop")},
				Links: []topology.Link{link("oldlink", "keep", "drop")},
			},
			want: []string{
				"DestroyLink:oldlink",
				"DestroyNode:drop",
				"CreateNode:add",
				"CreateLink:newlink",
				"UpdateNodeConfig:keep",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := summarize(Diff(tc.desired, tc.actual))
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("Diff() =\n  %v\nwant\n  %v", got, tc.want)
			}
		})
	}
}

// A link is a wire, not an arrow: a client may send either endpoint order.
// Treating A–B and B–A as different would tear down and rebuild every link on
// every cycle.
func TestDiffTreatsLinkEndpointsAsUnordered(t *testing.T) {
	nodes := []topology.Node{node("h1"), node("h2")}
	forward := topology.Topology{Nodes: nodes, Links: []topology.Link{link("l1", "h1", "h2")}}
	reversed := topology.Topology{Nodes: nodes, Links: []topology.Link{link("l1", "h2", "h1")}}

	if ops := Diff(forward, reversed); len(ops) != 0 {
		t.Errorf("expected no ops for a link sent in the opposite endpoint order, got %v", summarize(ops))
	}
}

// The op list feeds a debounced loop that may run many times a second. A
// non-deterministic order would make failures unreproducible and logs unreadable.
func TestDiffIsDeterministic(t *testing.T) {
	desired := topology.Topology{
		Nodes: []topology.Node{node("z"), node("a"), node("m"), node("b")},
		Links: []topology.Link{link("l3", "z", "a"), link("l1", "m", "b")},
	}
	first := summarize(Diff(desired, topology.Topology{}))
	for i := 0; i < 50; i++ {
		if got := summarize(Diff(desired, topology.Topology{})); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differed:\n  %v\nfirst:\n  %v", i, got, first)
		}
	}
	want := []string{"CreateNode:a", "CreateNode:b", "CreateNode:m", "CreateNode:z", "CreateLink:l1", "CreateLink:l3"}
	if !reflect.DeepEqual(first, want) {
		t.Errorf("Diff() = %v, want IDs sorted within each phase: %v", first, want)
	}
}

func TestDiffDetectsInterfaceConfigChange(t *testing.T) {
	desired := topology.Topology{Nodes: []topology.Node{node("h1", func(n *topology.Node) {
		n.Interfaces[0].Config = topology.Config{"ip": "10.0.0.5/24"}
	})}}
	actual := topology.Topology{Nodes: []topology.Node{node("h1")}}

	if got, want := summarize(Diff(desired, actual)), []string{"UpdateNodeConfig:h1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("Diff() = %v, want %v", got, want)
	}
}

func TestDiffIgnoresInterfaceDeclarationOrder(t *testing.T) {
	twoIfaces := func(first, second string) topology.Node {
		return node("h1", func(n *topology.Node) {
			n.Interfaces = []topology.Interface{{ID: first, Name: first}, {ID: second, Name: second}}
		})
	}
	desired := topology.Topology{Nodes: []topology.Node{twoIfaces("eth0", "eth1")}}
	actual := topology.Topology{Nodes: []topology.Node{twoIfaces("eth1", "eth0")}}

	if ops := Diff(desired, actual); len(ops) != 0 {
		t.Errorf("expected no ops for reordered interfaces, got %v", summarize(ops))
	}
}
