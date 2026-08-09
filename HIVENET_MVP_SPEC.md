# Hivenet — MVP Build Spec

A drag-and-drop canvas for building and running virtual network topologies. This document consolidates the full architecture decided for the MVP so it can be handed to a coding agent as a single, unambiguous build target.

## 1. What it is

Users drag predefined VM/container nodes (server, switch, router, user device, internet edge) onto a canvas, wire them together with lines, and get a *live*, running virtual network — not a diagram. Clicking a node opens a details panel with full config control and a terminal. Lines animate with moving dots when two nodes are actively communicating.

Scope for v1: **single-user, self-contained simulation only.** No integration with real hardware. Any "mirroring" of a real network is done by the user manually rebuilding a twin topology on the canvas — Hivenet does not read or write to physical infrastructure in v1.

## 2. Deployment shape

- **One Go binary, one container.** The built frontend (static Svelte assets) is embedded into the Go binary via `go:embed` and served from the same HTTP server that hosts the WebSocket endpoints. No separate frontend service.
- That single container runs inside a **KVM VM on Proxmox** (not an LXC — podman needs real `CAP_NET_ADMIN` and unprivileged nested LXC + podman is a permissions minefield not worth fighting). VM OS: **Ubuntu** (server/minimal).
- The Hivenet container talks to the **VM's podman socket** and creates sibling containers (the lab nodes) alongside itself — not nested inside itself:
  ```
  --network=host
  --pid=host
  --cap-add=NET_ADMIN,SYS_ADMIN
  -v /run/podman/podman.sock:/run/podman/podman.sock
  ```
  `--pid=host` is required to resolve each node container's PID and reach `/proc/<pid>/ns/net` for netns work.
- This capability set is effectively root-on-the-VM. Treat the VM as the entire blast radius (disposable, snapshot before destructive tests, ideally its own subnet/VLAN). Keep every netlink/podman privileged call behind one narrow module — later this can be lifted into a separate small privileged agent if the app container needs to drop capabilities.
- **No Terraform.** Its plan/apply/state-lock model doesn't fit a topology that changes every few seconds via UI interaction. Its own reconciler (below) replaces it. Terraform could reasonably provision the *host VM itself* later, but not per-node.

## 3. Tech stack

| Layer | Choice | Why |
|---|---|---|
| Backend | Go | Official podman bindings, real netlink libraries, compiles to a single static binary |
| Podman control | `github.com/containers/podman/v5/pkg/bindings` | Talks to the podman socket directly, no CLI shellouts |
| Networking | `github.com/vishvananda/netlink` + `github.com/vishvananda/netns` | Creates veth pairs, moves ends into container namespaces — this *is* the adapter layer in practice |
| WebSocket | `nhooyr.io/websocket` | Lightweight, modern, used for both the topology/status channel and per-node terminal channel |
| Frontend framework | Svelte (Vite or SvelteKit in SPA mode) | Explicitly not React/Next — smaller runtime, no vdom diff overhead, matters at 1Hz re-renders across many links |
| Canvas / node graph | **Svelte Flow** (xyflow) | Same node/edge/drag/connect model as React Flow, Svelte-native |
| Terminal | xterm.js | Framework-agnostic, connects to a `podman exec` pty over its own WebSocket |
| Styling | Tailwind | Fast to hit the clean/minimal reference aesthetic (see §6) |

## 4. Data model

Two separate documents — do not merge them:

- **Topology doc** (authoritative, versioned): nodes, interfaces, links, per-node config. This is what the reconciler acts on.
- **Layout doc** (client-owned, low-stakes): canvas x/y positions, zoom, cosmetics. Never touched by the reconciler.

```yaml
topology:
  nodes:
    - id: string
      type: host | switch | router | edge
      image: string          # container image ref
      owner: string          # constant default value in v1; see §8
      interfaces:
        - id: string
          name: string
          config: { ip: cidr, ... }   # type-specific, see §7
      config: {}              # type-specific node-level config
  links:
    - id: string
      endpoints:
        - { nodeId, ifaceId }
        - { nodeId, ifaceId }
version: int   # incremented on every backend-accepted change
```

## 5. Reconciler

The single writer for all podman/netlink state. Runs on a debounce after canvas/panel edits.

