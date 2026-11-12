import { buildTimeline } from "./timeline-projector";

import type { AgentEvent, TimelineItem } from "./models";

const HIDDEN_CONVERSATION_EVENT_TYPES = new Set([
  "session.created",
  "session.provider",
  "session.launch-environment",
  "turn.started",
  "turn.completed",
]);

const HIDDEN_CONVERSATION_LIFECYCLE_TEXT = new Set([
  ...HIDDEN_CONVERSATION_EVENT_TYPES,
  "Session created",
  "Turn started",
  "Turn completed",
]);

export function visibleConversationTimelineItems(events: AgentEvent[], items: TimelineItem[]): TimelineItem[] {
  const hiddenKeys = new Set(
    events
      .filter((event) => HIDDEN_CONVERSATION_EVENT_TYPES.has(event.type))
      .map((event) => eventIdentity(event)),
  );
  return items.filter((item) => item.key === undefined || !hiddenKeys.has(String(item.key)));
}

// projectConversationEvents turns semantic events into conversation timeline
// items, hiding routine lifecycle noise and annotating compact ranges so
// callers can lazily expand them. Shared by the live Chat timeline and the
// read-only History view.
export function projectConversationEvents(events: AgentEvent[]): TimelineItem[] {
  const sourceEvents = events || [];
  const items = visibleConversationTimelineItems(sourceEvents, buildTimeline(sourceEvents) as TimelineItem[]);
  const byKey = new Map(sourceEvents.map((event) => [eventIdentity(event), event]));
  for (const item of items) {
    const event = byKey.get(String(item.key));
    applyPUAMessagePayload(item, event?.data?.payload);
    const range = event?.data?.compactRange as { start?: number; end?: number } | undefined;
    if (!range) continue;
    item.compact = true;
    item.rangeStartEventId = Number(range.start) || Number(event?.id) || 0;
    item.rangeEndEventId = Number(range.end) || item.rangeStartEventId;
  }
  return items;
}

export function applyPUAMessagePayload(item: TimelineItem, rawPayload: unknown): TimelineItem {
  if (item.kind !== "message" || !rawPayload || typeof rawPayload !== "object" || Array.isArray(rawPayload)) return item;
  const payload = rawPayload as Record<string, unknown>;
  if (payload.schema === "pua.queue-preview.v1") {
    item.role = "system";
    item.sender = undefined;
    return item;
  }
  if (payload.schema !== "pua.resource-message.v1") return item;
  if (typeof payload.text === "string") item.text = payload.text;
  if (typeof payload.role === "string" && payload.role.trim()) item.role = payload.role.trim().toLowerCase();
  if (payload.type === "user_chat_input") item.role = "user";
  if (payload.type === "queue_preview") item.role = "system";
  if (payload.sender && typeof payload.sender === "object" && !Array.isArray(payload.sender)) {
    const source = payload.sender as Record<string, unknown>;
    const sender: { name?: string; id?: string; sessionId?: string } = {};
    if (typeof source.name === "string" && source.name.trim()) sender.name = source.name.trim();
    if (typeof source.id === "string" && source.id.trim()) sender.id = source.id.trim();
    if (typeof source.sessionId === "string" && source.sessionId.trim()) sender.sessionId = source.sessionId.trim();
    item.sender = Object.keys(sender).length ? sender : undefined;
  } else {
    item.sender = undefined;
  }
  return item;
}

export function isHiddenConversationLifecycleText(value: string | undefined): boolean {
  const text = String(value || "");
  return HIDDEN_CONVERSATION_LIFECYCLE_TEXT.has(text) || text === "Agent connected" || text.startsWith("Agent connected ·");
}

// markTurnFinalAssistant annotates the assistant messages of a single turn's
// item list: the last assistant message is the turn's final reply
// (turnFinal=true) while earlier mid-turn progress updates get
// turnFinal=false, so the UI can mute their rail. Items of other roles pass
// through untouched. Blocks are already turn-scoped, so the last assistant
// message within a block is the turn's last message.
export function markTurnFinalAssistant(items: TimelineItem[]): TimelineItem[] {
  let lastAssistant = -1;
  for (let index = items.length - 1; index >= 0; index--) {
    if (items[index]?.kind === "message" && items[index]?.role === "assistant") {
      lastAssistant = index;
      break;
    }
  }
  return items.map((item, index) => {
    if (item.kind !== "message" || item.role !== "assistant") return item;
    const turnFinal = index === lastAssistant;
    return item.turnFinal === turnFinal ? item : { ...item, turnFinal };
  });
}

