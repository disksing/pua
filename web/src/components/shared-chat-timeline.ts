import type { ConversationBlock, ResourceHistoryGeneration, ResourceHistoryTurnSummary, TimelineItem, TurnResumeSource, WaitingMessage } from "./models";
import { projectConversationEvents } from "./timeline-events";

const SESSION_NOTICE_EVENTS = new Set(["message.input", "provider.error", "session.archived", "session.state"]);

export interface SharedChatMessage {
  id: string;
  role: string;
  sender?: string;
  text: string;
  createdAt: string;
  inputMessageIds?: string[];
  turnId?: string;
  startEventId?: number;
  timelineAt?: string;
  continuation?: { id: string; text: string };
}

export interface HistoryRow {
  kind: "history";
  key: string;
  time: number;
  block: ConversationBlock;
  turn: ResourceHistoryTurnSummary;
  input?: SharedChatMessage;
}

type TimelineRow =
  | { kind: "message"; key: string; time: number; message: SharedChatMessage }
  | { kind: "gap" | "session"; key: string; time: number; block: ConversationBlock }
  | HistoryRow;
type GenerationBoundary = { kind: "generation"; key: string; time: number; generation: ResourceHistoryGeneration };
type TimelineSection = { key: string; generation?: ResourceHistoryGeneration; rows: TimelineRow[] };

export interface TimelineSource {
  role: string;
  label: string;
  icon: string;
  sender: string;
  text: string;
  resumed: boolean;
}

function source(trigger: TurnResumeSource, text: string, resumeSource?: TurnResumeSource): TimelineSource {
  const resumed = trigger.type === "turn_resume" || resumeSource !== undefined;
  const { role: rawRole = "", type, sender, sourceResourceId } = resumed ? resumeSource || {} : trigger;
  const role = type === "user_chat_input" ? "user" : rawRole.trim().toLowerCase();
  const senderId = sender?.id?.trim();
  const id = senderId || sourceResourceId?.trim() || "";
  const details = { sender: sender?.name?.trim() || senderId || (role === "agent" ? id : ""), text, resumed };
  if (type === "chat_notification") return { role: "notification", label: "User notification", icon: "message-square-dot", ...details };
  if (role === "scheduler" || type === "scheduler_tick" || type === "schedule_exact_due" ||
      (id === "scheduler" && (role === "agent" || role === "system"))) {
    return { role: "scheduler", label: "Scheduler", icon: "calendar-clock", ...details };
  }
  if (role === "agent") {
    if (id === "workspace") return { role, label: "Workspace Agent", icon: "building-2", ...details };
    if (/^project\d+$/.test(id)) return { role, label: "Project Agent", icon: "folder-kanban", ...details };
    if (/^project\d+\.task\d+$/.test(id)) return { role, label: "Task Agent", icon: "target", ...details };
    return { role, label: "Agent", icon: "bot", ...details };
  }
  if (role === "system") return { role, label: "System", icon: "bell", ...details };
  if (role === "user") return { role, label: "User", icon: "user", ...details };
  return { role: "unknown", label: "Unknown source", icon: "circle-help", ...details };
}

export function turnSource(turn: ResourceHistoryTurnSummary, input?: SharedChatMessage): TimelineSource {
  return source({
    role: input ? "user" : turn.triggerRole, type: turn.triggerType,
    sender: turn.triggerSender || (input ? { name: input.sender } : undefined), sourceResourceId: turn.triggerSourceResourceId,
  }, input?.text || legacyInputText(turn.triggerPreview), turn.resumeSource);
}

export function queueSource(message: WaitingMessage): TimelineSource {
  return source({ role: message.role, type: message.type, sender: message.sender, sourceResourceId: message.causation?.sourceResourceId },
    legacyInputText(message.text), message.resumeSource);
}

export function turnWorkingLabel(row: HistoryRow): string {
  const source = turnSource(row.turn, row.input);
  if (source.resumed) return "Agent is resuming work";
  switch (source.role) {
    case "notification": return "Agent is updating you";
    case "user": return "Agent is replying";
    case "agent": return "Agent is handling an agent message";
    case "scheduler": return "Agent is running scheduled work";
    case "system": return "Agent is handling a system message";
    default: return "Agent is working…";
  }
}

export function legacyInputText(text?: string): string {
  if (!text?.startsWith("User Chat input for resource ")) return text || "";
  const boundary = text.indexOf("\nReply: pua chat publish --reply-to=");
  const body = boundary >= 0 ? text.indexOf("\n\n", boundary) : -1;
  return body >= 0 ? text.slice(body + 2).replace(/^User message [^\n]+:\n/, "") : "Legacy Chat input";
}

