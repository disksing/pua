<script lang="ts">
  import "./EventTimeline.css";
  import { onDestroy, tick, untrack, type Snippet } from "svelte";
  import { ApiClient } from "../api/client";
  import { ChatSessionController } from "./chat-state";
  import { effectiveGenerationStatus } from "./generation-status";
  import GenerationMarker from "./GenerationMarker.svelte";
  import Icon from "./Icon.svelte";
  import TimelineNotice from "./TimelineNotice.svelte";
  import TimelineSourceButton from "./TimelineSourceButton.svelte";
  import TurnEventDetails from "./TurnEventDetails.svelte";
  import { projectConversationEvents } from "./timeline-events";
  import { historyItem, publicMessageIndex, queueSource, timelineRows, turnSource, type SharedChatMessage } from "./shared-chat-timeline";
  import type { ConversationBlock, DetailPanelModel, FileTreeModel, ResourceMessageStatus, TimelineItem } from "./models";

  let { model, visible, status, messages, bubble, empty, onOpenFile, onInspect }: {
    model: DetailPanelModel;
    visible: boolean;
    status: ResourceMessageStatus | null;
    messages: SharedChatMessage[];
    bubble: Snippet<[SharedChatMessage]>;
    empty: Snippet;
    onOpenFile: (path: string) => void;
    onInspect: () => void;
  } = $props();

  const base = $derived(`/api/workspaces/${encodeURIComponent(model.workspaceId)}/resources/${encodeURIComponent(model.resourceId)}`);
  const api = new ApiClient((input, init) => {
    const headers = new Headers(init?.headers);
    headers.set("X-PUA-User", model.currentUserName);
    return fetch(input, { ...init, headers });
  });
  const controller = new ChatSessionController({
    api, collapseNonUserTurns: false, trackReadState: true, syncStatus: false, keepHistoryOnGenerationChange: true,
  });
  let snapshot = $state.raw(controller.snapshot());
  const unsubscribe = controller.subscribe(next => { if (next.identity) snapshot = next; });
  const rows = $derived(timelineRows(messages, snapshot.blocks));
  const publicMessages = $derived(publicMessageIndex(messages));
  const historyIDs = $derived(new Set(snapshot.blocks.map(block => block.turn?.triggerMessageId).filter(Boolean)));
  const queue = $derived((status?.waitingMessages || []).filter(message => message.messageId && !historyIDs.has(message.messageId)));
  const legacy = $derived(findLegacy(model.detail?.artifacts || []));
  const uid = $props.id();
  let expanded = $state(new Set<string>());
  let root: HTMLDivElement;
  let reading = $state(false);
  let readThrough = $state(0);
  let readError = $state("");
  const unread = $derived((snapshot.completedTurnNumber || 0) > readThrough ? model.executionUnreadCount || 0 : 0);
  const canRead = $derived(snapshot.loaded && !snapshot.error && !snapshot.blocks.some(block => block.kind === "gap"));

  $effect(() => {
    const available = visible && Boolean(model.currentUserName);
    const next = status;
    untrack(() => {
      if (!available) {
        controller.activate("", "", null);
        return;
      }
      const wasActive = Boolean(controller.snapshot().identity);
      controller.activate(model.workspaceId, model.resourceId, next);
      if (!wasActive && snapshot.loaded && !model.detail?.archived) void controller.refresh();
    });
  });

  $effect.pre(() => {
    const pending = queue;
    if (!visible || !root) return;
    const focused = document.activeElement?.closest<HTMLElement>("[data-queued-input]");
    const id = focused?.dataset.queuedInput;
    if (!id || pending.some(message => message.messageId === id)) return;
    void tick().then(() => {
      if (!root?.isConnected || document.activeElement !== document.body) return;
      const delivered = root.querySelector<HTMLButtonElement>(`[data-history-input="${CSS.escape(id)}"] button`);
      (delivered || root.closest<HTMLElement>(".persistent-chat"))?.focus({ preventScroll: true });
    });
  });

  onDestroy(() => {
    unsubscribe();
    controller.dispose();
  });

  function findLegacy(entries: FileTreeModel[]): string {
    for (const entry of entries) {
      if (entry.type === "file" && entry.name === "legacy-log.md") return entry.path;
      const path = findLegacy(entry.children || []);
      if (path) return path;
    }
    return "";
  }

  async function loadOlder() {
    onInspect();
    const scroll = root.closest<HTMLElement>("#chatTimeline");
    if (!scroll) {
      await controller.loadOlder();
      return;
    }
    const top = scroll.getBoundingClientRect().top;
    const anchor = [...root.querySelectorAll<HTMLElement>("[data-chat-row]")]
      .find(element => element.getBoundingClientRect().bottom > top);
    const offset = anchor?.getBoundingClientRect().top;
    await controller.loadOlder();
    await tick();
    if (anchor?.isConnected && offset !== undefined) scroll.scrollTop += anchor.getBoundingClientRect().top - offset;
  }

  function toggle(block: ConversationBlock, button: HTMLButtonElement) {
    onInspect();
    const next = new Set(expanded);
    if (expanded.has(block.key)) {
      next.delete(block.key);
      if (block.turn) controller.collapseTurn(block.turn.reference);
    } else {
      next.add(block.key);
      if (block.turn) void controller.expandTurn(block.turn.reference);
    }
    expanded = next;
    button.focus({ preventScroll: true });
  }

  async function collapse(block: ConversationBlock, id: string) {
    const button = root.querySelector<HTMLButtonElement>(`[aria-controls="${CSS.escape(id)}"]`);
    if (!button) return;
    toggle(block, button);
    await tick();
    button.scrollIntoView({ block: "nearest" });
  }

  function expandCompact(item: TimelineItem) {
    if (!item.compact || !item.generationId || !item.rangeStartEventId || !item.rangeEndEventId) return;
    return controller.expandRange(item.generationId, item.rangeStartEventId, item.rangeEndEventId);
  }

  async function markRead() {
    const through = snapshot.completedTurnNumber || 0;
    if (!canRead || reading || !visible || document.visibilityState === "hidden" || through <= readThrough) return;
    reading = true;
    readError = "";
    try {
      await api.latest(`${base}/read`, {
        scope: "read", method: "PUT", headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ throughTurnNumber: through }),
      });
      readThrough = through;
      model.onSessionRead?.();
    } catch (error) {
      readError = error instanceof Error ? error.message : String(error);
    } finally {
      reading = false;
    }
  }
