import { ApiClient, ApiError, StaleResponseError } from "../api/client";
import type {
  AgentEvent, AgentNotice, AgentSemanticFrame, AgentTurnItem, ChatContextSnapshot, ConversationBlock,
  ResourceHistoryGeneration, ResourceHistoryPage, ResourceHistorySegment,
  ResourceHistoryTurnDetail, ResourceHistoryTurnSummary, ResourceMessageStatus, TimelineItem,
} from "./models";
import { applyPUAMessagePayload, compactTimelineEvents, isHiddenConversationLifecycleText, mergeCanonicalEventBatch, mergeCanonicalEvents } from "./timeline-events";
import { formatToolCallCount, normalizeToolCallCount } from "./tool-group";

// A small summary page keeps the initial history read cheap: the Chat view
// expands Turns bottom-up only until the viewport is filled, so a large
// summary page would mostly fetch metadata nobody scrolls to.
const HISTORY_LIMIT = 5;
const EVENT_LIMIT = 250;
const STREAM_BATCH_WINDOW_MS = 80;
const STATUS_SYNC_INTERVAL_MS = 2000;
// PUA notices are live, non-durable diagnostics. Keep them readable without
// letting a stale notice occupy the conversation until the page is refreshed.
const NOTICE_TIMEOUT_MS = 10000;
// Terminal materialization retries bridge the gap between a streamed terminal
// frame and the canonical Turn detail serving it. The backoff covers slow
// projection reads; when the budget is exhausted the Turn stays pending and
// later status/head syncs keep retrying instead of leaving a stuck error.
const TERMINAL_RETRY_DELAYS_MS = [250, 500, 1000, 2000, 4000];
const HIDDEN_EVENT_TYPES = new Set(["session.created", "session.provider", "session.launch-environment", "semantic.empty"]);

type EventSourceFactory = (url: string) => EventSource;
type SnapshotListener = (snapshot: ChatContextSnapshot) => void;

interface EventPage {
  schema?: string;
  frames?: AgentSemanticFrame[];
  page?: { hasMore?: boolean; nextAfter?: number };
}

interface ResourceChatContext {
  key: string;
  workspaceId: string;
  resourceId: string;
  status: ResourceMessageStatus | null;
  generationId: string;
  requestGeneration: number;
  streamGeneration: number;
  segments: Map<string, ResourceHistorySegment>;
  details: Map<string, ResourceHistoryTurnDetail>;
  detailLoading: Set<string>;
  detailErrors: Map<string, string>;
  // expandedTurns records the collapsed-by-policy (non-user-triggered) Turns
  // the user explicitly opened. Membership is in-memory only: a history
  // reload resets every such Turn back to its collapsed summary card.
  expandedTurns: Set<string>;
  liveEvents: Map<string, AgentEvent[]>;
  orphanEvents: Map<string, AgentEvent[]>;
  detailChain: Promise<void>;
  notices: AgentNotice[];
  noticeTimers: Map<string, ReturnType<typeof setTimeout>>;
  nextCursor: string;
  hasMoreBefore: boolean;
  loading: boolean;
  loadingOlder: boolean;
  loaded: boolean;
  error: string;
  stream: EventSource | null;
  pendingEvents: AgentEvent[];
  headRefreshing: boolean;
  terminalMaterializing: Set<string>;
  // terminalPending holds Turn ids whose terminal materialization exhausted
  // its retry budget; later status or head syncs retry them until the compact
  // detail lands. terminalError remembers the message such a failure put into
  // error so a successful heal can clear exactly what it set.
  terminalPending: Set<string>;
  terminalError: string;
  flushTimer: ReturnType<typeof setTimeout> | null;
  statusSyncTimer: ReturnType<typeof setInterval> | null;
  statusSyncInFlight: boolean;
}

export interface ChatSessionControllerOptions {
  api?: ApiClient;
  eventSourceFactory?: EventSourceFactory;
  onEvent?: (workspaceId: string, resourceId: string, event: AgentEvent) => void;
  onNotice?: (workspaceId: string, resourceId: string, notice: AgentNotice) => void;
  streamBatchWindowMs?: number;
  statusSyncIntervalMs?: number;
  terminalRetryDelaysMs?: number[];
  noticeTimeoutMs?: number;
  realtime?: boolean;
}

// The public name is retained to avoid churn in embedders. Its identity and
// transport are resource-scoped; AgentHub Sessions are an implementation detail.
export class ChatSessionController {
  private readonly api: ApiClient;
  private readonly eventSourceFactory: EventSourceFactory;
  private readonly contexts = new Map<string, ResourceChatContext>();
  private readonly listeners = new Set<SnapshotListener>();
  private readonly onEvent?: ChatSessionControllerOptions["onEvent"];
  private readonly onNotice?: ChatSessionControllerOptions["onNotice"];
  private readonly streamBatchWindowMs: number;
  private readonly statusSyncIntervalMs: number;
  private readonly terminalRetryDelaysMs: number[];
  private readonly noticeTimeoutMs: number;
  private readonly realtime: boolean;
  private activeKey = "";
  private disposed = false;

  constructor(options: ChatSessionControllerOptions = {}) {
    this.api = options.api ?? new ApiClient();
    this.eventSourceFactory = options.eventSourceFactory ?? ((url) => new EventSource(url));
    this.onEvent = options.onEvent;
    this.onNotice = options.onNotice;
    this.streamBatchWindowMs = Math.max(0, options.streamBatchWindowMs ?? STREAM_BATCH_WINDOW_MS);
    this.statusSyncIntervalMs = Math.max(1, options.statusSyncIntervalMs ?? STATUS_SYNC_INTERVAL_MS);
    this.terminalRetryDelaysMs = (options.terminalRetryDelaysMs ?? TERMINAL_RETRY_DELAYS_MS).map((value) => Math.max(0, value));
    this.noticeTimeoutMs = Math.max(0, options.noticeTimeoutMs ?? NOTICE_TIMEOUT_MS);
    this.realtime = options.realtime !== false;
  }

