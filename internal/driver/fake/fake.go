// Package fake provides an in-memory driver.Driver.
//
// It backs two things: the entire test suite for the reconciler, and every
// non-Linux development session — podman and netlink do not exist on macOS, so
// without this the app could not be run at all on the dev machine (CLAUDE.md,
// "Development platform reality").
//
// It models state, not behaviour: a "container" is a map entry. It deliberately
// does not simulate IP stacks or packet forwarding.
package fake

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/topology"
)

// Driver is an in-memory implementation of driver.Driver. The zero value is not
// usable; call New.
type Driver struct {
	mu    sync.Mutex
	nodes map[string]nodeRecord
	links map[string]linkRecord

	// counters is the last byte-counter reading set for a link by
	// SetLinkCounters, returned verbatim by LinkCounters. Tests drive
	// synthetic traffic by calling SetLinkCounters between poll ticks.
	counters map[string]driver.LinkCounters

	// failures maps "<op>:<id>" to the error that op should return, letting
	// tests exercise the no-rollback partial-failure path (spec §5.5).
	failures map[string]error

	// calls records every mutating op in order, for test assertions.
	calls []string
}

type nodeRecord struct {
	node  topology.Node
	owner string
}

type linkRecord struct {
	link  topology.Link
	owner string
}

// New returns an empty fake driver.
func New() *Driver {
	return &Driver{
		nodes:    make(map[string]nodeRecord),
		links:    make(map[string]linkRecord),
		counters: make(map[string]driver.LinkCounters),
		failures: make(map[string]error),
	}
}

// ReadActual returns the state the fake currently holds for owner.
func (d *Driver) ReadActual(ctx context.Context, owner string) (topology.Topology, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.failures["ReadActual:"]; err != nil {
		return topology.Topology{}, err
	}

	var out topology.Topology
	for _, rec := range d.nodes {
		if rec.owner != owner {
			continue
		}
		out.Nodes = append(out.Nodes, rec.node.Clone())
	}
	for _, rec := range d.links {
		if rec.owner != owner {
			continue
		}
		out.Links = append(out.Links, rec.link)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Links, func(i, j int) bool { return out.Links[i].ID < out.Links[j].ID })
	return out, nil
}

// CreateNode records the node. Creating a node that already exists is a no-op,
// matching the idempotency contract.
func (d *Driver) CreateNode(ctx context.Context, node topology.Node) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "CreateNode:"+node.ID)
	if err := d.failures["CreateNode:"+node.ID]; err != nil {
		return err
	}
	if _, exists := d.nodes[node.ID]; exists {
		return nil
	}
	d.nodes[node.ID] = nodeRecord{node: node.Clone(), owner: node.Owner}
	return nil
}

// DestroyNode removes the node and any link that referenced it, mirroring the
// real world: a veth end dies with the namespace it lives in.
func (d *Driver) DestroyNode(ctx context.Context, owner, nodeID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "DestroyNode:"+nodeID)
	if err := d.failures["DestroyNode:"+nodeID]; err != nil {
		return err
	}
	rec, exists := d.nodes[nodeID]
	if !exists || rec.owner != owner {
		return nil
	}
	delete(d.nodes, nodeID)
	for id, l := range d.links {
		if l.link.Endpoints[0].NodeID == nodeID || l.link.Endpoints[1].NodeID == nodeID {
			delete(d.links, id)
		}
	}
	return nil
}

// UpdateNodeConfig replaces the stored node's hot fields.
func (d *Driver) UpdateNodeConfig(ctx context.Context, node topology.Node) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "UpdateNodeConfig:"+node.ID)
	if err := d.failures["UpdateNodeConfig:"+node.ID]; err != nil {
		return err
	}
	rec, exists := d.nodes[node.ID]
	if !exists {
		return fmt.Errorf("update config: node %s does not exist", node.ID)
	}
	rec.node = node.Clone()
	d.nodes[node.ID] = rec
	return nil
}

// CreateLink records the veth pair. Both endpoint nodes must exist, which is
// what makes a failed CreateNode cascade into a failed CreateLink — the
// behaviour the no-rollback design depends on surfacing.
func (d *Driver) CreateLink(ctx context.Context, owner string, link topology.Link) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "CreateLink:"+link.ID)
	if err := d.failures["CreateLink:"+link.ID]; err != nil {
		return err
	}
	if _, exists := d.links[link.ID]; exists {
		return nil
	}
	for _, ep := range link.Endpoints {
		if _, exists := d.nodes[ep.NodeID]; !exists {
			return fmt.Errorf("create link %s: node %s does not exist", link.ID, ep.NodeID)
		}
	}
	d.links[link.ID] = linkRecord{link: link, owner: owner}
	return nil
}

// DestroyLink removes the veth pair.
func (d *Driver) DestroyLink(ctx context.Context, owner, linkID string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = append(d.calls, "DestroyLink:"+linkID)
	if err := d.failures["DestroyLink:"+linkID]; err != nil {
		return err
	}
	delete(d.links, linkID)
	return nil
}

// LinkCounters returns the counters last set for linkID via
// SetLinkCounters (zero value if never set), simulating
// driver.Driver.LinkCounters without any real kernel state.
func (d *Driver) LinkCounters(ctx context.Context, owner string, link topology.Link) (driver.LinkCounters, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.failures["LinkCounters:"+link.ID]; err != nil {
		return driver.LinkCounters{}, err
	}
	return d.counters[link.ID], nil
}

// SetLinkCounters lets a test advance a link's simulated byte counters,
// standing in for real traffic crossing its veth pair between poll ticks.
func (d *Driver) SetLinkCounters(linkID string, c driver.LinkCounters) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.counters[linkID] = c
}

// Close is a no-op.
func (d *Driver) Close() error { return nil }

// FailOp makes the named op fail with err. key is "<OpName>:<entityID>", for
// example "CreateNode:host-a". Pass a nil err to clear.
func (d *Driver) FailOp(key string, err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err == nil {
		delete(d.failures, key)
		return
	}
	d.failures[key] = err
}

// InjectNode adds a node behind the reconciler's back, simulating drift — state
// that appeared without Hivenet asking for it (spec §5.6).
func (d *Driver) InjectNode(owner string, node topology.Node) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.nodes[node.ID] = nodeRecord{node: node.Clone(), owner: owner}
}

// RemoveNode deletes a node behind the reconciler's back, simulating a
// container that died on its own. Its links go with it, because a veth end
// lives inside the container's network namespace.
func (d *Driver) RemoveNode(nodeID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.nodes, nodeID)
	for id, l := range d.links {
		if l.link.Endpoints[0].NodeID == nodeID || l.link.Endpoints[1].NodeID == nodeID {
			delete(d.links, id)
		}
	}
}

// Calls returns the mutating ops applied so far, in order.
func (d *Driver) Calls() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.calls...)
}

// ResetCalls clears the recorded call log without touching state.
func (d *Driver) ResetCalls() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls = nil
}
