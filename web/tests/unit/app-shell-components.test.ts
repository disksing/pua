import { mount, tick, unmount } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import MobileToolbar from "../../src/components/MobileToolbar.svelte";
import ActivityPanel from "../../src/components/ActivityPanel.svelte";
import ProjectTree from "../../src/components/ProjectTree.svelte";
import PaneResizeHandle from "../../src/components/PaneResizeHandle.svelte";
import SchedulerNav from "../../src/components/SchedulerNav.svelte";
import WorkspaceSwitcher from "../../src/components/WorkspaceSwitcher.svelte";
import type { ShellActivityItem, ShellInboxMessage, ShellResourceItem, ShellStatusPresentation } from "../../src/components/models";

const cleanups: Array<() => Promise<void>> = [];

beforeEach(() => {
  Object.defineProperty(window, "matchMedia", {
    configurable: true,
    value: vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() })),
  });
});

afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
  document.body.className = "";
});

const emptyStatus: ShellStatusPresentation = {
  hasTaskState: false, className: "", layoutClassName: "", slotClassName: "", statuses: [],
};

function resource(id: string, type: "project" | "task" = "project"): ShellResourceItem {
  return {
    id, type, title: id, ref: `#${id}`, active: false, expanded: false, ariaLabel: id, statusLabel: "",
    status: emptyStatus, summary: null, children: [], unreadCount: 0,
  };
}

function treeEditProps(editing = false) {
  return {
    editing,
    onToggleEditing: vi.fn(),
    onCreateFolder: vi.fn(async () => ""),
    onRenameFolder: vi.fn(async () => undefined),
    onDeleteFolder: vi.fn(async () => undefined),
    onToggleFolder: vi.fn(async () => undefined),
  };
}

function dragEvent(type: string, clientY = 0): Event {
  const event = new Event(type, { bubbles: true, cancelable: true });
  Object.defineProperty(event, "clientY", { value: clientY });
  Object.defineProperty(event, "dataTransfer", { value: { effectAllowed: "", dropEffect: "", setData: vi.fn() } });
  return event;
}

function pointerEvent(type: string, clientX: number, clientY: number): Event {
  const event = new Event(type, { bubbles: true, cancelable: true });
  Object.defineProperties(event, { clientX: { value: clientX }, clientY: { value: clientY } });
  return event;
}

