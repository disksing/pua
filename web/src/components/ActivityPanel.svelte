<script lang="ts">
  import "./AttentionList.css";

  import Icon from "./Icon.svelte";
  import StatusPresentation from "./StatusPresentation.svelte";
  import { type ResourceTitleResolver } from "./markdown";
  import type { ShellActivityItem, ShellActivityLists, ShellStatusPresentation } from "./models";

  let {
    activity,
    workspaceId = "",
    resolveResourceTitle = () => null,
    onNavigate = () => {},
    onSelect,
    onSelectChat,
    onToast,
  }: {
    activity: ShellActivityLists;
    workspaceId?: string;
    resolveResourceTitle?: ResourceTitleResolver;
    onNavigate?: (resourceId: string) => void;
    onSelect: (id: string) => Promise<void>;
    onSelectChat?: (id: string) => Promise<void>;
    onToast: (message: string) => void;
  } = $props();

  type ActivityTab = keyof ShellActivityLists;
  let activeTab = $state<ActivityTab>("running");
  // Short labels keep every category visible within the narrow sidebar; the
  // full name stays available via tooltip (title) and the tabpanel aria-label.
  const tabs: Array<{ key: ActivityTab; label: string; fullLabel: string }> = [
    { key: "running", label: "Running", fullLabel: "Running" },
    { key: "unread", label: "Chat", fullLabel: "Chat" },
    { key: "problems", label: "Issues", fullLabel: "Problems" },
  ];

  function tabCount(tab: ActivityTab): number {
    return tab === "unread" ? activity.unread.filter((item) => item.unreadCount > 0).length : activity[tab].length;
  }

  function statusClass(status: ShellStatusPresentation): string {
    return [status.layoutClassName, status.className].filter(Boolean).join(" ");
  }

  function iconName(item: ShellActivityItem): string {
    if (item.type === "project") return "folder";
    if (item.type === "task") return "file-text";
    if (item.type === "scheduler") return "calendar-clock";
    return "home";
  }

  function metadata(item: ShellActivityItem): string {
    if (activeTab === "unread") return `${item.ref || item.id} · ${item.unreadCount} unread chat message${item.unreadCount === 1 ? "" : "s"}`;
    return [
      item.ref || item.id,
      item.agentName ? `Agent ${item.agentName}` : "",
      item.turnNumber > 0 ? `Turn ${item.turnNumber}` : "No turns",
      item.statusLabel,
    ].filter(Boolean).join(" · ");
  }

  function emptyMessage(tab: ActivityTab): string {
    if (tab === "unread") return "No chats yet.";
    if (tab === "problems") return "No blocked or error Tasks.";
    return "No resources are currently running.";
  }

  async function select(item: ShellActivityItem): Promise<void> {
    try {
      await (activeTab === "unread" ? onSelectChat || onSelect : onSelect)(item.id);
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

</script>

<section class="attention-section" data-component-owner="attention-list">
  <div class="activity-tabs" role="tablist" aria-label="Activity categories">
    {#each tabs as tab}
      <button type="button" role="tab" title={tab.fullLabel} aria-selected={activeTab === tab.key} aria-controls={`activity-panel-${tab.key}`} class:active={activeTab === tab.key} onclick={() => { activeTab = tab.key; }}>{tab.label} <span class="activity-tab-count">{tabCount(tab.key)}</span></button>
    {/each}
  </div>
    <div id={`activity-panel-${activeTab}`} class="attention-list" role="tabpanel" aria-label={`${tabs.find((tab) => tab.key === activeTab)?.fullLabel || "Activity"} resources`}>
      {#if activity[activeTab].length === 0}
        <div class="activity-row empty-attention"><Icon name="message-square" /><div><strong>No items</strong><span>{emptyMessage(activeTab)}</span></div></div>
      {:else}
        {#each activity[activeTab] as item (item.id)}
          <button type="button" class={`activity-row ${statusClass(item.status)} ${item.selected ? "selected" : ""}`} aria-current={item.selected ? "page" : undefined} data-active-turn={item.activeTurn || undefined} aria-label={`${item.title}. ${metadata(item)}`} title={item.statusLabel || undefined} onclick={() => select(item)}>
            <span class="activity-status" aria-hidden="true">
              <span class="activity-status-fallback-slot" hidden={item.status.hasTaskState}><Icon name={iconName(item)} className="activity-status-fallback" /></span>
              <span class="activity-status-runtime-slot" hidden={!item.status.hasTaskState}><StatusPresentation status={item.status} className="activity-status-icon" /></span>
            </span>
            <span class="activity-title"><strong>{item.title}</strong><span class="activity-meta">{activeTab === "unread" ? item.ref || item.id : metadata(item)}</span></span>
            {#if activeTab === "unread" && item.unreadCount > 0}
              <span class="activity-chat-unread-count" aria-hidden="true">{item.unreadCount}</span>
            {/if}
          </button>
        {/each}
      {/if}
    </div>
</section>
