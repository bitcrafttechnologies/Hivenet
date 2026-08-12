<script>
  import { store, nodeState, nodeMessage, deleteNode } from './state.svelte.js';
  import { catalogEntry } from './nodeCatalog.js';

  let { onFocusNode } = $props();

  async function onDelete(event, id) {
    event.stopPropagation();
    try {
      await deleteNode(id);
    } catch (err) {
      console.error('failed to delete node', err);
    }
  }

  function onRowKeydown(event, id) {
    if (event.key === 'Enter' || event.key === ' ') {
      event.preventDefault();
      onFocusNode?.(id);
    }
  }
</script>

<div class="hv-card hv-outline">
  <div class="hv-outline-title">
    Topology
    <span class="hv-outline-count">{store.topologyDoc.topology.nodes.length} nodes</span>
  </div>
  {#if store.topologyDoc.topology.nodes.length === 0}
    <div class="hv-outline-empty">Drag a node from the palette to get started.</div>
  {/if}
  {#each store.topologyDoc.topology.nodes as node (node.id)}
    {@const entry = catalogEntry(node.type)}
    {@const state = nodeState(node.id)}
    <div
      class="hv-outline-row"
      role="button"
      tabindex="0"
      onclick={() => onFocusNode?.(node.id)}
      onkeydown={(e) => onRowKeydown(e, node.id)}
      title={nodeMessage(node.id)}
    >
      <svg viewBox="0 0 24 24" width="14" height="14" fill="none" stroke="currentColor" stroke-width="1.6">
        <path d={entry?.icon ?? ''} />
      </svg>
      <span class="hv-outline-id">{node.id}</span>
      <span class="hv-dot state-{state}"></span>
      <div class="hv-outline-actions">
        <button
          class="hv-outline-action"
          title="Terminal (build order step 10, not implemented yet)"
          disabled
        >&gt;_</button>
        <button
          class="hv-outline-action"
          title="Delete node"
          onclick={(e) => onDelete(e, node.id)}
        >&times;</button>
      </div>
    </div>
  {/each}
</div>

<style>
  .hv-outline {
    width: 220px;
    padding: 10px;
    max-height: 60vh;
    overflow-y: auto;
  }
  .hv-outline-title {
    font-size: 11px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--hv-text-dim);
    display: flex;
    justify-content: space-between;
    padding: 0 4px 6px;
  }
  .hv-outline-count {
    font-weight: 400;
    text-transform: none;
  }
  .hv-outline-empty {
    font-size: 11px;
    color: var(--hv-text-dim);
    padding: 6px 4px;
  }
  .hv-outline-row {
    display: flex;
    align-items: center;
    gap: 6px;
    padding: 6px 8px;
    border-radius: 8px;
    cursor: pointer;
    font-size: 12px;
  }
  .hv-outline-row:hover {
    background: var(--hv-bg);
  }
  .hv-outline-id {
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .hv-outline-actions {
    display: none;
    gap: 2px;
  }
  .hv-outline-row:hover .hv-outline-actions {
    display: flex;
  }
  .hv-outline-action {
    border: none;
    background: none;
    cursor: pointer;
    font-size: 11px;
    padding: 2px 4px;
    border-radius: 4px;
    color: var(--hv-text-dim);
  }
  .hv-outline-action:hover {
    background: var(--hv-border);
  }
  .hv-outline-action:disabled {
    cursor: not-allowed;
    opacity: 0.5;
  }
</style>
