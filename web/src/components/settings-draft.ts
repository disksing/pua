import type { SettingsDraft, SettingsModel } from "./models";
import { cloneAgentHubAgent, cloneAgentHubProvider } from "./agenthub-config";

export function createSettingsDraft(model: SettingsModel): SettingsDraft {
  return {
    tab: model.initialTab,
    workspacePath: "",
    createWorkspace: false,
    workspaceLanguage: "en",
    initialUserName: model.suggestedUserName,
    endpoint: model.agentHub.configuredEndpoint || "http://127.0.0.1:4646/agenthub",
    profiles: model.profiles.map((profile) => ({ ...profile })),
    agentProviders: (model.agentHub.agentConfig?.providers || model.agentHub.providers.map((provider) => ({
      id: provider.id,
      name: provider.name || provider.id,
      type: provider.type || provider.id,
      enabled: provider.enabled !== false,
      ...(provider.command ? { command: provider.command } : {}),
    }))).map(cloneAgentHubProvider),
    agentConfigs: (model.agentHub.agentConfig?.agents || model.agentHub.agents.map((agent) => ({
      name: agent.name,
      providerId: agent.providerId || "",
      ...(agent.options ? { options: { ...agent.options } } : {}),
      ...(agent.environment ? { environment: { ...agent.environment } } : {}),
    }))).map(cloneAgentHubAgent),
    dirty: false,
  };
}

export function cloneSettingsDraft(draft: SettingsDraft): SettingsDraft {
  return {
    ...draft,
    profiles: draft.profiles.map((profile) => ({ ...profile })),
    agentProviders: draft.agentProviders.map(cloneAgentHubProvider),
    agentConfigs: draft.agentConfigs.map(cloneAgentHubAgent),
  };
}

export function settingsErrorMessage(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}
