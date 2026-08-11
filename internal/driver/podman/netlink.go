//go:build linux

// netlink.go implements the veth-pair half of driver.Driver (CreateLink,
// DestroyLink, UpdateNodeConfig) plus the link-discovery part of
// ReadActual. This is build order step 5 (CLAUDE.md, "Build order"); see
// HIVENET_MVP_SPEC.md section 6 for the netns/veth model this follows.
package podman

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/topology"
)

// linkAlias encodes the full topology.Link onto both veth ends'
// IFLA_IFALIAS. The kernel caps interface *names* at IFNAMSIZ (spec section
// 6, driver.VethName), but the alias field has no such limit. Podman only
// knows about containers, not the veth pairs wired between their
// namespaces, so this alias is the sole source of truth ReadActual has for
// reconstructing links after a crash (CLAUDE.md, "never trust in-memory
// state as ground truth").
func linkAlias(link topology.Link) (string, error) {
	encoded, err := json.Marshal(link)
	if err != nil {
		return "", fmt.Errorf("encode link alias: %w", err)
	}
	return string(encoded), nil
}

// openNodeNetns opens the namespace a node's container runs in, following
// the symlink CreateNode leaves under /var/run/netns (spec section 6).
// Callers must close the returned handle.
func openNodeNetns(nodeID string) (netns.NsHandle, error) {
	ns, err := netns.GetFromPath(driver.NetnsPath(nodeID))
	if err != nil {
		return 0, fmt.Errorf("open netns for node %s: %w", nodeID, err)
	}
	return ns, nil
}

// closeNs releases a namespace handle. NsHandle is a bare fd (no Close
// method of its own in the netns package), so this goes through unix
// directly.
func closeNs(ns netns.NsHandle) {
	_ = unix.Close(int(ns))
}

// CreateLink creates a veth pair and moves each end into the netns of the
// corresponding endpoint's node (driver.Driver). Idempotent: if both ends
// already exist where expected, it is a no-op.
func (d *Driver) CreateLink(ctx context.Context, owner string, link topology.Link) error {
	ep0, ep1 := link.Endpoints[0], link.Endpoints[1]
	name0, name1 := driver.VethName(link.ID, 0), driver.VethName(link.ID, 1)

	ns0, err := openNodeNetns(ep0.NodeID)
	if err != nil {
		return fmt.Errorf("create link %s: %w", link.ID, err)
	}
	defer closeNs(ns0)
	ns1, err := openNodeNetns(ep1.NodeID)
	if err != nil {
		return fmt.Errorf("create link %s: %w", link.ID, err)
	}
	defer closeNs(ns1)

	h0, err := netlink.NewHandleAt(ns0)
	if err != nil {
		return fmt.Errorf("create link %s: netlink handle for node %s: %w", link.ID, ep0.NodeID, err)
	}
	defer h0.Close()
	h1, err := netlink.NewHandleAt(ns1)
	if err != nil {
		return fmt.Errorf("create link %s: netlink handle for node %s: %w", link.ID, ep1.NodeID, err)
	}
	defer h1.Close()

	_, err0 := h0.LinkByName(name0)
	_, err1 := h1.LinkByName(name1)
	if err0 == nil && err1 == nil {
		return nil // already fully wired
	}

	alias, err := linkAlias(link)
	if err != nil {
		return fmt.Errorf("create link %s: %w", link.ID, err)
	}

	// Veth pairs can only be created in the caller's current namespace, so
	// build the pair here (the Hivenet process's own root netns) and move
	// each end out. A same-named leftover from an interrupted previous
	// attempt is removed first -- LinkAdd is not itself idempotent.
	if stale, err := netlink.LinkByName(name0); err == nil {
		_ = netlink.LinkDel(stale)
	}
	veth := &netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: name0},
		PeerName:  name1,
	}
	if err := netlink.LinkAdd(veth); err != nil {
		return fmt.Errorf("create link %s: add veth pair: %w", link.ID, err)
	}

	rootEnd0, err := netlink.LinkByName(name0)
	if err != nil {
		return fmt.Errorf("create link %s: find %s after creation: %w", link.ID, name0, err)
	}
	rootEnd1, err := netlink.LinkByName(name1)
	if err != nil {
		return fmt.Errorf("create link %s: find %s after creation: %w", link.ID, name1, err)
	}

	if err := netlink.LinkSetNsFd(rootEnd0, int(ns0)); err != nil {
		return fmt.Errorf("create link %s: move %s into node %s netns: %w", link.ID, name0, ep0.NodeID, err)
	}
	if err := netlink.LinkSetNsFd(rootEnd1, int(ns1)); err != nil {
		return fmt.Errorf("create link %s: move %s into node %s netns: %w", link.ID, name1, ep1.NodeID, err)
	}

	movedEnd0, err := h0.LinkByName(name0)
	if err != nil {
		return fmt.Errorf("create link %s: find %s in node %s netns: %w", link.ID, name0, ep0.NodeID, err)
	}
	if err := h0.LinkSetAlias(movedEnd0, alias); err != nil {
		return fmt.Errorf("create link %s: tag %s: %w", link.ID, name0, err)
	}
	if err := h0.LinkSetUp(movedEnd0); err != nil {
		return fmt.Errorf("create link %s: bring %s up: %w", link.ID, name0, err)
	}

	movedEnd1, err := h1.LinkByName(name1)
	if err != nil {
		return fmt.Errorf("create link %s: find %s in node %s netns: %w", link.ID, name1, ep1.NodeID, err)
	}
	if err := h1.LinkSetAlias(movedEnd1, alias); err != nil {
		return fmt.Errorf("create link %s: tag %s: %w", link.ID, name1, err)
	}
	if err := h1.LinkSetUp(movedEnd1); err != nil {
		return fmt.Errorf("create link %s: bring %s up: %w", link.ID, name1, err)
	}
	return nil
}

