import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import AppShell from "../../src/components/AppShell.svelte";
import { createModelChannel } from "../../src/components/model-channel";
import type { AppShellModel, ShellResourceItem, ShellStatusPresentation } from "../../src/components/models";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
  document.body.className = "";
  vi.unstubAllGlobals();
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

function model(overrides: Partial<AppShellModel> = {}): AppShellModel {
  return {
    identity: "workspace-a", loading: false, error: "", version: "v0.1.0", activeWorkspaceId: "workspace-a", workspaceName: "Workspace A",
    userGate: { mode: "", users: [], suggestedUserName: "", missingUserName: "" },
    workspaces: [
      { id: "workspace-a", name: "Workspace A", path: "/tmp/a", iconSrc: "/favicon.svg" },
      { id: "workspace-b", name: "Workspace B", path: "/tmp/b", iconSrc: "/favicon.svg" },
    ],
    projects: [resource("project-a", "Project A"), resource("project-b", "Project B")], treeEditing: false, treeFilter: "all",
    doctor: { checking: false, complete: true, summary: { errors: 0, warnings: 0 }, workspaces: [] },
    paneSizes: { sidebarWidth: 280, chatWidth: 420, chatHeight: 320 },
    mobile: { sidebarOpen: false },
    route: { path: "", revision: 0, replace: true },
    onSwitchWorkspace: vi.fn(async () => undefined), onResolveWorkspaceUser: vi.fn(async () => undefined), onAddWorkspace: vi.fn(), onCreateProject: vi.fn(), onCreateTask: vi.fn(async () => undefined), onArchive: vi.fn(async () => undefined), onOpenSettings: vi.fn(), onRefreshDoctor: vi.fn(async () => undefined),
    onToggleProject: vi.fn(async () => undefined), onSelectResource: vi.fn(async () => undefined), onReorder: vi.fn(async () => undefined),
    onDragState: vi.fn(), onToggleTreeEditing: vi.fn(), onTreeFilter: vi.fn(), onCreateFolder: vi.fn(async () => ""), onRenameFolder: vi.fn(async () => undefined),
    onDeleteFolder: vi.fn(async () => undefined), onToggleFolder: vi.fn(async () => undefined), onPanePreview: vi.fn(), onPaneCommit: vi.fn(), onPaneViewport: vi.fn(), onMobileSidebar: vi.fn(),
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
  it("uses the transparent yellow umbrella for the PUA brand and default workspace icon", async () => {
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel: createModelChannel(model({
      workspaces: [{ id: "workspace-a", name: "Workspace A", path: "/tmp/a", iconSrc: "" }],
    })) } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector<HTMLImageElement>(".brand-mark img")?.getAttribute("src")).toBe("/workspace-icons/pua-yellow.png");
    expect(target.querySelector<HTMLImageElement>("#workspaceAvatar img")?.getAttribute("src")).toBe("/workspace-icons/pua-yellow.png");
  });

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

    const projectA = target.querySelector<HTMLElement>('.tree-row-navigation[aria-label="Project A"]')!;
    projectA.dataset.identityProbe = "stable";
    projectA.click();
    await vi.waitFor(() => expect(onSelectResource).toHaveBeenCalledWith("project-a"));

    channel.publish({
      ...initial,
      projects: initial.projects.map((project) => project.id === "project-a"
        ? { ...project, active: true, statusLabel: "Session running", status: { hasTaskState: true, className: "task-status-session-running", layoutClassName: "has-task-status", slotClassName: "task-status-single", statuses: [{ key: "session", className: "task-status-session-running", iconName: "loader-circle", recentOutput: true }] } }
        : project),
    });
    await tick();

    expect(target.querySelector('.tree-row-navigation[aria-label="Project A"]')).toBe(projectA);
    expect(projectA.dataset.identityProbe).toBe("stable");
    expect(projectA.closest(".tree-item")?.classList.contains("active")).toBe(true);
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

  it("exposes the tree filters and restores focus when a filtered row disappears", async () => {
    const initial = model();
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const filter = target.querySelector<HTMLSelectElement>('select[aria-label="Filter projects and tasks"]')!;
    expect([...filter.options].map((option) => option.text)).toEqual(["All", "Running", "Unread"]);
    expect(filter.value).toBe("all");
    expect(target.querySelector("#activityResize, .attention-section")).toBeNull();
    filter.value = "unread";
    filter.dispatchEvent(new Event("change", { bubbles: true }));
    expect(initial.onTreeFilter).toHaveBeenCalledWith("unread");
    channel.publish({ ...initial, treeFilter: "unread", projects: [initial.projects[0]] });
    await tick();

    const row = target.querySelector<HTMLElement>(".tree-item button, button.tree-item")!;
    row.focus();
    expect(document.activeElement).toBe(row);
    channel.publish({ ...initial, treeFilter: "unread", projects: [] });
    await vi.waitFor(() => expect(document.activeElement).toBe(filter));
    expect(target.querySelector("#projectTree")?.textContent).toContain("No projects or tasks with unread chat");
    expect(initial.onSelectResource).not.toHaveBeenCalled();

    channel.publish({ ...initial, treeFilter: "running", projects: [] });
    await tick();
    expect(target.querySelector("#projectTree")?.textContent).toContain("No running projects or tasks");
    channel.publish({ ...initial, treeEditing: true });
    await tick();
    expect(filter.disabled).toBe(true);
    expect(filter.value).toBe("all");
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
    vi.stubGlobal("matchMedia", () => ({ matches: true, addEventListener: vi.fn(), removeEventListener: vi.fn() }));
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

  it("reserves an empty right column without a stacked divider", async () => {
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
    expect(target.querySelector("#agentPanel")?.childElementCount).toBe(0);
    expect(target.querySelector("#detailsResizeY")).toBeNull();
    const vertical = target.querySelector<HTMLElement>("#detailsResize")!;
    expect(vertical.getAttribute("aria-orientation")).toBe("vertical");
  });

  it("keeps Workspace switching available while identity selection hides personal panes", async () => {
    const onResolveWorkspaceUser = vi.fn(async () => undefined);
    const initial = model({
      scheduler: resource("scheduler", "Scheduler"),
      userGate: {
        mode: "select",
        users: [{ name: "Alice", preference: "Concise replies" }, { name: "Bob", preference: "" }],
        suggestedUserName: "",
        missingUserName: "Missing",
      },
      onResolveWorkspaceUser,
    });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector("#workspaceSwitcher")).not.toBeNull();
    expect(target.querySelector("#schedulerNav")).toBeNull();
    expect(target.querySelector("#detailsPanel")).toBeNull();
    expect(target.querySelector("#agentPanel")).toBeNull();
    expect(target.textContent).toContain("Previously selected user Missing is no longer available");
    [...target.querySelectorAll<HTMLButtonElement>(".workspace-user-options button")].find((button) => button.textContent?.includes("Alice"))!.click();
    await vi.waitFor(() => expect(onResolveWorkspaceUser).toHaveBeenCalledWith("Alice", false));
  });

  it("can create and select a new identity from the multi-user gate", async () => {
    const onResolveWorkspaceUser = vi.fn(async () => undefined);
    const initial = model({
      userGate: {
        mode: "select",
        users: [{ name: "Alice", preference: "" }, { name: "Bob", preference: "" }],
        suggestedUserName: "",
        missingUserName: "",
      },
      onResolveWorkspaceUser,
    });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(AppShell, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLButtonElement>(".workspace-user-add-toggle")!.click();
    await tick();
    const newUser = target.querySelector<HTMLInputElement>("#workspaceNewUserName")!;
    newUser.value = "Carol";
    newUser.dispatchEvent(new Event("input", { bubbles: true }));
    target.querySelector<HTMLFormElement>(".workspace-user-add")!.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(onResolveWorkspaceUser).toHaveBeenCalledWith("Carol", true));
  });
});

it("shows live chat state in the trailing resource row slot without a separate chat list", async () => {
 const project = {...resource("project-a", "Project A"),chatOpen:true,unreadCount:2};
 const channel=createModelChannel(model({projects:[project]}));
 const target=document.body.appendChild(document.createElement("div"));
 const component=mount(AppShell,{target,props:{channel}});
 cleanups.push(()=>unmount(component));
 await tick();
 const title=target.querySelector('.name-text')!;
 expect(title.parentElement?.nextElementSibling?.classList.contains('resource-chat-status')).toBe(true);
 expect(title.parentElement?.nextElementSibling?.textContent?.trim()).toBe('2');
 expect(target.querySelector('.chat-open-icon')).not.toBeNull();
 expect(target.querySelector('.chat-list,[aria-label="Chats"]')).toBeNull();
 channel.publish(model({projects:[{...project,unreadCount:0}]}));await tick();
 expect(target.querySelector('.chat-open-icon')).not.toBeNull();
 expect(target.querySelector('.unread-badge')).toBeNull();
 channel.publish(model({projects:[{...project,chatOpen:false,unreadCount:0}]}));await tick();
 expect(target.querySelector('.chat-open-icon')).toBeNull();
});
