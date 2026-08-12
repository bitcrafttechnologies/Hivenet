<script>
  import { useSvelteFlow } from '@xyflow/svelte';
  import { store } from './state.svelte.js';
  import { triggerReconcile } from './api.js';

  const { fitView, zoomIn, zoomOut } = useSvelteFlow();

  function exportTopology() {
    const blob = new Blob([JSON.stringify(store.topologyDoc.topology, null, 2)], {
      type: 'application/json',
    });
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = 'topology.json';
    a.click();
    URL.revokeObjectURL(url);
  }
</script>

<div class="hv-card hv-toolbar">
  <span class="hv-toolbar-brand">Hivenet</span>
  <span class="hv-toolbar-sep"></span>
  <button class="hv-toolbar-btn" onclick={() => zoomOut()} title="Zoom out">-</button>
  <button class="hv-toolbar-btn" onclick={() => zoomIn()} title="Zoom in">+</button>
  <button class="hv-toolbar-btn" onclick={() => fitView()} title="Fit to view">Fit</button>
  <span class="hv-toolbar-sep"></span>
  <button class="hv-toolbar-btn" onclick={() => triggerReconcile()} title="Reconcile now">
    Reconcile
  </button>
  <button class="hv-toolbar-btn hv-toolbar-primary" onclick={exportTopology} title="Download topology.json">
    Export Topology
  </button>
  <span class="hv-toolbar-sep"></span>
  <span class="hv-conn hv-pill" class:open={store.connection.status === 'open'}>
    <span class="hv-dot state-{store.connection.status === 'open' ? 'up' : 'error'}"></span>
    {store.connection.status === 'open' ? 'connected' : store.connection.status}
  </span>
</div>

<style>
  .hv-toolbar {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 10px;
    border-radius: 999px;
  }
  .hv-toolbar-brand {
    font-weight: 700;
    font-size: 13px;
    padding: 0 6px;
  }
  .hv-toolbar-sep {
    width: 1px;
    height: 18px;
    background: var(--hv-border);
    margin: 0 2px;
  }
  .hv-toolbar-btn {
    border: none;
    background: none;
    padding: 6px 10px;
    border-radius: 999px;
    cursor: pointer;
    font-size: 12px;
    color: var(--hv-text);
  }
  .hv-toolbar-btn:hover {
    background: var(--hv-bg);
  }
  .hv-toolbar-primary {
    background: var(--hv-text);
    color: white;
  }
  .hv-toolbar-primary:hover {
    opacity: 0.85;
    background: var(--hv-text);
  }
  .hv-conn {
    display: flex;
    align-items: center;
    gap: 6px;
    font-size: 11px;
    color: var(--hv-text-dim);
    padding: 4px 10px;
  }
</style>