// markTurnAgentRuns annotates where the turn's bound agent starts talking.
// Reasoning and tool calls often precede the first assistant progress
// update; without this annotation the agent's name only rendered on the
// first message, visually attaching it to the progress update instead of
// the first event of the turn. The head of each uninterrupted run of
// agent-attributed activity (thinking, tools, approvals) gets agentStart
// and renders a header carrying the agent's name and the run's start time.
// Assistant messages belong to the run but always render their own meta
// row with the name and timestamp, so only non-message run heads carry the
// annotation. Runs break on user, system, and other-agent messages and on
// non-agent notices. Blocks are already turn-scoped, so runs never cross
// blocks.
export function markTurnAgentRuns(items: TimelineItem[]): TimelineItem[] {
  let inRun = false;
  return items.map((item) => {
    const attributed = item.kind === "activity" || item.kind === "thinking" || item.kind === "tools" || item.kind === "approval" ||
      (item.kind === "message" && item.role === "assistant");
    if (!attributed) {
      inRun = false;
      return item.agentStart ? { ...item, agentStart: false } : item;
    }
    const agentStart = !inRun && item.kind !== "message";
    inRun = true;
    return (item.agentStart ?? false) === agentStart ? item : { ...item, agentStart };
  });
}

// groupTimelineActivities is the compatibility seam between AgentHub Turn
// projection generations. New compact Turns already provide activity items;
// legacy compact Turns expose adjacent thinking/tool
// items. Grouping here gives both inputs the same one-level UI without
// rewriting old materialized history or requiring a source Event scan.
export function groupTimelineActivities(items: TimelineItem[]): TimelineItem[] {
  const grouped: TimelineItem[] = [];
  let run: TimelineItem[] = [];
  const flush = () => {
    if (!run.length) return;
    if (run.length === 1 && run[0].kind === "activity") grouped.push(run[0]);
    else grouped.push(activityFromRun(run));
    run = [];
  };
  for (const item of items) {
    if (item.kind === "activity" || item.kind === "thinking" || item.kind === "tools") run.push(item);
    else {
      flush();
      grouped.push(item);
    }
  }
  flush();
  return grouped;
}

function activityFromRun(run: TimelineItem[]): TimelineItem {
  const first = run[0];
  const last = run[run.length - 1];
  const children = run.flatMap((item) => item.kind === "activity" ? item.items || [] : [item]);
  const thinkingCount = run.reduce((total, item) => total + activityThinkingCount(item), 0);
  const reasoningUpdateCount = run.reduce((total, item) => total + activityReasoningUpdateCount(item), 0);
  const toolCallCount = run.reduce((total, item) => total + activityToolCallCount(item), 0);
  const starts = run.map((item) => Number(item.rangeStartEventId) || 0).filter((value) => value > 0);
  const ends = run.map((item) => Number(item.rangeEndEventId) || 0).filter((value) => value > 0);
  return {
    kind: "activity",
    key: first.key,
    generationId: first.generationId,
    items: children,
    time: last.time || first.time,
    startTime: first.startTime || first.time,
    active: run.some((item) => Boolean(item.active)) || children.some((item) => Boolean(item.active)),
    compact: run.some((item) => Boolean(item.compact)),
    rangeStartEventId: starts.length ? Math.min(...starts) : undefined,
    rangeEndEventId: ends.length ? Math.max(...ends) : undefined,
    thinkingCount,
    reasoningUpdateCount,
    toolCallCount,
  };
}

function activityThinkingCount(item: TimelineItem): number {
  if (item.kind === "thinking") return 1;
  if (item.kind !== "activity") return 0;
  if (item.thinkingCount !== undefined) return Math.max(0, Number(item.thinkingCount) || 0);
  return (item.items || []).filter((child) => child.kind === "thinking").length;
}

function activityReasoningUpdateCount(item: TimelineItem): number {
  if (item.kind === "thinking") return Math.max(1, Number(item.count) || 1);
  if (item.kind !== "activity") return 0;
  if (item.reasoningUpdateCount !== undefined) return Math.max(0, Number(item.reasoningUpdateCount) || 0);
  return (item.items || []).filter((child) => child.kind === "thinking").reduce((total, child) => total + Math.max(1, Number(child.count) || 1), 0);
}

function activityToolCallCount(item: TimelineItem): number {
  if (item.kind === "tools") return Math.max(0, Number(item.toolCallCount) || item.calls?.length || 0);
  if (item.kind !== "activity") return 0;
  if (item.toolCallCount !== undefined) return Math.max(0, Number(item.toolCallCount) || 0);
  return (item.items || []).filter((child) => child.kind === "tools").reduce((total, child) => total + Math.max(0, Number(child.toolCallCount) || child.calls?.length || 0), 0);
}