  subscribe(listener: SnapshotListener): () => void {
    this.listeners.add(listener);
    listener(this.snapshot());
    return () => this.listeners.delete(listener);
  }

  activate(workspaceId: string, resourceId: string, status: ResourceMessageStatus | null): void {
    if (this.disposed) return;
    const nextKey = contextKey(workspaceId, resourceId);
    const contextChanged = this.activeKey !== nextKey;
    if (this.activeKey && contextChanged) this.deactivate(this.contexts.get(this.activeKey));
    this.activeKey = nextKey;
    if (!nextKey) {
      this.emit();
      return;
    }
    const context = this.contexts.get(nextKey) ?? this.createContext(workspaceId, resourceId);
    const nextGeneration = String(status?.generation?.generationId || "");
    if (this.isStaleStatus(context, status, nextGeneration)) {
      // A context switch must still publish the newly active context,
      // otherwise the view keeps showing the previously active resource until
      // some unrelated event happens to emit. Keep the status poll alive so
      // the pending fresh status reconciles the generation.
      this.startStatusSync(context);
      if (contextChanged) this.emit();
      return;
    }
    // Parent view-model refreshes may carry the previous status while this
    // controller is waiting for the AgentHub-backed status request. Keep the
    // poll alive; generation ordering below rejects an older response.
    this.startStatusSync(context);
    const generationChanged = Boolean(context.generationId && nextGeneration && context.generationId !== nextGeneration);
    const previousSessionId = String(context.status?.session?.id || "");
    context.status = status;
    context.generationId = nextGeneration;
    const nextSessionId = String(status?.session?.id || "");
    const nextGenerationStatus = String(status?.generation?.status || "");
    if (generationChanged) {
      this.resetForGeneration(context);
      void this.loadInitial(context);
    } else if (context.loaded && !context.loading && this.shouldReloadUnresolvedGap(context, nextGeneration, previousSessionId, nextSessionId, nextGenerationStatus)) {
      this.reloadGapContext(context);
    } else if (!context.loaded && !context.loading) {
      void this.loadInitial(context);
    } else if (this.realtime) {
      this.connect(context);
    }
    if (contextChanged || generationChanged) this.emit();
  }

  async loadOlder(): Promise<boolean> {
    const context = this.activeContext();
    if (!context || context.loadingOlder || !context.hasMoreBefore || !context.nextCursor) return false;
    const generation = context.requestGeneration;
    const cursor = context.nextCursor;
    context.loadingOlder = true;
    context.error = "";
    this.emit();
    try {
      const page = await this.api.latest<ResourceHistoryPage>(historyPath(context, cursor), { scope: requestScope(context, "older") });
      if (!this.isCurrent(context, generation)) return false;
      this.mergePage(context, page);
      return page.segments.some((segment) => segment.turns?.length || segment.gap);
    } catch (error) {
      if (error instanceof StaleResponseError || !this.isCurrent(context, generation)) return false;
      context.error = errorMessage(error);
      return false;
    } finally {
      if (this.isCurrent(context, generation)) {
        context.loadingOlder = false;
        this.emit();
      }
    }
  }

  retryHistory(): void {
    const context = this.activeContext();
    if (!context || context.loading) return;
    this.reloadGapContext(context);
  }

  // A generation whose history was requested before its AgentHub Session was
  // bound is cached as a gap with no turns. Once a status refresh references
  // the session, the gap can heal, so the context reloads instead of showing
  // "History unavailable" until a manual refresh.
  private hasUnresolvedGap(context: ResourceChatContext, generationId: string): boolean {
    if (!generationId) return false;
    const segment = context.segments.get(generationId);
    return Boolean(segment?.gap && !(segment?.turns || []).length);
  }

  private shouldReloadUnresolvedGap(context: ResourceChatContext, generationId: string, previousSessionId: string, nextSessionId: string, nextGenerationStatus: string): boolean {
    if (!this.hasUnresolvedGap(context, generationId)) return false;
    if (nextSessionId && nextSessionId !== previousSessionId) return true;
    return nextGenerationStatus !== "starting" && context.segments.get(generationId)?.gap?.code === "session_starting";
  }

  private reloadGapContext(context: ResourceChatContext): void {
    context.loaded = false;
    context.nextCursor = "";
    context.hasMoreBefore = false;
    void this.loadInitial(context);
  }

  // expandTurn pins a collapsed-by-policy Turn open. The emit happens before
  // the detail request so an already-loaded Turn re-renders immediately.
  async expandTurn(reference: string): Promise<void> {
    const context = this.activeContext();
    if (!context || !reference) return;
    context.expandedTurns.add(reference);
    this.emit();
    await this.loadTurn(reference);
  }

  collapseTurn(reference: string): void {
    const context = this.activeContext();
    if (!context || !context.expandedTurns.delete(reference)) return;
    this.emit();
  }

  dismissNotice(notice: AgentNotice): void {
    const context = this.activeContext();
    if (!context || !this.removeNotice(context, noticeIdentity(notice))) return;
    this.emit();
  }

  // Turn detail requests are serialized per context. The Chat timeline fills
  // the viewport bottom-up one Turn at a time, and its visibility observer can
  // still produce bursts when a run of collapsed Turns enters view; a chain
  // keeps those bursts from becoming concurrent AgentHub round-trips. Guards
  // re-run when the queued task executes so duplicate requests collapse.
  async loadTurn(reference: string): Promise<void> {
    const context = this.activeContext();
    if (!context || !reference || context.details.has(reference)) return;
    const generation = context.requestGeneration;
    const task = context.detailChain.then(() => this.loadTurnDetail(context, reference, generation));
    context.detailChain = task.then(() => undefined, () => undefined);
    return task;
  }

