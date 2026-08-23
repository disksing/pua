<script lang="ts">
  import { onMount } from "svelte";

  import Icon from "../components/Icon.svelte";
  import Companion from "./Companion.svelte";
  import NewSessionDialog from "./NewSessionDialog.svelte";
  import SessionInspector from "./SessionInspector.svelte";
  import SessionInventory from "./SessionInventory.svelte";
  import SettingsDialog from "./SettingsDialog.svelte";
  import { api } from "./core/api";
  import { sessionsQuery } from "./core/archive";
  import type { AgentCatalog, AgentHubSession, SessionFilters, SessionListPage } from "./types";

  const PAGE_SIZE = 50;
  let sessions = $state<AgentHubSession[]>([]);
  let selected = $state<AgentHubSession | null>(null);
  let loading = $state(true);
  let error = $state("");
  let page = $state({ limit: PAGE_SIZE, nextCursor: "", hasMore: false });
  let cursors = $state<string[]>([""]);
  let pageIndex = $state(0);
  let filters = $state<SessionFilters>(filtersFromURL());
  let newSessionOpen = $state(false);
  let creating = $state(false);
  let createError = $state("");
  let settingsOpen = $state(false);
  let settingsRevision = $state(0);
  let catalog = $state<AgentCatalog>({ agents: [], providers: [], probes: [] });
  const beeperPage = window.location.pathname.replace(/\/+$/, "") === "/agenthub/beeper";

  onMount(() => {
    document.title = beeperPage ? "AgentHub Beeper" : "AgentHub";
    if (beeperPage) return;
    void Promise.all([loadSessions(), loadCatalog(), restoreSelectedRoute()]);
    const timer = window.setInterval(() => {
      if (document.visibilityState === "visible" && !settingsOpen && !newSessionOpen) void loadSessions(false);
    }, 15_000);
    const onPopState = () => { filters = filtersFromURL(); cursors = [""]; pageIndex = 0; void loadSessions(); void restoreSelectedRoute(); };
    window.addEventListener("popstate", onPopState);
    return () => { window.clearInterval(timer); window.removeEventListener("popstate", onPopState); };
  });

  async function loadSessions(showLoading = true): Promise<void> {
    if (showLoading) loading = true;
    error = "";
    try {
      const body = await api<SessionListPage>(sessionsQuery({
        archivedOnly: filters.archived,
        state: filters.archived ? [] : filters.states,
        sourceApp: filters.sourceApp.trim(),
        sourceInstanceId: filters.sourceInstanceId.trim(),
        sourceExternalId: filters.sourceExternalId.trim(),
        limit: PAGE_SIZE,
        cursor: cursors[pageIndex] || "",
      }));
      sessions = body.sessions || [];
      page = { limit: body.page?.limit || PAGE_SIZE, nextCursor: body.page?.nextCursor || "", hasMore: Boolean(body.page?.hasMore) };
      if (selected) selected = sessions.find((session) => session.id === selected?.id) || selected;
    } catch (reason) {
      error = message(reason);
    } finally {
      loading = false;
    }
  }

  async function loadCatalog(): Promise<void> {
    try {
      catalog = await api<AgentCatalog>("/v1/agents");
      catalog.agents = (catalog.agents || []).filter((agent) => agent.available !== false);
    } catch (reason) {
      error ||= message(reason);
    }
  }

  function applyFilters(): void {
    cursors = [""];
    pageIndex = 0;
    updateURL();
    void loadSessions();
  }

  function nextPage(): void {
    if (!page.nextCursor) return;
    cursors = [...cursors.slice(0, pageIndex + 1), page.nextCursor];
    pageIndex += 1;
    void loadSessions();
  }

  function previousPage(): void {
    if (!pageIndex) return;
    pageIndex -= 1;
    void loadSessions();
  }

  function openSession(session: AgentHubSession): void {
    selected = session;
    updateURL(session.id);
  }

  function closeSession(): void {
    selected = null;
    updateURL();
  }

  async function restoreSelectedRoute(): Promise<void> {
    const match = window.location.pathname.match(/^\/agenthub\/sessions\/([^/]+)\/?$/);
    if (!match) { selected = null; return; }
    const id = decodeURIComponent(match[1]);
    const listed = sessions.find((session) => session.id === id);
    if (listed) { selected = listed; return; }
    try {
      const body = await api<{ session: AgentHubSession }>(`/v1/sessions/${encodeURIComponent(id)}`);
      selected = body.session;
    } catch (reason) {
      error = message(reason);
      selected = null;
    }
  }

  function updateSession(session: AgentHubSession): void {
    selected = session;
    sessions = sessions.map((item) => item.id === session.id ? session : item);
  }

  function sessionArchived(id: string): void {
    sessions = sessions.filter((session) => session.id !== id);
    closeSession();
    void loadSessions();
  }

  async function createSession(payload: Record<string, string>): Promise<AgentHubSession | void> {
    if (creating) return;
    creating = true;
    createError = "";
    try {
      const body = await api<{ session: AgentHubSession }>("/v1/sessions", { method: "POST", body: JSON.stringify(payload) });
      newSessionOpen = false;
      filters.archived = false;
      filters.states = [];
      cursors = [""];
      pageIndex = 0;
      await loadSessions();
      openSession(body.session);
      return body.session;
    } catch (reason) {
      createError = message(reason);
    } finally {
      creating = false;
    }
  }

  function updateURL(sessionId = selected?.id || ""): void {
    const query = new URLSearchParams();
    if (filters.archived) query.set("scope", "archived");
    if (filters.states.length) query.set("state", filters.states.join(","));
    if (filters.sourceApp.trim()) query.set("sourceApp", filters.sourceApp.trim());
    if (filters.sourceInstanceId.trim()) query.set("sourceInstanceId", filters.sourceInstanceId.trim());
    if (filters.sourceExternalId.trim()) query.set("sourceExternalId", filters.sourceExternalId.trim());
    const path = sessionId ? `/agenthub/sessions/${encodeURIComponent(sessionId)}` : "/agenthub/";
    window.history.pushState({}, "", `${path}${query.size ? `?${query}` : ""}`);
  }

  function filtersFromURL(): SessionFilters {
    const query = new URLSearchParams(window.location.search);
    return {
      archived: query.get("scope") === "archived",
      states: (query.get("state") || "").split(",").filter(Boolean),
      sourceApp: query.get("sourceApp") || "",
      sourceInstanceId: query.get("sourceInstanceId") || "",
      sourceExternalId: query.get("sourceExternalId") || "",
    };
  }

  function message(reason: unknown): string { return reason instanceof Error ? reason.message : String(reason); }
