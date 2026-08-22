<script lang="ts">
  import "./AttentionList.css";

  import Icon from "./Icon.svelte";
  import StatusPresentation from "./StatusPresentation.svelte";
  import { markdownHTML, markdownResourceNavigation, type ResourceTitleResolver } from "./markdown";
  import type { ShellActivityItem, ShellActivityLists, ShellInboxMessage, ShellStatusPresentation } from "./models";

  let {
    activity,
    inbox,
    workspaceId = "",
    resolveResourceTitle = () => null,
    onNavigate = () => {},
    onSelect,
    onToggleFavorite,
    onOpenInboxMessage,
    onReplyInboxMessage,
    onDeleteInboxMessage,
    onToast,
  }: {
    activity: ShellActivityLists;
    inbox: ShellInboxMessage[];
    workspaceId?: string;
    resolveResourceTitle?: ResourceTitleResolver;
    onNavigate?: (resourceId: string) => void;
    onSelect: (id: string) => Promise<void>;
    onToggleFavorite: (id: string, favorite: boolean) => Promise<void>;
    onOpenInboxMessage: (id: string) => Promise<void>;
    onReplyInboxMessage: (id: string, text: string) => Promise<void>;
    onDeleteInboxMessage: (id: string) => Promise<void>;
    onToast: (message: string) => void;
  } = $props();

  type ActivityTab = keyof ShellActivityLists | "inbox";
  let activeTab = $state<ActivityTab>("running");
  // Short labels keep every category visible within the narrow sidebar; the
  // full name stays available via tooltip (title) and the tabpanel aria-label.
  const tabs: Array<{ key: ActivityTab; label: string; fullLabel: string }> = [
    { key: "running", label: "Running", fullLabel: "Running" },
    { key: "favorites", label: "Favs", fullLabel: "Favorites" },
    { key: "unread", label: "Unread", fullLabel: "Unread" },
    { key: "problems", label: "Issues", fullLabel: "Problems" },
    { key: "inbox", label: "Inbox", fullLabel: "Inbox" },
  ];

  let replyOpenId = $state("");
  let replyDraft = $state("");
  let replySending = $state(false);

  const unreadInboxCount = $derived(inbox.filter((message) => message.unread).length);

  function tabCount(tab: ActivityTab): number {
    if (tab === "inbox") return unreadInboxCount;
    return activity[tab].length;
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

  function canFavorite(item: ShellActivityItem): boolean {
    return item.type === "project" || item.type === "task";
  }

  function metadata(item: ShellActivityItem): string {
    return [
      item.ref || item.id,
      item.agentName ? `Agent ${item.agentName}` : "",
      item.turnNumber > 0 ? `Turn ${item.turnNumber}` : "No turns",
      item.statusLabel,
    ].filter(Boolean).join(" · ");
  }

  function emptyMessage(tab: ActivityTab): string {
    if (tab === "inbox") return "No agent messages yet.";
    if (tab === "favorites") return "Favorite a project or task to keep it here.";
    if (tab === "unread") return "All resource Turns have been read.";
    if (tab === "problems") return "No blocked or error Tasks.";
    return "No resources are currently running.";
  }

  async function select(item: ShellActivityItem): Promise<void> {
    try {
      await onSelect(item.id);
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

  async function toggleFavorite(event: Event, item: ShellActivityItem): Promise<void> {
    event.preventDefault();
    event.stopPropagation();
    if (event instanceof MouseEvent) (event.currentTarget as HTMLElement | null)?.blur();
    try {
      await onToggleFavorite(item.id, !item.favorite);
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

  function favoriteKeydown(event: KeyboardEvent, item: ShellActivityItem): void {
    if (event.key !== "Enter" && event.key !== " ") return;
    void toggleFavorite(event, item);
  }

  async function openInboxMessage(message: ShellInboxMessage): Promise<void> {
    try {
      await onOpenInboxMessage(message.id);
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

  function inboxRowKeydown(event: KeyboardEvent, message: ShellInboxMessage): void {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    void openInboxMessage(message);
  }

  function inboxTextHTML(text: string): string {
    const source = String(text || "");
    if (!window.marked || !window.DOMPurify) return escapeHTML(source).replaceAll("\n", "<br>");
    return markdownHTML(source, { workspaceId, resolveResourceTitle });
  }

  // Link clicks inside the message body follow the link itself; they must not
  // bubble to the row, which would open the source resource instead.
  function inboxTextClick(event: Event): void {
    if (event.target instanceof Element && event.target.closest("a")) event.stopPropagation();
  }

  function escapeHTML(value: string): string {
    return value.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;").replaceAll("'", "&#39;");
  }

  function toggleReply(event: Event, message: ShellInboxMessage): void {
    event.preventDefault();
    event.stopPropagation();
    if (event instanceof MouseEvent) (event.currentTarget as HTMLElement | null)?.blur();
    if (replyOpenId === message.id) {
      replyOpenId = "";
      replyDraft = "";
      return;
    }
    replyOpenId = message.id;
    replyDraft = "";
  }

  async function sendReply(message: ShellInboxMessage): Promise<void> {
    const text = replyDraft.trim();
    if (!text || replySending) return;
    replySending = true;
    try {
      await onReplyInboxMessage(message.id, text);
      replyOpenId = "";
      replyDraft = "";
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    } finally {
      replySending = false;
    }
  }

  async function deleteInboxMessage(event: Event, message: ShellInboxMessage): Promise<void> {
    event.preventDefault();
    event.stopPropagation();
    if (event instanceof MouseEvent) (event.currentTarget as HTMLElement | null)?.blur();
    try {
      await onDeleteInboxMessage(message.id);
      if (replyOpenId === message.id) {
        replyOpenId = "";
        replyDraft = "";
      }
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
  {#if activeTab === "inbox"}
    <div id="activity-panel-inbox" class="attention-list" role="tabpanel" aria-label="Inbox messages">
      {#if inbox.length === 0}
        <div class="activity-row empty-attention"><Icon name="inbox" /><div><strong>No messages</strong><span>{emptyMessage("inbox")}</span></div></div>
      {:else}
        {#each inbox as message (message.id)}
          <div class={`inbox-row ${message.unread ? "unread" : ""}`} role="button" tabindex="0" aria-label={`Message from ${message.resourceId}. ${message.unread ? "Unread." : ""}`} onclick={() => openInboxMessage(message)} onkeydown={(event) => inboxRowKeydown(event, message)}>
            <span class="inbox-row-head">
              <span class="inbox-unread-slot" aria-hidden="true">{#if message.unread}<span class="inbox-unread-dot"></span>{/if}</span>
              <span class="inbox-source">
                <strong>{message.resourceId}</strong>
                <span class="inbox-context">{message.resourceTitle !== message.resourceId ? `${message.resourceTitle} · ` : ""}{message.timeLabel}</span>
              </span>
              {#if message.replied}<span class="inbox-replied-badge" title="You replied to this message">Replied</span>{/if}
            </span>
            <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
            <div class="inbox-text markdown-rendered" use:markdownResourceNavigation={{ resolveResourceTitle, onNavigate }} onclick={inboxTextClick}>{@html inboxTextHTML(message.text)}</div>
            <span class="inbox-actions">
              <button type="button" class="inbox-action" title="Open the source resource" aria-label={`Open ${message.resourceId}`} onclick={(event) => { event.stopPropagation(); void openInboxMessage(message); }}><Icon name="arrow-right" /> Open</button>
              <button type="button" class="inbox-action" title="Reply to this message" aria-label={`Reply to message from ${message.resourceId}`} aria-expanded={replyOpenId === message.id} onclick={(event) => toggleReply(event, message)}><Icon name="reply" /> Reply</button>
              <button type="button" class="inbox-action inbox-delete" title="Delete this message" aria-label={`Delete message from ${message.resourceId}`} onclick={(event) => deleteInboxMessage(event, message)}><Icon name="trash-2" /></button>
            </span>
            {#if replyOpenId === message.id}
              <!-- svelte-ignore a11y_click_events_have_key_events, a11y_no_static_element_interactions -->
              <span class="inbox-reply-form" onclick={(event) => event.stopPropagation()}>
                <textarea class="inbox-reply-input" rows="2" placeholder={`Reply to ${message.resourceId}...`} bind:value={replyDraft} disabled={replySending}></textarea>
                <span class="inbox-reply-actions">
                  <button type="button" class="inbox-action" disabled={replySending} onclick={() => { replyOpenId = ""; replyDraft = ""; }}>Cancel</button>
                  <button type="button" class="inbox-action inbox-send" disabled={replySending || !replyDraft.trim()} onclick={() => sendReply(message)}><Icon name="send" /> Send</button>
                </span>
              </span>
            {/if}
          </div>
        {/each}
      {/if}
    </div>
  {:else}
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
            <span class="activity-title"><strong>{item.title}</strong><span class="activity-meta">{metadata(item)}</span></span>
            <span class="activity-actions">
              {#if canFavorite(item)}
                <!-- svelte-ignore a11y_no_static_element_interactions -->
                <span class:favorite={item.favorite} class="favorite-star" role="checkbox" aria-checked={item.favorite} tabindex="0" aria-label={item.favorite ? `Remove ${item.title} from favorites` : `Add ${item.title} to favorites`} title={item.favorite ? "Remove from favorites" : "Add to favorites"} onclick={(event) => toggleFavorite(event, item)} onkeydown={(event) => favoriteKeydown(event, item)}><Icon name="star" /></span>
              {/if}
            </span>
          </button>
        {/each}
      {/if}
    </div>
  {/if}
</section>