  private async loadTurnDetail(context: ResourceChatContext, reference: string, generation: number): Promise<void> {
    if (!this.isCurrent(context, generation) || context.details.has(reference) || context.detailLoading.has(reference)) return;
    const summary = this.findTurn(context, reference);
    if (!summary) return;
    context.detailLoading.add(reference);
    context.detailErrors.delete(reference);
    this.emit();
    try {
      const detail = await this.api.latest<ResourceHistoryTurnDetail>(turnPath(context, reference), { scope: requestScope(context, `turn:${reference}`) });
      if (!this.isCurrent(context, generation)) return;
      context.details.set(reference, detail);
      if (!detail.turn.closed && detail.turn.generation.generationId === context.generationId) {
        const events = await this.loadTurnRange(context, detail, generation);
        if (!this.isCurrent(context, generation)) return;
        context.liveEvents.set(reference, events);
      }
      if (this.realtime) this.connect(context);
    } catch (error) {
      if (error instanceof StaleResponseError || !this.isCurrent(context, generation)) return;
      context.detailErrors.set(reference, errorMessage(error));
    } finally {
      if (this.isCurrent(context, generation)) {
        context.detailLoading.delete(reference);
        this.emit();
      }
    }
  }

  async expandRange(generationId: string, start: number, end: number): Promise<void> {
    const context = this.activeContext();
    if (!context || !generationId || start <= 0 || end < start) return;
    const reference = this.turnReferenceForEvent(context, generationId, start);
    const summary = reference ? this.findTurn(context, reference) : undefined;
    if (!reference || !summary || end > summary.lastEventId) return;
    const generation = context.requestGeneration;
    // A block renders either compact Turn items or semantic Events. Load
    // the complete bounded Turn when expanding one compact range so messages
    // around the requested tool/thinking item remain visible as its details
    // replace the compact projection.
    const events = await this.fetchEventRange(context, generationId, summary.startEventId, summary.lastEventId, generation, `range:${start}:${end}`);
    if (!this.isCurrent(context, generation)) return;
    context.liveEvents.set(reference, compactTimelineEvents(mergeCanonicalEvents([...(context.liveEvents.get(reference) || []), ...events])));
    this.emit();
  }

  snapshot(): ChatContextSnapshot {
    const context = this.activeContext();
    if (!context) return emptySnapshot();
    return {
      identity: context.key,
      workspaceId: context.workspaceId,
      resourceId: context.resourceId,
      generationId: context.generationId,
      blocks: this.blocks(context),
      notices: [...context.notices],
      hasMoreBefore: context.hasMoreBefore,
      loading: context.loading,
      loadingOlder: context.loadingOlder,
      loaded: context.loaded,
      error: context.error,
    };
  }

  dispose(): void {
    if (this.disposed) return;
    this.disposed = true;
    for (const context of this.contexts.values()) {
      this.deactivate(context);
      this.clearNotices(context);
    }
    this.api.dispose();
    this.contexts.clear();
    this.listeners.clear();
    this.activeKey = "";
  }

  private createContext(workspaceId: string, resourceId: string): ResourceChatContext {
    const context: ResourceChatContext = {
      key: contextKey(workspaceId, resourceId), workspaceId, resourceId, status: null, generationId: "",
      requestGeneration: 1, streamGeneration: 0, segments: new Map(), details: new Map(), detailLoading: new Set(),
      detailErrors: new Map(), expandedTurns: new Set(), liveEvents: new Map(), orphanEvents: new Map(), detailChain: Promise.resolve(), notices: [], noticeTimers: new Map(), nextCursor: "", hasMoreBefore: false,
      loading: false, loadingOlder: false, loaded: false, error: "", stream: null, pendingEvents: [], headRefreshing: false,
      terminalMaterializing: new Set(), terminalPending: new Set(), terminalError: "", flushTimer: null, statusSyncTimer: null, statusSyncInFlight: false,
    };
    this.contexts.set(context.key, context);
    return context;
  }

  private async loadInitial(context: ResourceChatContext): Promise<void> {
    if (context.loading) return;
    const generation = context.requestGeneration;
    context.loading = true;
    context.error = "";
    this.emit();
    try {
      const page = await this.api.latest<ResourceHistoryPage>(historyPath(context), { scope: requestScope(context, "initial") });
      if (!this.isCurrent(context, generation)) return;
      context.segments.clear();
      context.details.clear();
      context.detailErrors.clear();
      context.expandedTurns.clear();
      context.liveEvents.clear();
      context.orphanEvents.clear();
      this.mergePage(context, page);
      context.loaded = true;
      this.connect(context);
    } catch (error) {
      if (error instanceof StaleResponseError || !this.isCurrent(context, generation)) return;
      context.error = errorMessage(error);
    } finally {
      if (this.isCurrent(context, generation)) {
        context.loading = false;
        this.emit();
      }
    }
  }

  private mergePage(context: ResourceChatContext, page: ResourceHistoryPage): void {
    for (const incoming of page.segments || []) {
      const id = incoming.generation.generationId;
      const existing = context.segments.get(id);
      if (!existing) {
        context.segments.set(id, { ...incoming, turns: [...(incoming.turns || [])] });
        continue;
      }
      const turns = new Map(existing.turns.map((turn) => [turn.reference, turn]));
      for (const turn of incoming.turns || []) turns.set(turn.reference, turn);
      existing.turns = [...turns.values()].sort((left, right) => left.startEventId - right.startEventId);
      existing.generation = incoming.generation;
      existing.gap = incoming.gap || existing.gap;
    }
    for (const segment of page.segments || []) for (const turn of segment.turns || []) {
      const orphan = context.orphanEvents.get(turn.turnId);
      if (!orphan) continue;
      context.liveEvents.set(turn.reference, compactTimelineEvents(mergeCanonicalEvents([...(context.liveEvents.get(turn.reference) || []), ...orphan])));
      context.orphanEvents.delete(turn.turnId);
    }
    context.nextCursor = String(page.page?.nextCursor || "");
    context.hasMoreBefore = Boolean(page.page?.hasMore && context.nextCursor);
  }

