<script lang="ts">
  import Icon from "../components/Icon.svelte";
  import { formatDateTime, relativeTime, sessionStatusLabel, sessionStatusTone } from "./core/display-session";
  import type { AgentHubSession, SessionFilters } from "./types";

  let {
    sessions,
    loading,
    error,
    filters = $bindable(),
    pageIndex,
    hasMore,
    onApplyFilters,
    onRefresh,
    onSelect,
    onPrevious,
    onNext,
  }: {
    sessions: AgentHubSession[];
    loading: boolean;
    error: string;
    filters: SessionFilters;
    pageIndex: number;
    hasMore: boolean;
    onApplyFilters: () => void;
    onRefresh: () => void;
    onSelect: (session: AgentHubSession) => void;
    onPrevious: () => void;
    onNext: () => void;
  } = $props();

  const states = ["starting", "ready", "running", "waiting_approval", "stopping", "stopped"];
  let sourceOpen = $state(false);

  function toggleState(value: string): void {
    filters.states = filters.states.includes(value)
      ? filters.states.filter((state) => state !== value)
      : [...filters.states, value];
  }

  function sourceSummary(session: AgentHubSession): string {
    return session.source?.app || "Direct / unknown";
  }

  function sourceDetail(session: AgentHubSession): string {
    return session.source?.externalId || session.source?.instanceId || "No source metadata";
  }
</script>

<section class="inventory" aria-labelledby="session-inventory-title">
  <header class="inventory-header">
    <div>
      <span class="eyebrow">Session inventory</span>
      <h1 id="session-inventory-title">Agent activity and history</h1>
      <p>Inspect Session state, provenance, agents, and recent activity. Open a row when intervention or conversation is needed.</p>
    </div>
    <button class="secondary-button refresh-button" type="button" disabled={loading} onclick={onRefresh}>
      <Icon name="refresh-cw" /><span>{loading ? "Refreshing…" : "Refresh"}</span>
    </button>
  </header>

  <form class="inventory-filters" onsubmit={(event) => { event.preventDefault(); onApplyFilters(); }}>
    <div class="scope-switch" aria-label="Session scope">
      <button type="button" class:active={!filters.archived} onclick={() => { filters.archived = false; onApplyFilters(); }}>Current</button>
      <button type="button" class:active={filters.archived} onclick={() => { filters.archived = true; onApplyFilters(); }}>Archived</button>
    </div>
    {#if !filters.archived}
      <details class="filter-menu">
        <summary><Icon name="list-filter" /><span>State</span>{#if filters.states.length}<strong>{filters.states.length}</strong>{/if}</summary>
        <div class="filter-popover">
          {#each states as state}
            <label><input type="checkbox" checked={filters.states.includes(state)} onchange={() => toggleState(state)} /><span>{state.replaceAll("_", " ")}</span></label>
          {/each}
          <button type="submit" class="primary-button">Apply states</button>
        </div>
      </details>
    {/if}
    <button class="secondary-button source-filter-toggle" type="button" aria-expanded={sourceOpen} onclick={() => sourceOpen = !sourceOpen}>
      <Icon name="box" /><span>Source</span>{#if filters.sourceApp || filters.sourceInstanceId || filters.sourceExternalId}<strong>Set</strong>{/if}
    </button>
    <span class="filter-note">Exact filters from the current AgentHub API</span>
    {#if sourceOpen}
      <div class="source-filter-fields">
        <label><span>Application</span><input bind:value={filters.sourceApp} placeholder="pua" /></label>
        <label><span>Instance ID</span><input bind:value={filters.sourceInstanceId} placeholder="optional" /></label>
        <label><span>External ID</span><input bind:value={filters.sourceExternalId} placeholder="optional" /></label>
        <button class="primary-button" type="submit">Apply source</button>
        <button class="secondary-button" type="button" onclick={() => { filters.sourceApp = ""; filters.sourceInstanceId = ""; filters.sourceExternalId = ""; onApplyFilters(); }}>Clear</button>
      </div>
    {/if}
  </form>

  {#if error}<div class="inventory-error" role="alert"><Icon name="triangle-alert" /><span>{error}</span><button type="button" onclick={onRefresh}>Retry</button></div>{/if}

  <div class="session-table-wrap" aria-busy={loading}>
    <table class="session-table">
      <thead><tr><th>Status</th><th>Session</th><th>Source</th><th>Agent</th><th>Last activity</th><th>Created</th><th><span class="sr-only">Open</span></th></tr></thead>
      <tbody>
        {#each sessions as session (session.id)}
          <tr tabindex="0" onclick={() => onSelect(session)} onkeydown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); onSelect(session); } }}>
            <td><span class={`status-badge ${sessionStatusTone(session)}`}><i></i>{sessionStatusLabel(session)}</span></td>
            <td><strong class="session-title">{session.title || "Untitled Session"}</strong><code>{session.id}</code></td>
            <td><strong>{sourceSummary(session)}</strong><small>{sourceDetail(session)}</small></td>
            <td><strong>{session.agentName || "No agent"}</strong><small>{session.provider || "Provider unknown"}</small></td>
            <td><strong>{relativeTime(session.lastActivityAt || session.updatedAt)}</strong><small>{formatDateTime(session.lastActivityAt || session.updatedAt)}</small></td>
            <td><span>{formatDateTime(session.createdAt)}</span></td>
            <td><button type="button" class="row-open" aria-label={`Open ${session.title || session.id}`} onclick={(event) => { event.stopPropagation(); onSelect(session); }}><Icon name="chevron-right" /></button></td>
          </tr>
        {:else}
          <tr class="empty-row"><td colspan="7"><Icon name="inbox" /><strong>No Sessions match this view</strong><span>{filters.archived ? "Archived Sessions will appear here." : "Adjust the existing state or source filters, or create a Session."}</span></td></tr>
        {/each}
      </tbody>
    </table>
    <div class="session-cards">
      {#each sessions as session (session.id)}
        <button type="button" class="session-card" onclick={() => onSelect(session)}>
          <span class={`status-badge ${sessionStatusTone(session)}`}><i></i>{sessionStatusLabel(session)}</span>
          <strong>{session.title || "Untitled Session"}</strong>
          <code>{session.id}</code>
          <span class="card-grid"><span><small>Source</small>{sourceSummary(session)}</span><span><small>Agent</small>{session.agentName || "No agent"}</span><span><small>Activity</small>{relativeTime(session.lastActivityAt || session.updatedAt)}</span><Icon name="chevron-right" /></span>
        </button>
      {/each}
    </div>
  </div>

  <footer class="inventory-pagination">
    <span>Page {pageIndex + 1} · {sessions.length} Sessions shown</span>
    <div><button type="button" class="secondary-button" disabled={pageIndex === 0 || loading} onclick={onPrevious}><Icon name="chevron-left" /><span>Previous</span></button><button type="button" class="secondary-button" disabled={!hasMore || loading} onclick={onNext}><span>Next</span><Icon name="chevron-right" /></button></div>
  </footer>
</section>
