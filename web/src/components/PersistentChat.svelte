<script lang="ts">
  import ChatComposer from "./ChatComposer.svelte";
  import UploadDialog from "./UploadDialog.svelte";
  import { createModelChannel } from "./model-channel";
  import type { ComposerModel, UploadDialogModel } from "./models";
  import SessionControls from "./SessionControls.svelte";
  import { onMount, tick, untrack } from "svelte";
  import { ApiClient } from "../api/client";
  import type { ModelChannel } from "./model-channel";
  import type { DetailPanelModel } from "./models";
  import { markdownHTML, markdownResourceNavigation } from "./markdown";
  let { channel }: { channel: ModelChannel<DetailPanelModel> } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  type Message = { id: string; role: string; sender?: string; text: string; createdAt: string };
  type Chat = { id: string; open: boolean; epoch: number; unreadCount: number; messages: Message[]; hasOlder?: boolean; hasNewer?: boolean };
  const PAGE_SIZE = 40;
  let chat = $state<Chat | null>(null);
  let root: HTMLElement;
  let followTail = true;
  let scrollIdentity = "";
  let hasOlder = $state(false);
  let loadingOlder = $state(false);
  let olderError = $state("");
  let loadOlder: () => void = () => {};
  let refreshNow: () => void = () => {};
  let draft = $state("");
  let draftResetVersion = $state(0);
  let uploadOpen = $state(false);
  let uploadCycle = $state(0);
  let bindingSaving = $state(false);
  let pending = $state(false);
  let error = $state("");
  let requestID = "", sentText = "", mutationRevision = 0;
  const client = new ApiClient();
  const base = $derived(`/api/workspaces/${encodeURIComponent(model.workspaceId)}/resources/${encodeURIComponent(model.resourceId)}/chat`);
  const identity = $derived(`${model.identity}:${model.currentUserName}`);
  const userName = $derived(model.currentUserName);
  const canLoad = $derived(Boolean(model.workspaceId && model.resourceId && model.currentUserName));
  const headers = $derived({ "Content-Type": "application/json", "X-PUA-User": userName });
  onMount(() => channel.subscribe(next => model = next));
  $effect(() => {
    const key = identity, path = base, userHeaders = { "X-PUA-User": userName };
    chat = null; draft = ""; error = ""; requestID = ""; pending = false; uploadOpen = false; bindingSaving = false;
    hasOlder = false; loadingOlder = false; olderError = "";
    if (!canLoad) return;
    let disposed = false, loading = false;
    async function refresh() {
      if (loading || untrack(() => pending)) return;
      loading = true; const revision = mutationRevision;
      try {
        let after = untrack(() => chat?.messages.at(-1)?.id);
        let next: Chat;
        let pages = 0;
        do {
          const query = new URLSearchParams({ limit: String(PAGE_SIZE) });
          if (after) query.set("after", after);
          next = await client.request<Chat>(`${path}?${query}`, { headers: userHeaders });
          if (disposed || key !== identity || revision !== mutationRevision || untrack(() => pending)) return;
          const current = untrack(() => chat);
          if (!current || !after) {
            chat = next;
            hasOlder = Boolean(next.hasOlder);
          } else {
            const known = new Set(current.messages.map(message => message.id));
            chat = { ...next, messages: [...current.messages, ...next.messages.filter(message => !known.has(message.id))] };
          }
          model.onChatState?.({ open: next.open, unreadCount: next.unreadCount || 0 });
          const last = next.messages.at(-1)?.id;
          if (!last || last === after) break;
          after = last;
        } while (next.hasNewer && ++pages < 10);
        await tick();
        if (disposed || key !== identity || revision !== mutationRevision) return;
        const latest = untrack(() => chat);
        const last = latest?.messages.at(-1);
        if (last && latest && latest.unreadCount > 0 && document.visibilityState !== "hidden" && followTail && !next.hasNewer) {
          const read = await client.request<Chat>(`${path}/read?limit=1`, { method:"POST", headers:{...userHeaders,"Content-Type":"application/json"}, body:JSON.stringify({id:last.id}) });
          if (!disposed && key === identity && revision === mutationRevision) model.onChatState?.({ open: read.open, unreadCount: read.unreadCount || 0 });
        }
      }
      catch (e) { if (!disposed) error = String(e); }
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
        const container = root?.closest<HTMLElement>("#chatTimeline");
        const height = container?.scrollHeight || 0, top = container?.scrollTop || 0;
        const known = new Set(active.messages.map(message => message.id));
        followTail = false;
        chat = { ...active, messages: [...next.messages.filter(message => !known.has(message.id)), ...active.messages] };
        hasOlder = Boolean(next.hasOlder);
        await tick();
        if (!disposed && key === identity && container) container.scrollTop = top + container.scrollHeight - height;
      } catch (e) { if (!disposed) olderError = String(e); }
      finally { if (!disposed) loadingOlder = false; }
    }
    loadOlder = () => { void older(); };
    refreshNow = () => { void refresh(); };
    void refresh(); const timer = setInterval(refresh, 2000);
    return () => { disposed = true; clearInterval(timer); loadOlder = () => {}; refreshNow = () => {}; };
  });
  let lastFocusRequest = 0;
  $effect(() => {
    const request = model.chatFocusRequest;
    if (!request || request === lastFocusRequest) return;
    lastFocusRequest = request;
    const key = identity;
    void tick().then(() => {
      if (key !== identity) return;
      root?.focus({ preventScroll: true });
      root?.scrollIntoView({ block: "nearest" });
    });
  });
  function trackScroll(element: HTMLElement) {
    root = element;
    const container = element.closest<HTMLElement>("#chatTimeline");
    const onScroll = () => {
      if (!container) return;
      followTail = container.scrollHeight - container.scrollTop - container.clientHeight < 120;
      if (container.scrollTop < 120) loadOlder();
    };
    container?.addEventListener("scroll", onScroll, { passive: true });
    return { destroy: () => container?.removeEventListener("scroll", onScroll) };
  }
  $effect(() => {
    const last = chat?.messages.at(-1)?.id;
    if (scrollIdentity !== identity) { scrollIdentity = identity; followTail = true; }
    if (!last || !followTail) return;
    void tick().then(() => { const container = root?.closest<HTMLElement>("#chatTimeline"); if (container) container.scrollTop = container.scrollHeight; });
  });
  async function act(operation: string, body: unknown = {}) {
    const key = identity; mutationRevision++; pending = true; error = "";
    try {
      const next = await client.request<Chat>(`${base}/${operation}?limit=1`, { method: "POST", headers, body: JSON.stringify(body) });
      if (key !== identity) return false;
      chat = chat ? { ...next, messages: chat.messages } : next;
      model.onChatState?.({ open: next.open, unreadCount: next.unreadCount || 0 });
      if (operation === "messages") {
        if (draft === sentText) { draft = ""; draftResetVersion++; }
        requestID = "";
      }
      if (!next.open) uploadOpen = false;
      return true;
    } catch (e) {
      if (operation === "messages") throw e;
      if (key === identity) error = String(e);
      return false;
    }
    finally { if (key === identity) { pending = false; refreshNow(); } }
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
        bindingSaving = true; error = "";
        try { await model.onSaveAgentBinding(binding); }
        catch (e) { if (key === identity) error = String(e); }
        finally { if (key === identity) bindingSaving = false; }
      },
    };
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

