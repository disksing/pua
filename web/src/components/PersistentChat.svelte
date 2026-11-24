<script lang="ts">
  import ChatComposer from "./ChatComposer.svelte";
  import ChatOptions from "./ChatOptions.svelte";
  import FilePreviewModal from "./FilePreviewModal.svelte";
  import Icon from "./Icon.svelte";
  import UploadDialog from "./UploadDialog.svelte";
  import { createModelChannel } from "./model-channel";
  import type { ComposerModel, FilePreviewModel, UploadDialogModel } from "./models";
  import SessionControls from "./SessionControls.svelte";
  import SharedChatTimeline from "./SharedChatTimeline.svelte";
  import { captureChatAnchor, positionTimelinePreview, type SharedChatMessage } from "./shared-chat-timeline";
  import { onDestroy, onMount, tick, untrack } from "svelte";
  import { ApiClient } from "../api/client";
  import type { ModelChannel } from "./model-channel";
  import type { DetailPanelModel } from "./models";
  import { markdownHTML, markdownResourceNavigation } from "./markdown";
  let { channel, visible = true }: { channel: ModelChannel<DetailPanelModel>; visible?: boolean } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  type Message = SharedChatMessage;
  type Chat = { id: string; open: boolean; epoch: number; unreadCount: number; messages: Message[]; queuedMessages?: Message[]; timelineInputs?: Record<string, string>; hasOlder?: boolean; hasNewer?: boolean; reset?: boolean; endPosition?: number };
  const PAGE_SIZE = 40;
  const QUOTE_WIDTH = 56;
  const QUOTE_HEIGHT = 44;
  let chat = $state<Chat | null>(null);
  let echoes = $state<Message[]>([]);
  const messages = $derived.by(() => {
    const confirmed = chat?.messages || [];
    const combined = echoes.length || chat?.queuedMessages?.length
      ? [...new Map([...confirmed, ...echoes, ...(chat?.queuedMessages || [])].map(message => [message.id, message])).values()]
      : confirmed;
    return combined.map(message => {
      const at = chat?.timelineInputs?.[message.id];
      return at ? { ...message, timelineAt: at } : message;
    });
  });
  const lastMessageID = $derived(messages.at(-1)?.id);
  let root: HTMLElement;
  let timeline: HTMLElement | undefined;
  let followTail = $state(true);
  let scrollIdentity = "";
  let hasOlder = $state(false);
  let loadingOlder = $state(false);
  let olderError = $state("");
  let loadOlder: () => void = () => {};
  let refreshNow: () => void = () => {};
  let draft = $state("");
  let draftResetVersion = $state(0);
  let quote = $state<{ text: string; messageId: string; left: number; top: number } | null>(null);
  let mouseSelectingQuote = false;
  let uploadOpen = $state(false);
  let uploadCycle = $state(0);
  let bindingSaving = $state(false);
  let pending = $state(false);
  let error = $state("");
  let actionError = $state("");
  let openButton = $state<HTMLButtonElement>();
  let preview = $state<{ section: string; path: string; mode?: "edit" | "annotate" } | null>(null);
  let requestID = "", sentText = "", mutationRevision = 0;
  const client = new ApiClient();
  const base = $derived(`/api/workspaces/${encodeURIComponent(model.workspaceId)}/resources/${encodeURIComponent(model.resourceId)}/chat`);
  const identity = $derived(`${model.identity}:${model.currentUserName}`);
  const userName = $derived(model.currentUserName);
  const canLoad = $derived(Boolean(model.workspaceId && model.resourceId && model.currentUserName));
  const archived = $derived(Boolean(model.detail?.archived));
  const canClose = $derived(Boolean(chat?.open) && model.resourceId !== "workspace" && !archived);
  const canQuote = $derived(visible && Boolean(chat?.open) && !archived && !pending);
  const firstOpen = $derived(Boolean(chat && !chat.open && !chat.epoch && !messages.length && !archived));
  const headers = $derived({ "Content-Type": "application/json", "X-PUA-User": userName });
  onMount(() => channel.subscribe(next => model = next));
  onDestroy(() => client.dispose());
  $effect(() => {
    const key = identity, path = base, userHeaders = { "X-PUA-User": userName };
    mutationRevision++;
    chat = null; draft = ""; error = ""; actionError = ""; requestID = ""; pending = false; uploadOpen = false; bindingSaving = false; preview = null;
    echoes = [];
    hasOlder = false; loadingOlder = false; olderError = "";
    if (!canLoad) return;
    let disposed = false, loading = false;
    async function refresh() {
      if (loading || untrack(() => pending || !visible)) return;
      loading = true; const revision = mutationRevision;
      try {
        let after = untrack(() => chat?.messages.at(-1)?.id);
        let next: Chat;
        let pages = 0;
        do {
          const query = new URLSearchParams({ limit: String(PAGE_SIZE) });
          if (after) query.set("after", after);
          const position = untrack(() => chat?.endPosition);
          if (after && position) query.set("afterPosition", String(position));
          next = await client.request<Chat>(`${path}?${query}`, { headers: userHeaders });
          if (disposed || key !== identity || revision !== mutationRevision || untrack(() => pending)) return;
          const current = untrack(() => chat);
          const known = new Set(current?.messages.map(message => message.id));
          if (after && !next.reset && next.endPosition && next.messages.some(message => known.has(message.id))) {
            next = await client.request<Chat>(`${path}?limit=${PAGE_SIZE}`, { headers: userHeaders });
            if (disposed || key !== identity || revision !== mutationRevision || untrack(() => pending)) return;
            after = undefined;
          }
          error = "";
          if (!current || !after || next.reset) {
            chat = next;
            hasOlder = Boolean(next.hasOlder);
          } else {
            chat = { ...next, timelineInputs: { ...current.timelineInputs, ...next.timelineInputs }, messages: [...current.messages, ...next.messages.filter(message => !known.has(message.id))] };
          }
          model.onChatState?.({ open: next.open, unreadCount: next.unreadCount || 0 });
          const received = new Set(chat.messages.map(message => message.id));
          echoes = untrack(() => [...new Map([...echoes, ...(current?.queuedMessages || [])]
            .filter(message => !received.has(message.id)).map(message => [message.id, message])).values()]);
          const last = next.messages.at(-1)?.id;
          if (!last || last === after) break;
          after = last;
        } while (next.hasNewer && ++pages < 10);
        await tick();
        if (disposed || key !== identity || revision !== mutationRevision) return;
        const latest = untrack(() => chat);
        const last = latest?.messages.at(-1);
        if (last && latest && latest.unreadCount > 0 && visible && document.visibilityState !== "hidden" && followTail && !next.hasNewer) {
          const read = await client.request<Chat>(`${path}/read?limit=1`, { method:"POST", headers:{...userHeaders,"Content-Type":"application/json"}, body:JSON.stringify({id:last.id, position:latest.endPosition}) });
          if (!disposed && key === identity && revision === mutationRevision) model.onChatState?.({ open: read.open, unreadCount: read.unreadCount || 0 });
        }
      }
      catch (e) { if (!disposed && key === identity && revision === mutationRevision) error = String(e); }
      finally { loading = false; }
    }
    async function older() {
      const current = untrack(() => chat);
      const first = current?.messages[0];
      if (disposed || !first || !untrack(() => hasOlder) || untrack(() => loadingOlder)) return;
      loadingOlder = true; olderError = "";
      try {
        const query = new URLSearchParams({ limit: String(PAGE_SIZE), before: first.id });
        const next = await client.request<Chat>(`${path}?${query}`, { headers: userHeaders });
        if (disposed || key !== identity) return;
        const active = untrack(() => chat);
        if (!active || active.id !== current.id) return;
        const restore = timeline ? captureChatAnchor(timeline, root) : undefined;
        const known = new Set(active.messages.map(message => message.id));
        followTail = false;
        chat = { ...active, timelineInputs: { ...next.timelineInputs, ...active.timelineInputs }, messages: [...next.messages.filter(message => !known.has(message.id)), ...active.messages] };
        hasOlder = Boolean(next.hasOlder);
        await tick();
        if (!disposed && key === identity) restore?.();
      } catch (e) { if (!disposed) olderError = String(e); }
      finally { if (!disposed) loadingOlder = false; }
    }
    loadOlder = () => { void older(); };
    refreshNow = () => { void refresh(); };
    void refresh(); const timer = setInterval(refresh, 2000);
    return () => { disposed = true; clearInterval(timer); loadOlder = () => {}; refreshNow = () => {}; };
  });
  function trackScroll(element: HTMLElement) {
    root = element;
    const container = element.querySelector<HTMLElement>("#chatTimeline");
    if (!container) return;
    timeline = container;
    const updateScrollPadding = () => {
      const markers = container.querySelectorAll(".history-generation.sticky");
      const headerHeight = Math.max(0, ...Array.from(markers, marker => marker.getBoundingClientRect().height));
      container.style.scrollPaddingBlockStart = headerHeight ? `${Math.ceil(headerHeight) + 8}px` : "";
    };
    const movePreview = () => {
      if (!("showPopover" in HTMLElement.prototype)) return;
      const preview = container.querySelector<HTMLElement>(".timeline-preview:popover-open");
      if (preview) positionTimelinePreview(preview, container);
    };
    const onScroll = () => {
      movePreview();
      followTail = container.scrollHeight - container.scrollTop - container.clientHeight < 120;
      if (container.scrollTop < 120) loadOlder();
      updateQuote();
    };
    const resize = typeof ResizeObserver === "undefined" ? null : new ResizeObserver(() => {
      updateScrollPadding();
      movePreview();
      if (visible && followTail && window.getSelection()?.isCollapsed !== false) pinToTail();
      updateQuote();
    });
    updateScrollPadding();
    resize?.observe(container);
    if (container.firstElementChild) resize?.observe(container.firstElementChild);
    container.addEventListener("scroll", onScroll, { passive: true });
    return { destroy: () => {
      resize?.disconnect();
      container.style.scrollPaddingBlockStart = "";
      container.removeEventListener("scroll", onScroll);
      timeline = undefined;
    } };
  }
  function selectedQuote() {
    if (mouseSelectingQuote || !canQuote || !root) return null;
    const selection = window.getSelection();
    if (!selection || selection.isCollapsed || selection.rangeCount !== 1) return null;
    const range = selection.getRangeAt(0);
    const start = range.startContainer;
    const body = (start instanceof Element ? start : start.parentElement)?.closest<HTMLElement>("[data-chat-message-body]");
    const messageId = body?.dataset.chatMessageBody;
    if (!body || !messageId || !root.contains(body) || !body.contains(range.endContainer)) return null;
    const text = selection.toString();
    if (!text.trim()) return null;

    const scroll = timeline;
    if (!scroll) return null;
    const bounds = scroll.getBoundingClientRect();
    const left = Math.max(0, bounds.left);
    const right = Math.min(document.documentElement.clientWidth, bounds.left + scroll.clientLeft + scroll.clientWidth);
    const top = Math.max(0, bounds.top + (parseFloat(scroll.style.scrollPaddingBlockStart) || 0));
    const bottom = Math.min(bounds.bottom, document.documentElement.clientHeight);
    const gap = 6;
    if (right - left < QUOTE_WIDTH + 2 * gap || bottom - top < QUOTE_HEIGHT + 2 * gap) return null;
    const rect = Array.from(range.getClientRects()).find(rect =>
      rect.width > 0 && rect.right > left && rect.left < right && rect.bottom > top && rect.top < bottom);
    if (!rect) return null;

    const origin = root.getBoundingClientRect();
    const center = (Math.max(left, rect.left) + Math.min(right, rect.right)) / 2;
    const above = rect.top - QUOTE_HEIGHT - gap;
    return {
      text, messageId,
      left: Math.max(left + gap, Math.min(center - QUOTE_WIDTH / 2, right - QUOTE_WIDTH - gap)) - origin.left,
      top: (above >= top + gap ? above : Math.min(rect.bottom + gap, bottom - QUOTE_HEIGHT - gap)) - origin.top,
    };
  }
  function updateQuote() {
    quote = selectedQuote();
  }
  function beginQuoteSelection(event: PointerEvent) {
    if (!canQuote || event.pointerType !== "mouse" || event.button !== 0) return;
    if (event.target instanceof Element && event.target.closest(".chat-quote")) return;
    mouseSelectingQuote = true;
    quote = null;
  }
  function endQuoteSelection(event: PointerEvent) {
    if (event.pointerType !== "mouse" || !mouseSelectingQuote) return;
    mouseSelectingQuote = false;
    if (event.type === "pointerup") updateQuote();
  }
  function clearQuoteSelection() {
    mouseSelectingQuote = false;
    quote = null;
  }
  async function appendQuote() {
    if (!canQuote || !quote) return;
    const key = identity;
    const { text, messageId } = quote;
    const separator = !draft || draft.endsWith("\n\n") ? "" : draft.endsWith("\n") ? "\n" : "\n\n";
    draft += `${separator}> ${text.replaceAll("\n", "\n> ")}\n来源消息：${messageId}`;
    draftResetVersion++;
    quote = null;
    window.getSelection()?.removeAllRanges();
    await tick();
    if (!canQuote || key !== identity) return;
    const input = root?.querySelector("textarea");
    input?.focus({ preventScroll: true });
    input?.setSelectionRange(input.value.length, input.value.length);
  }
  function pinToTail() {
    if (timeline) timeline.scrollTop = timeline.scrollHeight;
  }
  async function jumpToLatest() {
    followTail = true;
    await tick();
    pinToTail();
    if (visible) root?.focus({ preventScroll: true });
  }
  $effect(() => {
    if (scrollIdentity !== identity) { scrollIdentity = identity; followTail = true; }
    if (!visible || !lastMessageID || !untrack(() => followTail)) return;
    void tick().then(() => { if (visible && followTail) pinToTail(); });
  });
  async function act(operation: string, body: unknown = {}) {
    if (pending) return false;
    const key = identity, revision = ++mutationRevision;
    pending = true; actionError = "";
    try {
      const next = await client.request<Chat>(`${base}/${operation}?limit=1`, { method: "POST", headers, body: JSON.stringify(body) });
      if (key !== identity || revision !== mutationRevision) return false;
      chat = chat ? { ...next, timelineInputs: chat.timelineInputs, endPosition: chat.endPosition, messages: chat.messages } : next;
      model.onChatState?.({ open: next.open, unreadCount: next.unreadCount || 0 });
      if (operation === "messages") {
        const accepted = next.messages.find(message => message.role === "user" && message.text === sentText);
        if (accepted && !messages.some(message => message.id === accepted.id)) echoes = [...echoes, accepted];
        if (draft === sentText) { draft = ""; draftResetVersion++; }
        requestID = "";
      }
      if (!next.open) uploadOpen = false;
      return true;
    } catch (e) {
      if (operation === "messages") throw e;
      if (key === identity && revision === mutationRevision) actionError = `${operation === "open" ? "Could not open chat." : "Could not close chat."} ${e instanceof Error ? e.message : String(e)}`;
      return false;
    }
    finally { if (key === identity && revision === mutationRevision) { pending = false; refreshNow(); } }
  }
  async function toggleChat(operation: "open" | "close"): Promise<boolean> {
    const key = identity;
    if (archived || (operation === "close" && !canClose) || !await act(operation)) return false;
    await tick();
    if (key !== identity || !visible) return true;
    if (operation === "open") root?.querySelector("textarea")?.focus({ preventScroll: true });
    else openButton?.focus({ preventScroll: true });
    return true;
  }
  function composerModel(): ComposerModel {
    const key = identity;
    return {
      identity: `${key}:${chat?.epoch || 0}`, workspaceId: model.workspaceId, resourceId: model.resourceId,
      draft, draftKey: key, draftResetVersion, sending: pending,
      unavailableReason: model.detail?.archived ? "This resource is archived." : !chat?.open ? "Chat is closed." : "",
      agentBinding: model.agentBinding, agentProfiles: model.agentProfiles, agents: model.agents, bindingSaving,
      // Execution controls stay in SessionControls; never project internal
      // mailbox content into the public Chat composer.
      canEndTurn: false, endingTurn: false, canEndGeneration: false, endingGeneration: false,
      stopNotice: "", waitingMessages: [], canSteerWaiting: false, steeringMessageId: "",
      onEndTurn: () => {}, onEndGeneration: () => {}, onDismissStopNotice: () => {}, onSteerWaiting: async () => {},
      onDraft: (text) => { if (key === identity) draft = text; },
      onSend: async (text) => {
        if (key !== identity || pending || !chat?.open || !text.trim()) return { accepted: false, clear: false };
        if (!requestID || sentText !== text) { requestID = crypto.randomUUID(); sentText = text; }
        const accepted = await act("messages", { id: requestID, text: sentText });
        return { accepted, clear: accepted };
      },
      onOpenUpload: () => { uploadCycle++; uploadOpen = true; },
      onSaveAgentBinding: async (binding) => {
        bindingSaving = true; actionError = "";
        try { await model.onSaveAgentBinding(binding); }
        catch (e) { if (key === identity) actionError = String(e); }
        finally { if (key === identity) bindingSaving = false; }
      },
    };
  }
  function openLinkedFile(path: string): void {
    preview = { section: "Files", path };
  }
  function rejectReadOnlySave(): Promise<FilePreviewModel> {
    return Promise.reject(new Error("Chat file previews are read-only."));
  }
  function uploadModel(): UploadDialogModel {
    const key = identity, resourcePath = model.detail?.path || "";
    return {
      open: uploadOpen && Boolean(chat?.open) && !model.detail?.archived,
      identity: `${key}:${chat?.epoch || 0}:${Boolean(chat?.open)}:${uploadCycle}`,
      workspaceId: model.workspaceId, resourceId: model.resourceId, currentUserName: userName,
      onDone: (paths) => {
        if (key !== identity || !chat?.open) return;
        const links = paths.map(path => {
          const label = (path.split("/").at(-1) || path).replace(/[\[\]\\]/g, "\\$&");
          const href = encodeURI("/" + [resourcePath, path].filter(Boolean).join("/")).replace(/\(/g, "%28").replace(/\)/g, "%29");
          return `[${label}](${href})`;
        });
        if (links.length) { draft += `${draft ? "\n" : ""}${links.join("\n")}`; draftResetVersion++; }
        uploadOpen = false;
      },
    };
  }
  const composer = createModelChannel<ComposerModel>(composerModel());
  const upload = createModelChannel<UploadDialogModel>(uploadModel());
  $effect(() => { const next = composerModel(); untrack(() => composer.publish(next)); });
  $effect(() => { const next = uploadModel(); untrack(() => upload.publish(next)); });
  $effect(() => { if (!chat?.open) uploadOpen = false; });
  $effect(() => { if (!canQuote) quote = null; });

