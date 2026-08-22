import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import AppShell from "../../src/components/AppShell.svelte";
import { createModelChannel } from "../../src/components/model-channel";
import type { AppShellModel, ShellActivityItem, ShellResourceItem, ShellStatusPresentation } from "../../src/components/models";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
  document.body.className = "";
});

const emptyStatus: ShellStatusPresentation = {
  hasTaskState: false, className: "", layoutClassName: "", slotClassName: "", statuses: [],
};

function resource(id: string, title: string, type: "project" | "task" = "project"): ShellResourceItem {
  return {
    id, type, title, ref: type === "project" ? "#1" : "#2", active: false, expanded: false,
    ariaLabel: title, statusLabel: "", status: emptyStatus, summary: null, children: [], unreadCount: 0,
  };
}

function activity(status: ShellStatusPresentation = emptyStatus): ShellActivityItem {
  return {
    id: "project-a", type: "project", title: "Project A", ref: "#1", selected: true,
    activeTurn: status.hasTaskState, unreadCount: status.hasTaskState ? 1 : 0, turnNumber: status.hasTaskState ? 1 : 0,
    agentName: status.hasTaskState ? "Codex" : "", statusLabel: status.hasTaskState ? "Resource working" : "Active turn", status,
  };
}

function model(overrides: Partial<AppShellModel> = {}): AppShellModel {
  return {
    identity: "workspace-a", loading: false, error: "", version: "v0.1.0", activeWorkspaceId: "workspace-a", workspaceName: "Workspace A",
    workspaces: [
      { id: "workspace-a", name: "Workspace A", path: "/tmp/a", iconSrc: "/favicon.svg" },
      { id: "workspace-b", name: "Workspace B", path: "/tmp/b", iconSrc: "/favicon.svg" },
    ],
    projects: [resource("project-a", "Project A"), resource("project-b", "Project B")], treeEditing: false, activity: { running: [], unread: [], problems: [] }, inbox: [],
    doctor: { checking: false, complete: true, summary: { errors: 0, warnings: 0 }, workspaces: [] },
    paneSizes: { sidebarWidth: 280, chatWidth: 420, chatHeight: 320, sidebarAttentionHeight: 210 },
    mobile: { sidebarOpen: false },
    layout: { preference: "auto", effective: "three" },
    route: { path: "", revision: 0, replace: true },
    resolveResourceTitle: () => null,
    onSwitchWorkspace: vi.fn(async () => undefined), onAddWorkspace: vi.fn(), onCreateProject: vi.fn(), onOpenSettings: vi.fn(), onRefreshDoctor: vi.fn(async () => undefined),
    onToggleProject: vi.fn(async () => undefined), onSelectResource: vi.fn(async () => undefined), onReorder: vi.fn(async () => undefined),
    onDragState: vi.fn(), onToggleTreeEditing: vi.fn(), onCreateFolder: vi.fn(async () => ""), onRenameFolder: vi.fn(async () => undefined),
    onDeleteFolder: vi.fn(async () => undefined), onToggleFolder: vi.fn(async () => undefined), onOpenInboxMessage: vi.fn(async () => undefined), onReplyInboxMessage: vi.fn(async () => undefined), onDeleteInboxMessage: vi.fn(async () => undefined), onPanePreview: vi.fn(), onPaneCommit: vi.fn(), onPaneViewport: vi.fn(), onMobileSidebar: vi.fn(),
    onToast: vi.fn(),
    onHistoryNavigation: vi.fn(async () => undefined),
    ...overrides,
  };
}

function dragEvent(type: string, clientY = 0): Event {
  const event = new Event(type, { bubbles: true, cancelable: true });
  Object.defineProperty(event, "clientY", { value: clientY });
  Object.defineProperty(event, "dataTransfer", { value: { effectAllowed: "", dropEffect: "", setData: vi.fn() } });
  return event;
}

