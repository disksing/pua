import { readFileSync } from "node:fs";

import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import EventTimeline from "../../src/components/EventTimeline.svelte";
import { createModelChannel } from "../../src/components/model-channel";
import HistoryTimeline from "../../src/components/HistoryTimeline.svelte";
import type { AgentEvent, AgentSemanticFrame, AgentTurnItem, EventTimelineModel, ResourceMessageStatus, TimelineItem } from "../../src/components/models";
import { formatClock, projectConversationEvents } from "../../src/components/timeline-events";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  delete window.marked;
  delete window.DOMPurify;
  document.body.replaceChildren();
  vi.unstubAllGlobals();
});

function loadVendor<T>(relativePath: string, globalName: string): T {
  const source = readFileSync(new URL(relativePath, import.meta.url), "utf8");
  return new Function("window", "globalThis", `const module=undefined,exports=undefined,define=undefined;${source}\nreturn globalThis[${JSON.stringify(globalName)}];`)(window, window) as T;
}

function installMarkdownVendors(): void {
  window.marked = loadVendor<NonNullable<Window["marked"]>>("../../static/vendor/marked/marked.min.js", "marked");
  window.DOMPurify = loadVendor<NonNullable<Window["DOMPurify"]>>("../../static/vendor/dompurify/purify.min.js", "DOMPurify");
}

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  onmessage: ((event: MessageEvent) => void) | null = null;
  onerror: ((event: Event) => void) | null = null;
  closed = false;
  constructor(readonly url: string) { FakeEventSource.instances.push(this); }
  addEventListener(): void {}
  close(): void { this.closed = true; }
}

function semanticFrame(event: AgentEvent, mode: AgentSemanticFrame["mode"] = "replace"): AgentSemanticFrame {
  const sessionId = event.sessionId || "session-test";
  return {
    schema: "agenthub.semantic-events.v1", cursor: event.id, mode,
    source: { eventId: event.id, type: event.type, sessionId, turnId: event.turnId, time: event.time },
    events: [{
      id: `sem_${event.id}_0`, sourceEventId: event.id, index: 0, type: event.type,
      time: event.time, sessionId, turnId: event.turnId, data: event.data,
    }],
  };
}

function semanticPage(events: AgentEvent[]) {
  const latest = events.reduce((value, event) => Math.max(value, event.id), 0);
  return { schema: "agenthub.semantic-events.v1", frames: events.map((event) => semanticFrame(event)), page: { hasMore: false, nextAfter: latest }, latestCursor: latest };
}

function project(events: AgentEvent[]): TimelineItem[] {
  return events.map((event) => ({ kind: "message", key: event.id, role: "assistant", text: String(event.data?.text || "") }));
}

function status(resourceId: string, generationId = `gen-${resourceId}`, generation = 1, publicSessionState: ResourceMessageStatus["sessionState"] = "idle", generationStatus = "idle", sessionState = "idle"): ResourceMessageStatus {
  return {
    resourceId, sessionState: publicSessionState, acceptsMessages: true, canSteerWaiting: false, waitingMessages: [],
    generation: { generation, generationId, status: generationStatus },
    session: { id: `session-${resourceId}`, state: sessionState, currentTurnId: sessionState === "running" ? "turn-1" : undefined },
  };
}

function model(resourceId: string, nextStatus = status(resourceId), projector: (events: AgentEvent[]) => TimelineItem[] = project): EventTimelineModel {
  return {
    identity: `workspace-a:${resourceId}`, workspaceId: "workspace-a", resourceId,
    status: nextStatus, submitting: false,
    agentName: "Test Agent", resolveResourceTitle: () => null, onNavigate: vi.fn(), project: projector, onEvent: vi.fn(), onNotice: vi.fn(), onApproval: vi.fn(async () => undefined), onToast: vi.fn(),
  };
}

function history(resourceId: string, items: AgentTurnItem[] = [{ type: "message", role: "user", text: `message ${resourceId}`, startEventId: 1, endEventId: 1, startedAt: "2026-08-12T00:00:00Z", endedAt: "2026-08-12T00:00:00Z" }]) {
  const generation = { generation: 1, generationId: `gen-${resourceId}`, title: resourceId, status: "idle", createdAt: "2026-08-12T00:00:00Z", updatedAt: "2026-08-12T00:00:00Z" };
  const turn = { reference: `ref-${resourceId}`, turnId: "turn-1", status: "completed", closed: true, startedAt: "2026-08-12T00:00:00Z", durationMs: 10, triggerPreview: `summary ${resourceId}`, eventCount: 2, toolEventCount: 0, startEventId: 1, lastEventId: 2, endEventId: 2, generation };
  return { generation, turn, page: { resourceId, segments: [{ generation, turns: [turn] }], page: { limit: 20, hasMore: false } }, detail: { turn, latestEventId: 2, items } };
}

function generationModel(resourceId: string, generation: number, generationId: string, agentName: string): EventTimelineModel {
  const value = model(resourceId);
  value.agentName = agentName;
  value.status = {
    ...value.status!,
    resolvedAgent: agentName,
    generation: { ...value.status!.generation!, generation, generationId },
  };
  return value;
}

