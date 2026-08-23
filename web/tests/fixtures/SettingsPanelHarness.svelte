<script lang="ts">
  import AgentHubSettingsPanel from "../../src/components/AgentHubSettingsPanel.svelte";
  import AppearanceSettingsPanel from "../../src/components/AppearanceSettingsPanel.svelte";
  import NotificationSettingsPanel from "../../src/components/NotificationSettingsPanel.svelte";
  import ProfilesSettingsPanel from "../../src/components/ProfilesSettingsPanel.svelte";
  import type { SettingsDraft, SettingsModel } from "../../src/components/models";
  import WorkspaceSettingsPanel from "../../src/components/WorkspaceSettingsPanel.svelte";

  type Panel = "workspace" | "appearance" | "agenthub" | "profiles" | "notifications";

  let { panel, model, initialDraft }: { panel: Panel; model: SettingsModel; initialDraft: SettingsDraft } = $props();
  // svelte-ignore state_referenced_locally
  let draft = $state<SettingsDraft>({
    ...initialDraft,
    profiles: initialDraft.profiles.map((profile) => ({ ...profile })),
    agentProviders: initialDraft.agentProviders.map((provider) => ({ ...provider })),
    agentConfigs: initialDraft.agentConfigs.map((agent) => ({ ...agent, options: agent.options ? { ...agent.options } : undefined, environment: agent.environment ? { ...agent.environment } : undefined })),
  });
  let pending = $state("");

  function markDirty(): void {
    draft.dirty = true;
  }
</script>

{#if panel === "workspace"}
  <WorkspaceSettingsPanel workspaces={model.workspaces} activeWorkspaceId={model.activeWorkspaceId} workspaceIcons={model.workspaceIcons} bind:draft bind:pending onAddWorkspace={model.onAddWorkspace} onRemoveWorkspace={model.onRemoveWorkspace} onWorkspaceIcon={model.onWorkspaceIcon} onSaveWorkspaceName={model.onSaveWorkspaceName} onToast={model.onToast} />
{:else if panel === "appearance"}
  <AppearanceSettingsPanel appearance={model.appearance} onLayoutPreference={model.onLayoutPreference} onFontScale={model.onFontScale} onResetFontScales={model.onResetFontScales} onThemePreference={model.onThemePreference} />
{:else if panel === "agenthub"}
  <AgentHubSettingsPanel agentHub={model.agentHub} bind:draft bind:pending onDirty={markDirty} onSaveAgentHub={model.onSaveAgentHub} onToggleProvider={model.onToggleProvider} onToast={model.onToast} />
{:else if panel === "profiles"}
  <ProfilesSettingsPanel agents={model.agents} bind:draft bind:pending onDirty={markDirty} onSaveAgentHub={model.onSaveAgentHub} onToast={model.onToast} />
{:else}
  <NotificationSettingsPanel notifications={model.notifications} onBrowserNotifications={model.onBrowserNotifications} onCompletionSound={model.onCompletionSound} />
{/if}
