# CLAUDE.md — Hivenet

Working reference for agents on this repo. The authoritative product/architecture
document is [HIVENET_MVP_SPEC.md](HIVENET_MVP_SPEC.md) — **read it before making
design decisions.** This file covers what the spec doesn't: repo layout, commands,
conventions, and the constraints that are easy to violate accidentally.

## What Hivenet is

A drag-and-drop canvas that builds and runs *live* virtual network topologies —
not diagrams. Nodes are real containers, links are real veth pairs, and clicking a
node gives you a terminal into it. Single-user, self-contained simulation. v1 never
reads from or writes to physical infrastructure.

## Hard rule: every change goes in the changelog

`changelog` at the repo root is a required output, not a nicety. Any change to
code, schema, dependencies, or build process gets an entry **in the same turn as
the change**. Newest section on top. Record what changed and *why* — the why is
the part that isn't recoverable from a diff later.

## Repo layout

```
cmd/hivenet/            main — flag parsing, wiring, graceful shutdown
internal/
  topology/             desired-state document: nodes, interfaces, links, validation
  store/                in-memory topology store, version counter, change subscribers
  reconcile/            typed ops, diff, debounce/coalesce engine, status tracking
  driver/               THE privileged seam — interface only, no podman/netlink here
    fake/               in-memory driver: used by tests and by all non-Linux dev
  httpapi/              HTTP server, REST handlers, WebSocket endpoints
  auth/                 pluggable auth middleware seam, no-op default (spec §8)
web/                    frontend; web/dist is go:embed'd into the binary
```

## Commands

Use the Makefile — it sets `CGO_ENABLED=0` and `GOTOOLCHAIN=local`, both of
which matter (see "Toolchain" below). Bare `go test ./...` will fail on macOS.

```bash
make check          # vet + test + gofmt check — run before calling work done
make build          # -> bin/hivenet
make run            # serve on :8080 (make run ADDR=:9090 for another port)
make test
make fmt
```

Useful flags: `-addr`, `-debounce`, `-log-level debug` (logs every applied op).

```bash
curl localhost:8080/api/topology                    # current desired state
curl -X PUT localhost:8080/api/topology -d @topo.json
curl localhost:8080/api/status                      # last reconcile result
```

`web/dist/` must contain at least one file or **every** Go build fails —
`go:embed` errors on an empty pattern match. Don't delete the placeholder
without replacing it.

## Toolchain

Go here is **1.21.6**, and that is a problem in two ways:

1. Its external linker emits Mach-O binaries with no `LC_UUID` load command,
   which macOS 15+ refuses to run: `dyld: missing LC_UUID load command`. Any
   package importing `net/http` picks up the cgo resolver and hits this — it
   builds fine, then won't execute. `CGO_ENABLED=0` sidesteps it and is the
   right setting anyway (spec §2 wants a single static binary; no dependency
   needs cgo). The Makefile sets it. The consequence: **`go test -race` cannot
   run on the `httpapi` package here**, since race requires cgo. Every other
   package races clean.
2. The podman v5 bindings require Go ≥1.23, so **step 4 is blocked until Go is
   upgraded.** Do that on the dev machine and the VM together and bump the `go`
   directive in go.mod; it also makes the `LC_UUID` workaround unnecessary.

Dependencies are pinned to versions that still build on 1.21 — notably
`github.com/coder/websocket` **v1.8.13** (v1.8.14+ requires Go 1.23). After
upgrading Go, unpin it.

## Development platform reality

The dev machine is **macOS/arm64**; the deploy target is **Ubuntu KVM on Proxmox**.
`github.com/containers/podman/v5/pkg/bindings` and `vishvananda/netlink` are
Linux-only and will not compile on darwin.

Therefore: **nothing outside `internal/driver/<linux impl>` may import podman or
netlink.** The reconciler, store, topology model, and HTTP layer must stay
platform-neutral so the whole dev loop runs on a Mac against the fake driver. When
the real driver lands (build order steps 4–6) it goes behind `//go:build linux`,
with the fake selected on `!linux`. Violating this turns the Mac into a
non-development machine for the entire project.

Go here is 1.21.6. The podman v5 bindings need a newer toolchain — bump the VM's Go
(and the `go` directive) when step 4 starts, not before.

## Architecture invariants

These come from the spec and are the ones most likely to be broken by a
reasonable-looking change.

**The reconciler is the single writer.** Nothing else calls podman or netlink. HTTP
handlers mutate the store and return; the engine converges. No handler ever applies
state inline.

**Never trust in-memory state as ground truth.** Every reconcile cycle starts by
reading actual state from the driver. This is the only reason the system recovers
from a crash or VM reboot; caching "what we think we created" defeats it.

**Two documents, never merged.** The topology doc (authoritative, versioned,
reconciled) and the layout doc (client-owned x/y/zoom/cosmetics) are separate.
Canvas position changes must never bump the topology version or trigger a
reconcile.

