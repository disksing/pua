import { afterEach, describe, expect, it, vi } from "vitest";

import type { AppShellModel } from "../../src/components/models";
import type { DetailPanelModel } from "../../src/models/detail";
import { confirmDialogChannel } from "../../src/controllers/confirm-dialog-controller";

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
  const isProject = !id.includes(".");
  return {
    id,
    type: isProject ? "project" : "task",
    title,
    path: isProject ? id : `project1/${id.replace("project1.", "")}`,
    archived: false,
    agentBinding: { kind: "profile", name: "default" },
    createdAt: "2026-01-01T00:00:00Z",
    updatedAt: "2026-01-01T00:00:00Z",
    artifacts: [],
    files: [],
  };
}

describe("Archive resource flow", () => {
  let stopPUAApp: (() => void) | null = null;
  let unsubscribeConfirm: (() => void) | null = null;
  let autoConfirmArchive = true;
  const confirmRequests: Array<{ title: string; message: string; confirmLabel: string; danger: boolean }> = [];

  afterEach(async () => {
    unsubscribeConfirm?.();
    unsubscribeConfirm = null;
    stopPUAApp?.();
    stopPUAApp = null;
    document.body.replaceChildren();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  function stubConfirmDialog(): void {
    autoConfirmArchive = true;
    confirmRequests.length = 0;
    unsubscribeConfirm = confirmDialogChannel.subscribe((model) => {
      if (!model.open) return;
      confirmRequests.push({ title: model.title, message: model.message, confirmLabel: model.confirmLabel, danger: model.danger });
      model.onResult(autoConfirmArchive);
    });
  }

  async function startApp(options: { blockProjectStatus?: boolean; initialResourceId?: string } = {}) {
    window.history.replaceState({}, "", options.initialResourceId ? `/w/ws-test/r/${options.initialResourceId}` : "/");
    let releaseProjectStatus!: () => void;
    const projectStatusReady = new Promise<void>((resolve) => { releaseProjectStatus = resolve; });

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
          children: [taskTreeItem("project1.task1", "Task One"), taskTreeItem("project1.task2", "Task Two")],
        },
        {
          id: "project2",
          type: "project",
          title: "Project Two",
          path: "project2",
          archived: false,
          agentBinding: { kind: "profile", name: "default" },
          children: [],
        },
      ],
	      activity: { running: [], unread: [], problems: [] },
      wiki: { exists: false },
    };
    const state = {
      treeFetchCount: 0,
      archivedResourceIds: [] as string[],
      uiStateBodies: [] as Array<{ expandedProjects?: string[] }>,
    };
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
      if (url.pathname === "/api/workspaces/ws-test/ui-state" && method === "GET") return json({ expandedProjects: ["project1", "project2"] });
      if (url.pathname === "/api/workspaces/ws-test/ui-state" && method === "PUT") {
        state.uiStateBodies.push(JSON.parse(String(init?.body || "{}")) as { expandedProjects?: string[] });
        return json({});
      }
      if (url.pathname === "/api/workspaces/ws-test/tree" && method === "GET") {
        state.treeFetchCount++;
        return json(tree);
      }
      if (url.pathname === "/api/workspaces/ws-test/archive" && method === "POST") {
        state.archivedResourceIds.push(String((JSON.parse(String(init?.body || "{}")) as { resourceId?: string }).resourceId || ""));
        return json({ path: "archive/project1/task1", warnings: [] });
      }
      const detailMatch = url.pathname.match(/^\/api\/workspaces\/ws-test\/resources\/([^/]+)$/);
      if (detailMatch && method === "GET") {
        const id = decodeURIComponent(detailMatch[1]);
        return json(resourceDetail(id, id));
      }
      const statusMatch = url.pathname.match(/^\/api\/workspaces\/ws-test\/resources\/([^/]+)\/status$/);
      if (statusMatch && method === "GET") {
        if (options.blockProjectStatus && decodeURIComponent(statusMatch[1]) === "project1") await projectStatusReady;
        return json({ acceptsMessages: true, waitingMessages: [], canSteerWaiting: false, session: { state: "idle" } });
      }
      throw new Error(`Unexpected ${method} ${url.pathname}${url.search}`);
    }));

    const appShellModels: AppShellModel[] = [];
    const detailModels: DetailPanelModel[] = [];
    const publisher = {
      renderAppShell: vi.fn((model: AppShellModel) => { appShellModels.push(model); }),
      renderCreateDialog: vi.fn(),
      renderSettings: vi.fn(),
      renderUploadDialog: vi.fn(),
      renderComposer: vi.fn(),
      renderEventTimeline: vi.fn(),
      renderAgentPanelHeader: vi.fn(),
      renderDetailPanel: vi.fn((model: DetailPanelModel) => { detailModels.push(model); }),
      renderToast: vi.fn(),
    };
    const controller = await import("../../src/app-controller");
    stopPUAApp = controller.stopPUAApp;
    controller.startPUAApp(publisher);
    const initialRenderCount = appShellModels.length;

    // Wait for the initial tree load to finish.
    await vi.waitFor(() => {
      expect(appShellModels.length).toBeGreaterThan(initialRenderCount);
      const latest = appShellModels.at(-1);
      expect(latest?.loading).toBe(false);
      expect(latest?.projects.find((project) => project.id === "project1")?.children.map((task) => task.id)).toEqual(["project1.task1", "project1.task2"]);
    });

    async function selectResource(resourceId: string): Promise<void> {
      await appShellModels.at(-1)!.onSelectResource(resourceId);
      await vi.waitFor(() => {
        expect(detailModels.at(-1)?.resourceId).toBe(resourceId);
        expect(detailModels.at(-1)?.detail?.id).toBe(resourceId);
      });
    }

    return { state, appShellModels, detailModels, selectResource, releaseProjectStatus };
  }

  it("updates only the affected tree nodes without reloading the whole tree", async () => {
    stubConfirmDialog();
    const { state, appShellModels, detailModels, selectResource } = await startApp();
    await selectResource("project1.task1");

    const treeFetchesBeforeArchive = state.treeFetchCount;
    const loadingModelsBeforeArchive = appShellModels.filter((model) => model.loading).length;

    await detailModels.at(-1)!.onArchive("project1.task1");

    await vi.waitFor(() => {
      const latest = appShellModels.at(-1);
      const project1 = latest?.projects.find((project) => project.id === "project1");
      expect(project1?.children.map((task) => task.id)).toEqual(["project1.task2"]);
      // The redirect target becomes the active selection.
      expect(project1?.children[0]?.active).toBe(true);
      // Unrelated nodes are untouched.
      expect(latest?.projects.map((project) => project.id)).toEqual(["project1", "project2"]);
    });

    // Archiving requires confirmation via the shared confirm dialog.
    expect(confirmRequests).toEqual([
      { title: "Archive task", message: 'Archive task "Task One"? This ends its open working state and stops its agent.', confirmLabel: "Archive", danger: true },
    ]);
    expect(state.archivedResourceIds).toEqual(["project1.task1"]);
    // The tree endpoint is not re-fetched: the archived node is removed locally.
    expect(state.treeFetchCount).toBe(treeFetchesBeforeArchive);
    // Archiving never puts the sidebar back into the whole-tree loading state.
    expect(appShellModels.filter((model) => model.loading).length).toBe(loadingModelsBeforeArchive);
    // The detail panel follows the redirect target.
    await vi.waitFor(() => {
      expect(detailModels.at(-1)?.resourceId).toBe("project1.task2");
      expect(detailModels.at(-1)?.detail?.id).toBe("project1.task2");
    });
    expect(state.treeFetchCount).toBe(treeFetchesBeforeArchive);

    // Archiving a whole project also removes it from the expanded set that is
    // persisted to ui-state, so the archived project cannot linger on disk.
    await selectResource("project2");
    await detailModels.at(-1)!.onArchive("project2");
    await vi.waitFor(() => {
      const latest = appShellModels.at(-1);
      expect(latest?.projects.map((project) => project.id)).toEqual(["project1"]);
      expect(latest?.projects[0]?.active).toBe(true);
    });
    expect(confirmRequests.at(-1)).toEqual({ title: "Archive project", message: 'Archive project "Project Two"? This ends its open working state and stops its agent.', confirmLabel: "Archive", danger: true });
    expect(state.archivedResourceIds).toEqual(["project1.task1", "project2"]);
    expect(state.treeFetchCount).toBe(treeFetchesBeforeArchive);
    expect(appShellModels.filter((model) => model.loading).length).toBe(loadingModelsBeforeArchive);
    expect(state.uiStateBodies.at(-1)?.expandedProjects).toEqual(["project1"]);
  });

  it("does not archive when the confirmation is cancelled", async () => {
    stubConfirmDialog();
    const { state, appShellModels, detailModels, selectResource } = await startApp();
    await selectResource("project1.task1");

    autoConfirmArchive = false;
    await detailModels.at(-1)!.onArchive("project1.task1");

    // The confirmation dialog was shown, but the archive API was never called
    // and the tree stays untouched.
    expect(confirmRequests).toEqual([
      { title: "Archive task", message: 'Archive task "Task One"? This ends its open working state and stops its agent.', confirmLabel: "Archive", danger: true },
    ]);
    expect(state.archivedResourceIds).toEqual([]);
    const latest = appShellModels.at(-1);
    expect(latest?.projects.find((project) => project.id === "project1")?.children.map((task) => task.id)).toEqual(["project1.task1", "project1.task2"]);
    expect(detailModels.at(-1)?.resourceId).toBe("project1.task1");
  });

  it("publishes project details before a delayed status response", async () => {
    const { appShellModels, detailModels, releaseProjectStatus } = await startApp({ blockProjectStatus: true });
    const selection = appShellModels.at(-1)!.onSelectResource("project1");

    await vi.waitFor(() => {
      const latest = detailModels.at(-1);
      expect(latest?.resourceId).toBe("project1");
      expect(latest?.loading).toBe(false);
      expect(latest?.detail?.id).toBe("project1");
    });

    releaseProjectStatus();
    await selection;
  });

  it("publishes project details during a refresh before status is available", async () => {
    const { detailModels, releaseProjectStatus } = await startApp({ blockProjectStatus: true, initialResourceId: "project1" });

    await vi.waitFor(() => {
      const latest = detailModels.at(-1);
      expect(latest?.resourceId).toBe("project1");
      expect(latest?.loading).toBe(false);
      expect(latest?.detail?.id).toBe("project1");
    });

    releaseProjectStatus();
  });
});
