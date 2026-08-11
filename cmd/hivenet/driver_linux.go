//go:build linux

package main

import (
	"log/slog"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/driver/podman"
)

// driverName is logged at startup so it is never ambiguous which backend is
// actually running.
const driverName = "podman+netlink (build order steps 4-6: host/router/switch/edge node lifecycle, veth links, and per-node config)"

// newDriver selects the driver for this platform.
//
// The real implementation lands here: podman node lifecycle for all four
// node types (build order step 4, extended in step 6) plus netlink veth
// pairs, netns moves, bridge membership, and per-node config for links
// (build order step 5, extended in step 6). Both live behind this
// //go:build linux file so that
// nothing outside internal/driver/<linux impl> ever imports podman or
// netlink (CLAUDE.md, "Development platform reality").
func newDriver(logger *slog.Logger) (driver.Driver, error) {
	return podman.New(podman.DefaultSocket)
}
