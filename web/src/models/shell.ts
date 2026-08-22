import type { WorkspaceOption } from "./common";

export interface ShellStatusItem {
  key: string;
  className: string;
  iconName: string;
  recentOutput: boolean;
}

export interface ShellStatusPresentation {
  hasTaskState: boolean;
  className: string;
  layoutClassName: string;
  slotClassName: string;
  statuses: ShellStatusItem[];
}

export interface ShellProjectSummary {
  taskLabel: string;
  runningLabel: string;
  ariaLabel: string;
}

export interface ShellResourceItem {
  id: string;
  type: "scheduler" | "project" | "task" | "folder";
  title: string;
  ref: string;
  active: boolean;
  expanded: boolean;
  ariaLabel: string;
  statusLabel: string;
  status: ShellStatusPresentation;
  summary: ShellProjectSummary | null;
  children: ShellResourceItem[];
  projectId?: string;
  favorite?: boolean;
  unreadCount: number;
}

export interface ShellActivityItem {
  id: string;
  type: "workspace" | "scheduler" | "project" | "task";
  title: string;
  ref: string;
  selected: boolean;
  activeTurn: boolean;
  favorite: boolean;
  unreadCount: number;
  turnNumber: number;
  agentName: string;
  statusLabel: string;
  status: ShellStatusPresentation;
}

export interface ShellActivityLists {
  running: ShellActivityItem[];
  favorites: ShellActivityItem[];
  unread: ShellActivityItem[];
  problems: ShellActivityItem[];
}

export interface ShellInboxMessage {
  id: string;
  resourceId: string;
  resourceTitle: string;
  senderName: string;
  text: string;
  timeLabel: string;
  unread: boolean;
  replied: boolean;
}

export interface ShellWorkspaceItem extends WorkspaceOption {
  iconSrc: string;
  status?: ShellStatusPresentation;
  statusLabel?: string;
}

export interface DoctorIssueModel {
  severity: "error" | "warning" | string;
  code: string;
  message: string;
  path?: string;
  resourceId?: string;
  suggestion?: string;
}

export interface DoctorWorkspaceModel {
  id: string;
  name: string;
  path: string;
  report: {
    complete: boolean;
    summary: { errors: number; warnings: number };
    issues: DoctorIssueModel[];
  };
}

export interface DoctorSnapshotModel {
  checkedAt?: string;
  checking: boolean;
  complete: boolean;
  summary: { errors: number; warnings: number };
  error?: string;
  workspaces: DoctorWorkspaceModel[];
}

export interface ShellDragTarget {
  kind: "project" | "task" | "folder";
  id: string;
  projectId: string;
  // containerId is the virtual folder holding a Task row, or "" for rows at
  // the Project root. Folders only ever live at the root.
  containerId?: string;
}

export interface AppShellModel {
  identity: string;
  loading: boolean;
  error: string;
  version: string;
  activeWorkspaceId: string;
  workspaceName: string;
  workspaces: ShellWorkspaceItem[];
  scheduler?: ShellResourceItem | null;
  projects: ShellResourceItem[];
  treeEditing: boolean;
  activity: ShellActivityLists;
  inbox: ShellInboxMessage[];
  doctor: DoctorSnapshotModel;
  paneSizes: { sidebarWidth: number; chatWidth: number; chatHeight: number; sidebarAttentionHeight: number };
  mobile: { sidebarOpen: boolean };
  layout: { preference: "auto" | "three" | "two" | "split"; effective: "three" | "two" | "split" | "single" };
  route: { path: string; revision: number; replace: boolean };
  resolveResourceTitle: (resourceId: string) => string | null;
  onSwitchWorkspace: (id: string) => Promise<void>;
  onAddWorkspace: () => void;
  onCreateProject: () => void;
  onOpenSettings: () => void;
  onRefreshDoctor: () => Promise<void>;
  onToggleProject: (id: string) => Promise<void>;
  onSelectResource: (id: string) => Promise<void>;
  onReorder: (drag: ShellDragTarget, target: ShellDragTarget, after: boolean) => Promise<void>;
  onDragState: (drag: ShellDragTarget | null) => void;
  onToggleTreeEditing: () => void;
  onCreateFolder: (projectId: string) => Promise<string>;
  onRenameFolder: (id: string, name: string) => Promise<void>;
  onDeleteFolder: (id: string) => Promise<void>;
  onToggleFolder: (id: string) => Promise<void>;
  onToggleFavorite: (id: string, favorite: boolean) => Promise<void>;
  onOpenInboxMessage: (id: string) => Promise<void>;
  onReplyInboxMessage: (id: string, text: string) => Promise<void>;
  onDeleteInboxMessage: (id: string) => Promise<void>;
  onPanePreview: (name: keyof AppShellModel["paneSizes"], value: number) => void;
  onPaneCommit: (name: keyof AppShellModel["paneSizes"]) => void;
  onPaneViewport: () => void;
  onMobileSidebar: (open: boolean) => void;
  onHistoryNavigation: (pathname: string) => Promise<void>;
  onToast: (message: string) => void;
}