describe("AppShell responsibility components", () => {
  it("MobileToolbar owns navigation and backdrop actions", async () => {
    const onSidebar = vi.fn();
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(MobileToolbar, { target, props: { sidebarOpen: false, onSidebar } });
    cleanups.push(() => unmount(component));

    target.querySelector<HTMLButtonElement>("#mobileMenuButton")!.click();
    target.querySelector<HTMLButtonElement>("#mobileSidebarBackdrop")!.click();

    expect(onSidebar.mock.calls).toEqual([[true], [false]]);
    // The stacked single-column layout no longer switches views through tabs.
    expect(target.querySelector("#mobileChatButton")).toBeNull();
  });

  it("WorkspaceSwitcher owns menu dismissal, pending deduplication, and errors", async () => {
    let rejectSwitch!: (reason: unknown) => void;
    const pending = new Promise<void>((_resolve, reject) => { rejectSwitch = reject; });
    const onSwitch = vi.fn(() => pending);
    const onToast = vi.fn();
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(WorkspaceSwitcher, { target, props: {
      identity: "workspace-a", mobileSidebarOpen: false, activeWorkspaceId: "workspace-a",
      workspaces: [
        { id: "workspace-a", name: "Workspace A", path: "/tmp/a", iconSrc: "/favicon.svg" },
        { id: "workspace-b", name: "Workspace B", path: "/tmp/b", iconSrc: "/favicon.svg" },
      ],
      onSwitch, onOpen: vi.fn(async () => undefined), onAdd: vi.fn(), onToast,
    } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLButtonElement>("#workspaceSwitcher")!.click();
    await tick();
    const workspaceB = target.querySelector<HTMLButtonElement>('[data-workspace-id="workspace-b"]')!;
    workspaceB.click();
    workspaceB.click();
    expect(onSwitch).toHaveBeenCalledTimes(1);
    rejectSwitch(new Error("workspace unavailable"));
    await vi.waitFor(() => expect(onToast).toHaveBeenCalledWith("workspace unavailable"));

    target.querySelector<HTMLButtonElement>("#workspaceSwitcher")!.click();
    await tick();
    document.body.dispatchEvent(new MouseEvent("mousedown", { bubbles: true }));
    await tick();
    expect(target.querySelector("#workspaceMenu")).toBeNull();
  });

  it("WorkspaceSwitcher renders an empty state when a legacy response contains null workspaces", async () => {
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(WorkspaceSwitcher, { target, props: {
      identity: "no-workspace", mobileSidebarOpen: false, activeWorkspaceId: "",
      workspaces: null as never,
      onSwitch: vi.fn(async () => undefined), onOpen: vi.fn(async () => undefined), onAdd: vi.fn(), onToast: vi.fn(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector("#workspaceOpen")?.textContent).toContain("Workspace");
  });

  it("WorkspaceSwitcher exposes the full workspace name and path on menu options", async () => {
    const name = "workspace-title-that-is-intentionally-very-long-for-ellipsis-validation-task415";
    const path = "/Users/disksing/projects/pua-test/workspaces/workspace-title-that-is-intentionally-very-long-for-ellipsis-validation-task415";
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(WorkspaceSwitcher, { target, props: {
      identity: "workspace-a", mobileSidebarOpen: false, activeWorkspaceId: "workspace-a",
      workspaces: [{ id: "workspace-a", name, path, iconSrc: "/favicon.svg" }],
      onSwitch: vi.fn(async () => undefined), onOpen: vi.fn(async () => undefined), onAdd: vi.fn(), onToast: vi.fn(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLButtonElement>("#workspaceSwitcher")!.click();
    await tick();
    const option = target.querySelector<HTMLButtonElement>('[role="option"]')!;
    expect(option.title).toBe(`${name} — ${path}`);
    expect(option.getAttribute("aria-label")).toBe(`${name} — ${path}`);
  });

  it("WorkspaceSwitcher keeps its status icon static and toggles busy through a class", async () => {
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(WorkspaceSwitcher, { target, props: {
      identity: "workspace-a", mobileSidebarOpen: false, activeWorkspaceId: "workspace-a",
      workspaces: [{ id: "workspace-a", name: "Workspace A", path: "/tmp/a", iconSrc: "/favicon.svg" }],
      onSwitch: vi.fn(async () => undefined), onOpen: vi.fn(async () => undefined), onAdd: vi.fn(), onToast: vi.fn(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const button = target.querySelector<HTMLButtonElement>("#workspaceSwitcher")!;
    expect(button.querySelector('.workspace-switcher-icon-idle [data-lucide="chevrons-up-down"]')).not.toBeNull();
    expect(button.querySelector('.workspace-switcher-icon-busy [data-lucide="loader-circle"]')).not.toBeNull();
    expect(button.classList.contains("busy")).toBe(false);
  });

  it("WorkspaceSwitcher replaces the active Workspace icon while its Session is working", async () => {
    const workingStatus: ShellStatusPresentation = {
      hasTaskState: true, className: "task-status-session-running", layoutClassName: "has-task-status", slotClassName: "task-status-single",
      statuses: [{ key: "resource-working", className: "task-status-session-running", iconName: "loader-circle", recentOutput: false }],
    };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(WorkspaceSwitcher, { target, props: {
      identity: "workspace-a", mobileSidebarOpen: false, activeWorkspaceId: "workspace-a",
      workspaces: [{ id: "workspace-a", name: "Workspace A", path: "/tmp/a", iconSrc: "/favicon.svg", status: workingStatus, statusLabel: "Working" }],
      onSwitch: vi.fn(async () => undefined), onOpen: vi.fn(async () => undefined), onAdd: vi.fn(), onToast: vi.fn(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const avatar = target.querySelector<HTMLElement>("#workspaceAvatar")!;
    expect(avatar.querySelector('[data-lucide="loader-circle"]')).not.toBeNull();
    expect(avatar.querySelector("img")).toBeNull();
  });

  it("WorkspaceSwitcher opens the workspace from the name zone and keeps the menu on the chevron", async () => {
    const onOpen = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(WorkspaceSwitcher, { target, props: {
      identity: "workspace-a", mobileSidebarOpen: false, activeWorkspaceId: "workspace-a",
      workspaces: [{ id: "workspace-a", name: "Workspace A", path: "/tmp/a", iconSrc: "/favicon.svg" }],
      onSwitch: vi.fn(async () => undefined), onOpen, onAdd: vi.fn(), onToast: vi.fn(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const open = target.querySelector<HTMLButtonElement>("#workspaceOpen")!;
    expect(open.textContent).toContain("Workspace A");
    open.click();
    await vi.waitFor(() => expect(onOpen).toHaveBeenCalledTimes(1));
    // Pointer clicks drop focus so the head does not stay highlighted.
    expect(document.activeElement).not.toBe(open);
    // Opening the workspace does not open the switch menu.
    expect(target.querySelector("#workspaceMenu")).toBeNull();

    target.querySelector<HTMLButtonElement>("#workspaceSwitcher")!.click();
    await tick();
    expect(target.querySelector("#workspaceMenu")).not.toBeNull();
  });

  it("ProjectTree renders virtual folders and owns folder interactions", async () => {
    const onToggleFolder = vi.fn(async () => undefined);
    const folderA: ShellResourceItem = { ...resource("vf-1"), type: "folder", expanded: true, children: [resource("task-a", "task")] };
    const project = { ...resource("project-a"), expanded: true, children: [folderA, resource("task-b", "task")] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [project],
      onCreate: vi.fn(), onToggle: vi.fn(async () => undefined), onSelect: vi.fn(async () => undefined),
      onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(), onToggleFolder,
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const folderRow = target.querySelector<HTMLElement>(".folder-item")!;
    expect(folderRow.querySelector(".folder-count")?.textContent).toBe("1");
    expect(target.querySelector(".folder-task-group [data-task-id=\"task-a\"]")).not.toBeNull();
    // Virtual folders use the stacked folders glyph so they read as groupings,
    // distinct from the plain folder icon on Project rows.
    expect(folderRow.querySelector('[data-lucide="folders"]')).not.toBeNull();
    expect(folderRow.querySelector('[data-lucide="folder"]')).toBeNull();
    const projectRow = target.querySelector<HTMLElement>("button.tree-item")!;
    expect(projectRow.querySelector('[data-lucide="folder"]')).not.toBeNull();
    // Folders are not selectable resources: clicking toggles expansion.
    folderRow.click();
    await vi.waitFor(() => expect(onToggleFolder).toHaveBeenCalledWith("vf-1"));
    // Normal mode shows no folder management chrome.
    expect(folderRow.querySelector(".row-action-button")).toBeNull();
    expect(folderRow.querySelector(".drag-handle")).toBeNull();
  });

  it("ProjectTree shows an aggregated unread badge only on collapsed folders", async () => {
    const collapsed: ShellResourceItem = { ...resource("vf-1"), type: "folder", expanded: false, unreadCount: 5, children: [resource("task-a", "task")] };
    const expandedFolder: ShellResourceItem = { ...resource("vf-2"), type: "folder", expanded: true, unreadCount: 0, children: [{ ...resource("task-b", "task"), unreadCount: 2 }] };
    const project = { ...resource("project-a"), expanded: true, children: [collapsed, expandedFolder] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [project],
      onCreate: vi.fn(), onToggle: vi.fn(async () => undefined), onSelect: vi.fn(async () => undefined),
      onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const rows = target.querySelectorAll<HTMLElement>(".folder-item");
    expect(rows).toHaveLength(2);
    // The aggregated badge follows the folder's task count.
    const collapsedBadge = rows[0].querySelector<HTMLElement>(".folder-count + .unread-badge");
    expect(collapsedBadge?.textContent).toBe("5");
    expect(collapsedBadge?.getAttribute("aria-label")).toBe("5 unread Turns");
    // An expanded folder carries no aggregate badge; its visible Tasks render
    // their own badges inside the task group instead of the folder row.
    expect(rows[1].querySelector(".unread-badge")).toBeNull();
    expect(target.querySelector(".folder-task-group .task-item .unread-badge")?.textContent).toBe("2");
  });

  it("ProjectTree edit mode owns folder create, rename, delete, and drop-into", async () => {
    const onCreateFolder = vi.fn(async () => "vf-2");
    const onRenameFolder = vi.fn(async () => undefined);
    const onDeleteFolder = vi.fn(async () => undefined);
    const onReorder = vi.fn(async () => undefined);
    const folderA: ShellResourceItem = { ...resource("vf-1"), type: "folder", expanded: true, children: [resource("task-a", "task")] };
    const project = { ...resource("project-a"), expanded: true, children: [folderA, resource("task-b", "task")] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [project],
      onCreate: vi.fn(), onToggle: vi.fn(async () => undefined), onSelect: vi.fn(async () => undefined),
      onReorder, onDragState: vi.fn(), onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(true), onCreateFolder, onRenameFolder, onDeleteFolder,
    } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLElement>('[aria-label="New folder in project-a"]')!.click();
    await vi.waitFor(() => expect(onCreateFolder).toHaveBeenCalledWith("project-a"));

    const folderRow = target.querySelector<HTMLElement>(".folder-item")!;
    folderRow.querySelector<HTMLElement>('[aria-label="Rename vf-1"]')!.click();
    await tick();
    const input = folderRow.querySelector<HTMLInputElement>(".folder-rename-input")!;
    expect(input).not.toBeNull();
    input.value = "Grouped work";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    input.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await vi.waitFor(() => expect(onRenameFolder).toHaveBeenCalledWith("vf-1", "Grouped work"));

    folderRow.querySelector<HTMLElement>('[aria-label="Delete vf-1"]')!.click();
    await vi.waitFor(() => expect(onDeleteFolder).toHaveBeenCalledWith("vf-1"));

    // Dragging a root Task onto the folder row groups into the folder and
    // highlights the whole row instead of a before/after line.
    const rootTask = target.querySelector<HTMLElement>('[data-task-id="task-b"]')!;
    rootTask.querySelector<HTMLElement>(".drag-handle")!.dispatchEvent(dragEvent("dragstart"));
    folderRow.dispatchEvent(dragEvent("dragover", 1));
    await tick();
    expect(folderRow.classList.contains("drop-into")).toBe(true);
    folderRow.dispatchEvent(dragEvent("drop", 1));
    await vi.waitFor(() => expect(onReorder).toHaveBeenCalledWith(
      { kind: "task", id: "task-b", projectId: "project-a", containerId: "" },
      { kind: "folder", id: "vf-1", projectId: "project-a" },
      false,
    ));

    // Dragging a grouped Task onto a root Task carries its container.
    const groupedTask = target.querySelector<HTMLElement>('[data-task-id="task-a"]')!;
    groupedTask.querySelector<HTMLElement>(".drag-handle")!.dispatchEvent(dragEvent("dragstart"));
    rootTask.dispatchEvent(dragEvent("dragover", 1));
    rootTask.dispatchEvent(dragEvent("drop", 1));
    await vi.waitFor(() => expect(onReorder).toHaveBeenCalledWith(
      { kind: "task", id: "task-a", projectId: "project-a", containerId: "vf-1" },
      { kind: "task", id: "task-b", projectId: "project-a", containerId: "" },
      true,
    ));
  });

  it("ProjectTree owns keyed rows, project toggles, selection, and favorites", async () => {
    const onToggle = vi.fn(async () => undefined);
    const onSelect = vi.fn(async () => undefined);
    const onToggleFavorite = vi.fn(async () => undefined);
    const projectA = { ...resource("project-a"), expanded: true, children: [resource("task-a", "task")] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [projectA, resource("project-b")],
      onCreate: vi.fn(), onToggle, onSelect, onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite, onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLElement>('[data-project-toggle="project-a"]')!.click();
    target.querySelector<HTMLButtonElement>('[aria-label="task-a"]')!.click();
    await vi.waitFor(() => expect(onToggle).toHaveBeenCalledWith("project-a"));
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledWith("task-a"));
    target.querySelector<HTMLElement>('[aria-label="Add project-a to favorites"]')!.click();
    await vi.waitFor(() => expect(onToggleFavorite).toHaveBeenCalledWith("project-a", true));
    // Normal mode keeps the tree clean: no drag handles or row actions.
    expect(target.querySelector(".drag-handle")).toBeNull();
    expect(target.querySelector(".row-actions")).toBeNull();
  });

  it("ProjectTree edit mode owns typed drag transactions and hides status chrome", async () => {
    const onReorder = vi.fn(async () => undefined);
    const onDragState = vi.fn();
    const onSelect = vi.fn(async () => undefined);
    const projectA = { ...resource("project-a"), expanded: true, unreadCount: 2, favorite: true, children: [{ ...resource("task-a", "task"), unreadCount: 3 }] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [projectA, resource("project-b")],
      onCreate: vi.fn(), onToggle: vi.fn(async () => undefined), onSelect, onReorder, onDragState, onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(true),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    // Edit mode hides favorite stars, unread badges, and status icons, and
    // row clicks no longer navigate.
    expect(target.querySelector(".favorite-star")).toBeNull();
    expect(target.querySelector(".unread-badge")).toBeNull();
    expect(target.querySelector(".task-state-icon")).toBeNull();
    target.querySelector<HTMLButtonElement>('[aria-label="task-a"]')!.click();
    await tick();
    expect(onSelect).not.toHaveBeenCalled();

    const rows = target.querySelectorAll<HTMLElement>(".project-tree > .tree-item");
    rows[0].querySelector<HTMLElement>(".drag-handle")!.dispatchEvent(dragEvent("dragstart"));
    rows[1].dispatchEvent(dragEvent("dragover", 1));
    rows[1].dispatchEvent(dragEvent("drop", 1));
    await vi.waitFor(() => expect(onReorder).toHaveBeenCalledWith(
      { kind: "project", id: "project-a", projectId: "" },
      { kind: "project", id: "project-b", projectId: "" },
      true,
    ));
    expect(onDragState).toHaveBeenLastCalledWith(null);
  });

  it("ProjectTree shows a static Projects section label", async () => {
    const onCreate = vi.fn();
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [resource("project-a")],
      onCreate, onToggle: vi.fn(async () => undefined), onSelect: vi.fn(async () => undefined),
      onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const label = target.querySelector<HTMLElement>(".section-title .section-label")!;
    expect(label.textContent).toBe("Projects");
    expect(target.querySelector("#workspaceTitle")).toBeNull();
    target.querySelector<HTMLButtonElement>("#newProjectButton")!.click();
    expect(onCreate).toHaveBeenCalledTimes(1);
  });

  it("ProjectTree labels its ordering mode with a list-order action", async () => {
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [resource("project-a")],
      onCreate: vi.fn(), onToggle: vi.fn(async () => undefined), onSelect: vi.fn(async () => undefined),
      onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const button = target.querySelector<HTMLButtonElement>("#treeEditButton")!;
    expect(button.textContent?.trim()).toBe("Reorder");
    expect(button.title).toBe("Reorder projects");
    expect(button.querySelector('[data-lucide="list-ordered"]')).not.toBeNull();
    expect(button.querySelector('[data-lucide="square-pen"]')).toBeNull();
  });

  it("ProjectTree shows each resource's own unread count and caps the visible badge at 99+", async () => {
    const task = { ...resource("task-a", "task"), unreadCount: 120 };
    const project = { ...resource("project-a"), expanded: true, unreadCount: 2, children: [task] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [project],
      onCreate: vi.fn(), onToggle: vi.fn(), onSelect: vi.fn(), onReorder: vi.fn(), onDragState: vi.fn(), onToggleFavorite: vi.fn(), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const badges = target.querySelectorAll<HTMLElement>(".unread-badge");
    expect(badges).toHaveLength(2);
    expect(badges[0].textContent).toBe("2");
    expect(badges[0].getAttribute("aria-label")).toBe("2 unread Turns");
    expect(badges[1].textContent).toBe("99+");
    expect(badges[1].getAttribute("aria-label")).toBe("120 unread Turns");
    expect(target.querySelector(".project-tree > .tree-item .resource-ref + .unread-badge")).toBe(badges[0]);
    expect(target.querySelector(".task-item .resource-ref + .unread-badge")).toBe(badges[1]);
  });

  it("ProjectTree replaces the Task file icon with exactly one workflow state icon", async () => {
    const task = resource("task-a", "task");
    task.statusLabel = "Blocked: Need approval";
    task.status = {
      hasTaskState: true, className: "task-state-blocked", layoutClassName: "has-task-status", slotClassName: "task-status-single",
      statuses: [{ key: "task-blocked", className: "task-state-blocked", iconName: "circle-alert", recentOutput: false }],
    };
    const project = { ...resource("project-a"), expanded: true, children: [task] };
    const onSelect = vi.fn();
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [project],
      onCreate: vi.fn(), onToggle: vi.fn(), onSelect, onReorder: vi.fn(), onDragState: vi.fn(), onToggleFavorite: vi.fn(), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const taskRow = target.querySelector<HTMLElement>(".task-item")!;
    expect(taskRow.title).toBe("");
    expect(taskRow.querySelectorAll('[data-lucide="circle-alert"]')).toHaveLength(1);
    expect(taskRow.querySelector('[data-lucide="file-text"]')).toBeNull();
    expect(target.querySelector(".project-tree > .tree-item [data-component-owner='status-presentation']")).toBeNull();
    expect(target.querySelector(".task-state-tooltip")).toBeNull();

    taskRow.dispatchEvent(new MouseEvent("mouseenter", { bubbles: true }));
    await tick();
    expect(target.querySelector(".task-state-tooltip")?.textContent).toBe("Blocked: Need approval");

    taskRow.dispatchEvent(new MouseEvent("mouseleave", { bubbles: true }));
    await tick();
    expect(target.querySelector(".task-state-tooltip")).toBeNull();

    taskRow.querySelector<HTMLElement>(".task-state-icon")!.dispatchEvent(new MouseEvent("click", { bubbles: true }));
    await tick();
    expect(target.querySelector(".task-state-tooltip")?.textContent).toBe("Blocked: Need approval");
    expect(onSelect).not.toHaveBeenCalled();
  });

  it("ProjectTree replaces a working Project folder with a Session spinner", async () => {
    const project = resource("project-a");
    project.statusLabel = "Working";
    project.status = {
      hasTaskState: true, className: "task-status-session-running", layoutClassName: "has-task-status", slotClassName: "task-status-single",
      statuses: [{ key: "resource-working", className: "task-status-session-running", iconName: "loader-circle", recentOutput: false }],
    };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [project],
      onCreate: vi.fn(), onToggle: vi.fn(), onSelect: vi.fn(), onReorder: vi.fn(), onDragState: vi.fn(), onToggleFavorite: vi.fn(), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const row = target.querySelector<HTMLElement>(".project-tree > .tree-item")!;
    expect(row.querySelector('[data-lucide="loader-circle"]')).not.toBeNull();
    expect(row.querySelector('[data-lucide="folder"]')).toBeNull();
  });

  it("SchedulerNav replaces its clock with a Session spinner only while working", async () => {
    const scheduler = resource("scheduler") as ShellResourceItem;
    scheduler.type = "scheduler";
    scheduler.statusLabel = "Working";
    scheduler.status = {
      hasTaskState: true, className: "task-status-session-running", layoutClassName: "has-task-status", slotClassName: "task-status-single",
      statuses: [{ key: "resource-working", className: "task-status-session-running", iconName: "loader-circle", recentOutput: false }],
    };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SchedulerNav, { target, props: { item: scheduler, onSelect: vi.fn(), onToast: vi.fn() } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector('[data-lucide="loader-circle"]')).not.toBeNull();
    expect(target.querySelector('[data-lucide="clock-3"]')).toBeNull();
  });

  it("ProjectTree chevron keeps a stable icon and reflects expansion with its class", async () => {
    const expandedProject = { ...resource("project-a"), expanded: true, children: [resource("task-a", "task")] };
    const collapsedProject = { ...resource("project-b"), expanded: false, children: [resource("task-b", "task")] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [expandedProject, collapsedProject],
      onCreate: vi.fn(), onToggle: vi.fn(async () => undefined), onSelect: vi.fn(async () => undefined),
      onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    const expandedChevron = target.querySelector<HTMLElement>('[data-project-toggle="project-a"]')!;
    const collapsedChevron = target.querySelector<HTMLElement>('[data-project-toggle="project-b"]')!;
    expect(expandedChevron.classList.contains("expanded")).toBe(true);
    expect(collapsedChevron.classList.contains("expanded")).toBe(false);
    // The chevron uses a single icon and encodes direction through the
    // expanded class (CSS rotation) so the icon itself never changes.
    expect(expandedChevron.querySelector('[data-lucide="chevron-right"]')).not.toBeNull();
    expect(collapsedChevron.querySelector('[data-lucide="chevron-right"]')).not.toBeNull();
  });

  it("ProjectTree chevron toggle drops row focus so the star does not stick", async () => {
    const onToggle = vi.fn(async () => undefined);
    const onSelect = vi.fn(async () => undefined);
    const project = { ...resource("project-a"), children: [resource("task-a", "task")] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [project],
      onCreate: vi.fn(), onToggle, onSelect,
      onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    // A pointer click on the chevron focuses the row button in browsers; a
    // focused row keeps its focus-within rule active, pinning the follow
    // star visible after the pointer leaves. The toggle must drop focus.
    const row = target.querySelector<HTMLElement>(".tree-item")!;
    row.focus();
    target.querySelector<HTMLElement>('[data-project-toggle="project-a"]')!.click();
    await vi.waitFor(() => expect(onToggle).toHaveBeenCalledWith("project-a"));
    expect(onSelect).not.toHaveBeenCalled();
    expect(document.activeElement).not.toBe(row);
    expect(row.contains(document.activeElement)).toBe(false);
  });

  it("ProjectTree row click drops pointer focus but keeps keyboard focus", async () => {
    const onSelect = vi.fn(async () => undefined);
    const project = { ...resource("project-a"), expanded: true, children: [resource("task-a", "task")] };
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [project],
      onCreate: vi.fn(), onToggle: vi.fn(async () => undefined), onSelect,
      onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite: vi.fn(async () => undefined), onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    // A pointer click on the row focuses the row button in browsers; a
    // focused row keeps its focus-within rule active, pinning the follow
    // star visible after the pointer leaves. Selection must drop focus.
    const row = target.querySelector<HTMLElement>(".task-item")!;
    row.focus();
    row.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, detail: 1 }));
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledWith("task-a"));
    expect(document.activeElement).not.toBe(row);
    expect(row.contains(document.activeElement)).toBe(false);

    // Keyboard activation (detail === 0) keeps focus so keyboard
    // navigation position is not lost.
    row.focus();
    row.dispatchEvent(new MouseEvent("click", { bubbles: true, cancelable: true, detail: 0 }));
    await vi.waitFor(() => expect(onSelect).toHaveBeenCalledTimes(2));
    expect(document.activeElement).toBe(row);
  });

  it("ProjectTree favorite star drops pointer focus but keeps keyboard focus", async () => {
    const onToggleFavorite = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ProjectTree, { target, props: {
      identity: "workspace-a", loading: false, error: "", projects: [resource("project-a")],
      onCreate: vi.fn(), onToggle: vi.fn(async () => undefined), onSelect: vi.fn(async () => undefined),
      onReorder: vi.fn(async () => undefined), onDragState: vi.fn(), onToggleFavorite, onToast: vi.fn(),
      ...treeEditProps(),
    } });
    cleanups.push(() => unmount(component));
    await tick();

    // A pointer click must blur the star: a focused star keeps the row's
    // focus-within rule active, pinning it visible after the pointer leaves.
    const star = target.querySelector<HTMLElement>('[aria-label="Add project-a to favorites"]')!;
    star.focus();
    star.click();
    await vi.waitFor(() => expect(onToggleFavorite).toHaveBeenCalledWith("project-a", true));
    expect(document.activeElement).not.toBe(star);

    // Keyboard toggles keep focus so the visible focus ring is not lost.
    star.focus();
    star.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true }));
    await vi.waitFor(() => expect(onToggleFavorite).toHaveBeenCalledTimes(2));
    expect(document.activeElement).toBe(star);
  });

  it("ActivityPanel inbox lists messages, opens them, and sends inline replies", async () => {
    const onOpenInboxMessage = vi.fn(async () => undefined);
    const onReplyInboxMessage = vi.fn(async () => undefined);
    const onDeleteInboxMessage = vi.fn(async () => undefined);
    const inbox: ShellInboxMessage[] = [
      { id: "umsg-1", resourceId: "project1.task1", resourceTitle: "Task One", senderName: "project1.task1", text: "Build finished.", timeLabel: "2m ago", unread: true, replied: false },
      { id: "umsg-2", resourceId: "project1.task2", resourceTitle: "Task Two", senderName: "project1.task2", text: "Need input.", timeLabel: "5m ago", unread: false, replied: true },
    ];
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ActivityPanel, {
      target,
      props: {
        activity: { running: [], favorites: [], unread: [], problems: [] },
        inbox,
        onSelect: vi.fn(async () => undefined),
        onToggleFavorite: vi.fn(async () => undefined),
        onOpenInboxMessage,
        onReplyInboxMessage,
        onDeleteInboxMessage,
        onToast: vi.fn(),
      },
    });
    cleanups.push(() => unmount(component));
    await tick();

    // The inbox tab counts only unread messages.
    expect(target.querySelector('[role="tab"]:last-child')?.textContent?.trim()).toBe("Inbox 1");
    target.querySelector<HTMLElement>('[role="tab"]:last-child')!.click();
    await tick();

    const rows = target.querySelectorAll(".inbox-row");
    expect(rows.length).toBe(2);
    expect(rows[0].classList.contains("unread")).toBe(true);
    expect(rows[1].classList.contains("unread")).toBe(false);
    expect(rows[1].querySelector(".inbox-replied-badge")?.textContent).toBe("Replied");
    // The sender resource id leads; the resource title is secondary context.
    expect(rows[0].querySelector(".inbox-source strong")?.textContent).toBe("project1.task1");
    expect(rows[0].querySelector(".inbox-context")?.textContent).toContain("Task One");

    // Clicking a row opens the message (marks read and navigates).
    (rows[0] as HTMLElement).click();
    await vi.waitFor(() => expect(onOpenInboxMessage).toHaveBeenCalledWith("umsg-1"));

    // The reply affordance expands an inline form and posts the draft.
    const replyButton = rows[1].querySelector<HTMLElement>(".inbox-actions .inbox-action:nth-child(2)")!;
    replyButton.click();
    await tick();
    const input = target.querySelector<HTMLTextAreaElement>(".inbox-reply-input")!;
    input.value = "on it";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    target.querySelector<HTMLElement>(".inbox-send")!.click();
    await vi.waitFor(() => expect(onReplyInboxMessage).toHaveBeenCalledWith("umsg-2", "on it"));
    expect(onOpenInboxMessage).toHaveBeenCalledTimes(1);

    // The delete affordance asks the controller to remove the message.
    const deleteButton = rows[1].querySelector<HTMLElement>(".inbox-delete")!;
    deleteButton.click();
    await vi.waitFor(() => expect(onDeleteInboxMessage).toHaveBeenCalledWith("umsg-2"));
    expect(onOpenInboxMessage).toHaveBeenCalledTimes(1);
  });

  it("ActivityPanel shows tab counts and favorites without a dismiss control", async () => {
    const onSelect = vi.fn(async () => undefined);
    const onToggleFavorite = vi.fn(async () => undefined);
    const item: ShellActivityItem = {
      id: "project-a", type: "project", title: "Project A", ref: "#1", selected: true, activeTurn: false, favorite: true, unreadCount: 2,
      turnNumber: 2, agentName: "Codex", statusLabel: "2 unread", status: emptyStatus,
    };
    const activeItem: ShellActivityItem = {
      id: "task-b", type: "task", title: "Task B", ref: "#2", selected: false, activeTurn: true, favorite: false, unreadCount: 1,
      turnNumber: 3, agentName: "Codex", statusLabel: "Resource working", status: emptyStatus,
    };
    const target = document.body.appendChild(document.createElement("div"));
    const activity = { running: [activeItem], favorites: [item], unread: [activeItem, item], problems: [] };
    const component = mount(ActivityPanel, { target, props: { activity, inbox: [], onSelect, onToggleFavorite, onOpenInboxMessage: vi.fn(async () => undefined), onReplyInboxMessage: vi.fn(async () => undefined), onDeleteInboxMessage: vi.fn(async () => undefined), onToast: vi.fn() } });
    cleanups.push(() => unmount(component));
    await tick();

    expect([...target.querySelectorAll('[role="tab"]')].map((tab) => tab.textContent?.trim())).toEqual(["Running 1", "Favs 1", "Unread 2", "Issues 0", "Inbox 0"]);
    const activeRow = target.querySelector<HTMLElement>('[aria-label^="Task B."]')!;
    expect(activeRow.classList.contains("selected")).toBe(false);
    expect(activeRow.getAttribute("data-active-turn")).toBe("true");
    expect(target.querySelector('[aria-label^="Dismiss "]')).toBeNull();

    target.querySelector<HTMLElement>('[role="tab"]:nth-child(2)')!.click();
    await tick();
    const favorite = target.querySelector<HTMLElement>('[aria-label="Remove Project A from favorites"]')!;
    favorite.focus();
    favorite.click();
    await vi.waitFor(() => expect(onToggleFavorite).toHaveBeenCalledWith("project-a", false));
    expect(document.activeElement).not.toBe(favorite);
    expect(onSelect).not.toHaveBeenCalled();
    expect(target.querySelector(".activity-title")?.textContent).toContain("#1 · Agent Codex · Turn 2 · 2 unread");
  });

  it("PaneResizeHandle resizes and commits the Activity panel height", async () => {
    const app = document.body.appendChild(document.createElement("div"));
    app.id = "app";
    const sidebar = app.appendChild(document.createElement("aside"));
    sidebar.id = "mobileSidebar";
    const activity = sidebar.appendChild(document.createElement("section"));
    activity.className = "attention-section";
    const workspace = app.appendChild(document.createElement("main"));
    workspace.className = "workspace-panel";
    const chat = workspace.appendChild(document.createElement("aside"));
    chat.id = "agentPanel";
    vi.spyOn(app, "getBoundingClientRect").mockReturnValue({ width: 1_200 } as DOMRect);
    vi.spyOn(sidebar, "getBoundingClientRect").mockReturnValue({ width: 280, height: 800 } as DOMRect);
    vi.spyOn(activity, "getBoundingClientRect").mockReturnValue({ height: 210 } as DOMRect);
    vi.spyOn(workspace, "getBoundingClientRect").mockReturnValue({ width: 912 } as DOMRect);
    vi.spyOn(chat, "getBoundingClientRect").mockReturnValue({ width: 420 } as DOMRect);
    const onPreview = vi.fn();
    const onCommit = vi.fn();
    const component = mount(PaneResizeHandle, { target: sidebar, props: {
      id: "activityResize", kind: "sidebarAttentionHeight", className: "horizontal-resize sidebar-activity-resize",
      label: "Resize activity panel", onPreview, onCommit,
    } });
    cleanups.push(() => unmount(component));

    sidebar.querySelector<HTMLElement>("#activityResize")!.dispatchEvent(pointerEvent("pointerdown", 10, 100));
    window.dispatchEvent(pointerEvent("pointermove", 10, 140));
    expect(onPreview).toHaveBeenCalledWith("sidebarAttentionHeight", 170);
    expect(document.body.classList.contains("resizing-y")).toBe(true);
    window.dispatchEvent(pointerEvent("pointerup", 10, 140));
    expect(onCommit).toHaveBeenCalledWith("sidebarAttentionHeight");
    expect(document.body.classList.contains("resizing-y")).toBe(false);
  });
});
