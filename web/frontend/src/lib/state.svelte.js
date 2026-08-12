// Central client state: the authoritative topology doc, the live status
// overlay from the WebSocket, and the client-owned layout (positions).
// Svelte 5 forbids reassigning an *exported* $state binding directly
// (see https://svelte.dev/e/state_invalid_export) -- only its properties
// may be mutated. So all reactive state lives on one exported object,
// `store`, whose properties are freely reassigned by the functions below;
// every panel imports `store` and reads/writes through it instead of
// re-fetching or duplicating state via props.
import { getTopology, putTopology, getLayout, putLayout } from './api.js';
import { connectTopologySocket } from './ws.js';
import { catalogEntry } from './nodeCatalog.js';

class Store {
  // Mirrors internal/topology.Document: {topology:{nodes,links}, version}.
  topologyDoc = $state({ topology: { nodes: [], links: [] }, version: 0 });

  // Mirrors internal/reconcile.Status: {nodes:{id:EntityStatus}, links:{...}}.
  // Absent entries render as "pending" -- spec section 9: a dropped node is
  // optimistic and only confirmed by the backend's next topology_status push.
  statusMap = $state({ nodes: {}, links: {} });

  // Opaque client-owned blob from /api/layout: {[nodeId]: {x,y}}. Never
  // sent to the reconciler, never touched by it (spec section 4).
  layout = $state({});

  connection = $state({ status: 'connecting' }); // connecting | open | closed
}

export const store = new Store();

let socket;
let saveLayoutTimer;

function stateFor(map, id) {
  return map[id]?.state ?? 'pending';
}

export function nodeState(id) {
  return stateFor(store.statusMap.nodes, id);
}

export function linkState(id) {
  return stateFor(store.statusMap.links, id);
}

export function nodeMessage(id) {
  return store.statusMap.nodes[id]?.message ?? '';
}

/** Loads the current topology + layout once at startup and opens the socket. */
export async function init() {
  const [doc, savedLayout] = await Promise.all([getTopology(), getLayout()]);
  store.topologyDoc = doc ?? { topology: { nodes: [], links: [] }, version: 0 };
  store.layout = savedLayout ?? {};
  assignMissingPositions();

  socket = connectTopologySocket({
    onOpen: () => (store.connection = { status: 'open' }),
    onClose: () => (store.connection = { status: 'closed' }),
    onTopologyStatus: (status) => {
      store.statusMap = { nodes: status.nodes ?? {}, links: status.links ?? {} };
    },
    onTrafficStats: () => {
      // Traffic-driven canvas animation is deferred past step 8 (see changelog);
      // the socket already delivers these, nothing to wire up yet.
    },
  });
}

export function disconnect() {
  socket?.close();
}

/** Ensures every known node has a layout position, grid-placing new ones. */
function assignMissingPositions() {
  const cols = 4;
  const spacingX = 220;
  const spacingY = 140;
  let i = Object.keys(store.layout).length;
  for (const node of store.topologyDoc.topology.nodes ?? []) {
    if (store.layout[node.id]) continue;
    const col = i % cols;
    const row = Math.floor(i / cols);
    store.layout = { ...store.layout, [node.id]: { x: 80 + col * spacingX, y: 80 + row * spacingY } };
    i++;
  }
}

function genId(prefix) {
  const bytes = crypto.getRandomValues(new Uint8Array(4));
  const hex = Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
  return `${prefix}-${hex}`;
}

/**
 * Adds a node of `type` at canvas position {x,y} and persists it. The node
 * gets exactly one default interface ("eth0") so it has one connectable
 * handle on the canvas; adding more interfaces is node-detail-panel work
 * (build order step 9), not this one.
 */
export async function addNode(type, position) {
  const entry = catalogEntry(type);
  if (!entry) throw new Error(`unknown node type: ${type}`);

  const id = genId(type);
  const node = {
    id,
    type,
    image: entry.defaultImage,
    interfaces: [{ id: 'eth0', name: 'eth0' }],
  };

  const nextTopology = {
    nodes: [...store.topologyDoc.topology.nodes, node],
    links: store.topologyDoc.topology.links,
  };
  store.topologyDoc = await putTopology(nextTopology, store.topologyDoc.version);
  store.layout = { ...store.layout, [id]: position };
  persistLayout();
  return id;
}

/** Removes a node and any links touching it, then persists the topology. */
export async function deleteNode(id) {
  const nodes = store.topologyDoc.topology.nodes.filter((n) => n.id !== id);
  const links = store.topologyDoc.topology.links.filter(
    (l) => l.endpoints[0].nodeId !== id && l.endpoints[1].nodeId !== id,
  );
  store.topologyDoc = await putTopology({ nodes, links }, store.topologyDoc.version);
  const { [id]: _dropped, ...rest } = store.layout;
  store.layout = rest;
  persistLayout();
}

/** True if the given node/interface pair already has a link (at most one per interface). */
export function interfaceInUse(nodeId, ifaceId) {
  return store.topologyDoc.topology.links.some(
    (l) =>
      (l.endpoints[0].nodeId === nodeId && l.endpoints[0].ifaceId === ifaceId) ||
      (l.endpoints[1].nodeId === nodeId && l.endpoints[1].ifaceId === ifaceId),
  );
}

/** Connects two node/interface endpoints with a new link. */
export async function addLink(a, b) {
  if (interfaceInUse(a.nodeId, a.ifaceId) || interfaceInUse(b.nodeId, b.ifaceId)) {
    throw new Error('that interface already has a link');
  }
  const link = { id: genId('lk'), endpoints: [a, b] };
  const nextTopology = {
    nodes: store.topologyDoc.topology.nodes,
    links: [...store.topologyDoc.topology.links, link],
  };
  store.topologyDoc = await putTopology(nextTopology, store.topologyDoc.version);
}

export async function deleteLink(id) {
  const links = store.topologyDoc.topology.links.filter((l) => l.id !== id);
  store.topologyDoc = await putTopology(
    { nodes: store.topologyDoc.topology.nodes, links },
    store.topologyDoc.version,
  );
}

/** Updates one node's canvas position and debounces the /api/layout save. */
export function moveNode(id, position) {
  store.layout = { ...store.layout, [id]: position };
  persistLayout();
}

function persistLayout() {
  clearTimeout(saveLayoutTimer);
  saveLayoutTimer = setTimeout(() => putLayout(store.layout), 400);
}
