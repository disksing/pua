import type { ConversationBlock, ResourceHistoryGeneration, TimelineItem, WaitingMessage } from "./models";
import { projectConversationEvents } from "./timeline-events";

export interface SharedChatMessage {
  id: string;
  role: string;
  sender?: string;
  text: string;
  createdAt: string;
  inputMessageIds?: string[];
  turnId?: string;
  startEventId?: number;
}

export interface HistoryRow {
  kind: "history";
  key: string;
  time: number;
  block: ConversationBlock;
  input?: SharedChatMessage;
}

type TimelineRow = { kind: "message"; key: string; time: number; message: SharedChatMessage } | HistoryRow;
type GenerationBoundary = { kind: "generation"; key: string; time: number; generation: ResourceHistoryGeneration };
type TimelineSection = { key: string; generation?: ResourceHistoryGeneration; rows: TimelineRow[] };

export interface TimelineSource {
  role: string;
  label: string;
  icon: string;
  sender: string;
  text: string;
}

function source(role = "", type = "", sender?: WaitingMessage["sender"], text = "", sourceResourceId = ""): TimelineSource {
  role = role.trim().toLowerCase();
  const id = sender?.id?.trim() || (role === "user" ? "" : sourceResourceId);
  const name = sender?.name?.trim() || id;
  if (type === "chat_notification") return { role: "user", label: "User notification", icon: "user", sender: name, text };
  if (id === "scheduler" || type === "scheduler_tick" || type === "schedule_exact_due" || role === "scheduler") {
    return { role: "scheduler", label: "Scheduler", icon: "calendar-clock", sender: name, text };
  }
  if (role === "agent") return { role, label: "Agent", icon: "bot", sender: name, text };
  if (role === "system") return { role, label: "System", icon: "bell", sender: name, text };
  if (role === "user") return { role, label: "User", icon: "user", sender: name, text };
  return { role: "unknown", label: "Unknown source", icon: "circle-help", sender: name, text };
}

export function turnSource(block: ConversationBlock, input?: SharedChatMessage): TimelineSource {
  const turn = block.turn;
  return turn
    ? source(input ? "user" : turn.triggerRole, turn.triggerType, turn.triggerSender || (input ? { name: input.sender } : undefined), input?.text || legacyInputText(turn.triggerPreview))
    : source("system", "", undefined, "Session events");
}

export function queueSource(message: WaitingMessage): TimelineSource {
  return source(message.type === "user_chat_input" ? "user" : message.role, message.type, message.sender, legacyInputText(message.text), message.causation?.sourceResourceId);
}

export function turnWorkingLabel(row: HistoryRow): string {
  if (row.block.turn?.triggerType === "chat_notification") return "Agent is updating you";
  switch (turnSource(row.block, row.input).role) {
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

export function timelineSections(messages: SharedChatMessage[], blocks: ConversationBlock[]): TimelineSection[] {
  const byInput = new Map(messages.flatMap(message => (message.inputMessageIds || []).map(id => [id, message] as const)));
  const generations = new Map<string, GenerationBoundary>();
  const rows: (TimelineRow | GenerationBoundary)[] = messages.map(message => ({ kind: "message", key: `message:${message.id}`, time: timestamp(message.createdAt), message }));
  for (const block of blocks) {
    const events = block.events || [];
    const firstEvent = events[0];
    if (!block.turn && block.kind !== "gap" && (!firstEvent || firstEvent.turnId || !projectConversationEvents(events).length)) continue;
    const input = byInput.get(block.turn?.triggerMessageId || "");
    const time = timestamp(block.turn?.startedAt || firstEvent?.time || block.generation.createdAt);
    const id = block.generation.generationId;
    const marker = generations.get(id);
    const boundary = Math.min(time, timestamp(block.generation.createdAt) || time);
    if (!marker || boundary < marker.time) generations.set(id, { kind: "generation", key: `generation:${id}`, generation: block.generation, time: boundary });
    rows.push({ kind: "history", key: block.key, time, block, input });
  }
  rows.push(...generations.values());
  const order = { generation: 0, message: 1, history: 2 };
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
