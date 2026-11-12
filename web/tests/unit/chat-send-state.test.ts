import { afterEach, describe, expect, it, vi } from "vitest";

import type { AppShellModel, ComposerModel } from "../../src/components/models";
import type { AgentPanelHeaderModel } from "../../src/models/chat";
import type { DetailPanelModel } from "../../src/models/detail";

function json(value: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 200 ? "OK" : "Error",
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => value,
  } as unknown as Response;
}

function taskTreeItem(id: string, title: string) {
  return { id, type: "task", title, path: `project1/${id.replace("project1.", "")}`, archived: false, agentBinding: { kind: "profile", name: "default" } };
}

function resourceDetail(id: string, title: string) {
  return {
    id,
    type: "task",
    title,
    path: `project1/${id.replace("project1.", "")}`,
    archived: false,
    agentBinding: { kind: "profile", name: "default" },
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    artifacts: [],
    files: [],
  };
}

describe("Chat send state", () => {
  let stopPUAApp: (() => void) | null = null;

  afterEach(async () => {
    stopPUAApp?.();
    stopPUAApp = null;
    document.body.replaceChildren();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  async function startApp() {
    window.history.replaceState({}, "", "/");

    vi.stubGlobal("matchMedia", (query: string) => ({
      matches: false,
      media: query,
      onchange: null,
      addListener: vi.fn(),
      removeListener: vi.fn(),
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
      dispatchEvent: () => false,
    }));

    const tree = {
      agentBinding: { kind: "profile", name: "default" },
      projects: [
        {
          id: "project1",
          type: "project",
          title: "Project One",
          path: "project1",
          archived: false,
          agentBinding: { kind: "profile", name: "default" },
          children: [taskTreeItem("project1.task1", "Task One")],
        },
      ],
      activity: { running: [], unread: [], problems: [] },
      wiki: { exists: false },
    };
    const state = {
      statusBlocked: false,
      statusRequests: 0,
      statusCompleted: 0,
      messageBodies: [] as Array<{ text?: string }>,
      releaseStatus: () => {},
    };
    let statusGate = new Promise<void>((resolve) => { state.releaseStatus = resolve; });
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = new URL(String(input), window.location.origin);
      const method = init?.method || "GET";
      if (url.pathname === "/api/workspaces" && method === "GET") {
        return json({ activeId: "ws-test", workspaces: [{ id: "ws-test", name: "Test workspace", path: "/tmp/ws-test" }], agents: [], agentProfiles: [] });
      }
      if (url.pathname === "/api/settings/agenthub" && method === "GET") {
        return json({ connected: false, compatible: false, catalog: { providers: [], agents: [] }, config: { agentProfiles: [] } });
      }
      if (url.pathname === "/api/workspaces/ws-test/users") {
        if (method === "POST") return json({ version: 1, name: "User", preference: "" });
        return json({ users: [{ version: 1, name: "User", preference: "" }] });
      }
      if (url.pathname === "/api/workspaces/ws-test/ui-state" && method === "GET") return json({ expandedProjects: ["project1"] });
      if (url.pathname === "/api/workspaces/ws-test/ui-state" && method === "PUT") return json({});
      if (url.pathname === "/api/workspaces/ws-test/tree" && method === "GET") return json(tree);
      const messageMatch = url.pathname.match(/^\/api\/workspaces\/ws-test\/resources\/([^/]+)\/messages$/);
      if (messageMatch && method === "POST") {
        state.messageBodies.push(JSON.parse(String(init?.body || "{}")) as { text?: string });
        return json({ messageId: "msg-1", resourceId: decodeURIComponent(messageMatch[1]), status: "waiting" }, 202);
      }
      const detailMatch = url.pathname.match(/^\/api\/workspaces\/ws-test\/resources\/([^/]+)$/);
      if (detailMatch && method === "GET") {
        const id = decodeURIComponent(detailMatch[1]);
        return json(resourceDetail(id, id));
      }
      const statusMatch = url.pathname.match(/^\/api\/workspaces\/ws-test\/resources\/([^/]+)\/status$/);
      if (statusMatch && method === "GET") {
        state.statusRequests++;
        if (state.statusBlocked) await statusGate;
        state.statusCompleted++;
        return json({ acceptsMessages: true, waitingMessages: [], canSteerWaiting: false, session: { state: "idle" } });
      }
      if (url.pathname === "/api/workspaces/ws-test/inbox") return json({ messages: [] });
      if (url.pathname.match(/^\/api\/workspaces\/ws-test\/resources\/([^/]+)\/read$/) && method === "PUT") return json({ readTurnNumber: 0 });
      throw new Error(`Unexpected ${method} ${url.pathname}${url.search}`);
    }));

    const appShellModels: AppShellModel[] = [];
    const composerModels: ComposerModel[] = [];
    const headerModels: AgentPanelHeaderModel[] = [];
    const detailModels: DetailPanelModel[] = [];
    const publisher = {
      renderAppShell: vi.fn((model: AppShellModel) => { appShellModels.push(model); }),
      renderCreateDialog: vi.fn(),
      renderSettings: vi.fn(),
      renderUploadDialog: vi.fn(),
      renderComposer: vi.fn((model: ComposerModel) => { composerModels.push(model); }),
      renderEventTimeline: vi.fn(),
      renderAgentPanelHeader: vi.fn((model: AgentPanelHeaderModel) => { headerModels.push(model); }),
      renderDetailPanel: vi.fn((model: DetailPanelModel) => { detailModels.push(model); }),
      renderToast: vi.fn(),
    };
    const controller = await import("../../src/app-controller");
    stopPUAApp = controller.stopPUAApp;
    controller.startPUAApp(publisher);
    const initialRenderCount = appShellModels.length;
    await vi.waitFor(() => {
      expect(appShellModels.length).toBeGreaterThan(initialRenderCount);
      expect(appShellModels.at(-1)?.loading).toBe(false);
    });

    async function selectResource(resourceId: string): Promise<void> {
      await appShellModels.at(-1)!.onSelectResource(resourceId);
      await vi.waitFor(() => {
        expect(detailModels.at(-1)?.resourceId).toBe(resourceId);
        expect(detailModels.at(-1)?.detail?.id).toBe(resourceId);
      });
    }

    function resetStatusGate(): void {
      statusGate = new Promise<void>((resolve) => { state.releaseStatus = resolve; });
    }

    return { state, appShellModels, composerModels, headerModels, selectResource, resetStatusGate };
  }

  it("clears the submitting state once the message is accepted, without waiting for the status refresh", async () => {
    const { state, composerModels, headerModels, selectResource, resetStatusGate } = await startApp();
    await selectResource("project1.task1");

    const composer = composerModels.filter((model) => model.resourceId === "project1.task1").at(-1)!;
    // Hold the post-send status refresh on the server side; the send must
    // still complete as soon as the message POST is accepted.
    state.statusBlocked = true;
    resetStatusGate();
    const statusRequestsBefore = state.statusRequests;
    const statusCompletedBefore = state.statusCompleted;

    const result = await composer.onSend("hello task", { workspaceId: "ws-test", resourceId: "project1.task1", draftKey: composer.draftKey });
    expect(result.accepted).toBe(true);
    expect(state.messageBodies).toEqual([{ text: "hello task", role: "user", sender: { name: "User" } }]);
    // The blocked status refresh is still pending, yet the send resolved.
    expect(state.statusRequests).toBe(statusRequestsBefore + 1);
    expect(state.statusCompleted).toBe(statusCompletedBefore);
    await vi.waitFor(() => {
      expect(composerModels.filter((model) => model.resourceId === "project1.task1").at(-1)?.sending).toBe(false);
      expect(headerModels.filter((model) => model.resourceId === "project1.task1").at(-1)?.submitting).toBe(false);
    });

    state.statusBlocked = false;
    state.releaseStatus();
    await vi.waitFor(() => {
      expect(state.statusCompleted).toBeGreaterThan(statusCompletedBefore);
    });
  });
});
