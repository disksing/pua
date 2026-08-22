import { EditorView } from "@codemirror/view";
import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import { createPUAAppChannels } from "../../src/app-channels";
import DetailPanel from "../../src/components/DetailPanel.svelte";

const mounted: Array<ReturnType<typeof mount>> = [];

const managedBlock = [
  "<!-- managed by pua cli -->",
  "Generated guidance.",
  "<!-- end of pua cli prompt -->",
].join("\n");

function json(value: unknown, status = 200): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    statusText: status === 200 ? "OK" : "Conflict",
    headers: new Headers({ "content-type": "application/json" }),
    json: async () => value,
  } as unknown as Response;
}

describe("Workspace AGENTS save flow", () => {
  let stopPUAApp: (() => void) | null = null;

  afterEach(async () => {
    while (mounted.length) await unmount(mounted.pop()!);
    stopPUAApp?.();
    stopPUAApp = null;
    document.body.replaceChildren();
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it("renders and saves the full workspace AGENTS.md content including the managed block", async () => {
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

    let fullContent = `# Notes\n\n${managedBlock}\n`;
    let hashVersion = 0;
    const saveBodies: Array<{ content: string; expectedContentHash: string }> = [];
    const contentHash = () => `agents-v${hashVersion}`;
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
      if (url.pathname === "/api/workspaces/ws-test/ui-state" && method === "GET") return json({});
      if (url.pathname === "/api/workspaces/ws-test/ui-state" && method === "PUT") return json({});
      if (url.pathname === "/api/workspaces/ws-test/tree" && method === "GET") {
		return json({ agentBinding: { kind: "profile", name: "default" }, projects: [], activity: { running: [], unread: [], problems: [] }, wiki: { exists: false } });
      }
      if (url.pathname === "/api/workspaces/ws-test/files" && url.searchParams.get("path") === "AGENTS.md") {
        if (method === "PUT") {
          const body = JSON.parse(String(init?.body || "{}")) as { content?: string; expectedContentHash?: string };
          saveBodies.push({ content: String(body.content || ""), expectedContentHash: String(body.expectedContentHash || "") });
          if (body.expectedContentHash !== contentHash()) return json({ error: "stale AGENTS.md" }, 409);
          fullContent = String(body.content || "");
          hashVersion++;
        }
        return json({ path: "AGENTS.md", name: "AGENTS.md", content: fullContent, contentHash: contentHash() });
      }
      if (url.pathname === "/api/workspaces/ws-test/resources/workspace/status" && method === "GET") {
        return json({ acceptsMessages: true, waitingMessages: [], canSteerWaiting: false, session: { state: "idle" } });
      }
      throw new Error(`Unexpected ${method} ${url.pathname}${url.search}`);
    }));

    const channels = createPUAAppChannels();
    const publisher = {
      renderAppShell: vi.fn(),
      renderCreateDialog: vi.fn(),
      renderSettings: vi.fn(),
      renderUploadDialog: vi.fn(),
      renderComposer: vi.fn(),
      renderEventTimeline: vi.fn(),
      renderAgentPanelHeader: vi.fn(),
      renderDetailPanel: channels.detail.publish,
      renderToast: vi.fn(),
    };
    const controller = await import("../../src/app-controller");
    stopPUAApp = controller.stopPUAApp;
    controller.startPUAApp(publisher);

    await vi.waitFor(() => expect(channels.detail.current().workspaceAgents?.content).toContain("managed by pua cli"));
    const target = document.createElement("section");
    target.id = "detailsPanel";
    document.body.append(target);
    mounted.push(mount(DetailPanel, { target, props: { channel: channels.detail } }));
    await tick();

    // The full document, including the managed block, is rendered in the AGENTS.md tab.
    expect(target.querySelector('[data-doc-file="AGENTS.md"]')).not.toBeNull();
    expect(target.textContent).toContain("Generated guidance.");

    const edit = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!;
    edit.click();
    await vi.waitFor(() => expect(target.querySelector<HTMLElement>('[role="dialog"] .cm-editor')).not.toBeNull());
    const dialog = target.querySelector<HTMLElement>('[role="dialog"]')!;
    const view = EditorView.findFromDOM(dialog.querySelector<HTMLElement>(".cm-editor")!)!;
    view.dispatch({ changes: { from: view.state.doc.length, insert: "\nEdited.\n" } });
    await tick();

    const saveButton = Array.from(dialog.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Save")!;
    saveButton.click();
    await vi.waitFor(() => expect(saveBodies).toHaveLength(1));
    expect(saveBodies[0]).toEqual({
      content: `# Notes\n\n${managedBlock}\n\nEdited.\n`,
      expectedContentHash: "agents-v0",
    });
  });
});
