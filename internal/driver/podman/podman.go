//go:build linux

// Package podman implements the Podman-backed half of driver.Driver: the
// container lifecycle for host nodes (CreateNode, DestroyNode, ReadActual).
// This is build order step 4 (CLAUDE.md, "Build order"); link management
// needs netlink and lands in step 5, so CreateLink/DestroyLink return an
// error here until then. See HIVENET_MVP_SPEC.md, section 11, item 4:
// "Podman adapter: CreateNode/DestroyNode for the host node type only,
// prove the container lifecycle end to end."
package podman

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
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

// CreateNode starts node's container with no network namespace (spec
// section 6: "every node container launches with --network=none");
// connectivity is added later by CreateLink. Creating a node that already
// exists is a no-op, matching the idempotency contract in driver.Driver.
// Only host nodes are supported so far (spec section 11 item 4); other
// types are step 6.
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
	return nil
}

// DestroyNode stops and removes the node's container. Destroying a node
// that does not exist is a no-op, matching the idempotency contract in
// driver.Driver. It does not clean up a netns symlink (driver.NetnsPath)
// yet because nothing creates one until CreateLink lands in step 5.
func (d *Driver) DestroyNode(ctx context.Context, owner, nodeID string) error {
	name := containerName(owner, nodeID)

	exists, err := containers.Exists(d.ctx, name, nil)
	if err != nil {
		return fmt.Errorf("destroy node %s: check existing: %w", nodeID, err)
	}
	if !exists {
		return nil
	}

	if _, err := containers.Remove(d.ctx, name, new(containers.RemoveOptions).WithForce(true)); err != nil {
		return fmt.Errorf("destroy node %s: %w", nodeID, err)
	}
	return nil
}

// ReadActual lists containers Hivenet manages for owner and decodes each
// one's nodeLabel back into a topology.Node. The running container's label
// is the only source of truth; nothing here is cached, so a crash or VM
// reboot cannot desync it (CLAUDE.md, "Never trust in-memory state as
// ground truth").
func (d *Driver) ReadActual(ctx context.Context, owner string) (topology.Topology, error) {
	filters := map[string][]string{
		"label": {driver.ManagedLabel, driver.OwnerLabel + "=" + owner},
	}
	list, err := containers.List(d.ctx, new(containers.ListOptions).WithAll(true).WithFilters(filters))
	if err != nil {
		return topology.Topology{}, fmt.Errorf("read actual state for %s: list containers: %w", owner, err)
	}

	var out topology.Topology
	for _, c := range list {
		encoded, ok := c.Labels[nodeLabel]
		if !ok {
			continue // matched the filter but carries none of our metadata; be defensive
		}
		var node topology.Node
		if err := json.Unmarshal([]byte(encoded), &node); err != nil {
			return topology.Topology{}, fmt.Errorf("read actual state for %s: decode %s label on %s: %w", owner, nodeLabel, strings.Join(c.Names, ","), err)
		}
		out.Nodes = append(out.Nodes, node)
	}
	return out, nil
}

// CreateLink is netlink's job (veth pairs, netns moves) and lands in build
// order step 5 (CLAUDE.md, "Build order"). It always fails until then.
func (d *Driver) CreateLink(ctx context.Context, owner string, link topology.Link) error {
	return fmt.Errorf("create link %s: netlink adapter not implemented yet (build order step 5)", link.ID)
}

// DestroyLink is netlink's job; see CreateLink.
func (d *Driver) DestroyLink(ctx context.Context, owner, linkID string) error {
	return fmt.Errorf("destroy link %s: netlink adapter not implemented yet (build order step 5)", linkID)
}

// UpdateNodeConfig applies hot config fields. Host nodes declare no hot
// fields yet, so there is nothing to apply.
func (d *Driver) UpdateNodeConfig(ctx context.Context, node topology.Node) error {
	return nil
}

// Close releases the podman connection's idle HTTP connections.
func (d *Driver) Close() error {
	if conn, err := bindings.GetClient(d.ctx); err == nil {
		conn.Client.CloseIdleConnections()
	}
	return nil
}
