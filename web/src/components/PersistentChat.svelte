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
  type Chat = { id: string; open: boolean; epoch: number; unreadCount: number; messages: Array<{ id: string; role: string; text: string; createdAt: string }> };
  let chat = $state<Chat | null>(null);
  let root: HTMLElement;
  let followTail = true;
  let scrollIdentity = "";
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
    if (!canLoad) return;
    let disposed = false, loading = false;
    async function refresh() {
      if (loading) return; loading = true; const revision = mutationRevision;
      try { const next = await client.request<Chat>(path, { headers: userHeaders }); if (!disposed && key === identity && revision === mutationRevision && !pending) {
        chat = next;
        model.onChatState?.({ open: next.open, unreadCount: next.unreadCount || 0 });
        await tick();
        if (disposed || key !== identity || revision !== mutationRevision) return;
        const last=next.messages.at(-1);
        if (last && next.unreadCount > 0 && document.visibilityState !== "hidden" && followTail) {
          const read = await client.request<Chat>(`${path}/read`, { method:"POST", headers:{...userHeaders,"Content-Type":"application/json"}, body:JSON.stringify({id:last.id}) });
          if (!disposed && key === identity && revision === mutationRevision) model.onChatState?.({ open: read.open, unreadCount: read.unreadCount || 0 });
        }
      } }
      catch (e) { if (!disposed) error = String(e); }
      finally { loading = false; }
    }
    void refresh(); const timer = setInterval(refresh, 2000);
    return () => { disposed = true; clearInterval(timer); };
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
    const onScroll = () => { if (container) followTail = container.scrollHeight - container.scrollTop - container.clientHeight < 120; };
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
      const next = await client.request<Chat>(`${base}/${operation}`, { method: "POST", headers, body: JSON.stringify(body) });
      if (key !== identity) return false; chat = next;
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
    finally { if (key === identity) pending = false; }
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
  <header><strong>Chat</strong><span>{model.resourceTitle}</span>{#if chat?.open && model.resourceId !== "workspace" && !model.detail?.archived}<button disabled={pending} onclick={() => act("close")}>Close chat</button>{/if}</header>
  {#if chat}<SessionControls {model} />{/if}
  <div class="public-messages" aria-live="polite">
    {#each chat?.messages || [] as message (message.id)}
      <article class:user-message={message.role === "user"}>
        <small>{message.role === "user" ? model.currentUserName : "Agent"} · {new Date(message.createdAt).toLocaleString()}</small>
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
  header { display:flex; align-items:center; gap:12px; flex-wrap:wrap; }
  header span { flex:1; opacity:.65; }
  .public-messages { flex:1; display:flex; flex-direction:column; gap:16px; }
  article { padding:12px; border:1px solid var(--border-color, #8883); border-radius:10px; overflow-wrap:anywhere; }
  .user-message { margin-left:24px; background:var(--surface-hover, #8881); }
  small,.chat-hint { opacity:.7; }
  .public-chat-composer { position:sticky; bottom:0; background:var(--panel); margin:0 -16px -16px; padding-bottom:6px; }
  button { padding:7px 12px; cursor:pointer; }
  [role="alert"] { color:var(--danger-color, #c44); }
</style>
