<script lang="ts">
  import "./SettingsNavigation.css";

  import Icon from "./Icon.svelte";
  import type { SettingsDraft } from "./models";

  type SettingsTab = SettingsDraft["tab"];

  let { activeTab, dirty, onSelect }: { activeTab: SettingsTab; dirty: boolean; onSelect: (tab: SettingsTab) => void } = $props();

  const tabs: Array<{ id: SettingsTab; icon: string; label: string; sharesAgentDraft: boolean }> = [
    { id: "workspace", icon: "hard-drive", label: "Workspace", sharesAgentDraft: false },
    { id: "appearance", icon: "palette", label: "Appearance", sharesAgentDraft: false },
    { id: "agenthub", icon: "network", label: "Agents", sharesAgentDraft: true },
    { id: "profiles", icon: "route", label: "Profiles", sharesAgentDraft: true },
    { id: "notifications", icon: "bell", label: "Notifications", sharesAgentDraft: false },
  ];
</script>

<aside class="settings-tabs" data-component-owner="settings-navigation">
  <div class="settings-title">System Settings</div>
  {#each tabs as tab (tab.id)}
    <button
      type="button"
      class="settings-tab"
      class:active={activeTab === tab.id}
      class:dirty={dirty && tab.sharesAgentDraft}
      aria-current={activeTab === tab.id ? "page" : undefined}
      onclick={() => onSelect(tab.id)}
    >
      <Icon name={tab.icon} />
      <span>{tab.label}</span>
      {#if tab.sharesAgentDraft}<span class="settings-tab-dot" aria-hidden="true"></span>{/if}
    </button>
  {/each}
</aside>
