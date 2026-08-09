# CGO is off everywhere on purpose.
#
# Two reasons, one of them non-obvious:
#   1. Hivenet ships as a single static binary in one container (spec §2), and
#      that is exactly what CGO_ENABLED=0 produces. No cgo dependency is needed:
#      the podman bindings and netlink are both pure Go.
#   2. Go 1.21's external linker emits Mach-O binaries without an LC_UUID load
#      command, which macOS 15+ refuses to execute ("missing LC_UUID load
#      command"). Any package pulling in net/http would otherwise build fine and
#      then fail to run on the dev machine. Upgrading Go past 1.23 also fixes
#      this — and is required before build order step 4 regardless, since the
#      podman v5 bindings need a newer toolchain.
export CGO_ENABLED := 0

# The local toolchain is pinned so a dependency requiring a newer Go fails
# loudly at `go get` time rather than silently trying to download a toolchain.
export GOTOOLCHAIN := local

BINARY := bin/hivenet
ADDR   ?= :8080

.PHONY: all build run test vet fmt check clean

all: check build

build:
	go build -o $(BINARY) ./cmd/hivenet

run:
	go run ./cmd/hivenet -addr $(ADDR)

test:
	go test ./...

vet:
	go vet ./...

fmt:
	gofmt -w .

# What to run before calling a change done.
check: vet test
	@gofmt -l . | grep . && { echo "unformatted files above; run 'make fmt'"; exit 1; } || echo "all checks passed"

clean:
	rm -rf bin
