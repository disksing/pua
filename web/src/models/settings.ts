import type { AgentOption, WorkspaceOption } from "./common";

export interface ProfileDraft {
  key: string;
  description: string;
  agentName: string;
}

export interface NotificationPreferences {
  browser: boolean;
  sound: boolean;
  permission: string;
  permissionError: string;
  soundError: string;
}

export interface AgentHubSettings {
  mode?: string;
  configuredEndpoint: string;
  connected: boolean;
  compatible: boolean;
  error: string;
  apiVersion: string;
  version: string;
  capabilities: string[];
  providers: AgentHubProviderInfo[];
  agents: AgentHubAgentInfo[];
  probes: AgentHubProbeInfo[];
  agentConfig?: {
    providers: AgentHubConfigProvider[];
    agents: AgentHubConfigAgent[];
  };
}

export interface AgentHubProviderInfo {
  id: string;
  name?: string;
  type?: string;
  enabled?: boolean;
  command?: string;
}

export interface AgentHubAgentInfo {
  name: string;
  providerId?: string;
  options?: Record<string, string>;
  environment?: Record<string, string>;
  available?: boolean;
  unavailableReason?: string;
}

export interface AgentHubProbeInfo {
  providerId: string;
  type?: string;
  command?: string;
  available?: boolean;
}

export interface AgentHubConfigProvider {
  id: string;
  name: string;
  type: string;
  enabled: boolean;
  command?: string;
}

export interface AgentHubConfigAgent {
  name: string;
  providerId: string;
  options?: Record<string, string>;
  environment?: Record<string, string>;
}

export interface ThemeOption {
  id: string;
  label: string;
  description: string;
}

export interface AppearanceSettings {
  layout: "auto" | "three" | "two" | "split";
  fontScales: { sidebar: number; details: number; chat: number };
  theme: string;
  themeOptions: ThemeOption[];
}

export interface SettingsDraft {
  tab: "workspace" | "appearance" | "agenthub" | "profiles" | "notifications";
  workspacePath: string;
  createWorkspace: boolean;
  workspaceLanguage: "en" | "zh-CN";
  initialUserName: string;
  endpoint: string;
  profiles: ProfileDraft[];
  agentProviders: AgentHubConfigProvider[];
  agentConfigs: AgentHubConfigAgent[];
  dirty: boolean;
}

export interface SettingsModel {
  open: boolean;
  identity: string;
  dataVersion: number;
  initialTab: SettingsDraft["tab"];
  workspaces: WorkspaceOption[];
  activeWorkspaceId: string;
  workspaceIcons: Array<{ id: string; label: string; src: string }>;
  workspaceIconSavingId: string;
  suggestedUserName: string;
  appearance: AppearanceSettings;
  agentHub: AgentHubSettings;
  profiles: ProfileDraft[];
  agents: AgentOption[];
  notifications: NotificationPreferences;
  onClose: (dirty: boolean) => void;
  onAddWorkspace: (draft: SettingsDraft) => Promise<void>;
  onRemoveWorkspace: (id: string, draft: SettingsDraft) => Promise<void>;
  onWorkspaceIcon: (id: string, icon: string, draft: SettingsDraft) => Promise<void>;
  onSaveWorkspaceName: (id: string, name: string, draft: SettingsDraft) => Promise<void>;
  onLayoutPreference: (preference: AppearanceSettings["layout"]) => void;
  onFontScale: (column: keyof AppearanceSettings["fontScales"], value: number) => void;
  onResetFontScales: () => void;
  onThemePreference: (theme: string) => void;
  onSaveAgentHub: (draft: SettingsDraft) => Promise<void>;
  onToggleProvider: (providerId: string, enabled: boolean) => Promise<AgentHubConfigProvider>;
  onBrowserNotifications: (enabled: boolean) => void;
  onCompletionSound: (enabled: boolean) => void;
  onToast: (message: string) => void;
}
