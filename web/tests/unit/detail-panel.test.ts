import { mount, tick, unmount } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EditorView } from "@codemirror/view";

import DetailPanel from "../../src/components/DetailPanel.svelte";
import { createModelChannel } from "../../src/components/model-channel";
import type { DetailPanelModel } from "../../src/components/models";
import { MemoryStorage } from "../fixtures/memory-storage";

const { confirmDialogMock } = vi.hoisted(() => ({ confirmDialogMock: vi.fn() }));
vi.mock("../../src/controllers/confirm-dialog-controller", () => ({ confirmDialog: confirmDialogMock }));

const mounted: Array<ReturnType<typeof mount>> = [];

function resourceModel(overrides: Partial<DetailPanelModel> = {}): DetailPanelModel {
  return {
    identity: "ws:project1.task1:task",
    workspaceId: "ws",
    workspaceName: "Test workspace",
    resourceId: "project1.task1",
    resourceType: "task",
    resourceTitle: "Stable detail",
    parent: { id: "project1", title: "Project" },
    loading: false,
    detail: {
      id: "project1.task1", type: "task", title: "Stable detail", path: "project1/task1",
      files: [{ name: "task.md", path: "project1/task1/task.md", content: "# Stable detail\n\nSelected text.", contentHash: "doc-a" }],
      artifacts: [{ name: "folder", path: "project1/task1/artifacts/folder", type: "directory", children: [{ name: "nested.txt", path: "project1/task1/artifacts/folder/nested.txt", type: "file", size: 4 }] }, { name: "a.txt", path: "project1/task1/artifacts/a.txt", type: "file", size: 3 }, { name: "b.txt", path: "project1/task1/artifacts/b.txt", type: "file", size: 3 }],
      repos: [{ name: "pua", worktreePath: "project1/task1/worktree/pua", branch: "topic", targetBranch: "master" }, { name: "docs", worktreePath: "project1/task1/worktree/docs", branch: "docs-topic", targetBranch: "master" }],
    },
    wiki: null,
    workspaceAgents: null,
    workspaceDefaults: { project: { kind: "profile", name: "default" }, task: { kind: "profile", name: "default" } },
    workspaceUsers: [],
    currentUserName: "User",
    generationPolicy: { budgetEnabled: true, maxTurns: 20, maxAccumulatedTurnMinutes: 120, inactivityEnabled: true, maxInactivityMinutes: 1440 },
    stallWatchdogPolicy: { enabled: true, timeoutMinutes: 30 },
    agentBinding: { kind: "profile", name: "default" },
    agentProfiles: [{ key: "default", description: "Default", agentName: "fake-agent" }],
    agents: [{ id: "fake-agent", label: "Fake Agent", summary: "fake" }],
    resolveResourceTitle: () => null,
    onNavigate: vi.fn(), onCreateTask: vi.fn(), onArchive: vi.fn(),
    onSaveWorkspaceAgents: vi.fn(async () => ({ path: "AGENTS.md", content: "", contentHash: "saved" })),
    onSaveMarkdownFile: vi.fn(async (path, content) => ({ path, content, contentHash: "saved" })),
    onDeleteArtifact: vi.fn(async () => undefined),
    onSaveAgentBinding: vi.fn(async () => undefined),
    onRenameResource: vi.fn(async () => undefined),
    onSaveDescription: vi.fn(async () => undefined),
    onSaveWorkspaceDefaults: vi.fn(async () => undefined),
    onSaveWorkspaceUserPreference: vi.fn(async () => undefined),
    onSwitchWorkspaceUser: vi.fn(async () => undefined),
    onAddWorkspaceUser: vi.fn(async () => undefined),
    onDeleteWorkspaceUser: vi.fn(async () => undefined),
    onSaveGenerationPolicy: vi.fn(async () => undefined),
    onSaveStallWatchdogPolicy: vi.fn(async () => undefined),
    onSaveTaskDefault: vi.fn(async () => undefined),
    onToast: vi.fn(),
    ...overrides,
  };
}

function mountModel(model: DetailPanelModel) {
  const channel = createModelChannel(model);
  const target = document.createElement("section");
  target.id = "detailsPanel";
  document.body.append(target);
  const component = mount(DetailPanel, { target, props: { channel } });
  mounted.push(component);
  return { channel, target };
}

afterEach(async () => {
  while (mounted.length) await unmount(mounted.pop()!);
  document.body.replaceChildren();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
  delete window.Diff2Html;
});

beforeEach(() => vi.stubGlobal("localStorage", new MemoryStorage()));