  private mergeTurnSummary(context: ResourceChatContext, turn: ResourceHistoryTurnSummary): void {
    const id = turn.generation.generationId;
    const segment = context.segments.get(id);
    if (!segment) {
      context.segments.set(id, { generation: turn.generation, turns: [turn] });
    } else {
      const turns = new Map(segment.turns.map((candidate) => [candidate.reference, candidate]));
      turns.set(turn.reference, turn);
      segment.turns = [...turns.values()].sort((left, right) => left.startEventId - right.startEventId);
      segment.generation = turn.generation;
      // A successful targeted Turn read proves this generation is readable.
      // Do not let a stale gap classification override the canonical detail.
      segment.gap = undefined;
    }
    const orphan = context.orphanEvents.get(turn.turnId);
    if (!orphan) return;
    context.liveEvents.set(turn.reference, compactTimelineEvents(mergeCanonicalEvents([...(context.liveEvents.get(turn.reference) || []), ...orphan])));
    context.orphanEvents.delete(turn.turnId);
  }

  private blocks(context: ResourceChatContext): ConversationBlock[] {
    const blocks: ConversationBlock[] = [];
    const segments = [...context.segments.values()].sort((left, right) => left.generation.generation - right.generation.generation);
    const currentGeneration = segments.find((segment) => segment.generation.generationId === context.generationId)?.generation || statusGeneration(context);
    const orphanBlocks = currentGeneration ? this.orphanEventBlocks(context, currentGeneration) : [];
    for (const segment of segments) {
      if (segment.gap) {
        blocks.push({ kind: "gap", key: `gap:${segment.generation.generationId}`, generation: segment.generation, gap: segment.gap });
        continue;
      }
      const generationBlocks: ConversationBlock[] = [];
      for (const turn of [...(segment.turns || [])].sort((left, right) => left.startEventId - right.startEventId)) {
        const detail = context.details.get(turn.reference);
        const live = context.liveEvents.get(turn.reference);
        // Closed non-user Turns stay as summary cards until the user expands
        // them; loaded details and leftover live events stay attached to the
        // context but out of the projection. Open Turns always stream live.
        const collapsed = turnIsCollapsedByPolicy(turn) && !context.expandedTurns.has(turn.reference);
        generationBlocks.push({
          kind: "turn", key: `${segment.generation.generationId}:${turn.turnId}`, generation: segment.generation, turn,
          items: !collapsed && detail && !live ? compactTurnItems(detail, segment.generation.generationId) : undefined,
          events: collapsed ? undefined : live?.filter((event) => !HIDDEN_EVENT_TYPES.has(event.type)),
          loading: !collapsed && context.detailLoading.has(turn.reference), error: collapsed ? undefined : context.detailErrors.get(turn.reference),
        });
      }
      if (segment.generation.generationId === context.generationId) generationBlocks.push(...orphanBlocks);
      generationBlocks.sort((left, right) => blockStartEventId(left) - blockStartEventId(right));
      blocks.push(...generationBlocks);
    }
    if (orphanBlocks.length && !segments.some((segment) => segment.generation.generationId === context.generationId)) blocks.push(...orphanBlocks);
    return blocks;
  }

  // orphanEventBlocks turns session-level events (session.created, provider
  // and state transitions carry no turnId) into conversation blocks placed by
  // their durable event id instead of always below every turn. Events are
  // grouped into contiguous id runs so startup notices stay above the first
  // turn while terminal notices stay after the last one.
  private orphanEventBlocks(context: ResourceChatContext, generation: ResourceHistoryGeneration): ConversationBlock[] {
    const blocks: ConversationBlock[] = [];
    for (const [turnId, events] of context.orphanEvents) {
      const visible = events.filter((event) => !HIDDEN_EVENT_TYPES.has(event.type));
      let run: AgentEvent[] = [];
      for (const event of visible) {
        if (run.length && Number(event.id) !== Number(run[run.length - 1].id) + 1) {
          blocks.push(orphanEventBlock(context, turnId, generation, run));
          run = [];
        }
        run.push(event);
      }
      if (run.length) blocks.push(orphanEventBlock(context, turnId, generation, run));
    }
    return blocks.sort((left, right) => blockStartEventId(left) - blockStartEventId(right));
  }

  private connect(context: ResourceChatContext): void {
    if (!this.realtime || !this.isActive(context) || context.stream || !context.generationId || !isStreamable(context.status)) return;
    const after = currentGenerationHead(context);
    const query = new URLSearchParams({ generationId: context.generationId });
    if (after) query.set("after", String(after));
    const streamGeneration = ++context.streamGeneration;
    const stream = this.eventSourceFactory(`${resourceBase(context)}/stream?${query}`);
    context.stream = stream;
    stream.onmessage = (message) => {
      if (!this.isActiveStream(context, stream, streamGeneration)) return;
      try {
        const frame = JSON.parse(message.data) as AgentSemanticFrame;
        const events = eventsFromSemanticFrame(frame);
        for (const event of events) {
          if (!this.eventBelongsToContext(context, event)) continue;
          context.pendingEvents.push(event);
          this.onEvent?.(context.workspaceId, context.resourceId, event);
          if (isTurnTerminal(event)) void this.materializeTerminalTurn(context, String(event.turnId || ""));
        }
        this.scheduleEventFlush(context);
      } catch {
        context.error = "An Agent event could not be decoded.";
        this.emit();
      }
    };
    stream.addEventListener("pua.notice", (message) => {
      if (!this.isActiveStream(context, stream, streamGeneration)) return;
      try {
        const notice = JSON.parse((message as MessageEvent).data) as AgentNotice;
        this.flushEvents(context, false);
        this.appendNotice(context, notice);
        this.onNotice?.(context.workspaceId, context.resourceId, notice);
        this.emit();
      } catch {
        context.error = "A PUA notice could not be decoded.";
        this.emit();
      }
    });
    stream.onerror = () => {
      if (!this.isActiveStream(context, stream, streamGeneration)) {
        stream.close();
        return;
      }
      // Native EventSource retries transient failures while CONNECTING. A
      // fatal HTTP/SSE response moves it to CLOSED, where it will never retry;
      // release that dead object so a later status transition (notably a
      // stopped generation resuming) can establish a new stream.
      if (stream.readyState === 2) {
        context.stream = null;
        context.streamGeneration++;
        void this.refreshCurrentStatus(context);
      }
    };
  }

