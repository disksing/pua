<script lang="ts">
  import { onMount } from "svelte";

  import Icon from "../components/Icon.svelte";
  import { api } from "./core/api";
  import { formatDateTime, sessionStatusLabel, sessionStatusTone } from "./core/display-session";
  import SessionTimeline from "./SessionTimeline.svelte";
  import type { AgentHubSession, AgentHubTurnSummary } from "./types";

  let {
    session = $bindable(),
    onClose,
    onChanged,
    onArchived,
  }: {
    session: AgentHubSession;
    onClose: () => void;
    onChanged: (session: AgentHubSession) => void;
    onArchived: (id: string) => void;
  } = $props();

  let tab = $state<"overview" | "conversation" | "turns">("overview");
  let pending = $state("");
  let error = $state("");
  let draft = $state("");
  let turns = $state<AgentHubTurnSummary[]>([]);
  let turnsLoading = $state(false);
  let copied = $state(false);

  onMount(() => {
    const onKeyDown = (event: KeyboardEvent) => { if (event.key === "Escape") onClose(); };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  });

  async function refresh(): Promise<void> {
    try {
      const body = await api<{ session: AgentHubSession }>(`/v1/sessions/${session.id}`);
      session = body.session;
      onChanged(body.session);
    } catch (reason) {
      error = message(reason);
    }
  }

  async function perform(action: "stop" | "interrupt" | "resume"): Promise<void> {
    if (pending) return;
    pending = action;
    error = "";
    try {
      const body = await api<{ session?: AgentHubSession }>(`/v1/sessions/${session.id}/${action}`, { method: "POST", body: "{}" });
      if (body.session) {
        session = body.session;
        onChanged(body.session);
      } else await refresh();
    } catch (reason) {
      error = message(reason);
    } finally {
      pending = "";
    }
  }

  async function archive(): Promise<void> {
    if (pending) return;
    pending = "archive";
    error = "";
    try {
      await api(`/v1/sessions/${session.id}`, { method: "DELETE", body: "{}" });
      onArchived(session.id);
    } catch (reason) {
      error = message(reason);
    } finally {
      pending = "";
    }
  }

  async function send(): Promise<void> {
    const text = draft.trim();
    if (!text || pending) return;
    pending = "message";
    error = "";
    try {
      const body = await api<{ session?: AgentHubSession }>(`/v1/sessions/${session.id}/messages`, {
        method: "POST",
        body: JSON.stringify({ text, steer: Boolean(session.currentTurnId) }),
      });
      draft = "";
      if (body.session) {
        session = body.session;
        onChanged(body.session);
      }
    } catch (reason) {
      error = message(reason);
    } finally {
      pending = "";
    }
  }

  async function loadTurns(): Promise<void> {
    if (turnsLoading) return;
    turnsLoading = true;
    error = "";
    try {
      const body = await api<{ turns: AgentHubTurnSummary[] }>(`/v1/sessions/${session.id}/turns?latest=true&limit=50`);
      turns = [...(body.turns || [])].reverse();
    } catch (reason) {
      error = message(reason);
    } finally {
      turnsLoading = false;
    }
  }

  function selectTab(next: typeof tab): void {
    tab = next;
    if (next === "turns" && !turns.length) void loadTurns();
  }

  async function copyID(): Promise<void> {
    await navigator.clipboard?.writeText(session.id);
    copied = true;
    window.setTimeout(() => copied = false, 1200);
  }

  function message(reason: unknown): string {
    return reason instanceof Error ? reason.message : String(reason);
  }

  const archived = $derived(session.state === "archived");
  const archivable = $derived(session.state === "stopped" && !session.currentTurnId && !(session.pendingApprovalIds || []).length);
</script>