</script>

{#if beeperPage}
  <main class="beeper-page">
    <Companion standalone pauseLiveUpdates={settingsOpen} revision={settingsRevision} onOpenSettings={() => settingsOpen = true} />
    {#if settingsOpen}<SettingsDialog onClose={() => settingsOpen = false} onSaved={() => settingsRevision += 1} />{/if}
  </main>
{:else}
  <main class="agenthub-shell">
    <header class="agenthub-topbar">
      <a class="agenthub-brand" href="/agenthub/" aria-label="AgentHub Session inventory"><span><Icon name="waypoints" /></span><strong>AgentHub</strong><em>Session service</em></a>
      <nav aria-label="AgentHub tools">
        <a class="topbar-link" href="/agenthub/beeper" target="_blank" rel="noreferrer"><Icon name="activity" /><span>Beeper</span></a>
        <button class="topbar-link" type="button" onclick={() => settingsOpen = true}><Icon name="settings" /><span>Settings</span></button>
        <button class="primary-button" type="button" onclick={() => { createError = ""; newSessionOpen = true; }}><Icon name="plus" /><span>New Session</span></button>
      </nav>
    </header>

    <SessionInventory bind:filters {sessions} {loading} {error} {pageIndex} hasMore={page.hasMore} onApplyFilters={applyFilters} onRefresh={() => loadSessions()} onSelect={openSession} onPrevious={previousPage} onNext={nextPage} />

    {#if selected}<SessionInspector bind:session={selected} onClose={closeSession} onChanged={updateSession} onArchived={sessionArchived} />{/if}
    {#if newSessionOpen}<NewSessionDialog agents={catalog.agents} providers={catalog.providers} defaultCwd={selected?.cwd || ""} submitting={creating} error={createError} onSubmit={createSession} onClose={() => { if (!creating) newSessionOpen = false; }} />{/if}
    <Companion pauseLiveUpdates={settingsOpen} revision={settingsRevision} onOpenSettings={() => settingsOpen = true} />
    {#if settingsOpen}<SettingsDialog onClose={() => settingsOpen = false} onSaved={() => { settingsRevision += 1; void loadCatalog(); }} />{/if}
  </main>
{/if}