describe("DetailPanel", () => {
  it("opens the Markdown editor dialog and saves through the resource callback", async () => {
    const save = vi.fn(async (path: string, content: string) => ({ path, name: "task.md", content, contentHash: "saved-hash" }));
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      path: "project1/task1/task.md", name: "task.md", content: "# Stable detail\n\nSelected text.", contentHash: "doc-a",
    }), { headers: { "content-type": "application/json" } })));
    const { target } = mountModel(resourceModel({ onSaveMarkdownFile: save }));
    await tick();
    const edit = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!;
    edit.click();
    await vi.waitFor(() => expect(target.querySelector<HTMLElement>('[role="dialog"] .cm-editor')).not.toBeNull());
    const dialog = target.querySelector<HTMLElement>('[role="dialog"]')!;
    const view = EditorView.findFromDOM(dialog.querySelector<HTMLElement>(".cm-editor")!)!;
    view.dispatch({ changes: { from: view.state.doc.length, insert: "\nAdded in browser.\n" } });
    await tick();
    const saveButton = Array.from(dialog.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Save")!;
    saveButton.click();
    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save).toHaveBeenCalledWith("project1/task1/task.md", "# Stable detail\n\nSelected text.\nAdded in browser.\n", "doc-a");
  });

  it("opens the file in a full-screen window without download controls", async () => {
    localStorage.removeItem("pua:file-preview-handoff");
    const open = vi.fn();
    vi.stubGlobal("open", open);
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      path: "project1/task1/task.md", name: "task.md", content: "# Stable detail\n\nSelected text.", contentHash: "doc-a",
    }), { headers: { "content-type": "application/json" } })));
    const { target } = mountModel(resourceModel());
    await tick();
    const edit = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!;
    edit.click();
    await vi.waitFor(() => expect(target.querySelector<HTMLElement>('[role="dialog"] .cm-editor')).not.toBeNull());
    const dialog = target.querySelector<HTMLElement>('[role="dialog"]')!;
    expect(dialog.querySelector('a[aria-label="Download file"], a[title="Download file"], a.file-modal-download')).toBeNull();
    expect(dialog.textContent).not.toContain("Download");

    const fullscreen = Array.from(dialog.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Full screen")!;
    fullscreen.click();
    expect(open).toHaveBeenCalledOnce();
    const url = new URL(String(open.mock.calls[0][0]), "http://localhost");
    expect(url.pathname).toBe("/file");
    expect(url.searchParams.get("workspaceId")).toBe("ws");
    expect(url.searchParams.get("resourceId")).toBe("project1.task1");
    expect(url.searchParams.get("section")).toBe("Files");
    expect(url.searchParams.get("path")).toBe("project1/task1/task.md");
    expect(url.searchParams.get("mode")).toBe("edit");
    expect(url.searchParams.get("editable")).toBe("1");
    const handoff = JSON.parse(localStorage.getItem("pua:file-preview-handoff") || "{}");
    expect(handoff.mode).toBe("edit");
    expect(handoff.path).toBe("project1/task1/task.md");
  });

  it("renders the compact resource number inside an independently scrollable body", async () => {
    const { target } = mountModel(resourceModel());
    await tick();

    const header = target.querySelector(".details-header")!;
    const tabs = target.querySelector(".details-tabs")!;
    const content = target.querySelector("#detailsContent")!;
    expect(target.querySelector(".resource-ref-badge")?.textContent).toBe("#1");
    expect(header.contains(content)).toBe(false);
    expect(tabs.contains(content)).toBe(false);
    expect(content.querySelector('[data-doc-file="task.md"]')).not.toBeNull();
  });

  it("moves body headings into tab icons instead of repeating titles", async () => {
    const { target } = mountModel(resourceModel());
    await tick();

    const tabs = Array.from(target.querySelectorAll<HTMLButtonElement>('[role="tab"]'));
    expect(tabs.length).toBeGreaterThan(0);
    for (const tab of tabs) expect(tab.querySelector("[data-lucide]")).not.toBeNull();
    const taskTab = tabs.find((tab) => tab.textContent?.includes("Task"))!;
    expect(taskTab.querySelector('[data-lucide="file-text"]')).not.toBeNull();

    const documentSection = target.querySelector('[data-doc-file="task.md"]')!;
    expect(documentSection.querySelector("h3")).toBeNull();
    expect(documentSection.querySelector(".markdown-document-actions button")).not.toBeNull();

    const artifactsSection = target.querySelector('[data-component-owner="file-browser"]')!;
    expect(artifactsSection.querySelector("h3")).toBeNull();
    const worktreesSection = target.querySelector(".worktree-list")!.closest(".content-section")!;
    expect(worktreesSection.querySelector("h3")).toBeNull();
  });

  it("uses the Project number for a Project detail reference", async () => {
    const initial = resourceModel();
    const projectFile = { ...initial.detail!.files![0], name: "project.md", path: "project12/project.md" };
    const { target } = mountModel(resourceModel({
      identity: "ws:project12:project",
      resourceId: "project12",
      resourceType: "project",
      parent: null,
      detail: { ...initial.detail!, id: "project12", type: "project", files: [projectFile] },
    }));
    await tick();
    expect(target.querySelector(".resource-ref-badge")?.textContent).toBe("#12");
  });

  it("resets the detail body, rather than the fixed panel chrome, after navigation", async () => {
    const initial = resourceModel();
    const { channel, target } = mountModel(initial);
    await tick();
    const content = target.querySelector<HTMLElement>("#detailsContent")!;
    content.scrollTop = 80;

    channel.publish({ ...initial, identity: "ws:project1.task2:task", resourceId: "project1.task2" });
    await tick();
    expect(content.scrollTop).toBe(0);
    expect(target.querySelector(".resource-ref-badge")?.textContent).toBe("#2");
  });

  it("keeps the resource document tab selected when detail data arrives after navigation", async () => {
    const loaded = resourceModel();
    const loading = resourceModel({ detail: null, loading: true });
    const { channel, target } = mountModel(loading);
    await tick();
    channel.publish(loaded);
    await tick();
    const selected = target.querySelector('[role="tab"][aria-selected="true"]');
    expect(selected?.textContent).toContain("Task");
  });

  it("does not expose a retired Work tab for legacy detail data", async () => {
    const initial = resourceModel();
    const legacyWork = { name: "work.md", path: "project1/task1/work.md", content: "# Legacy checkpoint", contentHash: "legacy-work" };
    const { channel, target } = mountModel(resourceModel({ detail: { ...initial.detail!, files: [...initial.detail!.files!, legacyWork] } }));
    await tick();

    const tabs = Array.from(target.querySelectorAll<HTMLButtonElement>("[role=tab]"));
    expect(tabs.find((tab) => tab.textContent?.trim() === "Work")).toBeUndefined();
    expect(target.querySelector('[role="tab"][aria-selected="true"]')?.textContent).toContain("Task");

    channel.publish({ ...initial, detail: { ...initial.detail!, files: initial.detail!.files } });
    await tick();
    expect(target.querySelector('[role="tab"][aria-selected="true"]')?.textContent).toContain("Task");
  });

  it("keeps the Task tab visible with a missing-file notice when task.md is deleted", async () => {
    const initial = resourceModel();
    const { target } = mountModel(resourceModel({ detail: { ...initial.detail!, files: [] } }));
    await tick();

    const tabs = Array.from(target.querySelectorAll<HTMLButtonElement>("[role=tab]"));
    const taskTab = tabs.find((tab) => tab.textContent?.includes("Task"));
    expect(taskTab).toBeDefined();
    expect(taskTab!.getAttribute("aria-selected")).toBe("true");
    expect(target.textContent).toContain("Task brief is missing");
    expect(target.querySelector('[data-doc-file="task.md"]')).toBeNull();
  });

  it("keeps the Project tab visible with a missing-file notice when project.md is deleted", async () => {
    const initial = resourceModel();
    const projectDetail = { ...initial.detail!, id: "project12", type: "project" as const, files: [] };
    const { target } = mountModel(resourceModel({
      identity: "ws:project12:project",
      resourceId: "project12",
      resourceType: "project",
      parent: null,
      detail: projectDetail,
    }));
    await tick();

    const tabs = Array.from(target.querySelectorAll<HTMLButtonElement>("[role=tab]"));
    const projectTab = tabs.find((tab) => tab.textContent?.includes("Project"));
    expect(projectTab).toBeDefined();
    expect(projectTab!.getAttribute("aria-selected")).toBe("true");
    expect(target.textContent).toContain("Project brief is missing");
  });

  it("keeps document and artifact DOM identity across unrelated refreshes", async () => {
    const initial = resourceModel();
    const { channel, target } = mountModel(initial);
    await tick();
    const documentNode = target.querySelector(".markdown-view") as HTMLElement;
    documentNode.dataset.identityProbe = "document";

    channel.publish({ ...initial, resourceTitle: "Metadata refreshed", detail: { ...initial.detail!, title: "Metadata refreshed" } });
    await tick();
    expect(target.querySelector(".markdown-view")).toBe(documentNode);
    expect(documentNode.dataset.identityProbe).toBe("document");
    const changedFile = { ...initial.detail!.files![0], content: "# Updated detail", contentHash: "doc-b" };
    channel.publish({ ...initial, detail: { ...initial.detail!, files: [changedFile] } });
    await tick();
    expect(target.querySelector(".markdown-view")).toBe(documentNode);
    expect(documentNode.textContent).toContain("Updated detail");
    expect(target.querySelector("[data-document-identity]")?.getAttribute("data-document-identity")).toContain("doc-b");

    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Artifacts"))!.click();
    await tick();
    (Array.from(target.querySelectorAll(".artifact-row")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("folder"))!.click();
    await tick();
    const nestedNode = (Array.from(target.querySelectorAll(".artifact-row")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("nested.txt"))!;
    nestedNode.dataset.identityProbe = "expanded";
    channel.publish({ ...initial, resourceTitle: "Another metadata refresh", detail: { ...initial.detail!, title: "Another metadata refresh" } });
    await tick();
    expect((Array.from(target.querySelectorAll(".artifact-row")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("nested.txt"))).toBe(nestedNode);
    expect(nestedNode.dataset.identityProbe).toBe("expanded");

  });

  it("confirms before deleting an artifact", async () => {
    const onDeleteArtifact = vi.fn(async () => undefined);
    const { target } = mountModel(resourceModel({ onDeleteArtifact }));
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Artifacts"))!.click();
    await tick();
    const row = (Array.from(target.querySelectorAll(".artifact-row")) as HTMLElement[]).find((button) => button.textContent?.includes("a.txt"))!;
    const deleteButton = row.querySelector<HTMLElement>(".artifact-delete")!;
    expect(deleteButton).not.toBeNull();

    const confirmSpy = confirmDialogMock.mockReset().mockResolvedValue(false);
    deleteButton.click();
    await vi.waitFor(() => expect(confirmSpy).toHaveBeenCalledOnce());
    expect(onDeleteArtifact).not.toHaveBeenCalled();

    confirmSpy.mockResolvedValue(true);
    deleteButton.click();
    await vi.waitFor(() => expect(onDeleteArtifact).toHaveBeenCalledWith("project1/task1/artifacts/a.txt"));
  });

  it("hides artifact delete controls for archived resources", async () => {
    const initial = resourceModel();
    const { target } = mountModel(resourceModel({ detail: { ...initial.detail!, archived: true } }));
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Artifacts"))!.click();
    await tick();
    expect(target.querySelector(".artifact-delete")).toBeNull();
  });

  it("ignores a late preview response after selecting another file", async () => {
    const pending = new Map<string, (response: Response) => void>();
    vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL) => new Promise<Response>((resolve) => pending.set(String(input), resolve))));
    const { target } = mountModel(resourceModel());
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Artifacts"))!.click();
    await tick();
    (Array.from(target.querySelectorAll(".artifact-row")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("a.txt"))!.click();
    await tick();
    (Array.from(target.querySelectorAll(".artifact-row")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("b.txt"))!.click();
    await tick();
    const urls = [...pending.keys()];
    pending.get(urls[0])!(new Response(JSON.stringify({ path: "a.txt", content: "old response", contentHash: "old" }), { headers: { "content-type": "application/json" } }));
    pending.get(urls[1])!(new Response(JSON.stringify({ path: "b.txt", content: "current response", contentHash: "current" }), { headers: { "content-type": "application/json" } }));
    await vi.waitFor(() => expect(target.querySelector('[role="dialog"]')?.textContent).toContain("current response"));
    expect(target.querySelector('[role="dialog"]')?.textContent).not.toContain("old response");
  });

  it("preserves an open artifact preview while the same file is refreshed", async () => {
    const fetch = vi.fn(async () => new Response(JSON.stringify({
      path: "a.txt",
      content: Array.from({ length: 40 }, (_, index) => `line ${index + 1}`).join("\n"),
      contentHash: "a-v1",
    }), { headers: { "content-type": "application/json" } }));
    vi.stubGlobal("fetch", fetch);
    const initial = resourceModel();
    const { channel, target } = mountModel(initial);
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Artifacts"))!.click();
    await tick();
    const file = (Array.from(target.querySelectorAll(".artifact-row")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("a.txt"))!;
    file.click();
    await vi.waitFor(() => expect(target.querySelector("[data-preview-scroll]")).not.toBeNull());

    const scroller = target.querySelector<HTMLElement>("[data-preview-scroll]")!;
    scroller.scrollTop = 40;
    scroller.scrollLeft = 7;
    channel.publish({ ...initial, resourceTitle: "Refreshed", detail: { ...initial.detail!, title: "Refreshed" } });
    await tick();
    expect(target.querySelector<HTMLElement>("[data-preview-scroll]")).toBe(scroller);
    file.click();
    await tick();

    expect(fetch).toHaveBeenCalledOnce();
    expect(scroller.scrollTop).toBe(40);
    expect(scroller.scrollLeft).toBe(7);
  });

  it("ignores a late Diff response after switching worktrees", async () => {
    const pending = new Map<string, (response: Response) => void>();
    vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL) => new Promise<Response>((resolve) => pending.set(String(input), resolve))));
    window.Diff2Html = { html: (diff) => `<div>${diff}</div>` };
    const { target } = mountModel(resourceModel());
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Worktrees"))!.click();
    await tick();
    const viewButtons = Array.from(target.querySelectorAll(".worktree-row button")) as HTMLButtonElement[];
    viewButtons[0].click();
    await tick();
    (target.querySelector('[role="dialog"] button[aria-label="Close"]') as HTMLButtonElement).click();
    await tick();
    viewButtons[1].click();
    await tick();
    const urls = [...pending.keys()];
    pending.get(urls[0])!(new Response(JSON.stringify({ path: "pua", diff: "stale diff", hasChanges: true }), { headers: { "content-type": "application/json" } }));
    pending.get(urls[1])!(new Response(JSON.stringify({ path: "docs", diff: "current diff", hasChanges: true }), { headers: { "content-type": "application/json" } }));
    await vi.waitFor(() => expect(target.querySelector('[role="dialog"]')?.textContent).toContain("current diff"));
    expect(target.querySelector('[role="dialog"]')?.textContent).not.toContain("stale diff");
  });

  it("renders workspace AGENTS.md in full across AGENTS.md and Wiki tabs", async () => {
    const initial = resourceModel({
      identity: "ws:workspace", resourceId: "workspace", resourceType: "workspace", resourceTitle: "Test workspace", detail: null,
      workspaceAgents: { path: "AGENTS.md", content: "# Notes\n\n<!-- managed by pua cli -->\nGenerated guidance.\n<!-- end of pua cli prompt -->\n", contentHash: "hash-a" },
      wiki: { exists: true, entries: [{ name: "index.md", path: "wiki/index.md", type: "file", size: 10 }] },
    });
    const { target } = mountModel(initial);
    await tick();

    const tabs = Array.from(target.querySelectorAll<HTMLButtonElement>("[role=tab]"));
    expect(tabs.map((tab) => tab.textContent?.trim())).toEqual(["AGENTS.md", "Wiki", "Settings"]);
    expect(target.querySelector('[role="tab"][aria-selected="true"]')?.textContent).toContain("AGENTS.md");
    // The managed block is no longer hidden from the rendered document.
    expect(target.querySelector('[data-doc-file="AGENTS.md"]')).not.toBeNull();
    expect(target.textContent).toContain("Generated guidance.");
    expect(target.querySelector(".markdown-document-actions button")).not.toBeNull();
  });

  it("edits workspace AGENTS.md through the Markdown editor dialog", async () => {
    const save = vi.fn(async (_content: string, _hash: string) => ({ path: "AGENTS.md", name: "AGENTS.md", content: "# Notes\n\nEdited.\n", contentHash: "saved-hash" }));
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      path: "AGENTS.md", name: "AGENTS.md", content: "# Notes\n\n<!-- managed by pua cli -->\nsystem\n<!-- end of pua cli prompt -->\n", contentHash: "hash-a",
    }), { headers: { "content-type": "application/json" } })));
    const initial = resourceModel({
      identity: "ws:workspace", resourceId: "workspace", resourceType: "workspace", resourceTitle: "Test workspace", detail: null,
      workspaceAgents: { path: "AGENTS.md", content: "# Notes\n\n<!-- managed by pua cli -->\nsystem\n<!-- end of pua cli prompt -->\n", contentHash: "hash-a" },
      onSaveWorkspaceAgents: save,
    });
    const { target } = mountModel(initial);
    await tick();
    const edit = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!;
    edit.click();
    await vi.waitFor(() => expect(target.querySelector<HTMLElement>('[role="dialog"] .cm-editor')).not.toBeNull());
    const dialog = target.querySelector<HTMLElement>('[role="dialog"]')!;
    const view = EditorView.findFromDOM(dialog.querySelector<HTMLElement>(".cm-editor")!)!;
    view.dispatch({ changes: { from: view.state.doc.length, insert: "\nEdited.\n" } });
    await tick();
    const saveButton = Array.from(dialog.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Save")!;
    saveButton.click();
    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save).toHaveBeenCalledWith("# Notes\n\n<!-- managed by pua cli -->\nsystem\n<!-- end of pua cli prompt -->\n\nEdited.\n", "hash-a");
  });

  it("keeps file-browser directory icons stable and toggles expansion with a class", async () => {
    const { target } = mountModel(resourceModel());
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Artifacts"))!.click();
    await tick();

    const directoryRow = target.querySelector<HTMLButtonElement>(".artifact-row.directory")!;
    expect(directoryRow).not.toBeNull();
    expect(directoryRow.classList.contains("open")).toBe(false);
    // The chevron stays a single stable icon; direction comes from the open class.
    expect(directoryRow.querySelector('.artifact-chevron [data-lucide="chevron-right"]')).not.toBeNull();
    expect(directoryRow.querySelector('.artifact-chevron [data-lucide="chevron-down"]')).toBeNull();
    // Folder and folder-open are both rendered and switched through the open class.
    expect(directoryRow.querySelector('.artifact-folder-icon [data-lucide="folder"]')).not.toBeNull();
    expect(directoryRow.querySelector('.artifact-folder-icon [data-lucide="folder-open"]')).not.toBeNull();

    directoryRow.click();
    await tick();
    const expandedRow = target.querySelector<HTMLButtonElement>(".artifact-row.directory")!;
    expect(expandedRow.classList.contains("open")).toBe(true);
  });

  it("saves Workspace bindings and defaults from the settings tab", async () => {
    const saveBinding = vi.fn(async () => undefined);
    const saveDefaults = vi.fn(async () => undefined);
    const { target } = mountModel(resourceModel({
      identity: "ws:workspace", resourceId: "workspace", resourceType: "workspace", resourceTitle: "Test workspace", detail: null,
      workspaceAgents: { path: "AGENTS.md", content: "# Notes\n", contentHash: "hash-a" },
      onSaveAgentBinding: saveBinding, onSaveWorkspaceDefaults: saveDefaults,
    }));
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Settings"))!.click();
    await tick();

    expect(target.textContent).toContain("Workspace Agent");
    expect(target.textContent).toContain("New Project default");
    expect(target.textContent).toContain("New Task default");
    const selectors = Array.from(target.querySelectorAll<HTMLButtonElement>(".agent-binding-button"));
    expect(selectors.length).toBe(3);

    selectors[0].click();
    await tick();
    target.querySelector<HTMLButtonElement>('[data-binding="agent:fake-agent"]')!.click();
    await vi.waitFor(() => expect(saveBinding).toHaveBeenCalledWith({ kind: "agent", name: "fake-agent" }));
    await vi.waitFor(() => expect(Array.from(target.querySelectorAll<HTMLButtonElement>(".agent-binding-button"))[1].disabled).toBe(false));

    Array.from(target.querySelectorAll<HTMLButtonElement>(".agent-binding-button"))[1].click();
    await tick();
    target.querySelector<HTMLButtonElement>('[data-binding="agent:fake-agent"]')!.click();
    await vi.waitFor(() => expect(saveDefaults).toHaveBeenCalledWith({ project: { kind: "agent", name: "fake-agent" }, task: { kind: "profile", name: "default" } }));
  });

  it("manages Workspace users from the Workspace settings tab", async () => {
    const savePreference = vi.fn(async () => undefined);
    const deleteUser = vi.fn(async () => undefined);
    const { target } = mountModel(resourceModel({
      identity: "ws:workspace", resourceId: "workspace", resourceType: "workspace", resourceTitle: "Test workspace", detail: null,
      workspaceUsers: [
        { version: 1, name: "User", preference: "Default tone" },
        { version: 1, name: "Alice", preference: "Concise" },
      ],
      currentUserName: "User",
      onSaveWorkspaceUserPreference: savePreference,
      onDeleteWorkspaceUser: deleteUser,
    }));
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Settings"))!.click();
    await tick();

    expect(target.textContent).toContain("Users");
    const rows = target.querySelectorAll<HTMLElement>(".resource-settings-user-row");
    expect(rows).toHaveLength(2);
    expect(rows[0].querySelector<HTMLButtonElement>('[title="Switch to another user before deleting this user"]')?.disabled).toBe(true);

    const preference = rows[1].querySelector<HTMLTextAreaElement>('[aria-label="Preference for Alice"]')!;
    preference.value = "Use concise replies";
    preference.dispatchEvent(new InputEvent("input", { bubbles: true }));
    rows[1].querySelector<HTMLButtonElement>(".resource-settings-user-save")!.click();
    await vi.waitFor(() => expect(savePreference).toHaveBeenCalledWith("Alice", "Use concise replies"));

    const deleteButton = rows[1].querySelector<HTMLButtonElement>('[title="Delete Alice"]')!;
    await vi.waitFor(() => expect(deleteButton.disabled).toBe(false));
    deleteButton.click();
    await vi.waitFor(() => expect(deleteUser).toHaveBeenCalledWith("Alice"));
  });

  it("saves the Workspace Generation policy from the settings tab", async () => {
    const savePolicy = vi.fn(async () => undefined);
    const { target } = mountModel(resourceModel({
      identity: "ws:workspace", resourceId: "workspace", resourceType: "workspace", resourceTitle: "Test workspace", detail: null,
      workspaceAgents: { path: "AGENTS.md", content: "# Notes\n", contentHash: "hash-a" },
      onSaveGenerationPolicy: savePolicy,
    }));
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Settings"))!.click();
    await tick();

    const budgetEnabled = target.querySelector<HTMLInputElement>('[aria-label="Enable usage-based Generation rotation"]')!;
    const inactivityEnabled = target.querySelector<HTMLInputElement>('[aria-label="Enable inactivity-based Generation rotation"]')!;
    const maxTurns = target.querySelector<HTMLInputElement>('[aria-label="Maximum Turns per Generation"]')!;
    const maxMinutes = target.querySelector<HTMLInputElement>('[aria-label="Maximum accumulated Turn minutes per Generation"]')!;
    const maxInactivityMinutes = target.querySelector<HTMLInputElement>('[aria-label="Maximum inactivity minutes per Generation"]')!;
    expect(budgetEnabled.checked).toBe(true);
    expect(inactivityEnabled.checked).toBe(true);
    expect(maxTurns.value).toBe("20");
    expect(maxMinutes.value).toBe("120");
    expect(maxInactivityMinutes.value).toBe("1440");

    budgetEnabled.click();
    maxTurns.value = "25";
    maxTurns.dispatchEvent(new InputEvent("input", { bubbles: true }));
    maxMinutes.value = "150";
    maxMinutes.dispatchEvent(new InputEvent("input", { bubbles: true }));
    maxInactivityMinutes.value = "2880";
    maxInactivityMinutes.dispatchEvent(new InputEvent("input", { bubbles: true }));
    await tick();
    target.querySelector<HTMLButtonElement>(".resource-settings-generation-controls button")!.click();
    await vi.waitFor(() => expect(savePolicy).toHaveBeenCalledWith({ budgetEnabled: false, maxTurns: 25, maxAccumulatedTurnMinutes: 150, inactivityEnabled: true, maxInactivityMinutes: 2880 }));
  });

  it("shows the inheritable Task default on the project settings tab", async () => {
    const saveTaskDefault = vi.fn(async () => undefined);
    const initial = resourceModel({
      identity: "ws:project1:project", resourceId: "project1", resourceType: "project", resourceTitle: "Project", parent: null,
      detail: { id: "project1", type: "project", title: "Project", path: "project1", agentBinding: { kind: "profile", name: "default" }, files: [], artifacts: [] },
      onSaveTaskDefault: saveTaskDefault,
    });
    const { channel, target } = mountModel(initial);
    await tick();
    (Array.from(target.querySelectorAll(".details-tab")) as HTMLButtonElement[]).find((button) => button.textContent?.includes("Settings"))!.click();
    await tick();

    const selectors = Array.from(target.querySelectorAll<HTMLButtonElement>(".agent-binding-button"));
    expect(selectors.length).toBe(2);
    expect(selectors[1].textContent).toContain("Inherit");

    selectors[1].click();
    await tick();
    // An inherited (empty) binding must not inject a missing-profile placeholder.
    expect(target.querySelector('[data-binding="profile:"]')).toBeNull();
    expect(target.textContent).not.toContain("missing profile");
    target.querySelector<HTMLButtonElement>('[data-binding="profile:default"]')!.click();
    await vi.waitFor(() => expect(saveTaskDefault).toHaveBeenCalledWith("project1", { kind: "profile", name: "default" }));

    channel.publish(resourceModel({
      identity: "ws:project1:project", resourceId: "project1", resourceType: "project", resourceTitle: "Project", parent: null,
      detail: { id: "project1", type: "project", title: "Project", path: "project1", agentBinding: { kind: "profile", name: "default" }, taskDefault: { kind: "profile", name: "default" }, files: [], artifacts: [] },
      onSaveTaskDefault: saveTaskDefault,
    }));
    await vi.waitFor(() => expect(Array.from(target.querySelectorAll<HTMLButtonElement>(".agent-binding-button"))[1].disabled).toBe(false));

    Array.from(target.querySelectorAll<HTMLButtonElement>(".agent-binding-button"))[1].click();
    await tick();
    target.querySelector<HTMLButtonElement>('[data-binding="inherit"]')!.click();
    await vi.waitFor(() => expect(saveTaskDefault).toHaveBeenCalledWith("project1", null));
  });

  function projectModel(overrides: Partial<DetailPanelModel> = {}): DetailPanelModel {
    const base = resourceModel();
    return resourceModel({
      identity: "ws:project1:project",
      resourceId: "project1",
      resourceType: "project",
      resourceTitle: "Project Alpha",
      parent: null,
      detail: {
        ...base.detail!,
        id: "project1",
        type: "project",
        title: "Project Alpha",
        path: "project1",
        files: [{ name: "project.md", path: "project1/project.md", content: "# Project Alpha", contentHash: "doc-p" }],
        templates: [{ name: "feature-a", title: "Feature A", path: "project1/templates/feature-a.md", valid: true, schemaVersion: 2, fields: [] }],
      },
      ...overrides,
    });
  }

  async function openSettingsTab(target: HTMLElement): Promise<void> {
    await tick();
    (Array.from(target.querySelectorAll<HTMLButtonElement>(".details-tab")).find((button) => button.textContent?.includes("Settings")))!.click();
    await tick();
  }

  it("renames a Project inline from the settings tab", async () => {
    const rename = vi.fn(async () => undefined);
    const { target } = mountModel(projectModel({ onRenameResource: rename }));
    await openSettingsTab(target);

    const row = target.querySelector<HTMLElement>(".resource-settings-name-row")!;
    expect(row.querySelector(".resource-settings-value-text")?.textContent).toBe("Project Alpha");
    const edit = Array.from(row.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!;
    edit.click();
    await tick();

    const input = row.querySelector<HTMLInputElement>(".resource-settings-name-input")!;
    expect(input.value).toBe("Project Alpha");
    expect(row.classList.contains("editing")).toBe(true);
    input.value = "Project Beta";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    const save = Array.from(row.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Save")!;
    expect(row.querySelector("strong")?.textContent).toBe("Name");
    save.click();
    await vi.waitFor(() => expect(rename).toHaveBeenCalledWith("Project Beta"));
  });

  it("cancels a rename with Escape without calling the callback", async () => {
    const rename = vi.fn(async () => undefined);
    const { target } = mountModel(projectModel({ onRenameResource: rename }));
    await openSettingsTab(target);

    const row = target.querySelector<HTMLElement>(".resource-settings-name-row")!;
    Array.from(row.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!.click();
    await tick();
    const input = row.querySelector<HTMLInputElement>(".resource-settings-name-input")!;
    input.value = "Discarded";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await tick();

    expect(row.querySelector(".resource-settings-name-input")).toBeNull();
    expect(row.querySelector(".resource-settings-value-text")?.textContent).toBe("Project Alpha");
    expect(rename).not.toHaveBeenCalled();
  });

  it("renames a Task inline from the settings tab", async () => {
    const rename = vi.fn(async () => undefined);
    const { target } = mountModel(resourceModel({ onRenameResource: rename }));
    await openSettingsTab(target);

    const row = target.querySelector<HTMLElement>(".resource-settings-name-row")!;
    expect(row.querySelector(".resource-settings-value-text")?.textContent).toBe("Stable detail");
    Array.from(row.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!.click();
    await tick();
    const input = row.querySelector<HTMLInputElement>(".resource-settings-name-input")!;
    input.value = "Renamed task";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await vi.waitFor(() => expect(rename).toHaveBeenCalledWith("Renamed task"));
  });

  it("edits a Task description inline from the settings tab", async () => {
    const saveDescription = vi.fn(async () => undefined);
    const base = resourceModel({ onSaveDescription: saveDescription });
    const { target } = mountModel(resourceModel({ ...base, detail: { ...base.detail!, description: "Original summary" } }));
    await openSettingsTab(target);

    const row = target.querySelector<HTMLElement>(".resource-settings-desc-row")!;
    expect(row.querySelector(".resource-settings-value-text")?.textContent).toBe("Original summary");
    Array.from(row.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!.click();
    await tick();
    const input = row.querySelector<HTMLInputElement>(".resource-settings-desc-input")!;
    expect(input.value).toBe("Original summary");
    input.value = "Updated summary";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await vi.waitFor(() => expect(saveDescription).toHaveBeenCalledWith("Updated summary"));
  });

  it("clears a Project description with an empty value", async () => {
    const saveDescription = vi.fn(async () => undefined);
    const base = projectModel({ onSaveDescription: saveDescription });
    const { target } = mountModel(projectModel({ ...base, detail: { ...base.detail!, description: "Original summary" } }));
    await openSettingsTab(target);

    const row = target.querySelector<HTMLElement>(".resource-settings-desc-row")!;
    Array.from(row.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Edit")!.click();
    await tick();
    const input = row.querySelector<HTMLInputElement>(".resource-settings-desc-input")!;
    input.value = "   ";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    Array.from(row.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Save")!.click();
    await vi.waitFor(() => expect(saveDescription).toHaveBeenCalledWith(""));
  });

  it("shows an empty placeholder when a Project has no description", async () => {
    const { target } = mountModel(projectModel());
    await openSettingsTab(target);

    const row = target.querySelector<HTMLElement>(".resource-settings-desc-row")!;
    expect(row.querySelector(".resource-settings-value-empty")?.textContent).toBe("No description");
  });

  it("moves the Project template list into settings without a Template tab", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({
      path: "project1/templates/feature-a.md", name: "feature-a.md", content: "# Feature A", contentHash: "tpl-a",
    }), { headers: { "content-type": "application/json" } })));
    const { target } = mountModel(projectModel());
    await tick();

    const tabs = Array.from(target.querySelectorAll<HTMLButtonElement>("[role=tab]"));
    expect(tabs.find((tab) => tab.textContent?.trim() === "Template")).toBeUndefined();

    await openSettingsTab(target);
    const row = target.querySelector<HTMLButtonElement>(".template-row")!;
    expect(row.textContent).toContain("Feature A");
    row.click();
    await vi.waitFor(() => expect(target.querySelector<HTMLElement>('[role="dialog"]')).not.toBeNull());
  });

});