  private startStatusSync(context: ResourceChatContext): void {
    if (!this.realtime || context.statusSyncTimer) return;
    context.statusSyncTimer = setInterval(() => void this.refreshCurrentStatus(context), this.statusSyncIntervalMs);
  }

  private isStaleStatus(context: ResourceChatContext, status: ResourceMessageStatus | null, generationId: string): boolean {
    if (!context.generationId) return false;
    if (!generationId) return true;
    const currentGeneration = Number(context.status?.generation?.generation);
    const nextGeneration = Number(status?.generation?.generation);
    return Number.isFinite(currentGeneration) && Number.isFinite(nextGeneration) && nextGeneration < currentGeneration;
  }

  private async refreshCurrentStatus(context: ResourceChatContext): Promise<void> {
    if (context.statusSyncInFlight || !this.isActive(context)) return;
    context.statusSyncInFlight = true;
    const generation = context.requestGeneration;
    try {
      const status = await this.api.latest<ResourceMessageStatus>(`${resourceBase(context)}/status`, { scope: requestScope(context, "status") });
      if (!this.isCurrent(context, generation) || !status.generation?.generationId) return;
      const nextGeneration = String(status.generation.generationId);
      if (nextGeneration !== context.generationId && (context.generationId || context.loaded)) {
        this.activate(context.workspaceId, context.resourceId, status);
        return;
      }
      const previousGenerationId = context.generationId;
      const previousSessionId = String(context.status?.session?.id || "");
      context.status = status;
      context.generationId = nextGeneration;
      const nextSessionId = String(status.session?.id || "");
      const nextGenerationStatus = String(status.generation?.status || "");
      if (context.stream && (!isStreamable(status) || (previousSessionId && previousSessionId !== nextSessionId))) this.closeStream(context);
      if (context.loaded && !context.loading && this.shouldReloadUnresolvedGap(context, nextGeneration, previousSessionId, nextSessionId, nextGenerationStatus)) this.reloadGapContext(context);
      else if (!context.loaded && !context.loading) void this.loadInitial(context);
      else if (!context.stream) this.connect(context);
      // A terminal Turn whose compact materialization exhausted its retry
      // budget gets retried here, so a temporarily slow or unavailable history
      // head heals by itself instead of leaving a stuck timeline error.
      this.retryPendingTerminals(context);
      if (previousGenerationId !== nextGeneration) this.emit();
    } catch (error) {
      // A stream failure should not replace useful history with a transient
      // status error. The next stream failure or application status refresh
      // retries reconciliation.
      if (error instanceof StaleResponseError || !this.isCurrent(context, generation)) return;
    } finally {
      context.statusSyncInFlight = false;
    }
  }

  private async loadTurnRange(context: ResourceChatContext, detail: ResourceHistoryTurnDetail, generation: number): Promise<AgentEvent[]> {
    const start = Math.max(1, Number(detail.turn.startEventId) || 1);
    const end = Math.max(start, Number(detail.turn.lastEventId) || 0, Number(detail.latestEventId) || 0);
    return this.fetchEventRange(context, detail.turn.generation.generationId || context.generationId, start, end, generation, `live-turn:${detail.turn.reference}`);
  }

  private async fetchEventRange(context: ResourceChatContext, generationId: string, start: number, end: number, generation: number, scope: string): Promise<AgentEvent[]> {
    let after = start - 1;
    let events: AgentEvent[] = [];
    while (after < end) {
      const query = new URLSearchParams({ generationId, start: String(start), end: String(end), after: String(after), limit: String(EVENT_LIMIT) });
      const page = await this.api.latest<EventPage>(`${resourceBase(context)}/events?${query}`, { scope: requestScope(context, scope) });
      if (!this.isCurrent(context, generation)) return [];
      if (page.schema !== "agenthub.semantic-events.v1") throw new Error(`Unsupported AgentHub events schema: ${page.schema || "missing"}.`);
      const batch = normalizeEvents((page.frames || []).flatMap(eventsFromSemanticFrame)).filter((event) => this.eventBelongsToContext(context, event));
      events = mergeCanonicalEvents([...events, ...batch]);
      const next = Number(page.page?.nextAfter) || latestEventId(batch);
      if (!page.page?.hasMore || !next || next <= after) break;
      after = next;
    }
    return events;
  }

  private async materializeTerminalTurn(context: ResourceChatContext, turnId: string): Promise<void> {
    if (!turnId) return;
    const generationId = context.generationId;
    const generation = context.requestGeneration;
    const materializationKey = `${generationId}:${turnId}`;
    if (context.terminalMaterializing.has(materializationKey)) return;
    context.terminalMaterializing.add(materializationKey);
    context.terminalPending.delete(turnId);
    try {
      this.flushEvents(context, false);
      const existing = this.findTurnById(context, generationId, turnId);
      if (existing?.closed && context.details.has(existing.reference)) {
        context.liveEvents.delete(existing.reference);
        this.clearTerminalErrorIfHealed(context);
        return;
      }
      let failure = "";
      for (let attempt = 0; attempt <= this.terminalRetryDelaysMs.length; attempt++) {
        if (!this.isCurrent(context, generation)) return;
        try {
          const detail = await this.api.latest<ResourceHistoryTurnDetail>(turnByIDPath(context, generationId, turnId), { scope: requestScope(context, `terminal:${materializationKey}`) });
          if (!this.isCurrent(context, generation)) return;
          if (detail.turn.generation.generationId !== generationId || detail.turn.turnId !== turnId) {
            throw new Error("Targeted Turn history returned a mismatched identity.");
          }
          this.mergeTurnSummary(context, detail.turn);
          if (!detail.turn.closed) {
            failure = terminalFailureMessage(context, generationId, detail.turn);
            throw new Error(failure);
          }
          // A repeated terminal frame may arrive while the history requests are
          // in flight. Fold it before replacing the semantic live view with the
          // canonical compact detail so a later batch flush cannot regress it.
          this.flushEvents(context, false);
          context.details.set(detail.turn.reference, detail);
          context.liveEvents.delete(detail.turn.reference);
          this.clearTerminalErrorIfHealed(context);
          this.emit();
          return;
        } catch (error) {
          if (error instanceof StaleResponseError || !this.isCurrent(context, generation)) return;
          failure = error instanceof ApiError && error.code === "history_turn_not_found"
            ? terminalFailureMessage(context, generationId, undefined)
            : context.segments.get(generationId)?.gap
              ? terminalFailureMessage(context, generationId, undefined)
              : errorMessage(error);
          if (attempt < this.terminalRetryDelaysMs.length) await delay(this.terminalRetryDelaysMs[attempt]);
        }
      }
      // The projection stayed unconfirmed past the retry budget (a slow or
      // unavailable canonical detail). Keep the semantic live view, remember the Turn
      // as pending, and let later status/head syncs retry instead of leaving
      // the live block and a stale error on screen forever.
      context.terminalPending.add(turnId);
      this.setTerminalError(context, failure);
      this.emit();
    } finally {
      context.terminalMaterializing.delete(materializationKey);
    }
  }

