<script>
  import { Handle, Position } from '@xyflow/svelte';
  import { catalogEntry } from './nodeCatalog.js';

  let { data } = $props();

  const entry = $derived(catalogEntry(data.nodeType));
  const state = $derived(data.state ?? 'pending');
</script>

<div class="hv-node hv-card state-{state}" title={data.message || ''}>
  <div class="hv-node-head">
    <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6">
      <path d={entry?.icon ?? ''} />
    </svg>
    <span class="hv-node-label">{data.label}</span>
    <span class="hv-dot state-{state}"></span>
  </div>

  {#each data.interfaces ?? [] as iface, i (iface.id)}
    <Handle
      type="source"
      position={Position.Right}
      id={iface.id}
      style="top: {28 + i * 14}px"
    />
  {/each}
</div>

<style>
  .hv-node {
    padding: 8px 12px;
    min-width: 140px;
  }
  .hv-node-head {
    display: flex;
    align-items: center;
    gap: 6px;
  }
  .hv-node-label {
    font-weight: 600;
    font-size: 12px;
    flex: 1;
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .hv-node.state-pending {
    border-style: dashed;
    border-color: var(--hv-pending);
  }
  .hv-node.state-applying {
    border-color: var(--hv-traffic);
    box-shadow: 0 0 0 2px rgba(47, 111, 237, 0.15);
  }
  .hv-node.state-up {
    border-color: var(--hv-up);
  }
  .hv-node.state-error {
    border-color: var(--hv-error);
    border-width: 2px;
  }
</style>