describe("AppShell", () => {
  it("shows Doctor issues from the brand reminder and refreshes the report", async () => {
    const onRefreshDoctor = vi.fn(async () => undefined);
    const initial = model({
      onRefreshDoctor,
      doctor: {
        checking: false,
        complete: true,
        summary: { errors: 1, warnings: 0 },
        workspaces: [{
          id: "workspace-a",
          name: "Workspace A",
          path: "/tmp/a",
          report: {
            complete: true,
            summary: { errors: 1, warnings: 0 },
            issues: [{ severity: "error", code: "managed_section_modified", message: "PUA-managed instructions were modified", path: "AGENTS.md", suggestion: "Restore the managed section." }],
          },
        }],
      },
    });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLButtonElement>("#doctorButton")!.click();
    await tick();
    expect(target.querySelector('[role="dialog"]')).not.toBeNull();
    expect(target.textContent).toContain("PUA-managed instructions were modified");
    expect(target.textContent).toContain("managed_section_modified");

    target.querySelector<HTMLButtonElement>('[aria-label="Refresh workspace checks"]')!.click();
    await vi.waitFor(() => expect(onRefreshDoctor).toHaveBeenCalledTimes(1));
  });

  it("keeps keyed navigation nodes stable while canonical selection and status projections update", async () => {
    const onSelectResource = vi.fn(async () => undefined);
    const initial = model({ onSelectResource });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    target.id = "app";
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const projectA = target.querySelector<HTMLElement>('.tree-item[aria-label="Project A"]')!;
    projectA.dataset.identityProbe = "stable";
    target.querySelector<HTMLButtonElement>('.tree-item[aria-label="Project A"]')!.click();
    await vi.waitFor(() => expect(onSelectResource).toHaveBeenCalledWith("project-a"));

    channel.publish({
      ...initial,
      projects: initial.projects.map((project) => project.id === "project-a"
        ? { ...project, active: true, statusLabel: "Session running", status: { hasTaskState: true, className: "task-status-session-running", layoutClassName: "has-task-status", slotClassName: "task-status-single", statuses: [{ key: "session", className: "task-status-session-running", iconName: "loader-circle", recentOutput: true }] } }
        : project),
    });
    await tick();

    expect(target.querySelector('.tree-item[aria-label="Project A"]')).toBe(projectA);
    expect(projectA.dataset.identityProbe).toBe("stable");
    expect(projectA.classList.contains("active")).toBe(true);
  });

  it("keeps a hovered Task state tooltip open across unrelated Tree updates", async () => {
    const blockedStatus: ShellStatusPresentation = {
      hasTaskState: true,
      className: "task-state-blocked",
      layoutClassName: "has-task-status",
      slotClassName: "task-status-single",
      statuses: [{ key: "task-blocked", className: "task-state-blocked", iconName: "circle-alert", recentOutput: false }],
    };
    const taskA = { ...resource("task-a", "Task A", "task"), statusLabel: "Blocked: Need approval", status: blockedStatus };
    const taskB = { ...resource("task-b", "Task B", "task"), statusLabel: "Waiting: CI", status: blockedStatus };
    const project = { ...resource("project-a", "Project A"), expanded: true, children: [taskA, taskB] };
    const initial = model({ projects: [project] });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const taskRow = target.querySelector<HTMLElement>('[data-task-id="task-a"]')!;
    taskRow.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    expect(target.querySelector(".task-state-tooltip")?.textContent).toBe("Blocked: Need approval");

    const nativeMatches = taskRow.matches.bind(taskRow);
    const matches = vi.spyOn(taskRow, "matches").mockImplementation((selector) => selector === ":hover" || nativeMatches(selector));
    channel.publish({
      ...initial,
      projects: [{ ...project, children: [taskA, { ...taskB, statusLabel: "Completed elsewhere" }] }],
    });
    await tick();
    taskRow.dispatchEvent(new MouseEvent("mouseleave", { bubbles: true }));
    await Promise.resolve();

    expect(target.querySelector(".task-state-tooltip")?.textContent).toBe("Blocked: Need approval");
    matches.mockRestore();
  });

  it("keeps the Task state tooltip open when an unrelated container scrolls", async () => {
    const blockedStatus: ShellStatusPresentation = {
      hasTaskState: true,
      className: "task-state-blocked",
      layoutClassName: "has-task-status",
      slotClassName: "task-status-single",
      statuses: [{ key: "task-blocked", className: "task-state-blocked", iconName: "circle-alert", recentOutput: false }],
    };
    const taskA = { ...resource("task-a", "Task A", "task"), statusLabel: "Blocked: Need approval", status: blockedStatus };
    const project = { ...resource("project-a", "Project A"), expanded: true, children: [taskA] };
    const initial = model({ projects: [project] });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const taskRow = target.querySelector<HTMLElement>('[data-task-id="task-a"]')!;
    taskRow.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    expect(target.querySelector(".task-state-tooltip")?.textContent).toBe("Blocked: Need approval");

    // A scroll inside an unrelated container (e.g. the chat timeline
    // auto-scrolling on a new message) must not dismiss the tooltip.
    const chatScroll = document.body.appendChild(document.createElement("div"));
    chatScroll.dispatchEvent(new Event("scroll"));
    await tick();
    expect(target.querySelector(".task-state-tooltip")?.textContent).toBe("Blocked: Need approval");

    // A viewport scroll still invalidates the anchor position and hides it.
    document.dispatchEvent(new Event("scroll"));
    await tick();
    expect(target.querySelector(".task-state-tooltip")).toBeNull();
  });

  it("keeps the Activity grid stable when a new resource starts its first turn", async () => {
      const initial = model({ activity: { running: [activity()], unread: [], problems: [] } });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const row = target.querySelector<HTMLElement>('[data-component-owner="attention-list"] button.activity-row')!;
    const status = row.querySelector<HTMLElement>(".activity-status")!;
    expect([...row.children].map((child) => child.className)).toEqual([
      "activity-status", "activity-title",
    ]);
    const fallbackSlot = status.querySelector<HTMLElement>(".activity-status-fallback-slot")!;
    const runtimeSlot = status.querySelector<HTMLElement>(".activity-status-runtime-slot")!;
    expect(fallbackSlot.hidden).toBe(false);
    expect(runtimeSlot.hidden).toBe(true);
    expect(fallbackSlot.querySelector('[data-lucide="folder"]')).not.toBeNull();

    const runningStatus: ShellStatusPresentation = {
      hasTaskState: true,
      className: "task-status-session-running",
      layoutClassName: "has-task-status",
      slotClassName: "task-status-single",
      statuses: [{ key: "resource-running:0", className: "task-status-session-running", iconName: "loader-circle", recentOutput: true }],
    };
      channel.publish({ ...initial, activity: { ...initial.activity, running: [activity(runningStatus)] } });
    await tick();

    expect(target.querySelector('[data-component-owner="attention-list"] button.activity-row')).toBe(row);
    expect([...row.children].map((child) => child.className)).toEqual([
      "activity-status", "activity-title",
    ]);
    expect(row.querySelector(".activity-status")).toBe(status);
    expect(fallbackSlot.hidden).toBe(true);
    expect(runtimeSlot.hidden).toBe(false);
    expect(runtimeSlot.querySelectorAll('[data-lucide="loader-circle"]')).toHaveLength(1);
    expect(fallbackSlot.querySelector('[data-lucide="folder"]')).not.toBeNull();

      channel.publish({ ...initial, activity: { ...initial.activity, running: [activity()] } });
    await tick();

    expect(target.querySelector('[data-component-owner="attention-list"] button.activity-row')).toBe(row);
  });

  it("keeps drag state local and sends one typed reorder transaction in tree edit mode", async () => {
    const onReorder = vi.fn(async () => undefined);
    const onDragState = vi.fn();
    const channel = createModelChannel(model({ onReorder, onDragState, treeEditing: true }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const rows = target.querySelectorAll<HTMLElement>(".project-tree > .tree-item");
    rows[0].querySelector<HTMLElement>(".drag-handle")!.dispatchEvent(dragEvent("dragstart"));
    rows[1].dispatchEvent(dragEvent("dragover", 1));
    rows[1].dispatchEvent(dragEvent("drop", 1));
    await vi.waitFor(() => expect(onReorder).toHaveBeenCalledTimes(1));
    expect(onReorder).toHaveBeenCalledWith(
      { kind: "project", id: "project-a", projectId: "" },
      { kind: "project", id: "project-b", projectId: "" },
      true,
    );
    expect(onDragState).toHaveBeenNthCalledWith(1, { kind: "project", id: "project-a", projectId: "" });
    expect(onDragState).toHaveBeenLastCalledWith(null);
  });

  it("deduplicates workspace switching and reports a rejected switch", async () => {
    let rejectSwitch!: (reason: unknown) => void;
    const pending = new Promise<void>((_resolve, reject) => { rejectSwitch = reject; });
    const onSwitchWorkspace = vi.fn(() => pending);
    const onToast = vi.fn();
    const channel = createModelChannel(model({ onSwitchWorkspace, onToast }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLButtonElement>("#workspaceSwitcher")!.click();
    await tick();
    const workspaceB = target.querySelector<HTMLButtonElement>('[data-workspace-id="workspace-b"]')!;
    workspaceB.click();
    workspaceB.click();
    expect(onSwitchWorkspace).toHaveBeenCalledTimes(1);
    rejectSwitch(new Error("workspace unavailable"));
    await vi.waitFor(() => expect(onToast).toHaveBeenCalledWith("workspace unavailable"));
  });

  it("opens the workspace page from the switcher name zone", async () => {
    const onSelectResource = vi.fn(async () => undefined);
    const channel = createModelChannel(model({ onSelectResource }));
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLButtonElement>("#workspaceOpen")!.click();
    await vi.waitFor(() => expect(onSelectResource).toHaveBeenCalledWith("workspace"));
    expect(target.querySelector("#workspaceMenu")).toBeNull();
  });

  it("owns History API projection and forwards popstate paths to the navigation controller", async () => {
    window.history.replaceState({}, "", "/");
    const onHistoryNavigation = vi.fn(async () => undefined);
    const initial = model({ onHistoryNavigation });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    channel.publish({ ...initial, route: { path: "/w/workspace-a/r/project-a", revision: 1, replace: false } });
    await tick();
    expect(window.location.pathname).toBe("/w/workspace-a/r/project-a");

    window.history.replaceState({}, "", "/w/workspace-b/r/project-b");
    window.dispatchEvent(new PopStateEvent("popstate"));
    await vi.waitFor(() => expect(onHistoryNavigation).toHaveBeenCalledWith("/w/workspace-b/r/project-b"));
  });

  it("preserves Escape priority between the mobile sidebar and local menus", async () => {
    const onMobileSidebar = vi.fn();
    const initial = model({ mobile: { sidebarOpen: true }, onMobileSidebar });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLButtonElement>("#workspaceSwitcher")!.click();
    await tick();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    expect(onMobileSidebar).toHaveBeenCalledWith(false);
    expect(target.querySelector("#workspaceMenu")).not.toBeNull();

    channel.publish({ ...initial, mobile: { ...initial.mobile, sidebarOpen: false } });
    await tick();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
    await tick();
    expect(target.querySelector("#workspaceMenu")).toBeNull();
  });

  it("renders both panes with a horizontal divider for the stacked two-column layout", async () => {
    const initial = model();
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector("#detailsPanel")).not.toBeNull();
    expect(target.querySelector("#agentPanel")).not.toBeNull();
    expect(target.querySelector("#paneDetailsTab")).toBeNull();
    expect(target.querySelector("#paneChatTab")).toBeNull();
    const divider = target.querySelector<HTMLElement>("#detailsResizeY")!;
    expect(divider.getAttribute("role")).toBe("separator");
    expect(divider.getAttribute("aria-orientation")).toBe("horizontal");
    const vertical = target.querySelector<HTMLElement>("#detailsResize")!;
    expect(vertical.getAttribute("aria-orientation")).toBe("vertical");
  });
});