  private retryPendingTerminals(context: ResourceChatContext): void {
    if (!context.terminalPending.size || !this.isActive(context)) return;
    for (const turnId of [...context.terminalPending]) void this.materializeTerminalTurn(context, turnId);
  }

  private setTerminalError(context: ResourceChatContext, message: string): void {
    context.terminalError = message;
    context.error = message;
  }

  private clearTerminalErrorIfHealed(context: ResourceChatContext): void {
    if (!context.terminalError || context.terminalPending.size) return;
    // Only clear the exact message this path set; an unrelated error that
    // landed in the meantime stays visible.
    if (context.error === context.terminalError) context.error = "";
    context.terminalError = "";
  }

  private async refreshHead(context: ResourceChatContext): Promise<void> {
    if (context.headRefreshing) return;
    context.headRefreshing = true;
    const generation = context.requestGeneration;
    try {
      const page = await this.api.latest<ResourceHistoryPage>(historyPath(context), { scope: requestScope(context, "stream-head") });
      if (this.isCurrent(context, generation)) {
        this.mergePage(context, page);
        this.retryPendingTerminals(context);
        this.emit();
      }
    } catch (_) {
      // The semantic stream remains authoritative for the open Turn. A later event,
      // terminal materialization, or status refresh retries the summary head.
    } finally {
      context.headRefreshing = false;
    }
  }

  private findTurn(context: ResourceChatContext, reference: string): ResourceHistoryTurnSummary | undefined {
    return [...context.segments.values()].flatMap((segment) => segment.turns || []).find((turn) => turn.reference === reference);
  }

  private findTurnById(context: ResourceChatContext, generationId: string, turnId: string): ResourceHistoryTurnSummary | undefined {
    return [...context.segments.values()].flatMap((segment) => segment.turns || [])
      .find((turn) => turn.turnId === turnId && turn.generation.generationId === generationId);
  }

  private turnReferenceForEvent(context: ResourceChatContext, generationId: string, eventId: number): string {
    return [...context.segments.values()].filter((segment) => segment.generation.generationId === generationId)
      .flatMap((segment) => segment.turns || []).find((turn) => eventId >= turn.startEventId && eventId <= turn.lastEventId)?.reference || "";
  }

  // A streamed event of the open Turn usually arrives before the next history
  // refresh advances the summary's lastEventId, so the id range lookup misses
  // it. Falling back to the turnId keeps mid-turn events inside the existing
  // Turn block; otherwise every event bounces through a transient orphan block
  // that renders below the Turn and folds back on the next head refresh, which
  // makes the working indicator jitter on each tool call.
  private openTurnReferenceForEvent(context: ResourceChatContext, event: AgentEvent): string {
    const turnId = String(event.turnId || "");
    if (!turnId) return "";
    const summary = this.findTurnById(context, context.generationId, turnId);
    return summary && !summary.closed ? summary.reference : "";
  }

  private eventBelongsToContext(context: ResourceChatContext, event: AgentEvent): boolean {
    const sessionId = String(event.sessionId || "");
    return !sessionId || !context.status?.session?.id || sessionId === context.status.session.id;
  }

  private appendNotice(context: ResourceChatContext, notice: AgentNotice): void {
    const identity = noticeIdentity(notice);
    if (context.notices.some((candidate) => noticeIdentity(candidate) === identity)) return;
    context.notices.push(notice);
    const timer = setTimeout(() => {
      if (!this.removeNotice(context, identity) || !this.isActive(context)) return;
      this.emit();
    }, this.noticeTimeoutMs);
    context.noticeTimers.set(identity, timer);
    while (context.notices.length > 20) {
      const oldest = context.notices[0];
      if (!oldest) break;
      this.removeNotice(context, noticeIdentity(oldest));
    }
  }

  private removeNotice(context: ResourceChatContext, identity: string): boolean {
    const index = context.notices.findIndex((notice) => noticeIdentity(notice) === identity);
    if (index < 0) return false;
    context.notices.splice(index, 1);
    const timer = context.noticeTimers.get(identity);
    if (timer !== undefined) clearTimeout(timer);
    context.noticeTimers.delete(identity);
    return true;
  }

  private clearNotices(context: ResourceChatContext): void {
    for (const timer of context.noticeTimers.values()) clearTimeout(timer);
    context.noticeTimers.clear();
    context.notices = [];
  }

  private scheduleEventFlush(context: ResourceChatContext): void {
    if (context.flushTimer) return;
    context.flushTimer = setTimeout(() => {
      context.flushTimer = null;
      if (!this.isActive(context)) return;
      this.flushEvents(context, true);
    }, this.streamBatchWindowMs);
  }

