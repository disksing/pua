<script lang="ts">
  import "./EventTimeline.css";

  import { onDestroy, onMount, tick } from "svelte";

  import { ApiClient } from "../api/client";
  import ActivityGroup from "./ActivityGroup.svelte";
  import ApprovalCard from "./ApprovalCard.svelte";
  import { ChatSessionController, turnIsCollapsedByPolicy } from "./chat-state";
  import { effectiveGenerationStatus } from "./generation-status";
  import FilePreviewModal from "./FilePreviewModal.svelte";
  import LifecycleNotice from "./LifecycleNotice.svelte";
  import type { ModelChannel } from "./model-channel";
  import Icon from "./Icon.svelte";
  import type { ChatContextSnapshot, ConversationBlock, EventTimelineModel, FilePreviewModel, ResourceHistoryTurnSummary, TimelineItem } from "./models";
  import TimelineMessage from "./TimelineMessage.svelte";
  import TimelineNotice from "./TimelineNotice.svelte";
  import { formatClock, groupTimelineActivities, markTurnAgentRuns, markTurnFinalAssistant } from "./timeline-events";
  import UnknownEvent from "./UnknownEvent.svelte";

  let { channel }: { channel: ModelChannel<EventTimelineModel> } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  // svelte-ignore state_referenced_locally
  let projector = $state(channel.current().project);
  let snapshot = $state<ChatContextSnapshot>(emptySnapshot());
  let root: HTMLDivElement | undefined = $state();
  let controller: ChatSessionController | undefined;
  let deferredSnapshot: ChatContextSnapshot | null = null;
  let followAfterUpdate = false;
  let contextChanged = false;
  // Sticky pinned-to-bottom state. Transient layout changes outside the
  // scroller (composer send feedback, growing textarea, message queue) shrink
  // its clientHeight without firing a scroll event, so an instantaneous
  // isNearBottom probe at update time would falsely report that the user
  // scrolled away and permanently drop follow. User scrolls update this flag,
  // while an explicit message submission restores it; layout shifts re-pin
  // through the ResizeObserver below.
  let follow = true;
  let preview = $state<{ section: string; path: string } | null>(null);
  const client = new ApiClient();
  // Viewport fill state. Collapsed Turns expand bottom-up one at a time until
  // the conversation overflows the viewport (FILL_STEP_LIMIT bounds a
  // pathological run of tiny Turns); visibility-triggered expansions arriving
  // mid-fill are deferred and re-evaluated against the resulting layout.
  const TURN_OBSERVER_MARGIN = 240;
  const FILL_STEP_LIMIT = 40;
  let filling = false;
  // fillArmed stays false until the first fill pass for the current content
  // completes, so the IntersectionObserver fallback (which fires synchronously
  // on mount) cannot burst every summary into a detail request before the
  // bottom-up fill decides what the viewport actually needs.
  let fillArmed = false;
  const deferredTurnExpands = new Map<HTMLElement, string>();

  onMount(() => {
    const scroll = scroller();
    const trackFollow = () => { follow = isNearBottom(scroller()); };
    scroll?.addEventListener("scroll", trackFollow, { passive: true });
    const followResize = typeof ResizeObserver === "undefined" || !scroll ? null : new ResizeObserver(() => {
      if (follow && !hasActiveSelection()) scrollToBottom();
    });
    if (scroll && followResize) followResize.observe(scroll);
    controller = new ChatSessionController({
      onEvent: (workspaceId, resourceId, event) => model.onEvent(workspaceId, resourceId, event),
      onNotice: (workspaceId, resourceId, notice) => model.onNotice(workspaceId, resourceId, notice),
    });
    const unsubscribeSnapshot = controller.subscribe(receive);
    const unsubscribeModel = channel.subscribe((next) => {
      const previousIdentity = model.identity;
      const followRequested = !model.submitting && next.submitting;
      const workingChanged = turnIsWorking(model.status) !== turnIsWorking(next.status);
      const followWorkingChange = workingChanged && follow;
      model = next;
      if (next.project !== projector) projector = next.project;
      if (next.identity !== previousIdentity) {
        contextChanged = true;
        deferredSnapshot = null;
        preview = null;
        fillArmed = false;
        deferredTurnExpands.clear();
      }
      if (followRequested) {
        follow = true;
        clearTimelineSelection();
        if (deferredSnapshot) {
          const deferred = deferredSnapshot;
          deferredSnapshot = null;
          applySnapshot(deferred);
        }
      }
      controller?.activate(next.workspaceId, next.resourceId, next.status);
      void tick().then(() => {
        if ((followRequested || followWorkingChange) && !hasActiveSelection()) scrollToBottom();
      });
    });
    const selectionChanged = () => {
      if (!deferredSnapshot || hasActiveSelection()) return;
      const next = deferredSnapshot;
      deferredSnapshot = null;
      applySnapshot(next);
    };
    const keydown = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || !preview) return;
      event.preventDefault();
      preview = null;
    };
    document.addEventListener("selectionchange", selectionChanged);
    document.addEventListener("keydown", keydown);
    return () => {
      unsubscribeSnapshot();
      unsubscribeModel();
      document.removeEventListener("selectionchange", selectionChanged);
      document.removeEventListener("keydown", keydown);
      scroll?.removeEventListener("scroll", trackFollow);
      followResize?.disconnect();
      controller?.dispose();
      controller = undefined;
      if (scroll) scroll.removeAttribute("data-agent-resource-id");
    };
  });

  onDestroy(() => client.dispose());

  function receive(next: ChatContextSnapshot): void {
    if (snapshot.identity && next.identity === snapshot.identity && hasActiveSelection()) {
      deferredSnapshot = next;
      return;
    }
    applySnapshot(next);
  }

  function applySnapshot(next: ChatContextSnapshot): void {
    const scroll = scroller();
    const changed = next.identity !== snapshot.identity;
    if (changed || contextChanged) follow = true;
    if (!next.loaded) fillArmed = false;
    followAfterUpdate = follow;
    contextChanged = false;
    snapshot = next;
    if (scroll) scroll.dataset.agentResourceId = next.resourceId;
    void tick().then(() => {
      if (followAfterUpdate && !hasActiveSelection()) scrollToBottom();
      if (next.loaded) void fillViewport(next.identity);
    });
  }

  function observeTurn(node: HTMLElement, reference: string) {
    let current = reference;
    if (typeof IntersectionObserver === "undefined") {
      if (current) requestTurnExpand(node, current);
      return {
        update(next: string) { current = next; if (deferredTurnExpands.has(node)) deferredTurnExpands.set(node, next); if (current) requestTurnExpand(node, current); },
        destroy() { deferredTurnExpands.delete(node); },
      };
    }
    const observer = new IntersectionObserver((entries) => {
      if (entries.some((entry) => entry.isIntersecting) && current) requestTurnExpand(node, current);
    }, { root: scroller(), rootMargin: `${TURN_OBSERVER_MARGIN}px 0px` });
    observer.observe(node);
    return {
      update(next: string) { current = next; if (deferredTurnExpands.has(node)) deferredTurnExpands.set(node, next); },
      destroy() { observer.disconnect(); deferredTurnExpands.delete(node); },
    };
  }

  function blockItems(block: ConversationBlock): TimelineItem[] {
    const items = block.events ? projector(block.events).map((item) => ({ ...item, generationId: block.generation.generationId })) : block.items || [];
    return markTurnAgentRuns(groupTimelineActivities(markTurnFinalAssistant(items)));
  }

  function blockAgentName(block: ConversationBlock): string {
    return block.generation.agentName || block.generation.resolvedProfile || block.generation.binding?.name || model.agentName || "Agent";
  }

  // autoExpandReference feeds the visibility observer: only Turns that may
  // expand on scroll return their reference; collapsed-by-policy Turns return
  // an empty reference so the observer never requests their details.
  function autoExpandReference(block: ConversationBlock): string {
    const turn = block.turn;
    if (!turn || turnIsCollapsedByPolicy(turn)) return "";
    return turn.reference || "";
  }

  function triggerSourceLabel(turn: ResourceHistoryTurnSummary): string {
    const name = String(turn.triggerSender?.name || "").trim();
    if (name) return name;
    const role = String(turn.triggerRole || "").toLowerCase();
    if (role === "system") return "System";
    if (role === "agent") return "Agent";
    return "Message";
  }

  // collapsedTriggerRole maps the trigger's provenance onto the message row
  // roles (user/assistant/agent/system) so the digest's trigger message uses
  // the same rail and name colors as a regular message of that role.
  function collapsedTriggerRole(turn: ResourceHistoryTurnSummary): string {
    const role = String(turn.triggerRole || "").toLowerCase();
    return role === "system" ? "system" : "agent";
  }

  function collapsedTurnStatusLabel(turn: ResourceHistoryTurnSummary): string {
    const status = String(turn.status || "").trim().toLowerCase();
    return status && status !== "completed" ? status : "";
  }

  function expandCollapsedTurn(block: ConversationBlock): void {
    const reference = block.turn?.reference || "";
    if (reference) void controller?.expandTurn(reference);
  }

  function collapseExpandedTurn(block: ConversationBlock): void {
    const reference = block.turn?.reference || "";
    if (reference) controller?.collapseTurn(reference);
  }

  // fillViewport expands collapsed Turns bottom-up, one at a time, stopping as
  // soon as the conversation overflows the viewport. The newest expanded Turn
  // usually fills the screen on its own, so eagerly expanding every summary
  // near the viewport (the previous behavior) wasted AgentHub round-trips and
  // Markdown renders on history nobody scrolls to. Older summary pages are
  // pulled only when expanded Turns still leave the viewport underfilled.
  async function fillViewport(identity: string): Promise<void> {
    if (filling) return;
    filling = true;
    try {
      let steps = 0;
      while (steps < FILL_STEP_LIMIT && snapshot.identity === identity && snapshot.loaded) {
        const scroll = scroller();
        if (!scroll || !controller || hasActiveSelection()) break;
        if (scroll.scrollHeight > scroll.clientHeight + fillMargin(scroll)) break;
        const reference = nextUnexpandedTurnReference();
        if (reference) {
          await controller.loadTurn(reference);
          steps++;
          await tick();
          continue;
        }
        if (hasCollapsedTurnCards()) break;
        if (snapshot.hasMoreBefore && !snapshot.loadingOlder) {
          if (!await controller.loadOlder()) break;
          steps++;
          await tick();
          continue;
        }
        break;
      }
    } finally {
      filling = false;
      fillArmed = true;
      flushDeferredTurnExpands();
    }
  }

  function fillMargin(scroll: HTMLElement): number {
    return Math.max(TURN_OBSERVER_MARGIN, scroll.clientHeight / 2);
  }

  function nextUnexpandedTurnReference(): string {
    for (let index = snapshot.blocks.length - 1; index >= 0; index--) {
      const block = snapshot.blocks[index];
      if (block.kind !== "turn" || !block.turn?.reference) continue;
      // Collapsed-by-policy (non-user) Turns expand only on explicit click.
      if (turnIsCollapsedByPolicy(block.turn)) continue;
      if (block.items || block.events || block.loading || block.error) continue;
      return block.turn.reference;
    }
    return "";
  }

  // Collapsed-by-policy Turn cards still occupy some height, but when they
  // are all that remains unexpanded the fill must stop instead of paging
  // through older history just to fill the viewport.
  function hasCollapsedTurnCards(): boolean {
    return snapshot.blocks.some((block) => block.kind === "turn" && block.turn && !block.items && !block.events && !block.loading && !block.error);
  }

  function requestTurnExpand(node: HTMLElement, reference: string): void {
    if (!reference) return;
    if (filling || !fillArmed) {
      deferredTurnExpands.set(node, reference);
      return;
    }
    void controller?.loadTurn(reference);
  }

  // Intersections queued while the viewport fill was running are re-evaluated
  // against the layout the fill produced: sections still near the viewport
  // expand now, the rest re-trigger their observer when scrolled into view.
  function flushDeferredTurnExpands(): void {
    if (!deferredTurnExpands.size) return;
    const pending = [...deferredTurnExpands];
    deferredTurnExpands.clear();
    const scroll = scroller();
    if (!scroll) return;
    const viewport = scroll.getBoundingClientRect();
    for (const [node, reference] of pending) {
      if (!node.isConnected) continue;
      const bounds = node.getBoundingClientRect();
      if (bounds.bottom >= viewport.top - TURN_OBSERVER_MARGIN && bounds.top <= viewport.bottom + TURN_OBSERVER_MARGIN) void controller?.loadTurn(reference);
    }
  }

  async function loadOlder(): Promise<void> {
    const scroll = scroller();
    if (!scroll || snapshot.loadingOlder) return;
    const anchor = firstVisibleItem(scroll);
    const anchorTop = anchor?.getBoundingClientRect().top ?? 0;
    const previousHeight = scroll.scrollHeight;
    const previousTop = scroll.scrollTop;
    const identity = snapshot.identity;
    await controller?.loadOlder();
    await tick();
    if (snapshot.identity !== identity) return;
    if (anchor?.isConnected) scroll.scrollTop = previousTop + (anchor.getBoundingClientRect().top - anchorTop);
    else scroll.scrollTop = previousTop + (scroll.scrollHeight - previousHeight);
  }

  function expandCompact(item: TimelineItem): Promise<void> | undefined {
    if (!item.compact || !item.generationId || !item.rangeStartEventId || !item.rangeEndEventId) return;
    return controller?.expandRange(item.generationId, item.rangeStartEventId, item.rangeEndEventId);
  }

  function openLinkedFile(path: string): void {
    preview = { section: "Files", path };
  }

  function rejectReadOnlySave(): Promise<FilePreviewModel> {
    return Promise.reject(new Error("Chat file previews are read-only."));
  }

  function scroller(): HTMLElement | null { return root?.parentElement ?? null; }

  function hasActiveSelection(): boolean {
    const scroll = scroller();
    const selection = window.getSelection?.();
    return Boolean(scroll && selection && !selection.isCollapsed && selection.rangeCount && selection.getRangeAt(0).intersectsNode(scroll));
  }

  function clearTimelineSelection(): void {
    const scroll = scroller();
    const selection = window.getSelection?.();
    if (scroll && selection && !selection.isCollapsed && selection.rangeCount && selection.getRangeAt(0).intersectsNode(scroll)) {
      selection.removeAllRanges();
    }
  }

  function isNearBottom(scroll: HTMLElement | null): boolean {
    return Boolean(scroll && scroll.scrollHeight - scroll.scrollTop - scroll.clientHeight <= 32);
  }

  function turnIsWorking(status: EventTimelineModel["status"]): boolean {
    return status?.session?.state === "running" && Boolean(status.session.currentTurnId);
  }

  function scrollToBottom(): void {
    const scroll = scroller();
    if (scroll) scroll.scrollTop = scroll.scrollHeight;
  }

  function firstVisibleItem(scroll: HTMLElement): HTMLElement | null {
    const top = scroll.getBoundingClientRect().top;
    return [...scroll.querySelectorAll<HTMLElement>("[data-timeline-key]")].find((item) => item.getBoundingClientRect().bottom >= top) ?? null;
  }

  function timelineKey(item: TimelineItem): string {
    const key = item.kind === "activity" && item.rangeStartEventId
      ? String(item.rangeStartEventId)
      : String(item.key ?? item.approvalId ?? item.time ?? item.type ?? "event");
    return `${item.generationId || snapshot.generationId}:${item.kind}:${key}`;
  }

  function emptySnapshot(): ChatContextSnapshot {
    return { identity: "", workspaceId: "", resourceId: "", generationId: "", blocks: [], notices: [], hasMoreBefore: false, loading: false, loadingOlder: false, loaded: false, error: "" };
  }
