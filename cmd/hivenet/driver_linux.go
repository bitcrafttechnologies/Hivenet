//go:build linux

package main

import (
	"log/slog"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/driver/podman"
)

// driverName is logged at startup so it is never ambiguous which backend is
// actually running.
const driverName = "podman (netlink adapter is build order step 5; CreateLink/DestroyLink fail until then)"

// newDriver selects the driver for this platform.
//
// The podman half of the real implementation lands here (build order step
// 4): host node container lifecycle. Netlink (CreateLink/DestroyLink,
// build order step 5) is not implemented yet -- see
// internal/driver/podman, which returns a clear error for those calls in
// the meantime. Both live behind this //go:build linux file so that
// nothing outside internal/driver/<linux impl> ever imports podman or
// netlink (CLAUDE.md, "Development platform reality").
func newDriver(logger *slog.Logger) (driver.Driver, error) {
	return podman.New(podman.DefaultSocket)
}
