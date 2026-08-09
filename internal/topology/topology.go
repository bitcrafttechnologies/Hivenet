// Package topology defines Hivenet's authoritative desired-state document.
//
// It is deliberately free of podman, netlink and container concepts so that the
// model, its validation and the reconciler diff can be built and tested on any
// platform. See CLAUDE.md, "Development platform reality".
package topology

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
)

// DefaultOwner is the single owner value carried by every node in v1.
//
// Multi-tenancy is a seam, not a feature (spec §8): the field is threaded
// through every node, driver call and reconciler query so it never has to be
// retrofitted, but in v1 it never varies.
const DefaultOwner = "local"

// maxIfaceNameLen is the kernel's IFNAMSIZ limit less the trailing NUL. Names
// longer than this are rejected by netlink at link-creation time, so they are
// rejected here instead — at the point where a useful error can be shown.
const maxIfaceNameLen = 15

// idPattern constrains node, interface and link IDs. IDs end up in netns
// symlink paths and container labels, so they are restricted to a charset that
// is safe in both without escaping.
var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

// NodeType enumerates the v1 node catalog (spec §7).
type NodeType string

const (
	TypeHost   NodeType = "host"
	TypeSwitch NodeType = "switch"
	TypeRouter NodeType = "router"
	TypeEdge   NodeType = "edge"
)

// Valid reports whether t is a known node type.
func (t NodeType) Valid() bool {
	switch t {
	case TypeHost, TypeSwitch, TypeRouter, TypeEdge:
		return true
	}
	return false
}

// Config holds type-specific settings for a node or an interface.
//
// It stays untyped at this layer because the meaning of a field belongs to the
// per-type adapter, not the reconciler (spec §5, "Adapter layer"). The
// reconciler only ever asks whether two configs differ.
type Config map[string]any

// Equal reports whether two configs are semantically identical.
//
// It compares canonical JSON rather than using reflect.DeepEqual: configs
// arrive both from JSON decoding and from Go literals in driver code, so an int
// and a float64 holding the same number must not read as a difference — that
// would produce a phantom diff on every reconcile cycle. encoding/json sorts
// map keys at every level, which makes the encoding canonical.
func (c Config) Equal(other Config) bool {
	if len(c) == 0 && len(other) == 0 {
		return true
	}
	a, err := json.Marshal(c)
	if err != nil {
		return false
	}
	b, err := json.Marshal(other)
	if err != nil {
		return false
	}
	return bytes.Equal(a, b)
}

// Clone returns a deep copy so that callers cannot mutate stored state.
func (c Config) Clone() Config {
	if c == nil {
		return nil
	}
	out := make(Config, len(c))
	for k, v := range c {
		out[k] = cloneValue(v)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		m := make(map[string]any, len(t))
		for k, vv := range t {
			m[k] = cloneValue(vv)
		}
		return m
	case Config:
		return t.Clone()
	case []any:
		s := make([]any, len(t))
		for i, vv := range t {
			s[i] = cloneValue(vv)
		}
		return s
	default:
		// Everything else reaching this model is a JSON scalar, which is
		// immutable in Go.
		return v
	}
}

// Interface is one attachment point on a node. It becomes one end of a veth
// pair once a link references it.
type Interface struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Config Config `json:"config,omitempty"`
}

// Clone returns a deep copy.
func (i Interface) Clone() Interface {
	i.Config = i.Config.Clone()
	return i
}

// Node is one container in the topology.
type Node struct {
	ID         string      `json:"id"`
	Type       NodeType    `json:"type"`
	Image      string      `json:"image"`
	Owner      string      `json:"owner"`
	Interfaces []Interface `json:"interfaces,omitempty"`
	Config     Config      `json:"config,omitempty"`
}

// Clone returns a deep copy.
func (n Node) Clone() Node {
	out := n
	out.Config = n.Config.Clone()
	if n.Interfaces != nil {
		out.Interfaces = make([]Interface, len(n.Interfaces))
		for i, iface := range n.Interfaces {
			out.Interfaces[i] = iface.Clone()
		}
	}
	return out
}

// InterfaceByID returns the named interface and whether it was found.
func (n Node) InterfaceByID(id string) (Interface, bool) {
	for _, iface := range n.Interfaces {
		if iface.ID == id {
			return iface, true
		}
	}
	return Interface{}, false
}

// Endpoint identifies one end of a link.
type Endpoint struct {
	NodeID  string `json:"nodeId"`
	IfaceID string `json:"ifaceId"`
}

// Link is a veth pair joining two interfaces. Exactly two endpoints, always.
type Link struct {
	ID        string      `json:"id"`
	Endpoints [2]Endpoint `json:"endpoints"`
}

// Topology is the desired state the reconciler converges toward.
type Topology struct {
	Nodes []Node `json:"nodes"`
	Links []Link `json:"links"`
}

// Clone returns a deep copy.
func (t Topology) Clone() Topology {
	var out Topology
	if t.Nodes != nil {
		out.Nodes = make([]Node, len(t.Nodes))
		for i, n := range t.Nodes {
			out.Nodes[i] = n.Clone()
		}
	}
	if t.Links != nil {
		out.Links = make([]Link, len(t.Links))
		copy(out.Links, t.Links)
	}
	return out
}

