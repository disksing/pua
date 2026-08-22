import type { AgentEvent, AppShellModel, ShellResourceItem, ShellStatusPresentation } from "../../src/components/models";

export const performanceBudgets = {
  treeRenderMs: 5_000,
  markdownRenderMs: 1_000,
  eventMergeMs: 1_000,
  continuousDeltaMs: 1_500,
  maximumTreeElements: 15_000,
} as const;

const emptyStatus: ShellStatusPresentation = {
  hasTaskState: false, className: "", layoutClassName: "", slotClassName: "", statuses: [],
};

function resource(id: string, type: "project" | "task", children: ShellResourceItem[] = []): ShellResourceItem {
  return {
    id, type, title: `${type} ${id}`, ref: id, active: false, expanded: true, ariaLabel: id,
    statusLabel: "", status: emptyStatus, summary: null, children, unreadCount: 0,
  };
}

export function largeTreeModel(): AppShellModel {
  const projects = Array.from({ length: 120 }, (_, projectIndex) => {
    const projectId = `project-${projectIndex}`;
    return resource(projectId, "project", Array.from({ length: 5 }, (_, taskIndex) => resource(`${projectId}.task-${taskIndex}`, "task")));
  });
  const noop = () => undefined;
  const noopAsync = async () => undefined;
  return {
    identity: "performance-workspace", loading: false, error: "", version: "test", activeWorkspaceId: "performance-workspace", workspaceName: "Performance Workspace",
    userGate: { mode: "", users: [], suggestedUserName: "", missingUserName: "" },
    workspaces: [{ id: "performance-workspace", name: "Performance Workspace", path: "/tmp/performance", iconSrc: "/favicon.svg" }],
    projects, treeEditing: false, activity: { running: [], unread: [], problems: [] }, inbox: [], doctor: { checking: false, complete: true, summary: { errors: 0, warnings: 0 }, workspaces: [] }, paneSizes: { sidebarWidth: 280, chatWidth: 420, chatHeight: 320, sidebarAttentionHeight: 210 },
    mobile: { sidebarOpen: false }, layout: { preference: "auto", effective: "three" }, route: { path: "", revision: 0, replace: true },
    resolveResourceTitle: () => null,
    onSwitchWorkspace: noopAsync, onResolveWorkspaceUser: noopAsync, onAddWorkspace: noop, onCreateProject: noop, onOpenSettings: noop, onRefreshDoctor: noopAsync,
    onToggleProject: noopAsync, onSelectResource: noopAsync, onReorder: noopAsync, onDragState: noop,
    onToggleTreeEditing: noop, onCreateFolder: async () => "", onRenameFolder: noopAsync, onDeleteFolder: noopAsync, onToggleFolder: noopAsync, onOpenInboxMessage: noopAsync, onReplyInboxMessage: noopAsync, onDeleteInboxMessage: noopAsync,
    onPanePreview: noop, onPaneCommit: noop, onPaneViewport: noop, onMobileSidebar: noop,
    onHistoryNavigation: noopAsync, onToast: noop,
  };
}

export function largeMarkdown(): string {
  return Array.from({ length: 3_000 }, (_, index) => `## Section ${index}\n\n${"deterministic markdown ".repeat(8)}`).join("\n\n");
}

export function continuousEvents(count = 10_000): AgentEvent[] {
  return Array.from({ length: count }, (_, index) => ({
    id: index + 1, type: "message", sessionId: "performance-session", data: { text: `event ${index + 1}` },
  }));
}
