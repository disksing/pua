<script lang="ts">
  import "./SettingsModal.css";

  import { onMount } from "svelte";

  import AgentHubSettingsPanel from "./AgentHubSettingsPanel.svelte";
  import AppearanceSettingsPanel from "./AppearanceSettingsPanel.svelte";
  import Icon from "./Icon.svelte";
  import type { ModelChannel } from "./model-channel";
  import type { SettingsDraft, SettingsModel } from "./models";
  import NotificationSettingsPanel from "./NotificationSettingsPanel.svelte";
  import ProfilesSettingsPanel from "./ProfilesSettingsPanel.svelte";
  import { createSettingsDraft } from "./settings-draft";
  import SettingsNavigation from "./SettingsNavigation.svelte";
  import SystemInfoPanel from "./SystemInfoPanel.svelte";
  import WorkspaceSettingsPanel from "./WorkspaceSettingsPanel.svelte";

  let { channel }: { channel: ModelChannel<SettingsModel> } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  let identity = $state("");
  let dataVersion = $state(-1);
  // svelte-ignore state_referenced_locally
  let draft = $state<SettingsDraft>(createSettingsDraft(model));
  let pending = $state("");

  onMount(() => channel.subscribe((next) => {
    model = next;
    if (next.identity !== identity) {
      identity = next.identity;
      dataVersion = next.dataVersion;
      draft = createSettingsDraft(next);
      pending = "";
    } else if (next.dataVersion !== dataVersion && !draft.dirty) {
      const activeTab = draft.tab;
      dataVersion = next.dataVersion;
      draft = createSettingsDraft(next);
      draft.tab = activeTab;
    }
  }));

  onMount(() => {
    const keydown = (event: KeyboardEvent) => {
      if (model.open && event.key === "Escape") {
        event.preventDefault();
        model.onClose(draft.dirty);
      }
    };
    document.addEventListener("keydown", keydown);
    return () => document.removeEventListener("keydown", keydown);
  });

  function markDirty(): void {
    draft.dirty = true;
  }
</script>

{#if model.open}
  <button class="settings-overlay modal-enter" type="button" aria-label="Close settings" onclick={() => model.onClose(draft.dirty)}></button>
  <div class="settings-modal modal-enter" role="dialog" aria-modal="true" aria-label="System Settings">
    <SettingsNavigation activeTab={draft.tab} dirty={draft.dirty} onSelect={(tab) => draft.tab = tab} />
    <div class="settings-content">
      <div class="settings-header">
        <button type="button" class="settings-close" title="Close" aria-label="Close" onclick={() => model.onClose(draft.dirty)}><Icon name="x" /></button>
      </div>
      <div class="settings-body">
      {#if draft.tab === "system"}
        <SystemInfoPanel system={model.system} />
      {:else if draft.tab === "workspace"}
        <WorkspaceSettingsPanel workspaces={model.workspaces} activeWorkspaceId={model.activeWorkspaceId} workspaceIcons={model.workspaceIcons} bind:draft bind:pending onAddWorkspace={model.onAddWorkspace} onRemoveWorkspace={model.onRemoveWorkspace} onWorkspaceIcon={model.onWorkspaceIcon} onSaveWorkspaceName={model.onSaveWorkspaceName} onToast={model.onToast} />
      {:else if draft.tab === "appearance"}
        <AppearanceSettingsPanel appearance={model.appearance} onLayoutPreference={model.onLayoutPreference} onFontScale={model.onFontScale} onResetFontScales={model.onResetFontScales} onThemePreference={model.onThemePreference} />
      {:else if draft.tab === "agenthub"}
        <AgentHubSettingsPanel agentHub={model.agentHub} bind:draft bind:pending onDirty={markDirty} onSaveAgentHub={model.onSaveAgentHub} onSetProviderCommand={model.onSetProviderCommand} onToast={model.onToast} />
      {:else if draft.tab === "profiles"}
        <ProfilesSettingsPanel agents={model.agents} bind:draft bind:pending onDirty={markDirty} onSaveAgentHub={model.onSaveAgentHub} onToast={model.onToast} />
      {:else}
        <NotificationSettingsPanel notifications={model.notifications} onBrowserNotifications={model.onBrowserNotifications} onCompletionSound={model.onCompletionSound} />
      {/if}
      </div>
    </div>
  </div>
{/if}
