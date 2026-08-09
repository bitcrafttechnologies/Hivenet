//go:build !linux

package main

import (
	"log/slog"

	"github.com/bitcrafttech/hivenet/internal/driver"
	"github.com/bitcrafttech/hivenet/internal/driver/fake"
)

// driverName is logged at startup so it is never ambiguous which backend is
// actually running.
const driverName = "fake (podman and netlink are Linux-only; this is a dev build)"

// newDriver returns the in-memory driver.
//
// This is the only option off Linux and always will be: podman and netlink do
// not exist here. It is what makes the API, store, reconciler and frontend
// developable on a Mac (CLAUDE.md, "Development platform reality").
func newDriver(logger *slog.Logger) (driver.Driver, error) {
	logger.Info("in-memory driver: topology changes are simulated, not applied")
	return fake.New(), nil
}