  private flushEvents(context: ResourceChatContext, publish: boolean): void {
    if (!context.pendingEvents.length) return;
    const pending = context.pendingEvents;
    context.pendingEvents = [];
    for (const event of pending) {
      const reference = this.turnReferenceForEvent(context, context.generationId, Number(event.id)) || this.openTurnReferenceForEvent(context, event);
      if (reference) context.liveEvents.set(reference, compactTimelineEvents(mergeCanonicalEventBatch(context.liveEvents.get(reference) || [], [event])));
      else {
        const turnId = String(event.turnId || "current");
        context.orphanEvents.set(turnId, compactTimelineEvents(mergeCanonicalEventBatch(context.orphanEvents.get(turnId) || [], [event])));
        // Terminal events have a dedicated materialization path that retries
        // until the canonical Turn projection closes. Starting the generic
        // stream-head refresh as well only duplicates the same request.
        if (!isTurnTerminal(event)) void this.refreshHead(context);
      }
    }
    if (publish && this.isActive(context)) this.emit();
  }

  private closeStream(context: ResourceChatContext): void {
    context.streamGeneration++;
    context.stream?.close();
    context.stream = null;
  }

  private resetForGeneration(context: ResourceChatContext): void {
    if (context.flushTimer) clearTimeout(context.flushTimer);
    context.flushTimer = null;
    context.pendingEvents = [];
    context.requestGeneration++;
    this.closeStream(context);
    this.api.requests.abort(requestScope(context, "initial"));
    this.api.requests.abort(requestScope(context, "older"));
    this.api.requests.abort(requestScope(context, "status"));
    context.segments.clear();
    context.details.clear();
    context.detailLoading.clear();
    context.detailErrors.clear();
    context.expandedTurns.clear();
    context.liveEvents.clear();
    context.orphanEvents.clear();
    context.nextCursor = "";
    context.hasMoreBefore = false;
    context.loading = false;
    context.loadingOlder = false;
    context.loaded = false;
    context.error = "";
    context.headRefreshing = false;
    context.terminalMaterializing.clear();
    context.terminalPending.clear();
    context.terminalError = "";
    this.clearNotices(context);
  }

  private deactivate(context?: ResourceChatContext): void {
    if (!context) return;
    if (context.statusSyncTimer) clearInterval(context.statusSyncTimer);
    context.statusSyncTimer = null;
    if (context.flushTimer) clearTimeout(context.flushTimer);
    context.flushTimer = null;
    this.flushEvents(context, false);
    context.requestGeneration++;
    this.closeStream(context);
    context.loading = false;
    context.loadingOlder = false;
    this.api.requests.abort(requestScope(context, "initial"));
    this.api.requests.abort(requestScope(context, "older"));
    this.api.requests.abort(requestScope(context, "status"));
  }

  private isCurrent(context: ResourceChatContext, generation: number): boolean {
    return !this.disposed && this.isActive(context) && context.requestGeneration === generation;
  }

  private isActive(context: ResourceChatContext): boolean {
    return this.activeKey === context.key;
  }

  private isActiveStream(context: ResourceChatContext, stream: EventSource, generation: number): boolean {
    return !this.disposed && this.isActive(context) && context.stream === stream && context.streamGeneration === generation;
  }

  private activeContext(): ResourceChatContext | undefined {
    return this.activeKey ? this.contexts.get(this.activeKey) : undefined;
  }

  private emit(): void {
    const snapshot = this.snapshot();
    for (const listener of this.listeners) listener(snapshot);
  }
}

// turnIsCollapsedByPolicy marks closed Turns opened by a non-user trigger
// (agent-to-agent messages, scheduler and system notifications) that the Chat
// timeline renders as summary cards until the user expands them. Turns with
// an empty trigger role predate role tracking and keep the legacy
// auto-expanding behavior; open Turns always render live.
export function turnIsCollapsedByPolicy(turn: Pick<ResourceHistoryTurnSummary, "closed" | "triggerRole">): boolean {
  const role = String(turn.triggerRole || "").toLowerCase();
  return turn.closed && role !== "" && role !== "user";
}

function compactTurnItems(detail: ResourceHistoryTurnDetail, generationId: string): TimelineItem[] {
  return (detail.items || []).flatMap((item) => compactTurnItem(item, generationId));
}

function compactTurnItem(item: AgentTurnItem, generationId: string): TimelineItem[] {
  const key = `${generationId}:${item.startEventId}:${item.type}`;
  const base = { key, time: item.endedAt || item.startedAt, startTime: item.startedAt, generationId };
  const data = item.data && typeof item.data === "object" ? item.data : {};
  switch (item.type) {
    case "message": return [applyPUAMessagePayload({ ...base, kind: "message", role: item.role || "user", sender: item.sender, steer: item.steer, text: item.text || "" }, item.payload)];
    case "thinking": {
      const count = Math.max(1, Number(item.count) || 1);
      return [{ ...base, kind: "thinking", count, text: `Reasoning details omitted from compact history · ${count} update(s)`, compact: true, rangeStartEventId: item.startEventId, rangeEndEventId: item.endEventId }];
    }
    case "tool": {
      const count = normalizeToolCallCount(item.count, 1);
      return [{ ...base, kind: "tools", compact: true, toolCallCount: count, rangeStartEventId: item.startEventId, rangeEndEventId: item.endEventId, calls: [{ key, callId: key, name: "Tool activity", summary: `${formatToolCallCount(count)} · details omitted`, status: "completed" }] }];
    }
    case "activity": return [{
      ...base,
      kind: "activity",
      items: [],
      compact: true,
      thinkingCount: Math.max(0, Number(item.thinkingCount) || 0),
      reasoningUpdateCount: Math.max(0, Number(item.reasoningUpdateCount) || 0),
      toolCallCount: Math.max(0, Number(item.toolCallCount) || 0),
      rangeStartEventId: item.startEventId,
      rangeEndEventId: item.endEventId,
    }];
    case "approval": return [{ ...base, kind: "approval", approvalId: String(data.requestId || data.approvalId || key), title: String(data.title || "Approval"), question: String(data.question || ""), status: String(data.status || (data.decision ? "resolved" : "pending")), decision: String(data.decision || "") }];
    case "error": return [{ ...base, kind: "error", text: item.text || String(data.message || "Provider error") }];
    case "lifecycle": return item.text && !isHiddenConversationLifecycleText(item.text) ? [{ ...base, kind: "lifecycle", type: item.text, text: item.text }] : [];
    default: return [{ ...base, kind: "unknown", type: item.type, text: item.text || "" }];
  }
}