export function timelineSections(messages: SharedChatMessage[], blocks: ConversationBlock[], olderHeaders: ResourceHistoryGeneration[] = []): TimelineSection[] {
  const byInput = new Map(messages.flatMap(message => (message.inputMessageIds || []).map(id => [id, message] as const)));
  const generations = new Map<string, GenerationBoundary>();
  const bubbles = new Map<string, SharedChatMessage>();
  for (const message of messages) {
    const key = message.continuation?.id || message.id;
    const previous = bubbles.get(key);
    if (!previous || (message.startEventId || 0) > (previous.startEventId || 0)) {
      bubbles.set(key, message);
    }
  }
  const rows: (TimelineRow | GenerationBoundary)[] = [...bubbles].map(([id, message]) => ({
    kind: "message", key: `message:${id}`, time: timestamp(message.timelineAt || message.createdAt),
    message: message.continuation ? { ...message, text: message.continuation.text } : message,
  }));
  for (const block of blocks) {
    const { key, turn, events = [] } = block;
    const time = timestamp(turn?.startedAt || events[0]?.time || block.generation.createdAt);
    if (block.kind === "gap") {
      rows.push({ kind: "gap", key, time, block });
    } else if (turn) {
      rows.push({ kind: "history", key, time, block, turn, input: byInput.get(turn.triggerMessageId || "") });
    } else {
      const items = projectConversationEvents(events.filter(event => !event.turnId && SESSION_NOTICE_EVENTS.has(event.type)))
        .filter(item => item.kind !== "message" || item.role === "system");
      if (!items.length) continue;
      rows.push({ kind: "session", key, time, block: { ...block, events: undefined, items } });
    }
    const id = block.generation.generationId;
    const marker = generations.get(id);
    const boundary = Math.min(time, timestamp(block.generation.createdAt) || time);
    if (!marker || boundary < marker.time) generations.set(id, { kind: "generation", key: `generation:${id}`, generation: block.generation, time: boundary });
  }
  for (const generation of olderHeaders) {
    const id = generation.generationId;
    if (!generations.has(id)) generations.set(id, { kind: "generation", key: `generation:${id}`, generation, time: timestamp(generation.createdAt) });
  }
  rows.push(...generations.values());
  const order = { generation: 0, message: 1, history: 2, gap: 2, session: 2 };
  rows.sort((left, right) => left.time - right.time || order[left.kind] - order[right.kind]);
  const sections: TimelineSection[] = [];
  let section: TimelineSection = { key: "before-generations", rows: [] };
  for (const row of rows) {
    if (row.kind === "generation") {
      if (section.generation || section.rows.length) sections.push(section);
      section = { key: row.key, generation: row.generation, rows: [] };
    } else {
      section.rows.push(row);
    }
  }
  if (section.generation || section.rows.length) sections.push(section);
  return sections;
}

function timestamp(value?: string): number {
  return Date.parse(value || "") || 0;
}

export function captureChatAnchor(container: HTMLElement, root: HTMLElement): () => void {
  const top = container.getBoundingClientRect().top;
  const anchor = [...root.querySelectorAll<HTMLElement>("[data-chat-row]")].find(row => row.getBoundingClientRect().bottom > top);
  const key = anchor?.dataset.chatRow;
  const offset = anchor?.getBoundingClientRect().top ?? 0;
  const height = container.scrollHeight;
  return () => {
    const row = anchor?.isConnected ? anchor : key
      ? root.querySelector<HTMLElement>(`[data-chat-row="${CSS.escape(key)}"]`)
      : null;
    container.scrollTop += row ? row.getBoundingClientRect().top - offset : container.scrollHeight - height;
  };
}

export function historyItem(item: TimelineItem): TimelineItem {
  return item.kind === "message" && item.role === "user" ? { ...item, text: legacyInputText(item.text) } : item;
}

export function positionTimelinePreview(preview: HTMLElement, scroller?: HTMLElement) {
  const point = preview.previousElementSibling?.getBoundingClientRect();
  if (!point) return;
  const viewport = document.documentElement;
  const bounds = scroller?.getBoundingClientRect();
  if (bounds && (point.bottom <= bounds.top || point.top >= bounds.bottom)) {
    preview.hidePopover();
    return;
  }
  const box = preview.getBoundingClientRect();
  preview.style.left = `${Math.max(8, Math.min(point.left, viewport.clientWidth - box.width - 8))}px`;
  preview.style.top = `${point.bottom + box.height + 6 <= viewport.clientHeight ? point.bottom + 6 : Math.max(8, point.top - box.height - 6)}px`;
}
