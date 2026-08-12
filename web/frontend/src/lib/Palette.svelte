<script>
  import { NODE_CATALOG } from './nodeCatalog.js';

  // Native HTML5 DnD (spec section 9's canvas is Svelte Flow; the drag
  // source is a plain flyout, not part of the flow pane itself). The
  // dataTransfer type name is our own -- Flow.ondrop reads it back.
  function onDragStart(event, type) {
    event.dataTransfer.setData('application/x-hivenet-node-type', type);
    event.dataTransfer.effectAllowed = 'move';
  }
</script>

<div class="hv-card hv-palette">
  <div class="hv-palette-title">Nodes</div>
  {#each NODE_CATALOG as entry (entry.type)}
    <div
      class="hv-palette-item"
      draggable="true"
      role="button"
      tabindex="0"
      ondragstart={(e) => onDragStart(e, entry.type)}
      title={entry.blurb}
    >
      <svg viewBox="0 0 24 24" width="16" height="16" fill="none" stroke="currentColor" stroke-width="1.6">
        <path d={entry.icon} />
      </svg>
      <span>{entry.label}</span>
    </div>
  {/each}
  <div class="hv-palette-hint">Drag onto the canvas</div>
</div>

<style>
  .hv-palette {
    padding: 10px;
    width: 190px;
  }
  .hv-palette-title {
    font-size: 11px;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
    color: var(--hv-text-dim);
    margin-bottom: 6px;
    padding: 0 4px;
  }
  .hv-palette-item {
    display: flex;
    align-items: center;
    gap: 8px;
    padding: 7px 8px;
    border-radius: 8px;
    cursor: grab;
    font-size: 12px;
  }
  .hv-palette-item:hover {
    background: var(--hv-bg);
  }
  .hv-palette-hint {
    font-size: 10px;
    color: var(--hv-text-dim);
    padding: 6px 4px 0;
  }
</style>
