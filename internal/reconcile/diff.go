package reconcile

import (
	"bytes"
	"encoding/json"
	"sort"

	"github.com/bitcrafttech/hivenet/internal/topology"
)

// Diff reduces the difference between desired and actual state to an ordered,
// deterministic op list (spec §5.2).
//
// It is a pure function — no I/O, no clock, no randomness — which is what makes
// the reconciler's behaviour testable without podman or a kernel. Map iteration
// is always done over sorted IDs so that the same inputs produce a
// byte-identical op list every time.
func Diff(desired, actual topology.Topology) []Op {
	desiredNodes := nodeIndex(desired.Nodes)
	actualNodes := nodeIndex(actual.Nodes)

	var ops []Op

	// recreated tracks nodes going through destroy+create. Their links have to
	// be rebuilt too, because a veth end lives inside the node's network
	// namespace and dies with the container.
	recreated := make(map[string]bool)

	for _, id := range sortedKeys(desiredNodes) {
		d := desiredNodes[id]
		a, exists := actualNodes[id]
		if !exists {
			ops = append(ops, Op{Kind: OpCreateNode, NodeID: id, Reason: "node is not running"})
			// The container was just created with no network config at all, so any
			// hot config the desired node already carries still needs to be applied.
			// hotChange isn't the right test here: it also flags structural interface
			// declarations (which CreateLink, not this, is responsible for), so a bare
			// interface with no Config would wrongly look like a change against a
			// zero-value Node.
			if needsConfigApply(d) {
				ops = append(ops, Op{Kind: OpUpdateNodeConfig, NodeID: id, Reason: "initial config for a new container"})
			}
			continue
		}
		if reason := coldChange(d, a); reason != "" {
			recreated[id] = true
			ops = append(ops,
				Op{Kind: OpDestroyNode, NodeID: id, Reason: reason},
				Op{Kind: OpCreateNode, NodeID: id, Reason: reason},
			)
			// Same reasoning as the not-running case above: the node behind id was
			// just torn down and rebuilt from scratch, so it holds none of its old
			// hot config either.
			if needsConfigApply(d) {
				ops = append(ops, Op{Kind: OpUpdateNodeConfig, NodeID: id, Reason: "initial config for a recreated container"})
			}
			continue
		}
		if reason := hotChange(d, a); reason != "" {
			ops = append(ops, Op{Kind: OpUpdateNodeConfig, NodeID: id, Reason: reason})
		}
	}

	for _, id := range sortedKeys(actualNodes) {
		if _, wanted := desiredNodes[id]; !wanted {
			ops = append(ops, Op{Kind: OpDestroyNode, NodeID: id, Reason: "node is not in the desired topology"})
		}
	}

	desiredLinks := linkIndex(desired.Links)
	actualLinks := linkIndex(actual.Links)

	for _, id := range sortedKeys(desiredLinks) {
		d := desiredLinks[id]
		a, exists := actualLinks[id]
		switch {
		case !exists:
			ops = append(ops, Op{Kind: OpCreateLink, LinkID: id, Reason: "link does not exist"})
		case !endpointsEqual(d, a):
			const reason = "link endpoints changed"
			ops = append(ops,
				Op{Kind: OpDestroyLink, LinkID: id, Reason: reason},
				Op{Kind: OpCreateLink, LinkID: id, Reason: reason},
			)
		case recreated[d.Endpoints[0].NodeID] || recreated[d.Endpoints[1].NodeID]:
			const reason = "endpoint node is being recreated"
			ops = append(ops,
				Op{Kind: OpDestroyLink, LinkID: id, Reason: reason},
				Op{Kind: OpCreateLink, LinkID: id, Reason: reason},
			)
		}
	}

	for _, id := range sortedKeys(actualLinks) {
		if _, wanted := desiredLinks[id]; !wanted {
			ops = append(ops, Op{Kind: OpDestroyLink, LinkID: id, Reason: "link is not in the desired topology"})
		}
	}

	// Stable sort keeps the ID ordering established above within each phase.
	sort.SliceStable(ops, func(i, j int) bool { return ops[i].Kind.phase() < ops[j].Kind.phase() })
	return ops
}

// coldChange reports the reason a node must be destroyed and recreated, or ""
// if it can be updated in place.
//
// v1 treats exactly two fields as cold: type and image. Everything else is hot
// and applies live (spec §5, "Adapter layer"; CLAUDE.md, "Cold vs hot fields").
// When a field turns out to need a restart, add it here — not at a call site.
func coldChange(desired, actual topology.Node) string {
	switch {
	case desired.Type != actual.Type:
		return "node type changed from " + string(actual.Type) + " to " + string(desired.Type)
	case desired.Image != actual.Image:
		return "image changed from " + actual.Image + " to " + desired.Image
	default:
		return ""
	}
}

// needsConfigApply reports whether a freshly created (or just recreated) node
// carries any hot config that its CreateNode call never applied -- CreateNode
// only ever sees link geometry (topology.Link), never a node's interface
// Config, so declaring an interface with no Config on it is not by itself a
// reason to schedule an update (see hotChange, which -- unlike this -- also
// flags structural interface differences and is only meaningful once a node
// actually exists to compare against).
func needsConfigApply(n topology.Node) bool {
	if len(n.Config) > 0 {
		return true
	}
	for _, iface := range n.Interfaces {
		if len(iface.Config) > 0 {
			return true
		}
	}
	return false
}

// hotChange reports the reason a node needs a live config update, or "" if it
// already matches.
func hotChange(desired, actual topology.Node) string {
	if !desired.Config.Equal(actual.Config) {
		return "node config changed"
	}
	if !interfacesEqual(desired.Interfaces, actual.Interfaces) {
		return "interface config changed"
	}
	return ""
}

// endpointsEqual compares links as unordered pairs: A–B and B–A are the same
// wire, and a client is free to send either.
func endpointsEqual(a, b topology.Link) bool {
	if a.Endpoints[0] == b.Endpoints[0] && a.Endpoints[1] == b.Endpoints[1] {
		return true
	}
	return a.Endpoints[0] == b.Endpoints[1] && a.Endpoints[1] == b.Endpoints[0]
}

// interfacesEqual compares interface sets ignoring declaration order.
func interfacesEqual(a, b []topology.Interface) bool {
	if len(a) != len(b) {
		return false
	}
	if len(a) == 0 {
		return true
	}
	ja, err := json.Marshal(sortedInterfaces(a))
	if err != nil {
		return false
	}
	jb, err := json.Marshal(sortedInterfaces(b))
	if err != nil {
		return false
	}
	return bytes.Equal(ja, jb)
}

func sortedInterfaces(in []topology.Interface) []topology.Interface {
	out := make([]topology.Interface, len(in))
	for i, iface := range in {
		out[i] = iface.Clone()
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func nodeIndex(nodes []topology.Node) map[string]topology.Node {
	out := make(map[string]topology.Node, len(nodes))
	for _, n := range nodes {
		out[n.ID] = n
	}
	return out
}

func linkIndex(links []topology.Link) map[string]topology.Link {
	out := make(map[string]topology.Link, len(links))
	for _, l := range links {
		out[l.ID] = l
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
