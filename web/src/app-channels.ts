import { createModelChannel, type ModelChannel } from "./components/model-channel";
import type { AgentPanelHeaderModel, EventTimelineModel, UploadDialogModel, ComposerModel } from "./models/chat";
import type { ToastModel } from "./models/common";
import type { CreateDialogModel } from "./models/create";
import type { DetailPanelModel } from "./models/detail";
import type { SettingsModel } from "./models/settings";
import type { AppShellModel } from "./models/shell";
import { WORKSPACE_ICONS } from "./models/workspace-icons";

export interface PUAAppChannels {
  appShell: ModelChannel<AppShellModel>;
  create: ModelChannel<CreateDialogModel>;
  settings: ModelChannel<SettingsModel>;
  upload: ModelChannel<UploadDialogModel>;
  composer: ModelChannel<ComposerModel>;
  detail: ModelChannel<DetailPanelModel>;
  timeline: ModelChannel<EventTimelineModel>;
  agentHeader: ModelChannel<AgentPanelHeaderModel>;
  toast: ModelChannel<ToastModel>;
}

const noop = () => undefined;
const noopAsync = async () => undefined;

export function createPUAAppChannels(): PUAAppChannels {
  return {
    appShell: createModelChannel<AppShellModel>({
      identity: "", loading: true, error: "", version: "v0.1.0", activeWorkspaceId: "", workspaceName: "", userGate: { mode: "", users: [], suggestedUserName: "", missingUserName: "" }, workspaces: [], projects: [], treeEditing: false, activity: { running: [], unread: [], problems: [] }, inbox: [],
      doctor: { checking: true, complete: false, summary: { errors: 0, warnings: 0 }, workspaces: [] },
      paneSizes: { sidebarWidth: 280, chatWidth: 420, chatHeight: 320, sidebarAttentionHeight: 210 }, mobile: { sidebarOpen: false },
      layout: { preference: "auto", effective: "three" },
      route: { path: "", revision: 0, replace: true },
      resolveResourceTitle: () => null,
      onSwitchWorkspace: noopAsync, onResolveWorkspaceUser: noopAsync, onAddWorkspace: noop, onCreateProject: noop, onOpenSettings: noop, onRefreshDoctor: noopAsync, onToggleProject: noopAsync, onSelectResource: noopAsync,
      onReorder: noopAsync, onDragState: noop, onToggleTreeEditing: noop, onCreateFolder: async () => "", onRenameFolder: noopAsync, onDeleteFolder: noopAsync, onToggleFolder: noopAsync, onOpenInboxMessage: noopAsync, onReplyInboxMessage: noopAsync, onDeleteInboxMessage: noopAsync, onPanePreview: noop, onPaneCommit: noop, onPaneViewport: noop, onMobileSidebar: noop,
      onToast: noop, onHistoryNavigation: noopAsync,
    }),
    create: createModelChannel<CreateDialogModel>({
      open: false, identity: "", workspaceId: "", draft: { type: "project", projectId: "", templateName: "", templateFields: {}, title: "", description: "", detail: "", slug: "", startAfterCreate: false, startBinding: { kind: "profile", name: "" }, startPrompt: "" },
      templates: [], preview: null, previewKey: "", previewing: false, previewError: "", templateDigest: "", submitting: false,
      agents: [], agentProfiles: [], defaultTaskBinding: { kind: "profile", name: "default" },
      onClose: noop, onPreview: noopAsync, onSubmit: noopAsync, previewRequestKey: () => "", onConfirmTemplateSwitch: async () => true,
    }),
    settings: createModelChannel<SettingsModel>({
      open: false, identity: "", dataVersion: 0, initialTab: "system", workspaces: [], activeWorkspaceId: "", workspaceIcons: WORKSPACE_ICONS, workspaceIconSavingId: "", suggestedUserName: "", system: null,
      appearance: { layout: "auto", fontScales: { sidebar: 1, details: 1, chat: 1 }, theme: "default", themeOptions: [{ id: "default", label: "Default", description: "The standard PUA appearance" }] },
      agentHub: { mode: "embedded", configuredEndpoint: "", connected: false, compatible: false, error: "", apiVersion: "", version: "", capabilities: [], providers: [], agents: [], probes: [] }, profiles: [], agents: [],
      notifications: { browser: false, sound: false, permission: "default", permissionError: "", soundError: "" },
      onClose: noop, onAddWorkspace: noopAsync, onRemoveWorkspace: noopAsync, onWorkspaceIcon: noopAsync, onSaveWorkspaceName: noopAsync, onSaveAgentHub: noopAsync, onToggleProvider: async (providerId, enabled) => ({ id: providerId, name: providerId, type: providerId, enabled }),
      onLayoutPreference: noop, onFontScale: noop, onResetFontScales: noop, onThemePreference: noop,
      onBrowserNotifications: noop, onCompletionSound: noop, onToast: noop,
    }),
    upload: createModelChannel<UploadDialogModel>({ open: false, identity: "", workspaceId: "", resourceId: "", onDone: noop }),
	composer: createModelChannel<ComposerModel>({ identity: "", workspaceId: "", resourceId: "", draft: "", draftKey: "", draftResetVersion: 0, unavailableReason: "Loading work status.", sending: false, canEndTurn: false, endingTurn: false, canEndGeneration: false, endingGeneration: false, stopNotice: "", waitingMessages: [], canSteerWaiting: false, steeringMessageId: "", agentBinding: { kind: "profile", name: "default" }, agentProfiles: [], agents: [], bindingSaving: false, onDraft: noop, onSend: async () => ({ accepted: false, clear: false }), onOpenUpload: noop, onEndTurn: noop, onEndGeneration: noop, onDismissStopNotice: noop, onSteerWaiting: noopAsync, onSaveAgentBinding: noopAsync }),
	 detail: createModelChannel<DetailPanelModel>({ identity: "", workspaceId: "", workspaceName: "", resourceId: "", resourceType: "", resourceTitle: "", parent: null, loading: false, detail: null, wiki: null, workspaceAgents: null, workspaceDefaults: { project: { kind: "profile", name: "default" }, task: { kind: "profile", name: "default" } }, workspaceUsers: [], currentUserName: "", generationPolicy: { enabled: true, maxTurns: 20, maxAccumulatedTurnMinutes: 120 }, stallWatchdogPolicy: { enabled: true, timeoutMinutes: 30 }, agentBinding: { kind: "profile", name: "default" }, agentProfiles: [], agents: [], resolveResourceTitle: () => null, onNavigate: noop, onCreateTask: noop, onArchive: noop, onSaveWorkspaceAgents: async () => ({ path: "AGENTS.md" }), onSaveMarkdownFile: async (path) => ({ path }), onDeleteArtifact: noopAsync, onSaveAgentBinding: noopAsync, onRenameResource: noopAsync, onSaveDescription: noopAsync, onSaveWorkspaceDefaults: noopAsync, onSaveWorkspaceUserPreference: noopAsync, onSwitchWorkspaceUser: noopAsync, onAddWorkspaceUser: noopAsync, onDeleteWorkspaceUser: noopAsync, onSaveGenerationPolicy: noopAsync, onSaveStallWatchdogPolicy: noopAsync, onSaveTaskDefault: noopAsync, onToast: noop }),
    timeline: createModelChannel<EventTimelineModel>({ identity: "", workspaceId: "", resourceId: "", status: null, submitting: false, agentName: "Agent", resolveResourceTitle: () => null, onNavigate: noop, project: () => [], onEvent: noop, onNotice: noop, onApproval: noopAsync, onToast: noop }),
    agentHeader: createModelChannel<AgentPanelHeaderModel>({ identity: "", workspaceId: "", resourceId: "", status: null, submitting: false, agentName: "Agent", modelSummary: "", turnNumber: 0, turnStartedAt: "" }),
    toast: createModelChannel<ToastModel>({ message: "", revision: 0 }),
  };
}
