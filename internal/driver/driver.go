// Package driver is the single seam between the platform-neutral reconciler and
// the privileged podman/netlink calls that actually change the machine.
//
// Nothing in this package imports podman or netlink. Real implementations live
// in build-tagged files (//go:build linux) so that the reconciler, store and
// HTTP layer keep compiling and testing on macOS against driver/fake. See
// CLAUDE.md, "Development platform reality".
package driver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"

	"github.com/bitcrafttech/hivenet/internal/topology"
)

// ManagedLabel is the podman label every Hivenet-created container carries. The
// reconciler's read-actual step filters on it, so a container without it is
// invisible to Hivenet and will not be reaped.
const ManagedLabel = "hivenet.managed"

// OwnerLabel records which owner a container belongs to (spec §8).
const OwnerLabel = "hivenet.owner"

// netnsDir is where node network namespaces are symlinked so that `ip netns`
// and netns.GetFromPath can see them. Podman does not populate this directory
// itself (spec §6).
const netnsDir = "/var/run/netns"

// Driver applies desired state to the machine and reads back what is actually
// there. It is the only interface the reconciler calls; the reconciler contains
// no node-type-specific logic.
//
// Every method must be idempotent: it checks current state before acting, so
// re-running a full reconcile is always safe (spec §5.4). Implementations are
// called from the reconciler's single writer goroutine and need not be
// safe for concurrent use, but ReadActual may be called for status queries and
// should be.
type Driver interface {
	// ReadActual observes real state for owner and returns it as a topology.
	// It reads from podman and the kernel, never from a cache — this is what
	// lets the system recover from a crash or VM reboot (spec §5.1).
	ReadActual(ctx context.Context, owner string) (topology.Topology, error)

	// CreateNode starts the node's container with --network=none. Connectivity
	// is added later by CreateLink.
	CreateNode(ctx context.Context, node topology.Node) error

	// DestroyNode stops and removes the container and cleans up its netns
	// symlink. Removing the symlink is not optional: leftover entries in
	// /var/run/netns leak (spec §6).
	DestroyNode(ctx context.Context, owner, nodeID string) error

	// UpdateNodeConfig applies hot fields — those appliable without a restart.
	// Cold fields never reach here; the reconciler turns them into a
	// destroy/create pair.
	UpdateNodeConfig(ctx context.Context, node topology.Node) error

	// CreateLink creates a veth pair and moves each end into the netns of the
	// corresponding endpoint's node.
	CreateLink(ctx context.Context, owner string, link topology.Link) error

	// DestroyLink removes the veth pair. Deleting either end removes both.
	DestroyLink(ctx context.Context, owner, linkID string) error

	// Close releases any long-lived connection the driver holds.
	Close() error
}

// VethName derives the host-visible name for one end of a link's veth pair.
//
// Linux caps interface names at IFNAMSIZ (15 usable chars), which is far too
// short for arbitrary user-chosen IDs, so the name is a short hash of the link
// ID rather than anything human-readable (spec §6). The result is 13 chars.
// end must be 0 or 1, matching the index into Link.Endpoints.
func VethName(linkID string, end int) string {
	sum := sha256.Sum256([]byte(linkID))
	return fmt.Sprintf("hv%s%d", hex.EncodeToString(sum[:5]), end)
}

// NetnsPath is where a node's network namespace is symlinked. The reconciler
// creates this on node creation by linking /proc/<pid>/ns/net here, because
// podman does not register namespaces where `ip netns` can find them.
func NetnsPath(nodeID string) string {
	return filepath.Join(netnsDir, nodeID)
}

// ProcNetnsPath is the source of that symlink, resolved from the container's
// PID. Reaching it requires the Hivenet container to run with --pid=host.
func ProcNetnsPath(pid int) string {
	return fmt.Sprintf("/proc/%d/ns/net", pid)
}
