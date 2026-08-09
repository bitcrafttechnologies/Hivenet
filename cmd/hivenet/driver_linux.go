//go:build linux

package main

import (
	"log/slog"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/driver/fake"
)

// driverName is logged at startup so it is never ambiguous which backend is
// actually running.
const driverName = "fake (podman/netlink driver is build order steps 4-6)"

// newDriver selects the driver for this platform.
//
// The real podman/netlink implementation lands in build order steps 4–6 and
// will be constructed here. The build tags are in place already so that adding
// those imports can never break the macOS build — nothing outside a
// //go:build linux file may import podman or netlink (CLAUDE.md).
func newDriver(logger *slog.Logger) (driver.Driver, error) {
	logger.Warn("using the in-memory driver: no containers or veth pairs will be created")
	return fake.New(), nil
}
