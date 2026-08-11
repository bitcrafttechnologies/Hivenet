//go:build linux

// Package podman implements the Podman-backed half of driver.Driver: the
// container lifecycle for host nodes (CreateNode, DestroyNode, ReadActual)
// plus the netns bookkeeping CreateLink/DestroyLink depend on. Link
// management itself (veth pairs, netns moves) lives in netlink.go -- this
// file stays focused on talking to podman. See HIVENET_MVP_SPEC.md section
// 11, item 4/5: "Podman adapter" and "Netlink adapter".
package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/containers/podman/v5/pkg/bindings"
	"github.com/containers/podman/v5/pkg/bindings/containers"
	"github.com/containers/podman/v5/pkg/specgen"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/topology"
)

// DefaultSocket is where the system-wide podman.socket listens.
const DefaultSocket = "unix:///run/podman/podman.sock"

// nodeLabel carries the full topology.Node, JSON-encoded, on every
// Hivenet-managed container. ReadActual decodes it instead of trying to
// infer Type/Interfaces from podman inspect output, which has no notion of
// Hivenet's model.
const nodeLabel = "hivenet.node"

// Driver talks to a live podman over its REST API. The zero value is not
// usable; call New.
type Driver struct {
	// bindings thread the connection through a context value rather than a
	// field on a client struct -- this is podman's API, not a Hivenet choice.
	ctx context.Context
}

// New connects to the podman socket at addr. Empty addr uses DefaultSocket.
func New(addr string) (*Driver, error) {
	if addr == "" {
		addr = DefaultSocket
	}
	connCtx, err := bindings.NewConnection(context.Background(), addr)
	if err != nil {
		return nil, fmt.Errorf("connect podman at %s: %w", addr, err)
	}
	return &Driver{ctx: connCtx}, nil
}

var _ driver.Driver = (*Driver)(nil)

// invalidNameChars matches everything podman rejects in a container name.
var invalidNameChars = regexp.MustCompile(`[^a-zA-Z0-9_.-]`)

// containerName derives a podman container name from an owner/node ID
// pair. Node IDs are arbitrary user input, so this sanitizes rather than
// trusting them directly; unlike driver.VethName it does not need to hash,
// since podman's name limit is far more generous than IFNAMSIZ and keeping
// names human-readable helps debugging with plain podman ps.
func containerName(owner, nodeID string) string {
	return "hivenet_" + invalidNameChars.ReplaceAllString(owner+"_"+nodeID, "_")
}

// CreateNode starts the node's container with no network namespace (spec
// section 6: "every node container launches with --network=none");
// connectivity is added later by CreateLink. Creating a node that already
// exists is a no-op, matching the idempotency contract in driver.Driver.
// Only host nodes are supported so far (spec section 11 item 4); other
// types are step 6. On success the node's netns is symlinked under
// /var/run/netns (spec section 6) so CreateLink can address it later.
func (d *Driver) CreateNode(ctx context.Context, node topology.Node) error {
	if node.Type != topology.TypeHost {
		return fmt.Errorf("create node %s: node type %q not implemented yet (build order step 6)", node.ID, node.Type)
	}

	name := containerName(node.Owner, node.ID)

	exists, err := containers.Exists(d.ctx, name, nil)
	if err != nil {
		return fmt.Errorf("create node %s: check existing: %w", node.ID, err)
	}
	if exists {
		return nil
	}

	encoded, err := json.Marshal(node)
	if err != nil {
		return fmt.Errorf("create node %s: encode metadata: %w", node.ID, err)
	}

	spec := specgen.NewSpecGenerator(node.Image, false)
	spec.Name = name
	spec.NetNS = specgen.Namespace{NSMode: specgen.NoNetwork}
	spec.Labels = map[string]string{
		driver.ManagedLabel: "true",
		driver.OwnerLabel:   node.Owner,
		nodeLabel:           string(encoded),
	}
	// Nodes are network scaffolding a user execs into (spec section 3,
	// "Terminal"), not workloads with their own entrypoint, and
	// --network=none means most images' real services would fail to bind
	// anything useful yet anyway. sleep infinity keeps the container up
	// regardless of what CMD the image ships.
	spec.Command = []string{"sleep", "infinity"}

	created, err := containers.CreateWithSpec(d.ctx, spec, nil)
	if err != nil {
		return fmt.Errorf("create node %s: %w", node.ID, err)
	}
	if err := containers.Start(d.ctx, created.ID, nil); err != nil {
		return fmt.Errorf("create node %s: start: %w", node.ID, err)
	}

	pid, err := d.containerPID(name)
	if err != nil {
		return fmt.Errorf("create node %s: %w", node.ID, err)
	}
	if err := ensureNetnsSymlink(node.ID, pid); err != nil {
		return fmt.Errorf("create node %s: %w", node.ID, err)
	}
	return nil
}

