import { describe, expect, it } from "vitest";

import { buildTimeline } from "../../src/components/timeline-projector";
import { buildTimeline as buildAgentHubTimeline } from "../../src/agenthub/core/timeline";
import type { AgentEvent, TimelineItem } from "../../src/components/models";
import { compactTimelineEvents, formatClock, groupTimelineActivities, isHiddenConversationLifecycleText, markTurnAgentRuns, markTurnFinalAssistant, mergeCanonicalEventBatch, mergeCanonicalEvents, projectConversationEvents, visibleConversationTimelineItems } from "../../src/components/timeline-events";

function toolUpdate(id: number, callId: string, text: string): AgentEvent {
  return {
    id, type: "tool.call", data: {
      schemaVersion: 1, callId, operation: "update", status: "running",
      output: { mode: "replace", text },
    },
  };
}

describe("timeline event algorithms", () => {
  it("displays old and new queue notices as system and formal Chat inputs as user", () => {
    const payloads = [
      { schema: "pua.queue-preview.v1" },
      { schema: "pua.resource-message.v1", type: "queue_preview", role: "system", text: "new preview" },
      { schema: "pua.resource-message.v1", type: "user_chat_input", role: "system", text: "old question", sender: { name: "reader" } },
      { schema: "pua.resource-message.v1", type: "user_chat_input", role: "user", text: "new question", sender: { name: "reader" } },
      { schema: "pua.resource-message.v1", type: "chat_notification", role: "system", text: "update" },
    ];
    const events = payloads.map((payload, index) => ({
      id: index + 1, type: "message.input", data: { text: "old preview", role: "user", payload },
    }));
    const items = projectConversationEvents(events);
    expect(items.map((item) => item.role)).toEqual(["system", "system", "user", "user", "system"]);
    expect(items.map((item) => item.text)).toEqual(["old preview", "new preview", "old question", "new question", "update"]);
    expect(items[0].sender).toBeUndefined();
    expect(items[2].sender).toEqual({ name: "reader" });
    expect(events[2].data.payload.role).toBe("system");
  });

  it.each([projectConversationEvents, buildAgentHubTimeline])("uses assistant completion as an invisible boundary without an unknown event (%#)", (project) => {
    const items = project([
      { id: 1, type: "message.assistant.delta", turnId: "t1", data: { text: "Progress" } },
      { id: 2, type: "message.assistant.completed", turnId: "t1", data: {} },
      { id: 3, type: "message.assistant.delta", turnId: "t1", data: { text: "Final" } },
      { id: 4, type: "message.assistant.delta", turnId: "t1", data: { text: " reply" } },
      { id: 5, type: "message.assistant.completed", turnId: "t1", data: {} },
    ]);
    expect(items).toHaveLength(2);
    expect(items.map((item) => [item.kind, item.text])).toEqual([["message", "Progress"], ["message", "Final reply"]]);
  });

  it("merges overlap, out-of-order events, append deltas, and full healing deterministically", () => {
    const events = mergeCanonicalEvents([
      { id: 4, type: "message.assistant.delta", time: "t1", data: { text: "hel" } },
      { id: 2, type: "message.input", data: { text: "earlier" } },
      { id: 4, type: "message.assistant.delta", time: "t2", startTime: "t0", data: { text: "lo", append: true } },
      { id: 3, type: "tool.started", data: { text: "tool" } },
      { id: 4, type: "message.assistant.delta", time: "t3", data: { text: "hello" } },
    ]);

    expect(events.map((event) => event.id)).toEqual([2, 3, 4]);
    expect(events[2]).toMatchObject({ time: "t3", startTime: "t0", data: { text: "hello" } });
  });

  it("inserts a batch by canonical id without mutating the published input", () => {
    const current: AgentEvent[] = [{ id: 2, type: "message.input" }, { id: 5, type: "message.input", data: { text: "old" } }];
    const merged = mergeCanonicalEventBatch(current, [
      { id: 4, type: "message.input" },
      { id: 5, type: "message.input", data: { text: "new" } },
      { id: 3, type: "message.input", data: { text: "delta", append: true } },
      { id: 0, type: "ignored" },
    ]);

    expect(merged.map((event) => event.id)).toEqual([2, 3, 4, 5]);
    expect(merged[1].data).toMatchObject({ text: "delta", append: false });
    expect(merged[3].data?.text).toBe("new");
    expect(current.map((event) => event.id)).toEqual([2, 5]);
  });

  it("returns the existing reference for an empty batch", () => {
    const events: AgentEvent[] = [{ id: 1, type: "message.input" }];
    expect(mergeCanonicalEventBatch(events, [])).toBe(events);
  });

  it("keeps provider-neutral tool updates without inspecting provider payloads", () => {
    const compacted = compactTimelineEvents([
      toolUpdate(1, "call-a", "a1"),
      toolUpdate(2, "call-b", "b1"),
      toolUpdate(3, "call-a", "a2"),
      { id: 4, type: "message.assistant.delta", data: { text: "boundary" } },
      toolUpdate(5, "call-a", "a3"),
      toolUpdate(6, "call-a", "a4"),
    ]);

    expect(compacted.map((event) => event.id)).toEqual([1, 2, 3, 4, 5, 6]);
    expect(compacted[2].data).toMatchObject({ callId: "call-a", output: { text: "a2" } });
    expect(compacted[5].data).toMatchObject({ callId: "call-a", output: { text: "a4" } });
  });

  it("renders retryable provider errors as informational lifecycle events", () => {
    const [item] = buildTimeline([{
      id: 7, semanticId: "sem_7_0", semanticIndex: 0, type: "provider.error",
      data: { message: "Connection interrupted", retryable: true },
    }]) as TimelineItem[];

    expect(item).toMatchObject({ kind: "lifecycle", tone: "info", key: "sem_7_0", text: "Connection interrupted" });
  });

  it("preserves omitted tool fields across semantic updates", () => {
    const [activity] = buildTimeline([
      { id: 8, type: "tool.call", turnId: "turn-1", data: { callId: "call-1", operation: "start", toolKind: "command", name: "Command", summary: "go test ./...", status: "running" } },
      { id: 9, type: "tool.call", turnId: "turn-1", data: { callId: "call-1", operation: "finish", status: "completed", output: { mode: "replace", text: "ok" } } },
    ]) as TimelineItem[];

    expect(activity.items?.[0].calls?.[0]).toMatchObject({ name: "Command", summary: "go test ./...", status: "completed", output: "ok" });
  });

  it("keeps multiple semantic facts from one source cursor independently addressable", () => {
    const events: AgentEvent[] = [
      { id: 9, semanticId: "sem_9_0", semanticIndex: 0, type: "turn.started", turnId: "turn-1" },
      { id: 9, semanticId: "sem_9_1", semanticIndex: 1, type: "message.input", turnId: "turn-1", data: { text: "same frame" } },
    ];
    const merged = mergeCanonicalEvents(events);
    const items = projectConversationEvents(merged);

    expect(merged).toHaveLength(2);
    expect(items).toHaveLength(1);
    expect(items[0]).toMatchObject({ key: "sem_9_1", kind: "message", text: "same frame" });
  });

  it("hides routine session and turn boundaries without breaking terminal tool settlement", () => {
    const events: AgentEvent[] = [
      { id: 1, type: "session.created" },
      { id: 2, type: "session.provider", data: { agentName: "gpt-5.6-sol", provider: "codex" } },
      { id: 3, type: "message.input", turnId: "turn-1", data: { text: "hello" } },
      { id: 4, type: "turn.started", turnId: "turn-1" },
      { id: 5, type: "tool.call", turnId: "turn-1", data: { schemaVersion: 1, callId: "call-1", operation: "start", toolKind: "command", name: "Command", summary: "make", status: "running" } },
      { id: 6, type: "turn.completed", turnId: "turn-1" },
      { id: 7, type: "session.state", data: { state: "stopped", reason: "completed" } },
    ];

    const items = visibleConversationTimelineItems(events, buildTimeline(events) as TimelineItem[]);

    expect(items.map((item) => item.text)).not.toContain("Session created");
    expect(items.map((item) => item.text)).not.toContain("Agent connected · gpt-5.6-sol · via codex");
    expect(items.map((item) => item.text)).not.toContain("Turn started");
    expect(items.map((item) => item.text)).not.toContain("Turn completed");
    expect(items.find((item) => item.kind === "message")?.text).toBe("hello");
    expect(items.find((item) => item.kind === "activity")?.items?.find((item) => item.kind === "tools")?.calls?.[0].status).toBe("completed");
    expect(items.find((item) => item.kind === "lifecycle")?.text).toBe("Session stopped · provider completed");
  });

  it("uses PUA's opaque payload for presentation while preserving provider text", () => {
    const events: AgentEvent[] = [{
      id: 1,
      type: "message.input",
      data: {
        schemaVersion: 2,
        text: "Message from agent \"Review Agent\":\noriginal request",
        payload: {
          schema: "pua.resource-message.v1",
          text: "original request",
          role: "agent",
          sender: { id: "project1.task2", name: "Review Agent", sessionId: "ses-worker" },
        },
      },
    }];

    const [item] = projectConversationEvents(events);

    expect(item).toMatchObject({
      kind: "message",
      text: "original request",
      role: "agent",
      sender: { id: "project1.task2", name: "Review Agent", sessionId: "ses-worker" },
    });
    expect(events[0].data?.text).toBe("Message from agent \"Review Agent\":\noriginal request");
  });

  it("hides raw lifecycle names returned by compact history", () => {
    expect([
      "session.created",
      "session.provider",
      "session.launch-environment",
      "turn.started",
      "turn.completed",
    ].every((value) => isHiddenConversationLifecycleText(value))).toBe(true);
  });

  it("groups legacy compact thinking and tools without crossing visible boundaries", () => {
    const legacy: TimelineItem[] = [
      { kind: "thinking", key: "thought-a", count: 2, compact: true, rangeStartEventId: 2, rangeEndEventId: 3 },
      { kind: "tools", key: "tools-a", compact: true, toolCallCount: 2, rangeStartEventId: 4, rangeEndEventId: 6, calls: [] },
      { kind: "thinking", key: "thought-b", count: 1, compact: true, rangeStartEventId: 7, rangeEndEventId: 7 },
      { kind: "message", key: "reply", role: "assistant", text: "done" },
      { kind: "tools", key: "tools-b", compact: true, toolCallCount: 1, rangeStartEventId: 9, rangeEndEventId: 10, calls: [] },
    ];

    const grouped = groupTimelineActivities(legacy);

    expect(grouped.map((item) => item.kind)).toEqual(["activity", "message", "activity"]);
    expect(grouped[0]).toMatchObject({ thinkingCount: 2, reasoningUpdateCount: 3, toolCallCount: 2, rangeStartEventId: 2, rangeEndEventId: 7, compact: true });
    expect(grouped[0].items?.map((item) => item.kind)).toEqual(["thinking", "tools", "thinking"]);
    expect(grouped[2]).toMatchObject({ thinkingCount: 0, toolCallCount: 1, rangeStartEventId: 9, rangeEndEventId: 10 });
  });

  it("marks only the last assistant message of a turn as final", () => {
    const items: TimelineItem[] = [
      { kind: "message", role: "user", text: "question" },
      { kind: "message", role: "assistant", text: "progress" },
      { kind: "tools", key: "group-a" },
      { kind: "message", role: "assistant", text: "final reply" },
    ];

    const marked = markTurnFinalAssistant(items);

    expect(marked[0]).toBe(items[0]);
    expect(marked[1].turnFinal).toBe(false);
    expect(marked[2]).toBe(items[2]);
    expect(marked[3].turnFinal).toBe(true);
    // Non-assistant items pass through untouched.
    expect(items[0].turnFinal).toBeUndefined();
  });

  it("keeps item identity when turnFinal annotations are already correct", () => {
    const items: TimelineItem[] = [
      { kind: "message", role: "assistant", text: "progress", turnFinal: false },
      { kind: "message", role: "assistant", text: "final reply", turnFinal: true },
    ];

    const marked = markTurnFinalAssistant(items);

    expect(marked[0]).toBe(items[0]);
    expect(marked[1]).toBe(items[1]);
  });

  it("leaves a turn without assistant messages untouched", () => {
    const items: TimelineItem[] = [{ kind: "message", role: "user", text: "question" }];
    expect(markTurnFinalAssistant(items)[0]).toBe(items[0]);
  });

  it("attaches the agent run start to the first event before any progress update", () => {
    const items: TimelineItem[] = [
      { kind: "message", role: "user", text: "question" },
      { kind: "thinking", text: "reasoning" },
      { kind: "tools", calls: [] },
      { kind: "message", role: "assistant", text: "progress update" },
      { kind: "message", role: "assistant", text: "final reply" },
    ];

    const marked = markTurnAgentRuns(items);

    expect(marked[0]).toBe(items[0]);
    expect(marked[1]).toMatchObject({ agentStart: true });
    // Continuation items and assistant messages need no annotation, so
    // they pass through untouched: messages always render their own meta
    // row with the name and timestamp.
    expect(marked[2]).toBe(items[2]);
    expect(marked[3]).toBe(items[3]);
    expect(marked[4]).toBe(items[4]);
    expect(items[1].agentStart).toBeUndefined();
  });

  it("starts a new agent run after user, system, or other-agent messages", () => {
    const items: TimelineItem[] = [
      { kind: "message", role: "assistant", text: "progress" },
      { kind: "approval", approvalId: "a1" },
      { kind: "message", role: "user", text: "steer", steer: true },
      { kind: "thinking", text: "reconsidering" },
      { kind: "message", role: "agent", text: "delegated", sender: { name: "Builder" } },
      { kind: "message", role: "assistant", text: "final reply" },
    ];

    const marked = markTurnAgentRuns(items);

    // A run that opens with an assistant message needs no header: the
    // message's own meta row already labels the agent, so unannotated
    // items pass through untouched.
    expect(marked[0]).toBe(items[0]);
    expect(marked[1]).toBe(items[1]);
    expect(marked[2]).toBe(items[2]);
    expect(marked[3]).toMatchObject({ agentStart: true });
    expect(marked[4]).toBe(items[4]);
    expect(marked[5]).toBe(items[5]);
  });

  it("keeps item identity when agent run annotations are already correct", () => {
    const items: TimelineItem[] = [
      { kind: "thinking", text: "reasoning", agentStart: true },
      { kind: "message", role: "assistant", text: "final reply", agentStart: false },
    ];

    const marked = markTurnAgentRuns(items);

    expect(marked[0]).toBe(items[0]);
    expect(marked[1]).toBe(items[1]);
  });
});

