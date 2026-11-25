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
  import { captureChatAnchor, historyItem, queueSource, timelineSections, turnSource, turnWorkingLabel, type HistoryRow, type SharedChatMessage } from "./shared-chat-timeline";
  import type { ConversationBlock, DetailPanelModel, FileTreeModel, ResourceMessageStatus, TimelineItem } from "./models";

  let { model, visible, status, messages, queuedMessages, followLatestTurn, execution, followTail, bubble, empty, onOpenFile, onInspect }: {
    model: DetailPanelModel;
    visible: boolean;
    status: ResourceMessageStatus | null;
    messages: SharedChatMessage[];
    queuedMessages?: SharedChatMessage[];
    followLatestTurn: boolean;
    execution: Snippet<[Snippet | undefined, string, boolean]>;
    followTail: boolean;
    bubble: Snippet<[SharedChatMessage]>;
    empty: Snippet;
    onOpenFile: (path: string) => void;
    onInspect: () => void;
  } = $props();

  const earliestMessage = $derived(messages.reduce((time, message) =>
    Math.min(time, Date.parse(message.timelineAt || message.createdAt) || Infinity), Infinity));
  const api = new ApiClient((input, init) => {
    const headers = new Headers(init?.headers);
    headers.set("X-PUA-User", model.currentUserName);
    return fetch(input, { ...init, headers });
  });
  const controller = new ChatSessionController({
    api, collapseNonUserTurns: false, refreshHistoryOnStatusChange: true, syncStatus: false, keepHistoryOnGenerationChange: true,
    generationHeadersSince: () => Number.isFinite(earliestMessage) ? new Date(earliestMessage).toISOString() : "previous",
  });
  let snapshot = $state.raw(controller.snapshot());
  const unsubscribe = controller.subscribe(next => { if (next.identity) snapshot = next; });
  const historyIDs = $derived(new Set(snapshot.blocks.map(block => block.turn?.triggerMessageId).filter(Boolean)));
  const queue = $derived((status?.waitingMessages || []).filter(message => message.messageId && !historyIDs.has(message.messageId)));
  const pendingBubbles = $derived.by(() => {
    if (queuedMessages) {
      const placed = new Set(messages.filter(message => message.timelineAt).map(message => message.id));
      return queuedMessages.filter(message => !placed.has(message.id) && !message.inputMessageIds?.some(id => historyIDs.has(id)));
    }
    const byInput = new Map(messages.flatMap(message => message.timelineAt ? [] : (message.inputMessageIds || []).map(id => [id, message] as const)));
    return [...new Set(queue.flatMap(message => byInput.get(message.messageId) || []))];
  });
  const pendingBubbleIds = $derived(new Set(pendingBubbles.map(message => message.id)));
  const placedMessages = $derived(messages.filter(message => !pendingBubbleIds.has(message.id)));
  const sections = $derived(timelineSections(placedMessages, snapshot.blocks, snapshot.generationHeaders));
  const activeKey = $derived(status?.session?.currentTurnId && status.generation?.generationId
    && ["running", "waiting_approval"].includes(status.session.state || status.sessionState || "")
    ? `${status.generation.generationId}:${status.session.currentTurnId}` : "");
  const statusRow = $derived(sections.flatMap(section => section.rows).find((row): row is HistoryRow =>
    row.kind === "history" && row.key === activeKey));
  const activeRow = $derived(statusRow?.turn.closed ? undefined : statusRow);
  const legacy = $derived(findLegacy(model.detail?.artifacts || []));
  const uid = $props.id();
  let expanded = $state(new Set<string>());
  let autoExpanded = $state.raw<{ key: string; reference: string } | null>(null);
  let lastFollowedKey = "";
  const latestTurn = $derived(snapshot.blocks.filter(block => block.turn).at(-1));
  let pinnedHeading = $state("");
  let pinnedHeight = $state(0);
  let root: HTMLDivElement;
  let previousPending = new Set<string>();
  const firstHeading = $derived(sections.find(section => section.generation)?.generation);

  let requestedMessageTime = Infinity;
  $effect(() => {
    if (!visible || !snapshot.loaded || !snapshot.hasMoreBefore || earliestMessage >= requestedMessageTime) return;
    if (!firstHeading || firstHeading.generation <= 1 || earliestMessage >= Date.parse(firstHeading.createdAt)) return;
    requestedMessageTime = earliestMessage;
    untrack(() => void controller.refresh());
  });

  $effect(() => {
    const available = visible && Boolean(model.currentUserName);
    const next = status;
    untrack(() => {
      if (!available) {
        lastFollowedKey = "";
        collapseAutoExpanded();
        controller.activate("", "", null);
        return;
      }
      const wasActive = Boolean(controller.snapshot().identity);
      controller.activate(model.workspaceId, model.resourceId, next);
      if (!wasActive && snapshot.loaded && !model.detail?.archived) void controller.refresh();
    });
  });

  $effect(() => {
    if (!visible) return;
    const latest = followLatestTurn ? latestTurn : undefined;
    if (!followLatestTurn) {
      lastFollowedKey = "";
      untrack(collapseAutoExpanded);
    } else if (latest?.turn && latest.key !== lastFollowedKey) {
      const { key, turn } = latest;
      lastFollowedKey = key;
      untrack(() => {
        collapseAutoExpanded();
        if (expanded.has(key)) return;
        autoExpanded = { key, reference: turn.reference };
        void controller.expandTurn(turn.reference);
      });
    }
  });

  let previousFirstHeading = "";
  $effect.pre(() => {
    const ids = pendingBubbleIds;
    const moved = [...previousPending].some(id => !ids.has(id));
    previousPending = ids;
    const first = firstHeading?.generationId || "";
    const prepended = previousFirstHeading && first && first !== previousFirstHeading;
    previousFirstHeading = first;
    if ((!moved && !prepended) || untrack(() => followTail || !visible)) return;
    const scroll = root?.closest<HTMLElement>("#chatTimeline");
    if (!scroll) return;
    const restore = captureChatAnchor(scroll, root);
    void tick().then(() => { if (root.isConnected) restore(); });
  });

  $effect.pre(() => {
    const pending = queue;
    void activeRow?.key;
    if (!visible || !root) return;
    const focused = document.activeElement?.closest<HTMLElement>("[data-queued-input], [data-turn-chip]");
    const input = focused?.dataset.queuedInput;
    const turn = focused?.dataset.turnChip;
    if ((!turn && !input) || pending.some(message => message.messageId === input)) return;
    void tick().then(() => {
      if (!root?.isConnected || document.activeElement !== document.body) return;
      const selector = input ? `[data-history-input="${CSS.escape(input)}"]` : `[data-turn-chip="${CSS.escape(turn || "")}"]`;
      const target = root.querySelector<HTMLButtonElement>(`${selector} button`);
      (target || root.closest<HTMLElement>(".persistent-chat"))?.focus({ preventScroll: true });
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

  function trackGenerationHeadings(element: HTMLDivElement) {
    const scroll = element.closest<HTMLElement>("#chatTimeline");
    if (!scroll) return;
    $effect(() => {
      if (!visible || !sections.length) {
        pinnedHeading = "";
        return;
      }
      const headings = Array.from(element.querySelectorAll<HTMLElement>("[data-generation-heading]")).reverse();
      const update = () => {
        const top = scroll.getBoundingClientRect().top + scroll.clientTop;
        const active = headings.find(heading => heading.getBoundingClientRect().top <= top);
        pinnedHeading = active?.dataset.generationHeading || "";
        pinnedHeight = active?.getBoundingClientRect().height || 0;
      };
      untrack(update);
      const resize = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(update);
      resize?.observe(scroll);
      resize?.observe(element);
      for (const heading of headings) resize?.observe(heading);
      scroll.addEventListener("scroll", update, { passive: true });
      return () => {
        resize?.disconnect();
        scroll.removeEventListener("scroll", update);
      };
    });
  }

  async function loadOlder() {
    onInspect();
    const scroll = root.closest<HTMLElement>("#chatTimeline");
    if (!scroll) {
      await controller.loadOlder();
      return;
    }
    const restore = captureChatAnchor(scroll, root);
    await controller.loadOlder();
    await tick();
    if (root.isConnected) restore();
  }

  function isExpanded(key: string): boolean {
    return autoExpanded?.key === key || expanded.has(key);
  }

  function collapseAutoExpanded() {
    const reference = autoExpanded?.reference;
    autoExpanded = null;
    if (reference) controller.collapseTurn(reference);
  }

  function toggle(block: ConversationBlock, button: HTMLButtonElement) {
    onInspect();
    if (autoExpanded?.key === block.key) {
      collapseAutoExpanded();
    } else {
      const next = new Set(expanded);
      if (expanded.has(block.key)) {
        next.delete(block.key);
        if (block.turn) controller.collapseTurn(block.turn.reference);
      } else {
        next.add(block.key);
        if (block.turn) void controller.expandTurn(block.turn.reference);
      }
      expanded = next;
    }
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

</script>

{#snippet eventDetails(block: ConversationBlock)}
  <TurnEventDetails {block} project={projectConversationEvents} presentItem={historyItem}
    workspaceId={model.workspaceId} resolveResourceTitle={model.resolveResourceTitle}
    onNavigate={model.onNavigate} {onOpenFile} onExpand={expandCompact} readOnly
    contextIdentity={snapshot.identity} onApproval={async () => {}} onToast={model.onToast} />
{/snippet}

{#snippet turnChip(row: HistoryRow)}
  {@const { block, turn } = row}
  {@const detailsId = `${uid}-${block.key}`}
  {@const open = isExpanded(row.key)}
  <div class="history-chip" data-timeline-key={row.key} data-turn-chip={row.key} data-history-input={turn.triggerMessageId} data-chat-row={row.key}>
    <TimelineSourceButton source={turnSource(turn, row.input)} {onInspect}
      status={activeRow?.key === row.key ? turn.status : undefined}
      expanded={open} controls={open ? detailsId : undefined} onToggle={(button) => toggle(block, button)} />
  </div>
{/snippet}

{#snippet turnDetails(row: HistoryRow)}
  {#if isExpanded(row.key)}
    {@const { block, turn } = row}
    {@const detailsId = `${uid}-${block.key}`}
    {@const source = turnSource(turn, row.input)}
    <section class="expanded-history history-wide" id={detailsId} aria-label={`${source.label} Turn details`}>
      <header>
        <strong>{source.sender || source.label}</strong>
        <small>{[turn.status, `${turn.eventCount} events`, new Date(turn.startedAt).toLocaleString()].filter(Boolean).join(" · ")}</small>
      </header>
      {#if !block.items && !block.events && !block.error}
        {#if source.text && !row.input}<p class="trigger-preview">{source.text}</p>{/if}
        <div class="turn-loading" role="status"><Icon name="loader-circle" />Loading Turn details</div>
      {/if}
      {#if block.error}
        <TimelineNotice title="Turn unavailable" text={block.error} error />
        {#if !block.items && !block.events}<button type="button" class="secondary-button" onclick={() => controller.loadTurn(turn.reference)}>Retry</button>{/if}
      {/if}
      {@render eventDetails(block)}
      <button type="button" class="ghost-button collapse-history" onclick={() => collapse(block, detailsId)}><Icon name="chevron-up" />Collapse turn</button>
    </section>
  {/if}
{/snippet}

{#snippet runningChip()}
  {#if activeRow}{@render turnChip(activeRow)}{/if}
{/snippet}

{#snippet tail()}
  {#each snapshot.notices as notice, index (`${index}:${String(notice.data?.text || "")}`)}
    <div class="history-wide"><TimelineNotice title="PUA" text={String(notice.data?.text || "")} error={notice.data?.level === "error"} onDismiss={() => controller.dismissNotice(notice)} /></div>
  {/each}
  {#if snapshot.error}
    <div class="history-wide history-actions" role="status">
      <span>Execution history unavailable. {snapshot.error}</span>
      <button type="button" class="ghost-button" onclick={() => controller.retryHistory()}>Retry history</button>
    </div>
  {/if}
  {@render execution(activeRow ? runningChip : undefined, activeRow ? turnWorkingLabel(activeRow) : "", Boolean(statusRow?.turn.closed))}
  {#if activeRow}{@render turnDetails(activeRow)}{/if}
  {#if queue.length}
    <div class="queued-inputs" role="group" aria-label="Waiting messages">
      <span>Waiting · {queue.length}</span>
      {#each queue as message (message.messageId)}
        <div class="history-chip" data-queued-input={message.messageId}><TimelineSourceButton source={queueSource(message)} {onInspect} /></div>
      {/each}
    </div>
  {/if}
  {#each pendingBubbles as message (message.id)}
    <div class="chat-bubble-row" data-chat-row={`message:${message.id}`} data-queued-chat-message>{@render bubble(message)}</div>
  {/each}
{/snippet}

<div bind:this={root} use:trackGenerationHeadings class="shared-chat-stream public-messages" data-component-owner="event-timeline" style:--pinned-height={`${pinnedHeight}px`}>
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
  {#each sections as section, index (section.key)}
    {#if section.generation}
      <div class="generation-heading" data-generation-heading={section.key} class:pinned={pinnedHeading === section.key}>
        <GenerationMarker generation={section.generation} status={effectiveGenerationStatus({ generation: section.generation }, status)} />
      </div>
    {/if}
    {#each section.rows as row (row.key)}
      {#if row.kind === "message"}
        <div class="chat-bubble-row" data-chat-row={row.key}>{@render bubble(row.message)}</div>
      {:else if row.kind === "gap"}
        {@const starting = row.block.gap?.code === "session_starting"}
        <div class:conversation-session-starting={starting} class:conversation-gap={!starting} class="history-wide" data-timeline-key={row.key} data-chat-row={row.key}>
          <Icon name={starting ? "loader-circle" : "triangle-alert"} />
          <span><strong>{starting ? "Starting agent…" : "History unavailable"}</strong><small>{row.block.gap?.message || "This generation could not be read."}</small></span>
          {#if row.block.gap?.retryable}<button type="button" class="secondary-button" onclick={() => controller.retryHistory()}>Retry</button>{/if}
        </div>
      {:else if row.kind === "session"}
        <div class="history-wide" data-timeline-key={row.key} data-chat-row={row.key}>
          {@render eventDetails(row.block)}
        </div>
      {:else if row.kind === "history" && row.key !== activeRow?.key}
        {@render turnChip(row)}
        {@render turnDetails(row)}
      {/if}
    {/each}
    {#if index === sections.length - 1}{@render tail()}{/if}
  {:else}
    {@render empty()}
    {@render tail()}
  {/each}
</div>

<style>
  .shared-chat-stream { display: flex; flex: 1; flex-wrap: wrap; align-content: start; align-items: start; min-width: 0; gap: 8px 6px; }
  .generation-heading { display: flex; flex: 0 0 100%; min-width: 0; margin: 8px 0 2px; position: sticky; top: 0; }
  .generation-heading.pinned { z-index: 1; }
  .generation-heading:has(~ .generation-heading.pinned) { clip-path: inset(0 0 max(0px, calc(100% - var(--pinned-height))) 0); }
  .generation-heading > :global(.history-generation) { margin: 0; }
  .chat-bubble-row { display: flex; flex-direction: column; flex: 0 0 100%; min-width: 0; }
  .history-chip { display: flex; }
  .history-wide, .history-actions, .queued-inputs { flex: 0 0 100%; min-width: 0; box-sizing: border-box; }
  .history-actions, .queued-inputs { display: flex; flex-wrap: wrap; align-items: center; gap: 6px; color: var(--muted-strong); }
  .history-actions button, .collapse-history { display: inline-flex; align-items: center; gap: 5px; }
  .history-actions button :global(svg), .collapse-history :global(svg) { width: 14px; height: 14px; }
  .queued-inputs { border-top: 1px dashed var(--line); padding-top: 7px; }
  .queued-inputs > span { margin-right: 3px; font-size: 12px; }
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