function multiGenerationHistory(resourceId: string) {
  const agents = ["deepseek", "codex", "deepseek"];
  const generations = agents.map((agentName, index) => ({
    generation: index + 1,
    generationId: `gen-${index + 1}`,
    title: `${resourceId} generation ${index + 1}`,
    agentName,
    status: "idle",
    createdAt: `2026-08-1${2 + index}T00:00:00Z`,
    updatedAt: `2026-08-1${2 + index}T00:00:00Z`,
  }));
  const turns = generations.map((generation, index) => ({
    reference: `ref-${index + 1}`,
    turnId: `turn-${index + 1}`,
    status: "completed",
    closed: true,
    startedAt: generation.createdAt,
    durationMs: 10,
    triggerPreview: `summary ${index + 1}`,
    eventCount: 2,
    toolEventCount: 0,
    startEventId: index * 2 + 1,
    lastEventId: index * 2 + 2,
    endEventId: index * 2 + 2,
    generation,
  }));
  const details = turns.map((turn, index) => ({
    turn,
    latestEventId: turn.lastEventId,
    items: [{ type: "message", role: "assistant", text: `${agents[index]} reply`, startEventId: turn.startEventId, endEventId: turn.lastEventId, startedAt: turn.startedAt, endedAt: turn.startedAt }],
  }));
  return {
    page: { resourceId, segments: generations.map((generation, index) => ({ generation, turns: [turns[index]] })), page: { limit: 20, hasMore: false } },
    details,
  };
}

function conversationAuthors(target: HTMLElement): string[] {
  return [...target.querySelectorAll<HTMLElement>("section.conversation-turn .agent-message-meta strong")].map((node) => node.textContent || "");
}

// mockScrollMetrics gives a jsdom element browser-like scroll geometry:
// scrollTop assignments clamp into the valid range, and tests may change
// scrollHeight/clientHeight to simulate content growth or layout shifts.
function mockScrollMetrics(element: HTMLElement, initial: { scrollHeight: number; clientHeight: number }) {
  const metrics = { scrollHeight: initial.scrollHeight, clientHeight: initial.clientHeight, scrollTop: 0 };
  Object.defineProperty(element, "scrollHeight", { configurable: true, get: () => metrics.scrollHeight });
  Object.defineProperty(element, "clientHeight", { configurable: true, get: () => metrics.clientHeight });
  Object.defineProperty(element, "scrollTop", {
    configurable: true,
    get: () => metrics.scrollTop,
    set: (value: number) => { metrics.scrollTop = Math.max(0, Math.min(value, metrics.scrollHeight - metrics.clientHeight)); },
  });
  return metrics;
}

function historyGeneration(resourceId: string, generation: number, generationId: string, generationStatus: string) {
  const generationRecord = { generation, generationId, title: resourceId, status: generationStatus, createdAt: "2026-08-12T00:00:00Z", updatedAt: "2026-08-12T00:00:00Z" };
  const turn = { reference: `ref-${resourceId}-${generation}`, turnId: `turn-${generation}`, status: "completed", closed: true, startedAt: "2026-08-12T00:00:00Z", durationMs: 10, triggerPreview: `summary ${resourceId}`, eventCount: 2, toolEventCount: 0, startEventId: 1, lastEventId: 2, endEventId: 2, generation: generationRecord };
  return {
    generation: generationRecord,
    page: { resourceId, segments: [{ generation: generationRecord, turns: [turn] }], page: { limit: 20, hasMore: false } },
    detail: { turn, latestEventId: 2, items: [{ type: "message", role: "user", text: `message ${resourceId}`, startEventId: 1, endEventId: 1, startedAt: turn.startedAt, endedAt: turn.startedAt }] },
  };
}

