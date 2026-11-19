<script lang="ts">
  import type { ResourceHistoryGeneration } from "./models";

  let { generation, status = generation.status }: { generation: ResourceHistoryGeneration; status?: string } = $props();
  const agent = $derived(generation.agentName || generation.resolvedProfile || generation.binding?.name || "Unknown agent");
</script>

<div class="history-generation" data-generation-id={generation.generationId}>
  <span class="generation-label">Generation {generation.generation}</span>
  <strong>{agent}</strong>
  <span class="generation-meta">
    <span>{generation.provider || generation.providerId || "Unknown provider"}</span>
    <span>{generation.model || "Unknown model"}</span>
    <span class="generation-status" data-generation-status={status}>{status || "unknown"}</span>
  </span>
</div>

<style>
  .history-generation {
    align-items: baseline;
    box-sizing: border-box;
    flex: 0 0 100%;
    background: var(--bg);
    border: 1px solid var(--line-soft);
    border-radius: 6px;
    display: flex;
    flex-wrap: wrap;
    gap: 4px 14px;
    margin: 8px 0 2px;
    padding: 6px 10px;
    overflow-wrap: anywhere;
  }
  .generation-label {
    color: var(--faint);
    font-size: 11px;
    font-weight: 700;
    letter-spacing: .06em;
    text-transform: uppercase;
  }
  strong { font-size: 13px; }
  .generation-meta {
    display: flex;
    flex-wrap: wrap;
    gap: 3px 10px;
    margin-left: auto;
    color: var(--muted-strong);
    font-size: 11px;
  }
  .generation-status { font-weight: 700; }
  .generation-status::before {
    display: inline-block;
    width: 6px;
    height: 6px;
    margin-right: 4px;
    border-radius: 50%;
    background: currentColor;
    content: "";
    vertical-align: 1px;
  }
  [data-generation-status="completed"] { color: var(--green-deep); }
  [data-generation-status="running"], [data-generation-status="starting"] { color: var(--blue-deep); }
  [data-generation-status="waiting_approval"] { color: var(--amber-deep); }
  [data-generation-status="failed"] { color: var(--red-deep); }
</style>