**Cycle:**
1. **Read actual state** — `podman ps --filter label=hivenet.managed=true`, then inspect each container's netns for real interfaces/links. Never trust in-memory state as ground truth; this is what makes the reconciler recover cleanly from a crash or VM reboot.
2. **Diff** desired (topology doc) vs. actual → typed op list: `CreateNode`, `DestroyNode`, `UpdateNodeConfig`, `CreateLink`, `DestroyLink`.
3. **Order:** destroy links → destroy nodes → create nodes → create links → apply config updates last.
4. **Apply**, idempotently — every op checks current state before acting, so re-running a full reconcile is always safe.
5. **On partial failure:** leave successfully-applied ops in place, surface the error on the specific failed node (`error` state + message). **No automatic rollback** — rollback logic is a rabbit hole; "fix the one red node, re-run" is the v1 UX.
6. **Drift:** if actual state has diverged from last-persisted desired state (manual poke, crash mid-apply), surface it on the node. Do **not** silently auto-correct in v1.
7. **Trigger:** debounce ~300–500ms after the last discrete canvas/panel action. Single-writer via mutex/coalescing queue — a new debounce firing mid-apply queues the *latest* desired state, not every intermediate edit.

**Adapter layer:** every node type implements `apply(intent)` and `read() -> intent`. The reconciler calls only this interface — it never has type-specific logic. Each adapter classifies its own fields as:
- **hot** — live-appliable (`ip addr add`, route table change, `vtysh reload`) → gets a GUI control
- **cold** — requires destroy+recreate (image swap, resource limits) → GUI shows it but applying it triggers a full node recreate, or v1 punts it to the terminal tab

## 6. Networking internals

- Every node container launches with `--network=none`; all connectivity is explicit veth pairs wired by the reconciler.
- **Veth name cap is 15 chars** (`IFNAMSIZ`) — name links by a short hash of the link ID, not human-readable strings.
- Podman doesn't register its netns under `/var/run/netns/`, so `ip netns` can't see node namespaces. On node creation, symlink `/proc/<pid>/ns/net` → `/var/run/netns/<nodeId>`; clean up the symlink on teardown or entries leak.
- **Traffic animation (moving dots):** poll `/sys/class/net/<iface>/statistics/{rx,tx}_bytes` on the host end of each veth pair at ~1Hz, diff between polls, push deltas over WebSocket. No packet capture needed. Note the inversion — rx on the host-side veth end is tx from the container's perspective.

## 7. Node catalog (v1)

| Node type | Image | Notes |
|---|---|---|
| Host | `alpine` (custom Containerfile adding `iproute2`, `curl`, `iputils`, `tcpdump`) | Config surface: hostname, IP/CIDR per interface, default gateway, DNS |
| Router | `frrouting/frr` (official image) | Config surface: static routes table in v1 GUI; OSPF/BGP left to the terminal tab (`vtysh`) rather than building a GUI for dynamic routing protocols |
| Switch | **Linux bridge, kernel-native, no extra packages** (v1 default) | Simpler and always-available; VLAN via `bridge vlan` + 802.1q if needed. **Open vSwitch is a deliberate v2 upgrade**, not v1 — only pull it in if trunking/STP fidelity becomes a real need. This is a scope call worth keeping explicit rather than defaulting to OVS complexity now. |
| Internet/NAT edge | `alpine` + `iptables`/`nftables` | Provides NAT + default route out; acts as the topology's uplink node |

All four adapters expose only **hot** fields via GUI for v1; anything else is a terminal-tab, config-file-level operation.

**Explicitly cut from v1:** no GUI/VNC option per node (terminal only — a VNC server in every image doubles build surface for one demo screenshot), no dynamic routing GUI, no OVS, no rollback-on-failure, no multi-user auth, no Terraform.

## 8. Multi-tenancy seam (not built, but not foreclosed)

Multi-user is a someday-maybe; v1 is single-user. Hivenet is intended to be open-sourced for self-hosting, so the seam is built in now at zero cost rather than retrofitted later:

- Every node, every podman/netlink call, every reconciler query threads an `owner` field. In v1 it's always the same constant value.
- Auth is a pluggable middleware interface in the Go HTTP layer, shipping with a no-op default implementation. A self-hoster who wants multi-user auth implements the interface (OIDC, reverse-proxy auth, whatever) without touching the reconciler or adapters.
- **Explicitly not shipped:** session storage, user management UI, password handling, RBAC. Auth is a seam, not a feature — resist scope creep here.