// containerPID returns the host-visible PID of a running container, needed
// to reach its network namespace via /proc/<pid>/ns/net
// (driver.ProcNetnsPath).
func (d *Driver) containerPID(name string) (int, error) {
	data, err := containers.Inspect(d.ctx, name, nil)
	if err != nil {
		return 0, fmt.Errorf("inspect %s: %w", name, err)
	}
	if data.State == nil || data.State.Pid == 0 {
		return 0, fmt.Errorf("inspect %s: no running pid reported", name)
	}
	return data.State.Pid, nil
}

// ensureNetnsSymlink links a node's netns under /var/run/netns (spec
// section 6) so that `ip netns`, netns.GetFromPath, and later CreateLink
// calls can address it by node ID. Podman does not populate this directory
// itself. Idempotent: a stale symlink left over from a previous container
// that used this node ID is replaced rather than left dangling.
func ensureNetnsSymlink(nodeID string, pid int) error {
	target := driver.NetnsPath(nodeID)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create netns dir: %w", err)
	}
	if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale netns symlink: %w", err)
	}
	if err := os.Symlink(driver.ProcNetnsPath(pid), target); err != nil {
		return fmt.Errorf("symlink netns: %w", err)
	}
	return nil
}

// DestroyNode stops and removes the node's container and cleans up its
// netns symlink. Destroying a node that does not exist is a no-op,
// matching the idempotency contract in driver.Driver. Removing the symlink
// is not optional (spec section 6): a leftover entry in /var/run/netns
// outlives the container and is invisible to ReadActual, which only looks
// at podman -- so it would leak forever.
func (d *Driver) DestroyNode(ctx context.Context, owner, nodeID string) error {
	name := containerName(owner, nodeID)

	exists, err := containers.Exists(d.ctx, name, nil)
	if err != nil {
		return fmt.Errorf("destroy node %s: check existing: %w", nodeID, err)
	}
	if exists {
		if _, err := containers.Remove(d.ctx, name, new(containers.RemoveOptions).WithForce(true)); err != nil {
			return fmt.Errorf("destroy node %s: %w", nodeID, err)
		}
	}

	if err := os.Remove(driver.NetnsPath(nodeID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("destroy node %s: remove netns symlink: %w", nodeID, err)
	}
	return nil
}

// listOwnerNodes lists every container Hivenet manages for owner and
// decodes each one's nodeLabel back into a topology.Node. Nothing here is
// cached, matching CLAUDE.md's "never trust in-memory state as ground
// truth".
func (d *Driver) listOwnerNodes(owner string) ([]topology.Node, error) {
	filters := map[string][]string{
		"label": {driver.ManagedLabel, driver.OwnerLabel + "=" + owner},
	}
	list, err := containers.List(d.ctx, new(containers.ListOptions).WithAll(true).WithFilters(filters))
	if err != nil {
		return nil, fmt.Errorf("list containers: %w", err)
	}

	var out []topology.Node
	for _, c := range list {
		encoded, ok := c.Labels[nodeLabel]
		if !ok {
			continue // matched the filter but carries none of our metadata; be defensive
		}
		var node topology.Node
		if err := json.Unmarshal([]byte(encoded), &node); err != nil {
			return nil, fmt.Errorf("decode %s label on %s: %w", nodeLabel, strings.Join(c.Names, ","), err)
		}
		out = append(out, node)
	}
	return out, nil
}

// ReadActual observes real state for owner and returns it as a topology
// (driver.Driver). Node state comes from podman; link state comes from the
// veth aliases netlink.go leaves in each node's netns, since podman has no
// notion of the veth pairs wired between namespaces. A crash or VM reboot
// recovers cleanly because nothing here is cached (CLAUDE.md, "Never trust
// in-memory state as ground truth").
func (d *Driver) ReadActual(ctx context.Context, owner string) (topology.Topology, error) {
	nodes, err := d.listOwnerNodes(owner)
	if err != nil {
		return topology.Topology{}, fmt.Errorf("read actual state for %s: %w", owner, err)
	}

	links := make(map[string]topology.Link)
	for _, node := range nodes {
		// A node with no links yet has no netns symlink to a namespace
		// that's had any veth moved into it if CreateLink never ran, or the
		// symlink may be mid-teardown; either way that's not an error here,
		// it just means this node currently contributes no links.
		for _, l := range readNetnsLinksBestEffort(node.ID) {
			links[l.ID] = l
		}
	}

	out := topology.Topology{Nodes: nodes}
	for _, l := range links {
		out.Links = append(out.Links, l)
	}
	sort.Slice(out.Nodes, func(i, j int) bool { return out.Nodes[i].ID < out.Nodes[j].ID })
	sort.Slice(out.Links, func(i, j int) bool { return out.Links[i].ID < out.Links[j].ID })
	return out, nil
}

// Close releases the podman connection's idle HTTP connections.
func (d *Driver) Close() error {
	if conn, err := bindings.GetClient(d.ctx); err == nil {
		conn.Client.CloseIdleConnections()
	}
	return nil
}