<div class="inspector-backdrop" role="presentation" onclick={(event) => { if (event.target === event.currentTarget) onClose(); }}>
  <aside class="session-inspector" aria-label={`Session ${session.title || session.id}`}>
    <header class="inspector-header">
      <div class="inspector-title">
        <span class={`status-badge ${sessionStatusTone(session)}`}><i></i>{sessionStatusLabel(session)}</span>
        <h2>{session.title || "Untitled Session"}</h2>
        <code>{session.id}</code>
      </div>
      <div class="inspector-actions">
        {#if !archived && session.state !== "stopped"}
          <button type="button" class="secondary-button" disabled={Boolean(pending) || session.state === "stopping"} onclick={() => perform(session.currentTurnId ? "interrupt" : "stop")}><Icon name="square" /><span>{session.currentTurnId ? "Interrupt" : "Stop"}</span></button>
        {:else if session.state === "stopped"}
          <button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => perform("resume")}><Icon name="play" /><span>Resume</span></button>
        {/if}
        {#if !archived}<button type="button" class="icon-button" disabled={!archivable || Boolean(pending)} title={archivable ? "Archive Session" : "Stop the Session before archiving"} aria-label="Archive Session" onclick={archive}><Icon name="archive" /></button>{/if}
        <button type="button" class="icon-button" aria-label="Close Session" onclick={onClose}><Icon name="x" /></button>
      </div>
    </header>

    <nav class="inspector-tabs" aria-label="Session inspector sections">
      <button class:active={tab === "overview"} type="button" onclick={() => selectTab("overview")}>Overview</button>
      <button class:active={tab === "conversation"} type="button" onclick={() => selectTab("conversation")}>Conversation</button>
      <button class:active={tab === "turns"} type="button" onclick={() => selectTab("turns")}>Turns & Events</button>
    </nav>

    {#if error}<div class="inspector-error" role="alert"><Icon name="triangle-alert" /><span>{error}</span><button type="button" aria-label="Dismiss" onclick={() => error = ""}><Icon name="x" /></button></div>{/if}

    <div class="inspector-body">
      {#if tab === "overview"}
        <div class="overview-grid">
          <section><h3>Execution</h3><dl><div><dt>Agent</dt><dd>{session.agentName || "—"}</dd></div><div><dt>Provider</dt><dd>{session.provider || "—"}</dd></div><div><dt>Working directory</dt><dd><code>{session.cwd || "—"}</code></dd></div><div><dt>Provider Session ID</dt><dd><code>{session.providerSessionId || "—"}</code></dd></div></dl></section>
          <section><h3>Provenance</h3><dl><div><dt>Application</dt><dd>{session.source?.app || "Direct / unknown"}</dd></div><div><dt>Instance ID</dt><dd><code>{session.source?.instanceId || "—"}</code></dd></div><div><dt>External ID</dt><dd><code>{session.source?.externalId || "—"}</code></dd></div></dl>{#if session.source?.metadata && Object.keys(session.source.metadata).length}<div class="metadata-list">{#each Object.entries(session.source.metadata) as [key, value]}<code>{key}={value}</code>{/each}</div>{/if}</section>
          <section><h3>Timing</h3><dl><div><dt>Created</dt><dd>{formatDateTime(session.createdAt)}</dd></div><div><dt>Last activity</dt><dd>{formatDateTime(session.lastActivityAt)}</dd></div><div><dt>Updated</dt><dd>{formatDateTime(session.updatedAt)}</dd></div></dl></section>
          <section><h3>Identifiers</h3><div class="copy-id"><code>{session.id}</code><button type="button" class="icon-button" aria-label="Copy Session ID" onclick={copyID}><Icon name={copied ? "check" : "copy"} /></button></div><dl><div><dt>Current Turn</dt><dd><code>{session.currentTurnId || "—"}</code></dd></div><div><dt>Last event</dt><dd>{session.lastEventId || 0}</dd></div></dl></section>
        </div>
      {:else if tab === "conversation"}
        <div class="conversation-panel">
          {#key session.id}<SessionTimeline {session} onSessionChanged={refresh} />{/key}
          <div class="session-composer">
            {#if archived}<span>Archived Sessions are read-only.</span>
            {:else if session.state === "stopped"}<span>This Session is stopped. Resume it to continue the conversation.</span>
            {:else}<textarea bind:value={draft} aria-label="Message" placeholder={session.currentTurnId ? "Steer the active Turn…" : "Start a new Turn…"} onkeydown={(event) => { if (event.key === "Enter" && (event.metaKey || event.ctrlKey)) { event.preventDefault(); void send(); } }}></textarea><button type="button" class="primary-button send-message" disabled={!draft.trim() || Boolean(pending)} onclick={send}><Icon name="send" /><span>{pending === "message" ? "Sending…" : session.currentTurnId ? "Steer" : "Send"}</span></button>{/if}
          </div>
        </div>
      {:else}
        <div class="turns-panel">
          <header><div><h3>Recent Turns</h3><p>Compact, durable Turn summaries from AgentHub's existing materialized index.</p></div><button type="button" class="secondary-button" disabled={turnsLoading} onclick={loadTurns}><Icon name="refresh-cw" /><span>Refresh</span></button></header>
          {#if turnsLoading}<div class="timeline-loading"><span class="spinner"></span>Loading Turns…</div>{/if}
          {#each turns as turn (turn.turnId)}
            <article class="turn-row"><div><span class={`turn-status ${turn.status}`}>{turn.status}</span><strong>{turn.triggerPreview || "Turn"}</strong><code>{turn.turnId}</code></div><div class="turn-metrics"><span>{Math.round(turn.durationMs / 1000)}s</span><span>{turn.eventCount} events</span><span>{turn.toolEventCount} tool events</span><span>{formatDateTime(turn.startedAt)}</span></div>{#if turn.finalReplyPreview}<p>{turn.finalReplyPreview}</p>{/if}</article>
          {:else}
            {#if !turnsLoading}<div class="timeline-empty"><strong>No Turns recorded</strong><span>This Session has no materialized Turn history.</span></div>{/if}
          {/each}
        </div>
      {/if}
    </div>
  </aside>
</div>