## 9. Frontend architecture

- Canvas state (Svelte Flow) is **optimistic**: a dropped node renders immediately in a `pending` state, a patch is sent to the backend, and the node's visual state is only confirmed by the backend's next `topology_status` push.
- Node visual states: `pending` (dashed outline) → `applying` (spinner) → `up` (solid) / `error` (red border + message).
- Two WebSocket message types over one connection:
  - `topology_status` — event-driven, pushed after each reconcile cycle (per-node and per-link state)
  - `traffic_stats` — polled ~1Hz, `{linkId, direction, bytesDelta}`, drives the moving-dot animation
  - Kept as distinct message types so a status re-render never blocks the traffic animation tick.
- **Terminal is a separate WebSocket entirely** (straight to a `podman exec` pty per node, opened on panel open, closed on panel close) — decoupled so a laggy shell session can't back up the topology/traffic stream.
- Node detail panel **locks all fields while that node's reconcile is in-flight** — no editing a value that's about to be overwritten by an in-progress apply.
- Field-level save (blur/enter), not a single big "Apply" button — matches the debounce-and-converge model with small, legible diffs.

## 10. UI design direction (from reference)

Reference: a clean, minimal 3D-design-tool layout — off-white background, rounded soft-shadow cards, segmented pill tab controls, floating pill toolbar, icon+label rows with hover-reveal actions, "+" section headers, generous whitespace.

**Layout mapping:**

| Reference element | Hivenet equivalent |
|---|---|
| Top floating pill toolbar (select, undo/redo, frame, zoom%, Export) | Same toolbar: select/pan, undo/redo, fit-to-view, zoom %, primary action button (e.g. "Apply" or "Export Topology") |
| Left "Scene" panel — layer list with icon + name, hover-reveal lock/visibility icons, Scene/Assets pill tabs | **Topology outline panel** — list of placed nodes (icon + name + status dot), hover reveals quick actions (terminal, delete). Pairs with the node **palette** (drag source) as a separate flyout, not the same panel. |
| Right panel — collaborator avatars + Share, Design/Animation pill tabs, "+" sectioned cards (Materials, Styles, Background, Camera) | **Node detail panel** — collaborator avatars area repurposed as a future multi-user placeholder (v1: just current user), Config/Terminal pill tabs, sectioned "+" cards per §7 (Interfaces, Routes, etc.) |
| Materials swatch grid (2×2 spheres) | Node-type picker in the palette — visual icon swatches for host/router/switch/edge instead of material spheres |
| Styles thumbnail grid | **v2 idea, not v1**: topology templates ("simple LAN," "router + 2 hosts") as one-click starting points |
| Background color field, camera Isometric/Perspective toggle | Canvas background/grid setting, not core to v1 — low priority |
| Bottom floating prompt bar ("+", Inspiration dropdown, model selector, mic, submit) | **Not adopted for v1** — Hivenet has no AI-prompt input; skip this pattern entirely rather than force-fitting it |

**Visual tokens to start from:** light neutral background (`#F8F7F7`-range), white rounded cards with soft shadow, one accent color per status (green = up, red = error, amber = pending, blue = active traffic), small dense sans-serif type, pill-shaped segmented controls for tab switches throughout.

## 11. Build order (for a one-shot attempt)

1. Go backend scaffold: HTTP server, `go:embed` static serving, WebSocket endpoints (stubbed)
2. Topology doc schema + in-memory store + version counter
3. Reconciler core: read-actual → diff → typed ops (no podman/netlink calls yet, log the op list)
4. Podman adapter: `CreateNode`/`DestroyNode` for the host node type only, prove the container lifecycle end to end
5. Netlink adapter: veth pair creation + netns move + symlink management, prove two host nodes can ping each other
6. Extend adapters to router/switch/edge node types
7. Traffic-stats polling + `traffic_stats` WebSocket push
8. Svelte frontend: canvas (Svelte Flow) + palette + topology outline panel, wired to `topology_status`
9. Node detail panel: common chrome, per-type config tabs, field-level save, lock-on-pending
10. Terminal tab: pty WebSocket + xterm.js
11. Visual polish pass against §10
