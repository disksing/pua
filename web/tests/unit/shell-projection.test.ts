import { describe, expect, it } from "vitest";

import { createShellProjection, filterProjectTree } from "../../src/controllers/shell-projection";
import type { ShellResourceItem } from "../../src/models/shell";
import type { WorkspaceTree } from "../../src/models/workspace";

describe("shell projection", () => {
  const tree: WorkspaceTree = {
    projects: [{ id: "project1", type: "project", children: [
      { id: "task1", type: "task", state: "in_progress", runtime: { generationId: "gen-task1", status: "running", lastOutputAt: "2026-08-11T02:00:00Z" } },
      { id: "task2", type: "task" },
    ] }],
  };
  const projection = createShellProjection({ tree: () => tree, findResource: () => null, agentName: (id) => id || "agent" });

  it("filters the ordered hierarchy without changing the original expansion, selection, or counts", () => {
    function row(id: string, type: ShellResourceItem["type"], overrides: Partial<ShellResourceItem> = {}): ShellResourceItem {
      return {
        id, type, title: id, ref: "", active: false, expanded: false, ariaLabel: id, statusLabel: "",
        status: { hasTaskState: false, className: "", layoutClassName: "", slotClassName: "", statuses: [] },
        summary: null, children: [], unreadCount: 0, ...overrides
      };
    }
    const idle = row("In progress workflow", "task", { active: true, statusLabel: "In progress" });
    const unread = row("Unread closed chat", "task", { unreadCount: 2, chatOpen: false });
    const running = row("Waiting for approval", "task", { running: true });
    const folder = row("Folder", "folder", { children: [idle, unread, running] });
    const direct = row("Direct task", "task", { running: true, unreadCount: 1 });
    const project = row("Parent", "project", { children: [direct, folder] });
    const idleProject = row("Idle", "project", { children: [row("Empty folder", "folder")] });
    const ownTurn = row("Project turn", "project", { running: true, children: [idle] });
    const ownUnread = row("Project unread", "project", { unreadCount: 3, children: [idle] });
    const projects = [idleProject, ownTurn, project, ownUnread];

    expect(filterProjectTree(projects, "all", new Set())).toBe(projects);
    const runningTree = filterProjectTree(projects, "running", new Set([folder.id]));
    expect(runningTree.map(({ id }) => id)).toEqual([ownTurn.id, project.id]);
    expect(runningTree[0].children).toEqual([]);
    expect(runningTree[1].children).toMatchObject([{ id: direct.id }, { id: folder.id, expanded: false, children: [{ id: running.id }] }]);
    expect(runningTree[1].summary?.ariaLabel).toBe("2 running tasks");
    const unreadTree = filterProjectTree(projects, "unread", new Set());
    expect(unreadTree.map(({ id }) => id)).toEqual([project.id, ownUnread.id]);
    expect(unreadTree[0].children).toMatchObject([{ id: direct.id }, { id: folder.id, expanded: true, children: [{ id: unread.id }] }]);
    expect(unreadTree[0].summary?.ariaLabel).toBe("2 tasks with unread chat");
    expect(project.expanded).toBe(false);
    expect(folder.children).toEqual([idle, unread, running]);
    expect(idle.active).toBe(true);
  });

  it("keeps custom order stable and moves only known targets", () => {
    expect(projection.applyCustomOrder([{ id: "a" }, { id: "b" }, { id: "c" }], ["c", "a"]).map((item) => item.id)).toEqual(["c", "a", "b"]);
    expect(projection.moveIdInList(["a", "b", "c"], "a", "b", true)).toEqual(["b", "a", "c"]);
    expect(projection.moveIdInList(["a", "b"], "a", "missing", false)).toEqual(["a", "b"]);
  });

  it("projects Task workflow state independently from runtime state", () => {
    const task = { ...tree.projects[0].children![0], state: "blocked" as const, stateNote: "Need deployment approval", runtime: { generationId: "gen-task1", status: "idle" } };
    const state = projection.taskWorkflowState(task);
    expect(state.session).toMatchObject({ kind: "task-blocked", iconName: "circle-alert" });
    expect(state.label).toBe("Blocked: Need deployment approval");
  });

  it("counts open and in-progress tasks in the All summary", () => {
    expect(projection.projectTaskSummary(tree.projects[0])).toMatchObject({ taskCount: 2, runningCount: 1 });
  });

  it("uses Session working only for non-Task navigation state", () => {
    const project = tree.projects[0];
    for (const sessionState of ["idle", "attention_required", "unavailable", "archived"] as const) {
      project.runtime = { generationId: "gen-project1", status: "running", sessionState };
      expect(projection.resourceNavigationState(project)).toMatchObject({
        session: null,
        label: "",
        statusPresentation: { hasTaskState: false, statuses: [] },
      });
    }

    project.runtime = { generationId: "gen-project1", status: "idle", sessionState: "working" };
    expect(projection.resourceNavigationState(project)).toMatchObject({
      session: { kind: "resource-working", iconName: "loader-circle", label: "Working" },
      statusPresentation: { hasTaskState: true, statuses: [{ className: "task-status-session-running" }] },
    });
  });

  it("redirects task archives through the tree view order", () => {
    const redirectTree: WorkspaceTree = {
      projects: [
        { id: "project1", type: "project", children: [
          { id: "project1.task1", type: "task" },
          { id: "project1.task2", type: "task" },
          { id: "project1.task3", type: "task" },
        ] },
        { id: "project2", type: "project", children: [{ id: "project2.task1", type: "task" }] },
        { id: "project3", type: "project", children: [{ id: "project3.task1", type: "task" }] },
      ],
    };
    const redirectProjection = createShellProjection({ tree: () => redirectTree, findResource: () => null, agentName: (id) => id || "agent" });
    const order = { projectOrder: ["project3", "project1", "project2"], taskOrder: { project1: ["project1.task3", "project1.task1", "project1.task2"] } };

    // Next task wins when one exists in tree view order.
    expect(redirectProjection.archiveRedirectTarget("project1.task1", order.projectOrder, order.taskOrder)).toBe("project1.task2");
    // Last task falls back to the previous one; id order would have picked task3 instead.
    expect(redirectProjection.archiveRedirectTarget("project1.task2", order.projectOrder, order.taskOrder)).toBe("project1.task1");
    // A project with a single task falls back to its project.
    expect(redirectProjection.archiveRedirectTarget("project2.task1", order.projectOrder, order.taskOrder)).toBe("project2");
    // First project picks the next one; id order would have picked project2 instead.
    expect(redirectProjection.archiveRedirectTarget("project3", order.projectOrder, order.taskOrder)).toBe("project1");
    // Last project falls back to the previous one; id order would have picked project3 instead.
    expect(redirectProjection.archiveRedirectTarget("project2", order.projectOrder, order.taskOrder)).toBe("project1");
  });

  it("returns the workspace when archiving the only project or an unknown resource", () => {
    const singleTree: WorkspaceTree = {
      projects: [{ id: "project1", type: "project", children: [{ id: "project1.task1", type: "task" }] }],
    };
    const singleProjection = createShellProjection({ tree: () => singleTree, findResource: () => null, agentName: (id) => id || "agent" });
    expect(singleProjection.archiveRedirectTarget("project1", [], {})).toBe("workspace");
    expect(singleProjection.archiveRedirectTarget("missing", [], {})).toBe("workspace");
    expect(singleProjection.archiveRedirectTarget("scheduler", [], {})).toBe("workspace");
  });

  it("returns the workspace when no tree is available", () => {
    const emptyProjection = createShellProjection({ tree: () => null, findResource: () => null, agentName: (id) => id || "agent" });
    expect(emptyProjection.archiveRedirectTarget("project1", [], {})).toBe("workspace");
  });

  it("aggregates child unread counts only while collapsed", () => {
    const children = [{ unreadCount: 3 }, { unreadCount: 0 }, {}, { unreadCount: 2 }];
    expect(projection.aggregatedUnreadCount(1, children, false)).toBe(6);
    expect(projection.aggregatedUnreadCount(1, children, true)).toBe(1);
    expect(projection.aggregatedUnreadCount(0, children, true)).toBe(0);
    expect(projection.aggregatedUnreadCount(0, [], false)).toBe(0);
  });
});
