//go:build linux

package main

import (
	"log/slog"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/driver/podman"
)

// driverName is logged at startup so it is never ambiguous which backend is
// actually running.
const driverName = "podman+netlink (build order steps 4-5: host node lifecycle and veth links)"

// newDriver selects the driver for this platform.
//
// The real implementation lands here: podman host node lifecycle (build
// order step 4) plus netlink veth pairs and netns moves for links (build
// order step 5). Both live behind this //go:build linux file so that
// nothing outside internal/driver/<linux impl> ever imports podman or
// netlink (CLAUDE.md, "Development platform reality").
func newDriver(logger *slog.Logger) (driver.Driver, error) {
	return podman.New(podman.DefaultSocket)
}