</script>

<div bind:this={root} data-component-owner="event-timeline" class="event-timeline-root" data-chat-context={snapshot.identity}>
  {#if snapshot.resourceId}
    {#if snapshot.hasMoreBefore}
      <button type="button" class="load-older-events" class:busy={snapshot.loadingOlder} disabled={snapshot.loadingOlder} onclick={loadOlder}>
        <span class="load-older-icon load-older-icon-idle"><Icon name="chevrons-up" /></span><span class="load-older-icon load-older-icon-busy"><Icon name="loader-circle" /></span><span>{snapshot.loadingOlder ? "Loading..." : "Load older messages"}</span>
      </button>
    {/if}
    {#each snapshot.blocks as block, index (block.key)}
      {#if index === 0 || snapshot.blocks[index - 1].generation.generationId !== block.generation.generationId}
        <div class="conversation-generation" data-generation-id={block.generation.generationId}>
          <span>Generation {block.generation.generation}</span><strong>{block.generation.agentName || block.generation.resolvedProfile || block.generation.binding?.name || "Agent"}</strong><small data-generation-status={effectiveGenerationStatus(block, model.status)}>{effectiveGenerationStatus(block, model.status)}</small>
        </div>
      {/if}
      {#if block.kind === "gap"}
        {#if block.gap?.code === "session_starting"}
          <div class="conversation-session-starting" role="status" aria-live="polite" data-timeline-key={block.key}><Icon name="loader-circle" /><span><strong>Starting agent…</strong><small>Preparing the session. This can take a few seconds.</small></span></div>
        {:else}
          <div class="conversation-gap" data-timeline-key={block.key}><Icon name="triangle-alert" /><span><strong>History unavailable</strong><small>{block.gap?.message || "This generation could not be read."}</small></span>{#if block.gap?.retryable}<button type="button" class="secondary-button" onclick={() => controller?.retryHistory()}>Retry</button>{/if}</div>
        {/if}
      {:else}
        <section class="conversation-turn" class:conversation-turn-loading={block.loading} data-timeline-key={block.key} use:observeTurn={autoExpandReference(block)}>
          {#if block.turn && !block.items && !block.events}
            {#if turnIsCollapsedByPolicy(block.turn)}
              <!-- Non-user-triggered Turns (agent messages, scheduler and system
                   notifications) render as a two-message conversation digest:
                   the trigger message and the final reply keep the normal
                   sender-and-time message rows, and an ellipsis row stands in
                   for the elided middle. The digest sits on a background tinted
                   by the trigger role's color so the two messages read as one
                   Turn. Clicking the ellipsis loads and renders the full Turn;
                   open Turns stream live and fold back into this digest when
                   they close. -->
              <div class="turn-collapsed-digest" data-trigger-role={collapsedTriggerRole(block.turn)}>
              {#if block.turn.triggerPreview}
                <div class={`agent-message-row ${collapsedTriggerRole(block.turn)}`}>
                  <div class="agent-message-main">
                    <div class="agent-message-meta">
                      <strong>{triggerSourceLabel(block.turn)}</strong>
                      <span class="agent-message-tag agent-message-role-tag">{collapsedTriggerRole(block.turn)}</span>
                      {#if formatClock(block.turn.startedAt)}<span>{formatClock(block.turn.startedAt)}</span>{/if}
                    </div>
                    <div class="agent-message-bubble"><p class="turn-collapsed-text">{block.turn.triggerPreview}</p></div>
                  </div>
                </div>
              {/if}
              <button type="button" class="turn-collapsed-gap" title="Expand turn" onclick={() => expandCollapsedTurn(block)}>
                <Icon name="ellipsis" />
                {#if collapsedTurnStatusLabel(block.turn)}<span class="turn-collapsed-status" data-turn-status={String(block.turn.status || "").toLowerCase()}>{collapsedTurnStatusLabel(block.turn)}</span>{/if}
                {#if block.loading}<span class="turn-collapsed-gap-label">Loading turn details</span>{:else}<span class="turn-collapsed-gap-label">Expand turn</span>{/if}
              </button>
              {#if block.turn.finalReplyPreview}
                <div class="agent-message-row assistant final">
                  <div class="agent-message-main">
                    <div class="agent-message-meta">
                      <strong>{blockAgentName(block)}</strong>
                      {#if formatClock(block.turn.endedAt || block.turn.startedAt)}<span>{formatClock(block.turn.endedAt || block.turn.startedAt)}</span>{/if}
                    </div>
                    <div class="agent-message-bubble"><p class="turn-collapsed-text">{block.turn.finalReplyPreview}</p></div>
                  </div>
                </div>
              {/if}
              </div>
            {:else if block.turn.triggerPreview}
              <div class="turn-summary-preview">{block.turn.triggerPreview}</div>
            {/if}
          {/if}
          {#each blockItems(block) as item (timelineKey(item))}
            <div data-timeline-key={timelineKey(item)}>
              {#if item.agentStart}
                <!-- Reasoning, tool calls, and approvals render without their own
                     author label, so a run that starts with them gets a header
                     carrying the agent's name and the run's start time;
                     otherwise both would first appear on the initial progress
                     update instead of the turn's first event. -->
                <div data-component-owner="event-timeline" class="agent-run-header"><strong>{blockAgentName(block)}</strong>{#if formatClock(item.time)}<span>{formatClock(item.time)}</span>{/if}</div>
              {/if}
              {#if item.kind === "message"}
                <TimelineMessage {item} agentName={blockAgentName(block)} workspaceId={model.workspaceId} resolveResourceTitle={model.resolveResourceTitle} onNavigate={model.onNavigate} onOpenFile={openLinkedFile} />
              {:else if item.kind === "activity"}
                <ActivityGroup {item} onExpand={() => expandCompact(item)} />
              {:else if item.kind === "approval"}
                <ApprovalCard {item} generationId={block.generation.generationId} contextIdentity={snapshot.identity} onApproval={model.onApproval} onToast={model.onToast} />
              {:else if item.kind === "lifecycle"}
                <LifecycleNotice {item} />
              {:else if item.kind === "error"}
                <TimelineNotice title="Provider error" text={item.text || ""} error />
              {:else}
                <UnknownEvent {item} />
              {/if}
            </div>
          {/each}
          {#if block.turn && turnIsCollapsedByPolicy(block.turn) && (block.items?.length || block.events?.length)}
            <button type="button" class="turn-collapse-again" onclick={() => collapseExpandedTurn(block)}><Icon name="chevron-up" /><span>Collapse turn</span></button>
          {/if}
          {#if block.loading && !block.items && !block.events}<div class="turn-loading"><Icon name="loader-circle" /><span>Loading turn details</span></div>{/if}
          {#if block.error}<TimelineNotice title="Turn unavailable" text={block.error} error />{/if}
        </section>
      {/if}
    {/each}
    {#each snapshot.notices as notice, index (`notice:${snapshot.identity}:${index}:${String(notice.data?.text || "")}`)}
      <div data-timeline-key={`notice:${index}`}><TimelineNotice title="PUA" text={String(notice.data?.text || "")} error={notice.data?.level === "error"} onDismiss={() => controller?.dismissNotice(notice)} /></div>
    {/each}
    {#if snapshot.error}<TimelineNotice title="Timeline error" text={snapshot.error} error alert />{/if}
    {#if turnIsWorking(model.status)}
      <div class="turn-working-indicator" role="status" aria-live="polite" data-timeline-key="turn-working">
        <Icon name="loader-circle" /><span>working...</span>
      </div>
    {/if}
    {#if snapshot.loading && !snapshot.blocks.length}<div class="chat-timeline-empty"><Icon name="loader-circle" /><strong>Loading resource history</strong></div>{/if}
    {#if snapshot.loaded && !snapshot.loading && !snapshot.blocks.length && !snapshot.notices.length && !turnIsWorking(model.status)}<div class="chat-timeline-empty"><Icon name="bot" /><strong>No conversation yet</strong><span>Send a message to start this resource's conversation.</span></div>{/if}
  {:else}
    <div class="chat-timeline-empty"><Icon name="bot" /><strong>No resource selected</strong></div>
  {/if}
</div>

<FilePreviewModal {client} workspaceId={model.workspaceId} resourceId={model.resourceId} selection={preview} editable={false} resolveResourceTitle={model.resolveResourceTitle} onNavigate={model.onNavigate} onOpenFile={openLinkedFile} onSaveMarkdown={rejectReadOnlySave} onClose={() => preview = null} onError={model.onToast} />