describe("EventTimeline", () => {
  it("renders an unbound starting session as progress instead of a history error", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const starting = status("task-a", "gen-task-a", 1, "working", "starting", "starting");
    delete starting.session;
    const generation = { generation: 1, generationId: "gen-task-a", title: "task-a", status: "starting", createdAt: "2026-08-12T00:00:00Z", updatedAt: "2026-08-12T00:00:00Z" };
    const page = {
      resourceId: "task-a",
      segments: [{ generation, turns: [], gap: { code: "session_starting", message: "generation is waiting for its AgentHub Session to start", retryable: true } }],
      page: { limit: 5, hasMore: false },
    };
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify(page), { status: 200, headers: { "content-type": "application/json" } })));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel: createModelChannel(model("task-a", starting)) } });
    cleanups.push(() => unmount(component));

    const progress = await vi.waitFor(() => {
      const value = target.querySelector<HTMLElement>(".conversation-session-starting");
      expect(value?.textContent).toContain("Starting agent…");
      return value!;
    });
    expect(progress.getAttribute("role")).toBe("status");
    expect(progress.querySelector("[data-lucide='loader-circle']")).not.toBeNull();
    expect(target.querySelector(".conversation-gap")).toBeNull();
    expect(target.textContent).not.toContain("History unavailable");
  });

  it("opens a workspace file link from an assistant message in a read-only preview", async () => {
    installMarkdownVendors();
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = history("task-a", [{
      type: "message", role: "assistant", text: "[report](/project1/task388/artifacts/report.md)",
      startEventId: 1, endEventId: 1, startedAt: "2026-08-12T00:00:00Z", endedAt: "2026-08-12T00:00:00Z",
    }]);
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      if (path.includes("/files?path=")) return new Response(JSON.stringify({
        path: "project1-pua/task388/artifacts/report.md", name: "report.md", size: 18,
        binary: false, image: false, truncated: false, mimeType: "text/markdown", contentHash: "hash", content: "# Previewed report",
      }), { status: 200, headers: { "content-type": "application/json" } });
      return new Response(JSON.stringify(path.includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchMock);
    const viewModel = model("task-a");
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel: createModelChannel(viewModel) } });
    cleanups.push(() => unmount(component));

    const link = await vi.waitFor(() => {
      const value = target.querySelector<HTMLAnchorElement>("a[href='/project1/task388/artifacts/report.md']");
      expect(value).not.toBeNull();
      return value!;
    });
    const routeBefore = window.location.pathname;
    link.click();

    await vi.waitFor(() => expect(target.querySelector("[role='dialog'][aria-label='File preview']")?.textContent).toContain("Previewed report"));
    expect(window.location.pathname).toBe(routeBefore);
    expect(viewModel.onNavigate).not.toHaveBeenCalled();
    expect(fetchMock.mock.calls.map(([input]) => String(input))).toContain("/api/workspaces/workspace-a/files?path=%2Fproject1%2Ftask388%2Fartifacts%2Freport.md");
    expect(target.querySelector("[role='dialog'] .markdown-editor-shell")).toBeNull();
  });

  it("expands Turns bottom-up and stops once the conversation overflows the viewport", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const generation = { generation: 1, generationId: "gen-task-a", title: "task-a", status: "idle", createdAt: "2026-08-12T00:00:00Z", updatedAt: "2026-08-12T00:00:00Z" };
    const turns = [1, 2, 3].map((n) => ({
      reference: `ref-${n}`, turnId: `turn-${n}`, status: "completed", closed: true, startedAt: "2026-08-12T00:00:00Z", durationMs: 10,
      triggerPreview: `summary ${n}`, eventCount: 2, toolEventCount: 0, startEventId: n * 2 - 1, lastEventId: n * 2, endEventId: n * 2, generation,
    }));
    const details = turns.map((turn) => ({
      turn, latestEventId: turn.lastEventId,
      items: [{ type: "message", role: "user", text: `detail ${turn.turnId}`, startEventId: turn.startEventId, endEventId: turn.lastEventId, startedAt: turn.startedAt, endedAt: turn.startedAt }],
    }));
    const page = { resourceId: "task-a", segments: [{ generation, turns }], page: { limit: 5, hasMore: false } };
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const detailMatch = String(input).match(/\/history\/turns\/ref-(\d)/);
      const body = detailMatch ? details[Number(detailMatch[1]) - 1] : page;
      return new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchMock);
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    // The viewport counts as filled the moment one expanded Turn renders.
    Object.defineProperty(target, "scrollHeight", { configurable: true, get: () => target.querySelector(".agent-message-row") ? 2000 : 0 });
    Object.defineProperty(target, "clientHeight", { configurable: true, get: () => 600 });
    Object.defineProperty(target, "scrollTop", { configurable: true, get: () => 0, set: () => {} });
    // Deferred visibility triggers land far outside this viewport, so only the
    // bottom-up fill may expand Turns in this test.
    target.getBoundingClientRect = () => ({ top: 5000, bottom: 5600, left: 0, right: 800, width: 800, height: 600, x: 0, y: 5000, toJSON: () => ({}) }) as DOMRect;
    const component = mount(EventTimeline, { target, props: { channel: createModelChannel(model("task-a")) } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(target.textContent).toContain("detail turn-3"));
    await new Promise((resolve) => setTimeout(resolve, 50));
    const detailRequests = fetchMock.mock.calls.map(([input]) => String(input)).filter((path) => path.includes("/history/turns/ref-"));
    expect(detailRequests.every((path) => path.includes("/history/turns/ref-3"))).toBe(true);
    expect(detailRequests.length).toBeGreaterThan(0);
    expect(target.textContent).toContain("summary 1");
    expect(target.textContent).toContain("summary 2");
    expect(target.textContent).not.toContain("detail turn-1");
    expect(target.textContent).not.toContain("detail turn-2");
  });

  it("pulls older summary pages while expanded Turns leave the viewport underfilled", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const generation = { generation: 1, generationId: "gen-task-a", title: "task-a", status: "idle", createdAt: "2026-08-12T00:00:00Z", updatedAt: "2026-08-12T00:00:00Z" };
    const makeTurn = (id: string, first: number) => ({
      reference: `ref-${id}`, turnId: id, status: "completed", closed: true, startedAt: "2026-08-12T00:00:00Z", durationMs: 10,
      triggerPreview: `summary ${id}`, eventCount: 2, toolEventCount: 0, startEventId: first, lastEventId: first + 1, endEventId: first + 1, generation,
    });
    const newest = makeTurn("new", 3);
    const oldest = makeTurn("old", 1);
    const detailOf = (turn: ReturnType<typeof makeTurn>) => ({
      turn, latestEventId: turn.lastEventId,
      items: [{ type: "message", role: "user", text: `detail ${turn.turnId}`, startEventId: turn.startEventId, endEventId: turn.lastEventId, startedAt: turn.startedAt, endedAt: turn.startedAt }],
    });
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      let body: unknown;
      if (path.includes("/history/turns/ref-new")) body = detailOf(newest);
      else if (path.includes("/history/turns/ref-old")) body = detailOf(oldest);
      else if (path.includes("cursor=older")) body = { resourceId: "task-a", segments: [{ generation, turns: [oldest] }], page: { limit: 5, hasMore: false } };
      else body = { resourceId: "task-a", segments: [{ generation, turns: [newest] }], page: { limit: 5, nextCursor: "older", hasMore: true } };
      return new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchMock);
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    mockScrollMetrics(target, { scrollHeight: 0, clientHeight: 600 });
    const component = mount(EventTimeline, { target, props: { channel: createModelChannel(model("task-a")) } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => {
      expect(target.textContent).toContain("detail new");
      expect(target.textContent).toContain("detail old");
    });
    expect(fetchMock.mock.calls.map(([input]) => String(input)).some((path) => path.includes("cursor=older"))).toBe(true);
  });

  it("shows working only while a Turn is actively running", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = history("task-a");
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const active = model("task-a");
    active.status!.sessionState = "working";
    active.status!.session = { ...active.status!.session, state: "running", currentTurnId: "turn-1" };
    const channel = createModelChannel(active);
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(target.querySelector(".turn-working-indicator")?.textContent).toBe("working..."));
    expect(target.querySelector(".turn-working-indicator")?.getAttribute("role")).toBe("status");
    expect(target.querySelector(".turn-working-indicator [data-lucide='loader-circle']")).not.toBeNull();

    const awaitingApproval = model("task-a");
    awaitingApproval.status!.sessionState = "attention_required";
    awaitingApproval.status!.session = { ...awaitingApproval.status!.session, state: "waiting_approval", currentTurnId: "turn-1" };
    channel.publish(awaitingApproval);
    await tick();
    expect(target.querySelector(".turn-working-indicator")).toBeNull();

    channel.publish(model("task-a"));
    await tick();
    expect(target.querySelector(".turn-working-indicator")).toBeNull();
  });

  it("mutes the rail of mid-turn assistant progress updates and keeps the final reply in ink", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const startedAt = "2026-08-12T00:00:00Z";
    const fixture = history("task-a", [
      { type: "message", role: "user", text: "question", startEventId: 1, endEventId: 1, startedAt, endedAt: startedAt },
      { type: "message", role: "assistant", text: "progress update", startEventId: 2, endEventId: 2, startedAt, endedAt: startedAt },
      { type: "message", role: "assistant", text: "final reply", startEventId: 3, endEventId: 3, startedAt, endedAt: startedAt },
    ]);
    Object.assign(fixture.turn, { eventCount: 3, lastEventId: 3, endEventId: 3 });
    fixture.detail.turn = fixture.turn;
    fixture.detail.latestEventId = 3;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const channel = createModelChannel(model("task-a"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(target.textContent).toContain("final reply"));
    const rows = [...target.querySelectorAll<HTMLElement>(".agent-message-row.assistant")];
    expect(rows).toHaveLength(2);
    expect(rows[0].classList.contains("final")).toBe(false);
    expect(rows[1].classList.contains("final")).toBe(true);
    expect(target.querySelector(".agent-message-row.user")?.classList.contains("final")).toBe(false);
  });

  it("labels the first agent event of a turn instead of the first progress update", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const startedAt = "2026-08-12T00:00:00Z";
    const fixture = history("task-a", [
      { type: "message", role: "user", text: "question", startEventId: 1, endEventId: 1, startedAt, endedAt: startedAt },
      { type: "thinking", startEventId: 2, endEventId: 2, count: 1, startedAt, endedAt: startedAt },
      { type: "tool", startEventId: 3, endEventId: 4, count: 1, startedAt, endedAt: startedAt },
      { type: "message", role: "assistant", text: "final reply", startEventId: 5, endEventId: 5, startedAt, endedAt: startedAt },
    ]);
    Object.assign(fixture.turn, { eventCount: 5, toolEventCount: 2, lastEventId: 5, endEventId: 5 });
    fixture.detail.turn = fixture.turn;
    fixture.detail.latestEventId = 5;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const channel = createModelChannel(model("task-a"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(target.textContent).toContain("final reply"));
    const section = target.querySelector<HTMLElement>("section.conversation-turn")!;
    const rows = [...section.querySelectorAll<HTMLElement>(":scope > [data-timeline-key]")];
    expect(rows).toHaveLength(3);

    // Reasoning and tool calls precede the reply: the run header introduces
    // combined activity group, carrying both the agent's name and the run's
    // start time while preserving the legacy compact input.
    const header = rows[1].querySelector<HTMLElement>(".agent-run-header");
    expect(header?.querySelector("strong")?.textContent).toBe("Test Agent");
    expect(header?.querySelector("span")?.textContent).toBe(formatClock(startedAt));
    expect(rows[1].querySelector(".agent-activity-group")).not.toBeNull();

    // The reply belongs to the same run but still renders its own meta row
    // with the agent's name and the message's timestamp.
    const reply = rows[2].querySelector<HTMLElement>(".agent-message-row.assistant");
    expect(reply?.querySelector(".agent-message-meta strong")?.textContent).toBe("Test Agent");
    expect(reply?.querySelector(".agent-message-meta span")?.textContent).toBe(formatClock(startedAt));
    expect(reply?.classList.contains("final")).toBe(true);

    // Exactly one run header renders for the opening activity; every
    // message meta row keeps its sender's name.
    expect(section.querySelectorAll(".agent-run-header")).toHaveLength(1);
    expect(conversationAuthors(target)).toEqual(["User", "Test Agent"]);
  });

  it("keeps the generation boundary while hiding routine lifecycle detail", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = history("task-a");
    fixture.detail.items.push(
      { type: "lifecycle", role: "", text: "session.created", startEventId: 2, endEventId: 2, startedAt: fixture.turn.startedAt, endedAt: fixture.turn.startedAt },
      { type: "lifecycle", role: "", text: "session.provider", startEventId: 3, endEventId: 3, startedAt: fixture.turn.startedAt, endedAt: fixture.turn.startedAt },
      { type: "lifecycle", role: "", text: "turn.started", startEventId: 4, endEventId: 4, startedAt: fixture.turn.startedAt, endedAt: fixture.turn.startedAt },
      { type: "lifecycle", role: "", text: "turn.completed", startEventId: 5, endEventId: 5, startedAt: fixture.turn.startedAt, endedAt: fixture.turn.startedAt },
      { type: "lifecycle", role: "", text: "Session created", startEventId: 6, endEventId: 6, startedAt: fixture.turn.startedAt, endedAt: fixture.turn.startedAt },
      { type: "lifecycle", role: "", text: "Agent connected · gpt-5.6-sol · via codex", startEventId: 7, endEventId: 7, startedAt: fixture.turn.startedAt, endedAt: fixture.turn.startedAt },
      { type: "lifecycle", role: "", text: "Turn started", startEventId: 8, endEventId: 8, startedAt: fixture.turn.startedAt, endedAt: fixture.turn.startedAt },
      { type: "lifecycle", role: "", text: "Turn completed", startEventId: 9, endEventId: 9, startedAt: fixture.turn.startedAt, endedAt: fixture.turn.startedAt },
    );
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const channel = createModelChannel(model("task-a"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(target.textContent).toContain("message task-a"));
    expect(target.textContent).toContain("Generation 1");
    expect(target.textContent).not.toContain("session.created");
    expect(target.textContent).not.toContain("session.provider");
    expect(target.textContent).not.toContain("turn.started");
    expect(target.textContent).not.toContain("turn.completed");
    expect(target.textContent).not.toContain("Session created");
    expect(target.textContent).not.toContain("Agent connected");
    expect(target.textContent).not.toContain("Turn started");
    expect(target.textContent).not.toContain("Turn completed");
    expect(target.querySelector("[data-generation-id='gen-task-a']")).not.toBeNull();
    expect(FakeEventSource.instances[0].url).toContain("/resources/task-a/stream?");
  });

  it("keeps the current Generation badge aligned through lifecycle, refresh, and Generation changes", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    let fixture = historyGeneration("task-a", 1, "gen-task-a", "running");
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const channel = createModelChannel(model("task-a", status("task-a", "gen-task-a", 1, "working", "starting", "starting")));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    const badge = () => target.querySelector<HTMLElement>(".conversation-generation small");
    await vi.waitFor(() => expect(badge()?.textContent).toBe("starting"));

    // A stale generation status must not leave the badge idle while the
    // resource is working.
    channel.publish(model("task-a", status("task-a", "gen-task-a", 1, "working", "idle", "running")));
    await tick();
    expect(badge()?.textContent).toBe("running");

    channel.publish(model("task-a", status("task-a", "gen-task-a", 1, "idle", "idle", "idle")));
    await tick();
    expect(badge()?.textContent).toBe("idle");

    channel.publish(model("task-a", status("task-a", "gen-task-a", 1, "idle", "stopped", "stopped")));
    await tick();
    expect(badge()?.textContent).toBe("stopped");

    fixture = historyGeneration("task-a", 2, "gen-task-a-2", "running");
    channel.publish(model("task-a", status("task-a", "gen-task-a-2", 2, "working", "running", "running")));
    await vi.waitFor(() => expect(target.querySelector("[data-generation-id='gen-task-a-2'] small")?.textContent).toBe("running"));

    // The history page can still contain the old running value after a
    // refresh; the live status must win for the current Generation.
    channel.publish(model("task-a", status("task-a", "gen-task-a-2", 2, "idle", "idle", "idle")));
    await tick();
    expect(target.querySelector("[data-generation-id='gen-task-a-2'] small")?.textContent).toBe("idle");
  });

  it("adds a replacement Generation to the mounted Chat without a channel refresh", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = multiGenerationHistory("task-a");
    let currentGeneration = 1;
    const fetchImpl = vi.fn<typeof fetch>(async (input) => {
      const path = String(input);
      if (path.endsWith("/status")) {
        return new Response(JSON.stringify(status("task-a", `gen-${currentGeneration}`, currentGeneration)), { status: 200, headers: { "content-type": "application/json" } });
      }
      if (path.includes("/history/turns/ref-")) {
        const index = Number(path.match(/ref-(\d+)/)?.[1] || 0) - 1;
        return new Response(JSON.stringify(fixture.details[index]), { status: 200, headers: { "content-type": "application/json" } });
      }
      const page = {
        ...fixture.page,
        segments: fixture.page.segments.slice(0, currentGeneration),
      };
      return new Response(JSON.stringify(page), { status: 200, headers: { "content-type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchImpl);
    const channel = createModelChannel(generationModel("task-a", 1, "gen-1", "deepseek"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(target.querySelector("[data-generation-id='gen-1']")).not.toBeNull());
    currentGeneration = 2;

    await vi.waitFor(() => expect(target.querySelector("[data-generation-id='gen-2']")).not.toBeNull(), { timeout: 4000 });
    expect(target.textContent).toContain("codex reply");
    expect(channel.current().status?.generation?.generationId).toBe("gen-1");
  });

  it("invalidates the old resource view and stream immediately on resource switch", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const resource = String(input).includes("task-b") ? "task-b" : "task-a";
      const fixture = history(resource);
      return new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } });
    }));
    const channel = createModelChannel(model("task-a"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await vi.waitFor(() => expect(target.textContent).toContain("message task-a"));
    const oldStream = FakeEventSource.instances[0];

    channel.publish(model("task-b"));
    await tick();
    expect(target.querySelector('[data-chat-context="workspace-a:task-b"]')).not.toBeNull();
    expect(target.textContent).not.toContain("message task-a");
    await vi.waitFor(() => expect(target.textContent).toContain("message task-b"));
    expect(oldStream.closed).toBe(true);
  });

  it("keeps historical assistant authors bound to their Generation across switches and reload", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = multiGenerationHistory("task-a");
    const fetchImpl = vi.fn<typeof fetch>(async (input) => {
      const path = String(input);
      if (path.includes("/history/turns/ref-")) {
        const index = Number(path.match(/ref-(\d+)/)?.[1] || 0) - 1;
        return new Response(JSON.stringify(fixture.details[index]), { status: 200, headers: { "content-type": "application/json" } });
      }
      return new Response(JSON.stringify(fixture.page), { status: 200, headers: { "content-type": "application/json" } });
    });
    vi.stubGlobal("fetch", fetchImpl);
    const channel = createModelChannel(generationModel("task-a", 3, "gen-3", "deepseek"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel } });

    await vi.waitFor(() => expect(conversationAuthors(target)).toEqual(["deepseek", "codex", "deepseek"]));

    channel.publish(generationModel("task-a", 2, "gen-2", "codex"));
    await vi.waitFor(() => expect(conversationAuthors(target)).toEqual(["deepseek", "codex", "deepseek"]));

    channel.publish(generationModel("task-a", 3, "gen-3", "deepseek"));
    await vi.waitFor(() => expect(conversationAuthors(target)).toEqual(["deepseek", "codex", "deepseek"]));

    unmount(component);
    const reloaded = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(reloaded));
    await vi.waitFor(() => expect(conversationAuthors(target)).toEqual(["deepseek", "codex", "deepseek"]));
  });

  it("keeps following stream updates when layout shifts shrink the scroller without a scroll event", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = history("task-a");
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const channel = createModelChannel(model("task-a"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const metrics = mockScrollMetrics(target, { scrollHeight: 1000, clientHeight: 500 });
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(target.textContent).toContain("message task-a"));
    await vi.waitFor(() => expect(metrics.scrollTop).toBe(500));

    // The composer send feedback (or a growing textarea) steals height from
    // the scroller without firing a scroll event. The pinned follow state
    // must survive that shift so streamed updates still scroll to the bottom.
    metrics.clientHeight = 440;
    metrics.scrollHeight = 1100;
    FakeEventSource.instances[0].onmessage?.({ data: JSON.stringify(semanticFrame({ id: 100, time: "2026-08-12T00:00:01Z", type: "message.assistant.delta", sessionId: "session-task-a", turnId: "turn-1", data: { text: "live reply" } })) } as MessageEvent);

    await vi.waitFor(() => expect(target.textContent).toContain("live reply"));
    await vi.waitFor(() => expect(metrics.scrollTop).toBe(1100 - 440));

    metrics.scrollHeight = 1250;
    FakeEventSource.instances[0].onmessage?.({ data: JSON.stringify(semanticFrame({ id: 101, time: "2026-08-12T00:00:02Z", type: "message.assistant.delta", sessionId: "session-task-a", turnId: "turn-1", data: { text: "second reply" } })) } as MessageEvent);
    await vi.waitFor(() => expect(metrics.scrollTop).toBe(1250 - 440));
  });

  it("stops following after the user scrolls up and resumes at the bottom", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = history("task-a");
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const channel = createModelChannel(model("task-a"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const metrics = mockScrollMetrics(target, { scrollHeight: 1000, clientHeight: 500 });
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(metrics.scrollTop).toBe(500));

    target.scrollTop = 100;
    target.dispatchEvent(new Event("scroll"));
    metrics.scrollHeight = 1100;
    FakeEventSource.instances[0].onmessage?.({ data: JSON.stringify(semanticFrame({ id: 100, time: "2026-08-12T00:00:01Z", type: "message.assistant.delta", sessionId: "session-task-a", turnId: "turn-1", data: { text: "live reply" } })) } as MessageEvent);
    await vi.waitFor(() => expect(target.textContent).toContain("live reply"));
    await new Promise((resolve) => setTimeout(resolve, 150));
    expect(metrics.scrollTop).toBe(100);

    target.scrollTop = 1100 - 500;
    target.dispatchEvent(new Event("scroll"));
    metrics.scrollHeight = 1200;
    FakeEventSource.instances[0].onmessage?.({ data: JSON.stringify(semanticFrame({ id: 101, time: "2026-08-12T00:00:02Z", type: "message.assistant.delta", sessionId: "session-task-a", turnId: "turn-1", data: { text: "second reply" } })) } as MessageEvent);
    await vi.waitFor(() => expect(metrics.scrollTop).toBe(1200 - 500));
  });

  it("clears an old timeline selection and resumes following when the user sends", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = history("task-a");
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const channel = createModelChannel(model("task-a"));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const metrics = mockScrollMetrics(target, { scrollHeight: 1000, clientHeight: 500 });
    const component = mount(EventTimeline, { target, props: { channel } });
    cleanups.push(() => unmount(component));

    await vi.waitFor(() => expect(target.textContent).toContain("message task-a"));
    await vi.waitFor(() => expect(metrics.scrollTop).toBe(500));
    target.scrollTop = 100;
    target.dispatchEvent(new Event("scroll"));

    const selected = target.querySelector<HTMLElement>(".agent-message-bubble")!;
    const range = document.createRange();
    range.selectNodeContents(selected);
    window.getSelection()?.removeAllRanges();
    window.getSelection()?.addRange(range);
    expect(window.getSelection()?.toString()).toContain("message task-a");

    channel.publish({ ...model("task-a"), submitting: true });
    await vi.waitFor(() => expect(metrics.scrollTop).toBe(500));
    expect(window.getSelection()?.rangeCount).toBe(0);

    metrics.scrollHeight = 1200;
    FakeEventSource.instances[0].onmessage?.({ data: JSON.stringify(semanticFrame({ id: 100, time: "2026-08-12T00:00:01Z", type: "message.assistant.delta", sessionId: "session-task-a", turnId: "turn-1", data: { text: "reply after send" } })) } as MessageEvent);
    await vi.waitFor(() => expect(target.textContent).toContain("reply after send"));
    await vi.waitFor(() => expect(metrics.scrollTop).toBe(1200 - 500));
  });

  it("keeps compact tool counts and expands to the same group in Chat and History", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const startedAt = "2026-08-12T00:00:00Z";
    const fixture = history("task-a", [
      { type: "message", role: "user", text: "run tools", startEventId: 1, endEventId: 1, startedAt, endedAt: startedAt },
      { type: "tool", startEventId: 2, endEventId: 5, count: 2, startedAt, endedAt: startedAt },
      { type: "message", role: "assistant", text: "done", startEventId: 6, endEventId: 6, startedAt, endedAt: startedAt },
    ]);
    Object.assign(fixture.turn, { eventCount: 6, toolEventCount: 4, lastEventId: 6, endEventId: 6 });
    fixture.detail.turn = fixture.turn;
    const events: AgentEvent[] = [
      { id: 1, type: "message.input", turnId: "turn-1", sessionId: "session-task-a", data: { role: "user", text: "run tools" } },
      { id: 2, type: "tool.call", turnId: "turn-1", sessionId: "session-task-a", data: { schemaVersion: 1, callId: "call-1", operation: "start", toolKind: "command", name: "Command", summary: "echo one", status: "running" } },
      { id: 3, type: "tool.call", turnId: "turn-1", sessionId: "session-task-a", data: { schemaVersion: 1, callId: "call-1", operation: "finish", toolKind: "command", name: "Command", summary: "echo one", status: "completed" } },
      { id: 4, type: "tool.call", turnId: "turn-1", sessionId: "session-task-a", data: { schemaVersion: 1, callId: "call-2", operation: "start", toolKind: "mcp", name: "MCP", summary: "fixture / lookup", status: "running" } },
      { id: 5, type: "tool.call", turnId: "turn-1", sessionId: "session-task-a", data: { schemaVersion: 1, callId: "call-2", operation: "finish", toolKind: "mcp", name: "MCP", summary: "fixture / lookup", status: "completed", output: { text: "ok", mode: "replace" } } },
      { id: 6, type: "message.assistant.delta", turnId: "turn-1", sessionId: "session-task-a", data: { text: "done" } },
    ];
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => {
      const path = String(input);
      const body = path.includes("/events?") ? semanticPage(events) : path.includes("/history/turns/ref-") ? fixture.detail : fixture.page;
      return new Response(JSON.stringify(body), { status: 200, headers: { "content-type": "application/json" } });
    }));

    const chatTarget = document.body.appendChild(document.createElement("div"));
    chatTarget.className = "chat-timeline";
    const chat = mount(EventTimeline, { target: chatTarget, props: { channel: createModelChannel(model("task-a", status("task-a"), projectConversationEvents)) } });
    cleanups.push(() => unmount(chat));
    await vi.waitFor(() => expect(chatTarget.querySelector(".agent-activity-group")).not.toBeNull());
    expect(chatTarget.querySelector(".agent-activity-title")?.textContent).toBe("2 tool calls");
    expect(chatTarget.querySelector(".agent-activity-preview")?.textContent).toContain("2 tool calls");
    expect(chatTarget.querySelectorAll(".agent-tool-item")).toHaveLength(1);

    const chatGroup = chatTarget.querySelector<HTMLDetailsElement>(".agent-activity-group")!;
    chatGroup.open = true;
    chatGroup.dispatchEvent(new Event("toggle"));
    await vi.waitFor(() => expect(chatTarget.querySelectorAll(".agent-tool-item")).toHaveLength(2));
    expect([...chatTarget.querySelectorAll<HTMLDetailsElement>(".agent-tool-item")].every((tool) => !tool.open)).toBe(true);
    expect(chatTarget.querySelector(".agent-activity-title")?.textContent).toBe("2 tool calls");
    expect(chatTarget.textContent).toContain("Command");
    expect(chatTarget.textContent).toContain("MCP");

    const historyTarget = document.body.appendChild(document.createElement("div"));
    const historyComponent = mount(HistoryTimeline, { target: historyTarget, props: {
      workspaceId: "workspace-a", resourceId: "task-a", artifacts: [], resolveResourceTitle: () => null,
      onNavigate: () => undefined, onOpenFile: () => undefined, onOpenLegacy: () => undefined,
    } });
    cleanups.push(() => unmount(historyComponent));
    await vi.waitFor(() => expect(historyTarget.querySelector(".history-turn-header")).not.toBeNull());
    historyTarget.querySelector<HTMLButtonElement>(".history-turn-header")!.click();
    await vi.waitFor(() => expect(historyTarget.querySelector(".agent-activity-group")).not.toBeNull());
    expect(historyTarget.querySelector(".agent-activity-title")?.textContent).toBe("2 tool calls");
    expect(historyTarget.querySelector(".agent-activity-preview")?.textContent).toContain("2 tool calls");
    const historyGroup = historyTarget.querySelector<HTMLDetailsElement>(".agent-activity-group")!;
    historyGroup.open = true;
    historyGroup.dispatchEvent(new Event("toggle"));
    await vi.waitFor(() => expect(historyTarget.querySelectorAll(".agent-tool-item")).toHaveLength(2));
    expect([...historyTarget.querySelectorAll<HTMLDetailsElement>(".agent-tool-item")].every((tool) => !tool.open)).toBe(true);
    expect(historyTarget.querySelector(".agent-activity-title")?.textContent).toBe("2 tool calls");
  });

  it("renders closed non-user turns as a two-message digest that expands on click and collapses again", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = history("task-a", [{ type: "message", role: "assistant", text: "full turn detail", startEventId: 2, endEventId: 2, startedAt: "2026-08-12T00:00:00Z", endedAt: "2026-08-12T00:00:00Z" }]);
    Object.assign(fixture.turn, {
      triggerRole: "agent",
      triggerSender: { name: "project1.task2" },
      triggerPreview: "please review my patch",
      finalReplyPreview: "looks good, merged",
    });
    fixture.page.segments[0].turns = [fixture.turn];
    fixture.detail.turn = fixture.turn;
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetchMock);
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel: createModelChannel(model("task-a")) } });
    cleanups.push(() => unmount(component));

    // The digest keeps the normal message rows: trigger message with the
    // sender's name and role tag, an ellipsis expander, then the final reply
    // with the agent's name. The detail is neither rendered nor fetched
    // until the user clicks.
    const gap = await vi.waitFor(() => {
      const value = target.querySelector<HTMLButtonElement>(".turn-collapsed-gap");
      expect(value).not.toBeNull();
      return value!;
    });
    // The digest groups both messages on a role-tinted background.
    expect(target.querySelector(".turn-collapsed-digest")?.getAttribute("data-trigger-role")).toBe("agent");
    const triggerRow = target.querySelector<HTMLElement>(".agent-message-row.agent");
    expect(triggerRow?.querySelector(".agent-message-meta strong")?.textContent).toBe("project1.task2");
    expect(triggerRow?.querySelector(".agent-message-role-tag")?.textContent).toBe("agent");
    expect(triggerRow?.textContent).toContain("please review my patch");
    const replyRow = target.querySelector<HTMLElement>(".agent-message-row.assistant.final");
    expect(replyRow?.querySelector(".agent-message-meta strong")?.textContent).toBe("Test Agent");
    expect(replyRow?.textContent).toContain("looks good, merged");
    expect(gap.textContent).toContain("Expand turn");
    expect(target.textContent).not.toContain("full turn detail");
    await new Promise((resolve) => setTimeout(resolve, 50));
    expect(fetchMock.mock.calls.map(([input]) => String(input)).some((path) => path.includes("/history/turns/ref-"))).toBe(false);

    gap.click();
    await vi.waitFor(() => expect(target.textContent).toContain("full turn detail"));
    const collapse = target.querySelector<HTMLButtonElement>(".turn-collapse-again");
    expect(collapse).not.toBeNull();

    collapse!.click();
    await vi.waitFor(() => expect(target.querySelector(".turn-collapsed-gap")).not.toBeNull());
    expect(target.textContent).not.toContain("full turn detail");
  });

  it("marks a failed non-user turn's digest with its status", async () => {
    FakeEventSource.instances = [];
    vi.stubGlobal("EventSource", FakeEventSource);
    const fixture = history("task-a");
    Object.assign(fixture.turn, { triggerRole: "system", status: "failed", finalReplyPreview: "" });
    fixture.page.segments[0].turns = [fixture.turn];
    fixture.detail.turn = fixture.turn;
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL) => new Response(JSON.stringify(String(input).includes("/history/turns/ref-") ? fixture.detail : fixture.page), { status: 200, headers: { "content-type": "application/json" } })));
    const target = document.body.appendChild(document.createElement("div"));
    target.className = "chat-timeline";
    const component = mount(EventTimeline, { target, props: { channel: createModelChannel(model("task-a")) } });
    cleanups.push(() => unmount(component));

    const gap = await vi.waitFor(() => {
      const value = target.querySelector<HTMLButtonElement>(".turn-collapsed-gap");
      expect(value).not.toBeNull();
      return value!;
    });
    expect(gap.querySelector(".turn-collapsed-status")?.textContent).toBe("failed");
    expect(target.querySelector(".turn-collapsed-digest")?.getAttribute("data-trigger-role")).toBe("system");
    const triggerRow = target.querySelector<HTMLElement>(".agent-message-row.system");
    expect(triggerRow?.querySelector(".agent-message-meta strong")?.textContent).toBe("System");
    // No final reply preview: the digest renders only the trigger message.
    expect(target.querySelector(".agent-message-row.assistant.final")).toBeNull();
  });
});