// NodeByID returns the named node and whether it was found.
func (t Topology) NodeByID(id string) (Node, bool) {
	for _, n := range t.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

// LinkByID returns the named link and whether it was found.
func (t Topology) LinkByID(id string) (Link, bool) {
	for _, l := range t.Links {
		if l.ID == id {
			return l, true
		}
	}
	return Link{}, false
}

// Equal reports whether two topologies describe the same desired state,
// ignoring the order of nodes, interfaces and links.
func (t Topology) Equal(other Topology) bool {
	a, err := json.Marshal(t.canonical())
	if err != nil {
		return false
	}
	b, err := json.Marshal(other.canonical())
	if err != nil {
		return false
	}
	return bytes.Equal(a, b)
}

// canonical returns a copy with every collection sorted by ID, so that
// serialising it yields a stable representation regardless of input ordering.
func (t Topology) canonical() Topology {
	out := t.Clone()
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	for i := range out.Nodes {
		ifaces := out.Nodes[i].Interfaces
		sort.Slice(ifaces, func(a, b int) bool { return ifaces[a].ID < ifaces[b].ID })
	}
	sort.Slice(out.Links, func(i, j int) bool { return out.Links[i].ID < out.Links[j].ID })
	return out
}

// Document pairs a topology with its version counter. The version increments on
// every backend-accepted change (spec §4) and is what the frontend uses to tell
// a confirmed state from an optimistic one.
type Document struct {
	Topology Topology `json:"topology"`
	Version  int      `json:"version"`
}

// Clone returns a deep copy.
func (d Document) Clone() Document {
	return Document{Topology: d.Topology.Clone(), Version: d.Version}
}

// ValidationError is a single problem with a topology, addressed by a JSON-ish
// path so the frontend can attach it to the field that caused it.
type ValidationError struct {
	Path string `json:"path"`
	Msg  string `json:"msg"`
}

func (e ValidationError) Error() string { return e.Path + ": " + e.Msg }

// Normalize fills in the defaults the frontend is not required to send: the
// constant v1 owner, and sequential eth-style interface names. It is called by
// the store before Validate.
func (t *Topology) Normalize() {
	for i := range t.Nodes {
		if t.Nodes[i].Owner == "" {
			t.Nodes[i].Owner = DefaultOwner
		}
		for j := range t.Nodes[i].Interfaces {
			if t.Nodes[i].Interfaces[j].Name == "" {
				t.Nodes[i].Interfaces[j].Name = fmt.Sprintf("eth%d", j)
			}
		}
	}
}

// Validate reports every structural problem with the topology at once, so a
// caller can surface them all rather than one per round trip.
//
// It enforces the invariants the reconciler and the netlink layer depend on:
// unique, path-safe IDs; links that reference real interfaces; and at most one
// link per interface, since a veth end can only live in one pair.
func (t Topology) Validate() error {
	var errs []error
	add := func(path, msg string) { errs = append(errs, ValidationError{Path: path, Msg: msg}) }

	seenNodes := make(map[string]bool, len(t.Nodes))
	for i, n := range t.Nodes {
		path := fmt.Sprintf("nodes[%d]", i)
		switch {
		case n.ID == "":
			add(path+".id", "must not be empty")
		case !idPattern.MatchString(n.ID):
			add(path+".id", "must be 1-64 chars of [A-Za-z0-9_-] and start alphanumeric")
		case seenNodes[n.ID]:
			add(path+".id", "duplicate node id "+n.ID)
		}
		seenNodes[n.ID] = true

		if !n.Type.Valid() {
			add(path+".type", fmt.Sprintf("unknown node type %q", n.Type))
		}
		if n.Image == "" {
			add(path+".image", "must not be empty")
		}

		seenIfaces := make(map[string]bool, len(n.Interfaces))
		for j, iface := range n.Interfaces {
			ipath := fmt.Sprintf("%s.interfaces[%d]", path, j)
			switch {
			case iface.ID == "":
				add(ipath+".id", "must not be empty")
			case !idPattern.MatchString(iface.ID):
				add(ipath+".id", "must be 1-64 chars of [A-Za-z0-9_-] and start alphanumeric")
			case seenIfaces[iface.ID]:
				add(ipath+".id", "duplicate interface id "+iface.ID)
			}
			seenIfaces[iface.ID] = true

			if len(iface.Name) > maxIfaceNameLen {
				add(ipath+".name", fmt.Sprintf("must be at most %d chars (kernel IFNAMSIZ)", maxIfaceNameLen))
			}
		}
	}

	seenLinks := make(map[string]bool, len(t.Links))
	// claimed maps "nodeID/ifaceID" to the link already using it.
	claimed := make(map[string]string)
	for i, l := range t.Links {
		path := fmt.Sprintf("links[%d]", i)
		switch {
		case l.ID == "":
			add(path+".id", "must not be empty")
		case !idPattern.MatchString(l.ID):
			add(path+".id", "must be 1-64 chars of [A-Za-z0-9_-] and start alphanumeric")
		case seenLinks[l.ID]:
			add(path+".id", "duplicate link id "+l.ID)
		}
		seenLinks[l.ID] = true

		for e, ep := range l.Endpoints {
			epath := fmt.Sprintf("%s.endpoints[%d]", path, e)
			node, ok := t.NodeByID(ep.NodeID)
			if !ok {
				add(epath+".nodeId", "references unknown node "+ep.NodeID)
				continue
			}
			if _, ok := node.InterfaceByID(ep.IfaceID); !ok {
				add(epath+".ifaceId", fmt.Sprintf("node %s has no interface %s", ep.NodeID, ep.IfaceID))
				continue
			}
			key := ep.NodeID + "/" + ep.IfaceID
			if other, taken := claimed[key]; taken {
				add(epath, fmt.Sprintf("interface already used by link %s; an interface can belong to at most one link", other))
				continue
			}
			claimed[key] = l.ID
		}

		if l.Endpoints[0] == l.Endpoints[1] {
			add(path+".endpoints", "both endpoints are the same interface")
		}
	}

	return errors.Join(errs...)
}