// formatClock renders the short wall-clock label shared by message meta
// rows and agent run headers. Timestamps from the current local day keep
// the bare hour/minute form; older ones also carry the date so distant
// history stays unambiguous. The date layout follows the existing UI
// convention (two-digit month/day plus hour/minute).
export function formatClock(value?: string, now?: Date): string {
  const date = new Date(value || "");
  if (Number.isNaN(date.valueOf())) return "";
  const reference = now || new Date();
  const sameDay = date.getFullYear() === reference.getFullYear() && date.getMonth() === reference.getMonth() && date.getDate() === reference.getDate();
  if (sameDay) return date.toLocaleTimeString("en-US", { hour: "2-digit", minute: "2-digit" });
  return date.toLocaleString(undefined, { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" });
}

export function mergeCanonicalEvents(events: AgentEvent[]): AgentEvent[] {
  const byId = new Map<string, AgentEvent>();
  for (const incoming of events) {
    const id = Number(incoming?.id) || 0;
    if (!id) continue;
    const key = eventIdentity(incoming);
    const existing = byId.get(key);
    byId.set(key, existing ? mergeEvent(existing, incoming) : normalizeAppendEvent(incoming));
  }
  return [...byId.values()].sort(compareEvents);
}

export function mergeCanonicalEvent(events: AgentEvent[], incoming: AgentEvent): AgentEvent[] {
  return mergeCanonicalEventBatch(events, [incoming]);
}

export function mergeCanonicalEventBatch(events: AgentEvent[], incomingEvents: AgentEvent[]): AgentEvent[] {
  if (!incomingEvents.length) return events;
  const next = [...events];
  for (const incoming of incomingEvents) mergeCanonicalEventInto(next, incoming);
  return next;
}

function mergeCanonicalEventInto(events: AgentEvent[], incoming: AgentEvent): void {
  const id = Number(incoming?.id) || 0;
  if (!id) return;
  const identity = eventIdentity(incoming);
  let low = 0;
  let high = events.length;
  while (low < high) {
    const middle = (low + high) >>> 1;
    if (compareEvents(events[middle], incoming) < 0) low = middle + 1;
    else high = middle;
  }
  let index = -1;
  let insertAt = low;
  while (insertAt < events.length && compareEvents(events[insertAt], incoming) === 0) {
    if (eventIdentity(events[insertAt]) === identity) {
      index = insertAt;
      break;
    }
    insertAt++;
  }
  if (index < 0) {
    events.splice(insertAt, 0, normalizeAppendEvent(incoming));
    return;
  }
  events[index] = mergeEvent(events[index], incoming);
}

export function compactTimelineEvents(events: AgentEvent[]): AgentEvent[] {
  return events;
}

function mergeEvent(existing: AgentEvent, incoming: AgentEvent): AgentEvent {
  if (incoming.data?.append !== true) return { ...incoming, startTime: incoming.startTime || existing.startTime };
  if (incoming.type === "tool.call") {
    const oldOutput = existing.data?.output as { text?: string } | undefined;
    const newOutput = incoming.data?.output as { text?: string; mode?: string } | undefined;
    if (newOutput?.mode === "append") {
      return {
        ...existing, ...incoming, startTime: incoming.startTime || existing.startTime,
        data: { ...existing.data, ...incoming.data, append: false, output: { ...newOutput, mode: "replace", text: `${oldOutput?.text || ""}${newOutput.text || ""}` } },
      };
    }
  }
  const currentText = typeof existing.data?.text === "string" ? existing.data.text : "";
  const fragment = typeof incoming.data.text === "string" ? incoming.data.text : "";
  return {
    ...existing,
    ...incoming,
    startTime: incoming.startTime || existing.startTime,
    data: { ...existing.data, ...incoming.data, append: false, text: currentText + fragment },
  };
}

function normalizeAppendEvent(event: AgentEvent): AgentEvent {
  if (event.data?.append !== true) return event;
  return { ...event, data: { ...event.data, append: false } };
}

function eventIdentity(event: AgentEvent): string {
  return event.semanticId || `${Number(event.id) || 0}:${Number(event.semanticIndex) || 0}`;
}

function compareEvents(left: AgentEvent, right: AgentEvent): number {
  return Number(left.id) - Number(right.id) || (Number(left.semanticIndex) || 0) - (Number(right.semanticIndex) || 0);
}