**All ops are idempotent.** Every op checks current state before acting, so
re-running a full reconcile is always safe. This is what makes crash recovery and
coalesced debounce work.

**Op ordering is fixed:** destroy links → destroy nodes → create nodes → create
links → config updates last. `reconcile.Op.order()` encodes this; change the
constants, not the call sites.

**No rollback on partial failure.** Leave applied ops in place, mark the specific
failed node `error` with a message. "Fix the red node, re-run" is the v1 UX.
Rollback is explicitly out of scope — don't add it.

**Drift is surfaced, never auto-corrected** in v1.

**`owner` threads through everything** — every node, driver call, and reconciler
query. In v1 it's always `topology.DefaultOwner`. Do not remove the parameter
because it "isn't used yet"; re-threading it later is the expensive path.

**Cold vs hot fields.** v1 treats only `type` and `image` as cold (require
destroy+recreate). Everything else is hot and applies live via `UpdateNodeConfig`.
If a field turns out to need a recreate, add it to the cold set in
`reconcile/diff.go` — don't special-case it at a call site.

## Networking gotchas (from spec §6 — all bite silently)

- **Veth names cap at 15 chars** (`IFNAMSIZ`). Name interfaces from a short hash of
  the link ID, never from user-supplied node names. `driver.VethName` does this;
  use it.
- **Podman doesn't register netns under `/var/run/netns/`**, so `ip netns` can't see
  node namespaces. Symlink `/proc/<pid>/ns/net` → `/var/run/netns/<nodeId>` on
  create, and **remove it on teardown or entries leak**.
- **rx/tx is inverted on the host veth end** — rx on the host side is tx from the
  container's perspective. Getting this backwards makes the traffic animation flow
  the wrong direction and looks like a UI bug.
- Node containers launch `--network=none`. All connectivity is explicit veth pairs
  wired by the reconciler.
- An interface participates in **at most one link** — a veth end can only be in one
  pair. `topology.Validate` enforces this.

## Deviations from the spec (deliberate, keep them)

- **WebSocket library**: spec §3 names `nhooyr.io/websocket`. That module was
  handed over to Coder and now lives at `github.com/coder/websocket` — same
  library, same API, still maintained. We use the new path.
- **Adopt-on-start**: the store is in-memory, so a restart would come up with an
  empty desired state and reconcile the running lab out of existence. Instead
  `Engine.Adopt` seeds the store from actual state at boot, so restarting the
  app is not restarting the lab. Revisit when persistence lands.
- **Version as a concurrency check**: `PUT /api/topology` treats a non-zero
  `version` as an If-Match. A stale version gets 409 instead of silently
  clobbering a change the client never saw. Sending `version: 0` skips the check.
- **No-op writes don't bump the version**, so a client re-sending an identical
  document can't trigger a reconcile storm.

## Explicitly out of scope for v1

Do not add these, even if they seem like small wins: Open vSwitch (switch is a
kernel Linux bridge), per-node VNC/GUI, dynamic routing protocol GUI (OSPF/BGP go
to the terminal tab via `vtysh`), rollback-on-failure, multi-user auth /
sessions / RBAC, Terraform for per-node provisioning, AI prompt bar in the UI.

## Conventions

- Errors wrap with `fmt.Errorf("...: %w", err)` and name the entity: node ID, link
  ID, op kind.
- Exported types get a doc comment saying what invariant they hold, not what they
  are.
- Tests live beside the code. The diff engine and validation are pure functions —
  they should stay table-driven and exhaustively tested, since they're where
  correctness actually lives.
- No `panic` outside `main` wiring.
- Config values are `map[string]any` compared via canonical JSON, not
  `reflect.DeepEqual` — a JSON round-trip turns ints into float64 and would
  otherwise produce phantom diffs on every cycle.

## Build order (spec §11) — current position

1. ✅ Go backend scaffold: HTTP server, `go:embed`, WebSocket endpoints (stubbed)
2. ✅ Topology doc schema + in-memory store + version counter
3. ✅ Reconciler core: read-actual → diff → typed ops
4. ⬜ Podman adapter: `CreateNode`/`DestroyNode` for host nodes (Linux only)
   — **blocked on the Go upgrade**, see "Toolchain"
5. ⬜ Netlink adapter: veth pairs + netns move + symlinks; two hosts ping
6. ⬜ Extend adapters to router/switch/edge
7. ⬜ Traffic-stats polling + `traffic_stats` push
8. ⬜ Svelte frontend: canvas, palette, outline panel
9. ⬜ Node detail panel: per-type config, field-level save, lock-on-pending
10. ⬜ Terminal tab: pty WebSocket + xterm.js
11. ⬜ Visual polish pass against spec §10

Steps 4–6 require the Ubuntu VM with a podman socket. They cannot be developed or
tested on the Mac — write them against the driver interface and validate on the VM.