// DestroyLink removes the veth pair (driver.Driver). Deleting either end
// removes both, so this only needs to find one of them; it searches every
// node the owner has rather than trusting caller-supplied endpoints, since
// after a crash nothing but the kernel's own state can be trusted.
// Destroying a link that does not exist is a no-op, matching the
// idempotency contract in driver.Driver.
func (d *Driver) DestroyLink(ctx context.Context, owner, linkID string) error {
	nodes, err := d.listOwnerNodes(owner)
	if err != nil {
		return fmt.Errorf("destroy link %s: %w", linkID, err)
	}

	for _, node := range nodes {
		ns, err := openNodeNetns(node.ID)
		if err != nil {
			continue // node has no netns right now; nothing to check here
		}
		h, err := netlink.NewHandleAt(ns)
		if err != nil {
			closeNs(ns)
			continue
		}

		var found netlink.Link
		for _, end := range [2]int{0, 1} {
			if lk, err := h.LinkByName(driver.VethName(linkID, end)); err == nil {
				found = lk
				break
			}
		}
		if found == nil {
			h.Close()
			closeNs(ns)
			continue
		}

		delErr := h.LinkDel(found)
		h.Close()
		closeNs(ns)
		if delErr != nil {
			return fmt.Errorf("destroy link %s: %w", linkID, delErr)
		}
		return nil
	}
	return nil // already gone: matches the idempotency contract in driver.Driver
}

// readNetnsLinksBestEffort decodes every link alias found in a node's
// netns back into a topology.Link. It is the link half of ReadActual:
// podman's container list has no notion of the veth pairs wired between
// namespaces, so this alias is the only source of truth for actual link
// state. Errors (no netns yet, transient netlink failure) are swallowed
// rather than failing the whole read -- a link that is genuinely missing
// surfaces on its own as drift once the reconciler diffs desired against
// actual, rather than as an opaque ReadActual failure here.
func readNetnsLinksBestEffort(nodeID string) []topology.Link {
	ns, err := openNodeNetns(nodeID)
	if err != nil {
		return nil
	}
	defer closeNs(ns)

	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return nil
	}
	defer h.Close()

	ifaces, err := h.LinkList()
	if err != nil {
		return nil
	}

	var out []topology.Link
	for _, iface := range ifaces {
		alias := iface.Attrs().Alias
		if alias == "" {
			continue
		}
		var link topology.Link
		if err := json.Unmarshal([]byte(alias), &link); err != nil {
			continue // not one of ours (or corrupt); ignore rather than fail the whole read
		}
		out = append(out, link)
	}
	return out
}

// UpdateNodeConfig applies hot config fields (driver.Driver). For host
// nodes in v1 the only hot field is per-interface IP/CIDR (spec section
// 7). It is applied here, keyed off each interface's veth alias, rather
// than in CreateLink, because CreateLink only ever sees link geometry
// (topology.Link), never a node's interface Config.
func (d *Driver) UpdateNodeConfig(ctx context.Context, node topology.Node) error {
	if len(node.Interfaces) == 0 {
		return nil
	}

	ns, err := openNodeNetns(node.ID)
	if err != nil {
		return fmt.Errorf("update node %s config: %w", node.ID, err)
	}
	defer closeNs(ns)
	h, err := netlink.NewHandleAt(ns)
	if err != nil {
		return fmt.Errorf("update node %s config: netlink handle: %w", node.ID, err)
	}
	defer h.Close()

	ifaces, err := h.LinkList()
	if err != nil {
		return fmt.Errorf("update node %s config: list interfaces: %w", node.ID, err)
	}

	for _, iface := range ifaces {
		alias := iface.Attrs().Alias
		if alias == "" {
			continue
		}
		var link topology.Link
		if err := json.Unmarshal([]byte(alias), &link); err != nil {
			continue
		}

		ifaceID := ""
		for _, ep := range link.Endpoints {
			if ep.NodeID == node.ID {
				ifaceID = ep.IfaceID
			}
		}
		if ifaceID == "" {
			continue
		}

		var cfg topology.Interface
		for _, i := range node.Interfaces {
			if i.ID == ifaceID {
				cfg = i
				break
			}
		}
		if cfg.ID == "" {
			continue
		}

		cidr, ok := cfg.Config["ip"].(string)
		if !ok || cidr == "" {
			continue
		}
		addr, err := netlink.ParseAddr(cidr)
		if err != nil {
			return fmt.Errorf("update node %s config: interface %s: parse ip %q: %w", node.ID, ifaceID, cidr, err)
		}
		if err := h.AddrReplace(iface, addr); err != nil {
			return fmt.Errorf("update node %s config: interface %s: apply %s: %w", node.ID, ifaceID, cidr, err)
		}
	}
	return nil
}