</script>
<section tabindex="-1" use:trackScroll class="persistent-chat" aria-label="Resource chat">
  {#snippet heading()}
    <header><strong>Chat</strong><span>{model.resourceTitle}</span>{#if chat?.open && model.resourceId !== "workspace" && !model.detail?.archived}<button disabled={pending} onclick={() => act("close")}>Close chat</button>{/if}</header>
  {/snippet}
  {#if chat}<SessionControls {model} {heading} />{:else}<div class="chat-heading">{@render heading()}</div>{/if}
  <div class="public-messages" aria-live="polite">
    {#if hasOlder}
      <div class="older-messages">
        <button disabled={loadingOlder} onclick={() => loadOlder()}>{loadingOlder ? "Loading older messages…" : "Load older messages"}</button>
        {#if olderError}<small role="alert">{olderError}</small>{/if}
      </div>
    {/if}
    {#each chat?.messages || [] as message (message.id)}
      <article class:user-message={message.role === "user"}>
        <small>{message.role === "user" ? (message.sender || "User") : "Agent"} · {new Date(message.createdAt).toLocaleString()}</small>
        <div class="markdown-rendered" use:markdownResourceNavigation={{ resolveResourceTitle: model.resolveResourceTitle, onNavigate: model.onNavigate }}>{@html markdownHTML(message.text, { resolveResourceTitle: model.resolveResourceTitle, workspaceId: model.workspaceId })}</div>
      </article>
    {/each}
    {#if chat && !chat.messages.length}<p class="chat-hint">Start a conversation about this resource. Execution events are available in their own tab.</p>{/if}
  </div>
  {#if error}<p role="alert">{error}</p>{/if}
  {#if chat && !model.detail?.archived}
    {#if chat.open}
      <div class="public-chat-composer" data-component-owner="chat-composer">
        <ChatComposer channel={composer} />
      </div>
    {:else}<div class="chat-hint"><p>Chat is closed. History is preserved; background work continues.</p><button disabled={pending} onclick={() => act("open")}>{chat.epoch ? "Reopen chat" : "Open chat"}</button></div>{/if}
  {/if}
</section>
<div data-component-owner="upload-dialog" style="display: contents"><UploadDialog channel={upload} /></div>
<style>
  .persistent-chat { display:flex; flex-direction:column; min-height:100%; gap:12px; padding:16px; }
  .persistent-chat :global(.chat-heading) { position:sticky; top:0; z-index:2; display:flex; flex-direction:column; gap:8px; margin:-16px -16px 0; padding:16px; background:var(--panel); border-bottom:1px solid var(--border-color, #8883); }
  .persistent-chat :global(.chat-heading)::before { content:""; position:absolute; left:0; right:0; top:-10px; height:10px; background:var(--panel); }
  header { display:flex; align-items:center; gap:12px; flex-wrap:wrap; }
  header span { flex:1; opacity:.65; }
  .public-messages { flex:1; display:flex; flex-direction:column; gap:16px; }
  .older-messages { display:flex; flex-direction:column; align-items:center; gap:4px; }
  article { padding:12px; border:1px solid var(--border-color, #8883); border-radius:10px; overflow-wrap:anywhere; }
  .user-message { margin-left:24px; background:var(--surface-hover, #8881); }
  small,.chat-hint { opacity:.7; }
  .public-chat-composer { position:sticky; bottom:0; background:var(--panel); margin:0 -16px -16px; padding-bottom:6px; }
  button { padding:7px 12px; cursor:pointer; }
  [role="alert"] { color:var(--danger-color, #c44); }
</style>
