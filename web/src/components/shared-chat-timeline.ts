import type { ConversationBlock, ResourceHistoryGeneration, TimelineItem, WaitingMessage } from "./models";

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

type TimelineRow =
  | { kind: "message"; key: string; time: number; message: SharedChatMessage }
  | { kind: "generation"; key: string; time: number; generation: ResourceHistoryGeneration }
  | { kind: "history"; key: string; time: number; block: ConversationBlock; input?: SharedChatMessage };

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

export function legacyInputText(text?: string): string {
  if (!text?.startsWith("User Chat input for resource ")) return text || "";
  const boundary = text.indexOf("\nReply: pua chat publish --reply-to=");
  const body = boundary >= 0 ? text.indexOf("\n\n", boundary) : -1;
  return body >= 0 ? text.slice(body + 2).replace(/^User message [^\n]+:\n/, "") : "Legacy Chat input";
}

export function timelineRows(messages: SharedChatMessage[], blocks: ConversationBlock[]): TimelineRow[] {
  const byInput = new Map(messages.flatMap(message => (message.inputMessageIds || []).map(id => [id, message] as const)));
  const generations = new Map<string, { generation: ResourceHistoryGeneration; time: number }>();
  const rows: TimelineRow[] = messages.map(message => ({ kind: "message", key: `message:${message.id}`, time: timestamp(message.createdAt), message }));
  for (const block of blocks) {
    const firstEvent = block.events?.[0];
    if (!block.turn && block.kind !== "gap" && (!firstEvent || firstEvent.turnId)) continue;
    const input = byInput.get(block.turn?.triggerMessageId || "");
    const time = timestamp(block.turn?.startedAt || firstEvent?.time || block.generation.createdAt);
    const id = block.generation.generationId;
    const marker = generations.get(id);
    if (!marker || time < marker.time) generations.set(id, { generation: block.generation, time });
    rows.push({ kind: "history", key: block.key, time, block, input });
  }
  for (const { generation, time } of generations.values()) {
    rows.push({ kind: "generation", key: `generation:${generation.generationId}`, time, generation });
  }
  const order = { generation: 0, message: 1, history: 2 };
  return rows.sort((left, right) => left.time - right.time || order[left.kind] - order[right.kind]);
}

function timestamp(value?: string): number {
  return Date.parse(value || "") || 0;
}

export function publicMessageIndex(messages: SharedChatMessage[]): Set<string> {
  const keys = new Set<string>();
  for (const message of messages) {
    for (const input of message.inputMessageIds || []) keys.add(input);
    if (message.turnId && message.startEventId) keys.add(`${message.turnId}:${message.startEventId}`);
  }
  return keys;
}

export function historyItem(item: TimelineItem, turnId: string, publicMessages: Set<string>): TimelineItem | undefined {
  if (item.kind !== "message") return item;
  if (item.role === "user" && item.messageId && publicMessages.has(item.messageId)) return;
  if (item.role === "assistant" && item.startEventId && publicMessages.has(`${turnId}:${item.startEventId}`)) return;
  return item.role === "user" ? { ...item, text: legacyInputText(item.text) } : item;
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