</script>
<svelte:window onpointerdowncapture={beginQuoteSelection} onpointerupcapture={endQuoteSelection}
  onpointercancelcapture={endQuoteSelection} onblur={clearQuoteSelection} />
<svelte:document onselectionchange={updateQuote} />
<section tabindex="-1" use:trackScroll class="persistent-chat" class:quoting={canQuote && quote !== null} aria-label="Resource chat" hidden={!visible}>
  <SessionControls {model} enabled={visible && canLoad && !archived && Boolean(chat || model.detail || model.resourceType === "workspace")} {followTail} onLatest={jumpToLatest}>
    {#snippet children(execution, notices, status)}
      <div id="chatTimeline" class="chat-timeline">
        <div class="chat-history">
          {#if hasOlder}
            <div class="older-messages">
              <button type="button" disabled={loadingOlder} onclick={() => loadOlder()}>{loadingOlder ? "Loading older messages…" : "Load older messages"}</button>
              {#if olderError}<small role="alert">{olderError}</small>{/if}
            </div>
          {/if}
          {#key identity}
            <SharedChatTimeline {model} {visible} {status} {messages} queuedMessages={chat?.queuedMessages}
              followLatestTurn={chat?.open === false} {execution} {followTail} onOpenFile={openLinkedFile} onInspect={() => followTail = false}>
              {#snippet bubble(message)}
                <article class:own-message={message.role === "user" && message.sender === userName}>
                  <small class="message-meta"><strong>{message.role === "user" ? (message.sender || "User") : (message.sender?.trim() || "Agent")}</strong> <time datetime={message.createdAt}>{new Date(message.createdAt).toLocaleString()}</time></small>
                  <div class="markdown-rendered" data-chat-message-body={message.id} use:markdownResourceNavigation={{ resolveResourceTitle: model.resolveResourceTitle, onNavigate: model.onNavigate, onOpenFile: openLinkedFile }}>{@html markdownHTML(message.text, { resolveResourceTitle: model.resolveResourceTitle, workspaceId: model.workspaceId })}</div>
                </article>
              {/snippet}
              {#snippet empty()}
                {#if chat && !firstOpen}
                  <div class="chat-empty">
                    <strong>{archived || !chat.open ? "No chat history" : "No messages yet"}</strong>
                    {#if chat.open && !archived}<p>Send a message to start the conversation.</p>{/if}
                  </div>
                {:else if !chat && canLoad && !error}
                  <div class="chat-empty" role="status">Loading chat…</div>
                {/if}
              {/snippet}
            </SharedChatTimeline>
          {/key}
        </div>
      </div>
      <div class="public-chat-dock">
        {@render notices()}
        {#if error}<div class="chat-error" role="alert">Could not load chat. {error}</div>{/if}
        {#if actionError}<div class="chat-error" role="alert">{actionError}</div>{/if}
        {#if chat}
          {#if archived}
            <div class="chat-closed"><strong>Archived · Read only</strong></div>
          {:else if chat.open}
            <div class="public-chat-composer" data-component-owner="chat-composer">
              <ChatComposer channel={composer}>
                {#snippet actions()}
                  {#if canClose}
                    {#key identity}<ChatOptions disabled={pending} onCloseChat={() => toggleChat("close")} />{/key}
                  {/if}
                {/snippet}
              </ChatComposer>
            </div>
          {:else}
            <div class="chat-closed">
              <div><strong>{firstOpen ? "Chat hasn’t been opened" : "Chat is closed"}</strong><p>{firstOpen ? "Open a shared chat to message this resource." : "Background work can continue."}</p></div>
              <button type="button" class="secondary-button" bind:this={openButton} disabled={pending} onclick={() => toggleChat("open")}>{chat.epoch ? "Reopen chat" : "Open chat"}</button>
            </div>
          {/if}
        {/if}
      </div>
    {/snippet}
  </SessionControls>
  {#if canQuote && quote}
    <button type="button" class="chat-quote secondary-button" style:left={`${quote.left}px`} style:top={`${quote.top}px`}
      style:width={`${QUOTE_WIDTH}px`} style:height={`${QUOTE_HEIGHT}px`}
      onpointerdown={(event) => event.preventDefault()} onclick={appendQuote}>引用</button>
  {/if}
</section>
<div data-component-owner="upload-dialog" style="display: contents"><UploadDialog channel={upload} /></div>
<FilePreviewModal {client} workspaceId={model.workspaceId} resourceId={model.resourceId} selection={preview} editable={false} resolveResourceTitle={model.resolveResourceTitle} onNavigate={model.onNavigate} onOpenFile={openLinkedFile} onSaveMarkdown={rejectReadOnlySave} onClose={() => preview = null} onError={model.onToast} />
<style>
  .persistent-chat {
    display:flex;
    flex:1;
    flex-direction:column;
    min-width:0;
    min-height:0;
    container:shared-chat / inline-size;
    background:var(--shared-chat-canvas);
    font-size:13px;
    line-height:1.55;
  }
  .persistent-chat[hidden] { display:none; }
  .chat-timeline {
    flex:1;
    min-height:0;
    overflow-x:hidden;
    overflow-y:auto;
    overflow-anchor:none;
    scroll-padding-block-end:8px;
  }
  .chat-history { display:flex; flex-direction:column; min-height:100%; gap:8px; padding:12px 16px 8px; }
  .quoting { position:relative; }
  .chat-quote { position:absolute; z-index:3; justify-content:center; user-select:none; }
  .older-messages { display:flex; flex-direction:column; align-items:center; gap:4px; }
  article {
    align-self:flex-start;
    min-width:0;
    max-width:min(86%, 720px);
    padding:10px 14px;
    background:var(--panel);
    border:1px solid var(--line-soft);
    border-radius:14px 14px 14px 4px;
    overflow-wrap:anywhere;
  }
  .own-message {
    align-self:flex-end;
    background:color-mix(in srgb, var(--green-line) 70%, var(--panel));
    border-color:var(--green-line);
    border-radius:14px 14px 4px 14px;
  }
  @container shared-chat (min-width:1024px) {
    .own-message { align-self:flex-start; }
  }
  .message-meta { display:flex; flex-wrap:wrap; align-items:baseline; gap:0 8px; margin-bottom:3px; color:var(--muted-strong); opacity:1; }
  article .markdown-rendered { overflow-wrap:anywhere; font-size:14px; }
  article :global(.markdown-rendered img) { max-width:100%; height:auto; }
  article :global(.markdown-rendered table) { display:block; overflow-x:auto; table-layout:auto; }
  .chat-empty { margin:auto; padding:20px 6px; text-align:center; color:var(--muted-strong); }
  .chat-empty strong { display:block; font-size:15px; color:var(--text); }
  .chat-empty p, .chat-closed p { margin:2px 0 0; }
  .public-chat-dock {
    position:relative;
    z-index:2;
    display:flex;
    flex:none;
    flex-direction:column;
    gap:4px;
    min-width:0;
    padding:0 16px 8px;
    background:var(--shared-chat-canvas, var(--bg));
  }
  .public-chat-dock:empty { display:none; }
  .public-chat-composer :global(.chat-input) { margin:2px 0 0; }
  .chat-closed { display:flex; align-items:center; justify-content:space-between; flex-wrap:wrap; gap:6px 12px; padding:6px 0 3px; color:var(--muted-strong); }
  .chat-closed strong { color:var(--text); }
  .chat-closed button, .older-messages button { min-height:36px; padding:6px 12px; }
  .chat-error { border-left:2px solid var(--red); padding:5px 8px; overflow-wrap:anywhere; }
  [role="alert"] { color:var(--red-deep); }
  @container shared-chat (max-width:500px) {
    .chat-closed button, .older-messages button { min-height:44px; }
  }
</style>