</script>

<div bind:this={root} class="shared-chat-stream public-messages" data-component-owner="event-timeline">
  {#if snapshot.hasMoreBefore || legacy}
    <div class="history-actions">
      {#if snapshot.hasMoreBefore}
        <button type="button" class="ghost-button" disabled={snapshot.loadingOlder} onclick={loadOlder}>
          <Icon name="chevrons-up" />{snapshot.loadingOlder ? "Loading older Turns…" : "Load older Turns"}
        </button>
      {/if}
      {#if legacy}<button type="button" class="ghost-button" onclick={() => onOpenFile(legacy)}><Icon name="archive-restore" />Open legacy history</button>{/if}
    </div>
  {/if}
  {#each rows as row (row.key)}
    {#if row.kind === "generation"}
      <GenerationMarker generation={row.generation} status={effectiveGenerationStatus(row, status)} />
    {:else if row.kind === "message"}
      <div class="chat-bubble-row" data-chat-row>{@render bubble(row.message)}</div>
    {:else if row.block.kind === "gap"}
      {@const starting = row.block.gap?.code === "session_starting"}
      <div class:conversation-session-starting={starting} class:conversation-gap={!starting} class="history-wide" data-timeline-key={row.key} data-chat-row>
        <Icon name={starting ? "loader-circle" : "triangle-alert"} />
        <span><strong>{starting ? "Starting agent…" : "History unavailable"}</strong><small>{row.block.gap?.message || "This generation could not be read."}</small></span>
        {#if row.block.gap?.retryable}<button type="button" class="secondary-button" onclick={() => controller.retryHistory()}>Retry</button>{/if}
      </div>
    {:else}
      {@const block = row.block}
      {@const detailsId = `${uid}-${block.key}`}
      {@const source = turnSource(block, row.input)}
      {@const open = expanded.has(row.key)}
      <div class="history-chip" data-timeline-key={row.key} data-history-input={block.turn?.triggerMessageId} data-chat-row>
        <TimelineSourceButton {source} {onInspect} status={block.turn?.closed ? undefined : block.turn?.status} expanded={open} controls={open ? detailsId : undefined} onToggle={(button) => toggle(block, button)} />
      </div>
      {#if open}
        <section class="expanded-history history-wide" id={detailsId} aria-label={`${source.label} Turn details`}>
          <header>
            <strong>{source.sender || source.label}</strong>
            {#if block.turn}
              <small>{[block.turn.status, `${block.turn.eventCount} events`, new Date(block.turn.startedAt).toLocaleString()].filter(Boolean).join(" · ")}</small>
            {/if}
          </header>
          {#if !block.items && !block.events && !block.error}
            {#if source.text && !row.input}<p class="trigger-preview">{source.text}</p>{/if}
            <div class="turn-loading" role="status"><Icon name="loader-circle" />Loading Turn details</div>
          {/if}
          {#if block.error}
            <TimelineNotice title="Turn unavailable" text={block.error} error />
            {#if block.turn && !block.items && !block.events}<button type="button" class="secondary-button" onclick={() => controller.loadTurn(block.turn?.reference || "")}>Retry</button>{/if}
          {/if}
          <TurnEventDetails {block} project={projectConversationEvents}
            presentItem={(item) => historyItem(item, block.turn?.turnId || "", publicMessages)}
            workspaceId={model.workspaceId} resolveResourceTitle={model.resolveResourceTitle}
            onNavigate={model.onNavigate} {onOpenFile} onExpand={expandCompact} readOnly
            contextIdentity={snapshot.identity} onApproval={async () => {}} onToast={model.onToast} />
          <button type="button" class="ghost-button collapse-history" onclick={() => collapse(block, detailsId)}><Icon name="chevron-up" />Collapse turn</button>
        </section>
      {/if}
    {/if}
  {/each}
  {#if !rows.length}{@render empty()}{/if}
  {#each snapshot.notices as notice, index (`${index}:${String(notice.data?.text || "")}`)}
    <div class="history-wide"><TimelineNotice title="PUA" text={String(notice.data?.text || "")} error={notice.data?.level === "error"} onDismiss={() => controller.dismissNotice(notice)} /></div>
  {/each}
  {#if snapshot.error}
    <div class="history-wide history-actions" role="status">
      <span>Execution history unavailable. {snapshot.error}</span>
      <button type="button" class="ghost-button" onclick={() => controller.retryHistory()}>Retry history</button>
    </div>
  {/if}
  {#if unread}
    <div class="history-actions unread-execution">
      <button type="button" class="ghost-button" disabled={!canRead || reading} onclick={markRead} title="Mark execution Turns as read">
        <Icon name="check-check" />{unread} unread execution {unread === 1 ? "Turn" : "Turns"}
      </button>
      {#if readError}<small role="alert">{readError}</small>{/if}
    </div>
  {/if}
  {#if queue.length}
    <div class="queued-inputs" role="group" aria-label="Waiting messages">
      <span>Waiting · {queue.length}</span>
      {#each queue as message (message.messageId)}
        <div class="history-chip" data-queued-input={message.messageId}><TimelineSourceButton source={queueSource(message)} {onInspect} /></div>
      {/each}
    </div>
  {/if}
</div>

<style>
  .shared-chat-stream { display: flex; flex: 1; flex-wrap: wrap; align-content: start; align-items: start; min-width: 0; gap: 8px 6px; }
  .chat-bubble-row { display: flex; flex-direction: column; flex: 0 0 100%; min-width: 0; }
  .history-chip { display: flex; }
  .history-wide, .history-actions, .queued-inputs { flex: 0 0 100%; min-width: 0; box-sizing: border-box; }
  .history-actions, .queued-inputs { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; color: var(--muted-strong); }
  .history-actions button, .collapse-history { display: inline-flex; align-items: center; gap: 5px; }
  .history-actions button :global(svg), .collapse-history :global(svg) { width: 14px; height: 14px; }
  .queued-inputs { border-top: 1px dashed var(--line); padding-top: 7px; }
  .queued-inputs > span { margin-right: 3px; font-size: 12px; }
  .unread-execution button { color: var(--blue-deep); }
  .expanded-history {
    padding: 10px min(3%, 14px);
    border: 1px solid var(--line);
    border-radius: 8px;
    background: var(--panel);
    overflow-wrap: anywhere;
  }
  .expanded-history > header { display: flex; flex-wrap: wrap; align-items: baseline; gap: 4px 10px; padding-bottom: 5px; }
  .expanded-history > header small { color: var(--muted-strong); }
  .trigger-preview { margin: 0; white-space: pre-wrap; }
  .collapse-history { margin-top: 6px; }
  @media (pointer: coarse) {
    .history-actions button, .collapse-history { min-height: 44px; }
  }
</style>
