<script>
  import { onMount, onDestroy } from 'svelte';
  import {
    SvelteFlow,
    Background,
    BackgroundVariant,
    ConnectionMode,
    useSvelteFlow,
  } from '@xyflow/svelte';
  import '@xyflow/svelte/dist/style.css';

  import {
    store,
    init,
    disconnect,
    nodeState,
    nodeMessage,
    linkState,
    interfaceInUse,
    addNode,
    addLink,
    deleteNode,
    deleteLink,
    moveNode,
  } from './state.svelte.js';
  import CanvasNode from './CanvasNode.svelte';
  import Palette from './Palette.svelte';
  import OutlinePanel from './OutlinePanel.svelte';
  import Toolbar from './Toolbar.svelte';

  const nodeTypes = { hvNode: CanvasNode };
  const { screenToFlowPosition, setCenter } = useSvelteFlow();

  let nodes = $state.raw([]);
  let edges = $state.raw([]);

  // Canvas structure is always rebuilt from the authoritative topology doc
  // plus the client-owned layout and the live status overlay -- see
  // state.svelte.js. Position edits during an active drag are handled by
  // Svelte Flow's own bind: mutation; this effect only re-fires once the
  // drag is persisted (onnodedragstop -> moveNode -> layout changes), so
  // there's no fight over who owns the array mid-drag.
  $effect(() => {
    nodes = (store.topologyDoc.topology.nodes ?? []).map((n) => ({
      id: n.id,
      type: 'hvNode',
      position: store.layout[n.id] ?? { x: 0, y: 0 },
      data: {
        nodeType: n.type,
        label: n.id,
        interfaces: n.interfaces ?? [],
        state: nodeState(n.id),
        message: nodeMessage(n.id),
      },
    }));
  });

  $effect(() => {
    edges = (store.topologyDoc.topology.links ?? []).map((l) => ({
      id: l.id,
      source: l.endpoints[0].nodeId,
      sourceHandle: l.endpoints[0].ifaceId,
      target: l.endpoints[1].nodeId,
      targetHandle: l.endpoints[1].ifaceId,
      style: edgeStyle(linkState(l.id)),
    }));
  });

  function edgeStyle(state) {
    const colors = {
      up: 'var(--hv-up)',
      error: 'var(--hv-error)',
      applying: 'var(--hv-traffic)',
      pending: 'var(--hv-pending)',
    };
    const color = colors[state] ?? colors.pending;
    const dashed = state === 'pending' ? '4 3' : '0';
    return `stroke:${color};stroke-width:1.6;stroke-dasharray:${dashed}`;
  }

  onMount(() => {
    init();
  });
  onDestroy(() => {
    disconnect();
  });

  function onDragOver(event) {
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = 'move';
  }

  async function onDrop(event) {
    event.preventDefault();
    const type = event.dataTransfer?.getData('application/x-hivenet-node-type');
    if (!type) return;
    const position = screenToFlowPosition({ x: event.clientX, y: event.clientY });
    try {
      await addNode(type, position);
    } catch (err) {
      console.error('failed to add node', err);
    }
  }

  function isValidConnection(connection) {
    if (connection.source === connection.target) return false;
    if (interfaceInUse(connection.source, connection.sourceHandle)) return false;
    if (interfaceInUse(connection.target, connection.targetHandle)) return false;
    return true;
  }

  async function onConnect(connection) {
    try {
      await addLink(
        { nodeId: connection.source, ifaceId: connection.sourceHandle },
        { nodeId: connection.target, ifaceId: connection.targetHandle },
      );
    } catch (err) {
      console.error('failed to create link', err);
    }
  }

  async function onNodeDragStop(event) {
    const n = event?.targetNode;
    if (!n) return;
    moveNode(n.id, n.position);
  }

  async function onDeleteElements(event) {
    for (const n of event?.nodes ?? []) {
      try {
        await deleteNode(n.id);
      } catch (err) {
        console.error('failed to delete node', err);
      }
    }
    for (const e of event?.edges ?? []) {
      try {
        await deleteLink(e.id);
      } catch (err) {
        console.error('failed to delete link', err);
      }
    }
  }

  function focusNode(id) {
    const pos = store.layout[id];
    if (!pos) return;
    setCenter(pos.x, pos.y, { zoom: 1, duration: 300 });
  }
</script>

<div class="hv-shell">
  <div class="hv-overlay hv-overlay-top">
    <Toolbar />
  </div>
  <div class="hv-overlay hv-overlay-left">
    <OutlinePanel onFocusNode={focusNode} />
  </div>
  <div class="hv-overlay hv-overlay-right">
    <Palette />
  </div>
  <SvelteFlow
    bind:nodes
    bind:edges
    {nodeTypes}
    connectionMode={ConnectionMode.Loose}
    {isValidConnection}
    fitView
    ondragover={onDragOver}
    ondrop={onDrop}
    onconnect={onConnect}
    onnodedragstop={onNodeDragStop}
    ondelete={onDeleteElements}
  >
    <Background variant={BackgroundVariant.Dots} gap={16} size={1} />
  </SvelteFlow>
</div>

<style>
  .hv-shell {
    position: relative;
    width: 100vw;
    height: 100vh;
  }
  .hv-overlay {
    position: absolute;
    z-index: 5;
  }
  .hv-overlay-top {
    top: 14px;
    left: 50%;
    transform: translateX(-50%);
  }
  .hv-overlay-left {
    top: 70px;
    left: 14px;
  }
  .hv-overlay-right {
    top: 70px;
    right: 14px;
  }
</style>