describe("formatClock", () => {
  const now = new Date("2026-08-22T17:30:00");

  it("returns an empty label for missing or invalid timestamps", () => {
    expect(formatClock()).toBe("");
    expect(formatClock("not-a-date")).toBe("");
    expect(formatClock("", now)).toBe("");
  });

  it("shows only the time for messages from the same local day", () => {
    const label = formatClock("2026-08-22T09:05:00", now);
    expect(label).toBe(new Date("2026-08-22T09:05:00").toLocaleTimeString("en-US", { hour: "2-digit", minute: "2-digit" }));
    expect(label).not.toMatch(/\d{2}\/\d{2}/);
  });

  it("shows the date and time for messages from an earlier day", () => {
    const date = new Date("2026-08-10T09:05:00");
    const label = formatClock("2026-08-10T09:05:00", now);
    expect(label).toBe(
      date.toLocaleString(undefined, { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }),
    );
    // The label must carry the date on top of the bare clock.
    expect(label).toContain("08");
    expect(label).toContain("10");
    expect(label).not.toBe(date.toLocaleTimeString("en-US", { hour: "2-digit", minute: "2-digit" }));
  });

  it("treats the cross-day boundary by local calendar day", () => {
    // 23:59 the previous day already carries a date, while 00:00 on the
    // reference day keeps the bare clock even at the day boundary.
    const previous = formatClock("2026-08-21T23:59:00", now);
    expect(previous).toBe(
      new Date("2026-08-21T23:59:00").toLocaleString(undefined, { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" }),
    );
    const midnight = formatClock("2026-08-22T00:00:00", now);
    expect(midnight).toBe(new Date("2026-08-22T00:00:00").toLocaleTimeString("en-US", { hour: "2-digit", minute: "2-digit" }));
  });
});
