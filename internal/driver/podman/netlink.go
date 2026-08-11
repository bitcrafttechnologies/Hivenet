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
	"net"
	"os"
	"runtime"

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

	ownerNodes, err := d.listOwnerNodes(owner)
	if err != nil {
		return fmt.Errorf("create link %s: %w", link.ID, err)
	}
	typeOf := make(map[string]topology.NodeType, len(ownerNodes))
	for _, n := range ownerNodes {
		typeOf[n.ID] = n.Type
	}

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
	if typeOf[ep0.NodeID] == topology.TypeSwitch {
		if err := ensureBridgeMember(h0, name0); err != nil {
			return fmt.Errorf("create link %s: %w", link.ID, err)
		}
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
	if typeOf[ep1.NodeID] == topology.TypeSwitch {
		if err := ensureBridgeMember(h1, name1); err != nil {
			return fmt.Errorf("create link %s: %w", link.ID, err)
		}
	}
	return nil
}

// ensureBridgeMember makes vethName a port of the switch's kernel bridge
// inside the namespace h is bound to, creating the bridge first if this
// is the switch's first port (spec section 7: switch nodes are "Linux
// bridge, kernel-native"). Idempotent: re-attaching an already-attached
// port, or bringing up an already-up bridge, is a no-op in the kernel.
func ensureBridgeMember(h *netlink.Handle, vethName string) error {
	const switchBridgeName = "hvbr0"

	br, err := h.LinkByName(switchBridgeName)
	if err != nil {
		if _, notFound := err.(netlink.LinkNotFoundError); !notFound {
			return fmt.Errorf("find switch bridge: %w", err)
		}
		attrs := netlink.NewLinkAttrs()
		attrs.Name = switchBridgeName
		if err := h.LinkAdd(&netlink.Bridge{LinkAttrs: attrs}); err != nil {
			return fmt.Errorf("create switch bridge: %w", err)
		}
		br, err = h.LinkByName(switchBridgeName)
		if err != nil {
			return fmt.Errorf("find switch bridge after create: %w", err)
		}
	}
	if err := h.LinkSetUp(br); err != nil {
		return fmt.Errorf("bring up switch bridge: %w", err)
	}

	veth, err := h.LinkByName(vethName)
	if err != nil {
		return fmt.Errorf("find port %s: %w", vethName, err)
	}
	if err := h.LinkSetMaster(veth, br); err != nil {
		return fmt.Errorf("attach port %s to switch bridge: %w", vethName, err)
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

// LinkCounters reads the raw byte counters off endpoint 0's veth end,
// inside that node's netns, for the traffic poller (HIVENET_MVP_SPEC.md
// section 11 step 7). It does not touch endpoint 1: a veth pair's two ends
// are one physical wire, so end 0's TxBytes/RxBytes already carry both
// directions (TxBytes out to endpoint 1, RxBytes in from endpoint 1).
func (d *Driver) LinkCounters(ctx context.Context, owner string, link topology.Link) (driver.LinkCounters, error) {
	ep0 := link.Endpoints[0]
	name0 := driver.VethName(link.ID, 0)

	ns0, err := openNodeNetns(ep0.NodeID)
	if err != nil {
		return driver.LinkCounters{}, fmt.Errorf("link counters %s: %w", link.ID, err)
	}
	defer closeNs(ns0)

	h0, err := netlink.NewHandleAt(ns0)
	if err != nil {
		return driver.LinkCounters{}, fmt.Errorf("link counters %s: netlink handle for node %s: %w", link.ID, ep0.NodeID, err)
	}
	defer h0.Close()

	lk, err := h0.LinkByName(name0)
	if err != nil {
		return driver.LinkCounters{}, fmt.Errorf("link counters %s: find %s in node %s netns: %w", link.ID, name0, ep0.NodeID, err)
	}
	stats := lk.Attrs().Statistics
	if stats == nil {
		return driver.LinkCounters{}, fmt.Errorf("link counters %s: %s reported no statistics", link.ID, name0)
	}
	return driver.LinkCounters{TxBytes: stats.TxBytes, RxBytes: stats.RxBytes}, nil
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

// setIPForward toggles kernel IPv4 forwarding inside ns. Forwarding is a
// /proc/sys file, not something rtnetlink exposes, so applying it means
// actually joining the namespace on the calling OS thread rather than
// just binding a netlink socket to it as netlink.NewHandleAt does. The
// thread is locked and its original namespace restored before returning,
// so this cannot leak into other goroutines' netlink calls.
func setIPForward(ns netns.NsHandle, enabled bool) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	origin, err := netns.Get()
	if err != nil {
		return fmt.Errorf("get current netns: %w", err)
	}
	defer origin.Close()

	if err := netns.Set(ns); err != nil {
		return fmt.Errorf("enter netns: %w", err)
	}
	defer netns.Set(origin)

	val := []byte("0\n")
	if enabled {
		val = []byte("1\n")
	}
	if err := os.WriteFile("/proc/sys/net/ipv4/ip_forward", val, 0o644); err != nil {
		return fmt.Errorf("write ip_forward: %w", err)
	}
	return nil
}

// UpdateNodeConfig applies hot config fields (driver.Driver). Two shapes
// of config exist: per-interface (spec section 7's "IP/CIDR per
// interface"), keyed off each interface's veth alias since CreateLink
// only ever sees link geometry (topology.Link), never a node's interface
// Config; and per-node (build order step 6's router/edge additions --
// "ipForward" turns on kernel forwarding, "routes" installs a static
// routes table). Switch nodes have no hot fields here: their only
// configurable behavior, bridge membership, is structural and lives in
// CreateLink instead.
func (d *Driver) UpdateNodeConfig(ctx context.Context, node topology.Node) error {
	if len(node.Interfaces) == 0 && len(node.Config) == 0 {
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
	if raw, ok := node.Config["ipForward"]; ok {
		enabled, _ := raw.(bool)
		if err := setIPForward(ns, enabled); err != nil {
			return fmt.Errorf("update node %s config: %w", node.ID, err)
		}
	}

	if raw, ok := node.Config["routes"]; ok {
		routes, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("update node %s config: routes must be a list", node.ID)
		}
		for i, r := range routes {
			rm, ok := r.(map[string]any)
			if !ok {
				return fmt.Errorf("update node %s config: routes[%d] must be an object", node.ID, i)
			}
			destStr, _ := rm["dest"].(string)
			viaStr, _ := rm["via"].(string)
			if destStr == "" || viaStr == "" {
				return fmt.Errorf("update node %s config: routes[%d] needs dest and via", node.ID, i)
			}
			_, dest, err := net.ParseCIDR(destStr)
			if err != nil {
				return fmt.Errorf("update node %s config: routes[%d] dest %q: %w", node.ID, i, destStr, err)
			}
			via := net.ParseIP(viaStr)
			if via == nil {
				return fmt.Errorf("update node %s config: routes[%d] via %q is not an IP", node.ID, i, viaStr)
			}
			if err := h.RouteReplace(&netlink.Route{Dst: dest, Gw: via}); err != nil {
				return fmt.Errorf("update node %s config: apply route %s via %s: %w", node.ID, destStr, viaStr, err)
			}
		}
	}

	return nil
}