function contextKey(workspaceId: string, resourceId: string): string {
  return workspaceId && resourceId ? `${workspaceId}:${resourceId}` : "";
}

function orphanEventBlock(context: ResourceChatContext, turnId: string, generation: ResourceHistoryGeneration, events: AgentEvent[]): ConversationBlock {
  const firstEventId = events[0]?.id ?? 0;
  return { kind: "turn", key: `${context.generationId}:${turnId || "current"}:${firstEventId}`, generation, events };
}

function blockStartEventId(block: ConversationBlock): number {
  if (block.turn) return Number(block.turn.startEventId) || 0;
  const first = block.events?.[0];
  return first ? Number(first.id) || 0 : 0;
}

function requestScope(context: ResourceChatContext, kind: string): string {
  return `resource-chat:${context.key}:${kind}`;
}

function resourceBase(context: ResourceChatContext): string {
  return `/api/workspaces/${encodeURIComponent(context.workspaceId)}/resources/${encodeURIComponent(context.resourceId)}`;
}

function historyPath(context: ResourceChatContext, cursor = ""): string {
  const query = new URLSearchParams({ limit: String(HISTORY_LIMIT) });
  if (cursor) query.set("cursor", cursor);
  return `${resourceBase(context)}/history/turns?${query}`;
}

function turnPath(context: ResourceChatContext, reference: string): string {
  return `${resourceBase(context)}/history/turns/${encodeURIComponent(reference)}`;
}

function turnByIDPath(context: ResourceChatContext, generationId: string, turnId: string): string {
  const query = new URLSearchParams({ generationId, turnId });
  return `${resourceBase(context)}/history/turns/by-id?${query}`;
}

function currentGenerationHead(context: ResourceChatContext): number {
  const summaries = [...context.segments.values()].filter((segment) => segment.generation.generationId === context.generationId).flatMap((segment) => segment.turns || []);
  const live = [...context.liveEvents.values()].flat();
  return Math.max(0, ...summaries.map((turn) => Number(turn.lastEventId) || 0), ...live.map((event) => Number(event.id) || 0));
}

function statusGeneration(context: ResourceChatContext): ResourceHistoryGeneration | null {
  const generation = context.status?.generation;
  if (!generation?.generationId) return null;
  return {
    generation: generation.generation,
    generationId: generation.generationId,
    title: "Current generation",
    status: generation.status,
    createdAt: "",
    updatedAt: "",
    agentName: generation.agentName || context.status?.resolvedAgent,
    resolvedProfile: context.status?.resolvedProfile,
    replacementPending: generation.replacementPending,
  };
}

function normalizeEvents(events?: AgentEvent[]): AgentEvent[] {
  return Array.isArray(events) ? events.filter((event) => Number(event?.id) > 0) : [];
}

function eventsFromSemanticFrame(frame: AgentSemanticFrame): AgentEvent[] {
  if (frame?.schema !== "agenthub.semantic-events.v1" || !Number(frame.cursor)) throw new Error("An Agent semantic frame could not be decoded.");
  const events = Array.isArray(frame.events) ? frame.events : [];
  if (!events.length) {
    return [{
      id: Number(frame.cursor), semanticId: `empty:${frame.cursor}`, semanticIndex: -1,
      type: "semantic.empty", time: frame.source?.time, startTime: frame.source?.startTime,
      sessionId: frame.source?.sessionId, turnId: frame.source?.turnId, data: {},
    }];
  }
  return events.map((event) => ({
    id: Number(event.sourceEventId || frame.cursor), semanticId: String(event.id || ""), semanticIndex: Number(event.index) || 0,
    type: String(event.type || "unknown"), time: event.time || frame.source?.time, startTime: event.startTime || frame.source?.startTime,
    sessionId: event.sessionId || frame.source?.sessionId, turnId: event.turnId || frame.source?.turnId,
    data: frame.mode === "append" ? { ...(event.data || {}), append: true } : event.data,
  }));
}

function latestEventId(events: AgentEvent[]): number {
  return events.reduce((latest, event) => Math.max(latest, Number(event.id) || 0), 0);
}

function isStreamable(status: ResourceMessageStatus | null): boolean {
  const generation = status?.generation;
  return Boolean(generation?.generationId && status?.session?.id &&
    (["starting", "running", "waiting_approval", "idle", "stopping", "recovering"].includes(String(generation.status || "")) ||
      (["idle-suspended", "stopped"].includes(String(generation.status || "")) && generation.resumable === true)));
}

function isTurnTerminal(event: AgentEvent): boolean {
  return ["turn.completed", "turn.failed", "turn.cancelled"].includes(event.type);
}

// terminalFailureMessage classifies why the canonical Turn is not confirmed
// after a terminal frame: an upstream gap means History itself is degraded, a
// missing summary means the targeted projection is not available yet, and an
// open summary means the projection has not folded the terminal yet.
// All three are transient, so every message says the retry continues.
function terminalFailureMessage(context: ResourceChatContext, generationId: string, summary: ResourceHistoryTurnSummary | undefined): string {
  if (context.segments.get(generationId)?.gap) return "Turn history is temporarily unavailable; the timeline will keep retrying in the background.";
  if (!summary) return "The completed Turn is not available in canonical History yet; the timeline will keep retrying in the background.";
  return "Turn projection is not closed yet; the timeline will keep retrying in the background.";
}

function delay(milliseconds: number): Promise<void> {
  return new Promise((resolve) => setTimeout(resolve, milliseconds));
}

function noticeIdentity(notice: AgentNotice): string {
  const data = notice.data || {};
  return [notice.type, data.method, data.kind, data.lifecycle, data.resourceId, data.text].map((value) => String(value ?? "")).join(":");
}

function errorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

function emptySnapshot(): ChatContextSnapshot {
  return { identity: "", workspaceId: "", resourceId: "", generationId: "", blocks: [], notices: [], hasMoreBefore: false, loading: false, loadingOlder: false, loaded: false, error: "" };
}
