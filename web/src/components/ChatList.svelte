<script lang="ts">
 import { onMount } from "svelte";
 import { ApiClient } from "../api/client";
 import type { ModelChannel } from "./model-channel";
 import type { DetailPanelModel } from "./models";
 let { channel }: { channel: ModelChannel<DetailPanelModel> } = $props();
 // svelte-ignore state_referenced_locally
 let model = $state(channel.current());
 let chats = $state<Array<{resourceId:string; open:boolean; unread:number}>>([]);
 const client = new ApiClient();
 const workspace = $derived(model.workspaceId), user = $derived(model.currentUserName);
 onMount(() => channel.subscribe(next => model = next));
 $effect(() => {
  const id = workspace, name = user; chats = [];
  if (!id || !name) return;
  let disposed=false, loading=false;
  async function refresh() {
   if (loading) return; loading=true;
   try { const response=await client.request<{chats:typeof chats}>(`/api/workspaces/${encodeURIComponent(id)}/chats`, {headers:{"X-PUA-User":name}}); if (!disposed) chats=response.chats; }
   catch { /* Retain the last successful list while reconnecting. */ }
   finally { loading=false; }
  }
  void refresh(); const timer=setInterval(refresh,3000); return () => {disposed=true;clearInterval(timer);};
 });
</script>
<section class="chat-list" aria-label="Chats"><strong>Chats</strong>
 {#each chats as chat (chat.resourceId)}<button class:selected={model.resourceId===chat.resourceId} onclick={() => model.onNavigate(chat.resourceId)}><span>{model.resolveResourceTitle(chat.resourceId) || chat.resourceId}</span>{#if chat.unread}<b>{chat.unread}</b>{/if}{#if !chat.open}<small>Closed</small>{/if}</button>{/each}
</section>
<style>.chat-list { padding:10px; max-height:200px; overflow:auto; } button { display:flex; width:100%; gap:8px; padding:6px; text-align:left; border:0; background:transparent; cursor:pointer; } button span { flex:1; } .selected { background:var(--surface-hover,#8882); } small { opacity:.6; }</style>
