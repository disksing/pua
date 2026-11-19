<script lang="ts">
  import Icon from "./Icon.svelte";

  let { label = "working..." }: { label?: string } = $props();
</script>

<div class="turn-working-indicator" role="status" aria-live="polite" data-timeline-key="turn-working">
  <Icon name="loader-circle" /><span>{label}</span>
</div>

<style>
  .turn-working-indicator {
    display: flex;
    align-items: center;
    gap: 7px;
    margin: var(--turn-working-margin, 10px 0);
    color: var(--muted-strong);
    font-size: 12px;
    font-weight: 600;
  }
  .turn-working-indicator :global(svg) {
    width: 14px;
    height: 14px;
    animation: spin 1s linear infinite;
  }
  @keyframes spin {
    to { transform: rotate(360deg); }
  }
  @media (prefers-reduced-motion: reduce) {
    .turn-working-indicator :global(svg) { animation: none; }
  }
</style>
