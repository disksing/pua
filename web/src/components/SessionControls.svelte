<script lang="ts">
 import { ApiClient } from "../api/client";
 import { projectConversationEvents } from "./timeline-events";
 import ApprovalCard from "./ApprovalCard.svelte";
 import type { Snippet } from "svelte";
 import type { AgentEvent, DetailPanelModel, TimelineItem } from "./models";
 let { model, heading }: { model:DetailPanelModel; heading:Snippet } = $props();
 type Status = { sessionState:string; configError?:string; lastError?:string; generation?:{generationId:string}; session?:{pendingApprovalIds?:string[]} };
 let status = $state<Status | null>(null), approvals = $state<TimelineItem[]>([]), error = $state(""), pending = $state(false);
 const client = new ApiClient();
 const base = $derived(`/api/workspaces/${encodeURIComponent(model.workspaceId)}/resources/${encodeURIComponent(model.resourceId)}`);
 const user = $derived(model.currentUserName);
 $effect(() => {
  const path=base, name=user; let disposed=false, loading=false; status=null; approvals=[];
  async function refresh() {
   if (loading || !name) return; loading=true;
   try {
    const next=await client.request<Status>(`${path}/status`,{headers:{"X-PUA-User":name}});
    if (disposed) return; status=next; error="";
    const ids=next.session?.pendingApprovalIds || [];
    if (ids.length && next.generation) {
     const page=await client.request<{events:AgentEvent[]}>(`${path}/events?generationId=${encodeURIComponent(next.generation.generationId)}&latest=true&limit=250`,{headers:{"X-PUA-User":name}});
     if (!disposed) approvals=projectConversationEvents(page.events || []).filter(item => item.kind==="approval" && ids.includes(item.approvalId || ""));
    } else approvals=[];
   } catch(e) { if (!disposed) error=String(e); } finally { loading=false; }
  }
  void refresh(); const timer=setInterval(refresh,3000); return () => {disposed=true;clearInterval(timer);};
 });
 async function control(action:string,body?:unknown) {
  const generationId=status?.generation?.generationId; if (!generationId) return;
  await client.request(`${base}/${action}?generationId=${encodeURIComponent(generationId)}`,{method:"POST",headers:{"X-PUA-User":user,"Content-Type":"application/json"},body:body ? JSON.stringify(body):undefined});
 }
 async function stop() { pending=true; error=""; try {await control("turn/end");} catch(e) {error=String(e);} finally {pending=false;} }
</script>
<div class="chat-heading">
 {@render heading()}
 <div class="session-controls" aria-label="Execution controls">
  <span>Execution: {status?.sessionState || "Loading…"}</span>
  {#if status?.sessionState === "working" || status?.sessionState === "attention_required"}<button disabled={pending} onclick={stop}>Stop execution</button>{/if}
  {#if error || status?.configError || status?.lastError}<small role="status">{error || status?.configError || status?.lastError}</small>{/if}
 </div>
</div>
{#each approvals as item (item.approvalId)}<ApprovalCard {item} generationId={status?.generation?.generationId || ""} contextIdentity={base} onApproval={async (_generation,id,reply) => {await control("approval",{requestId:id,...reply}); approvals=approvals.filter(a => a.approvalId!==id);}} onToast={model.onToast} />{/each}
<style>.session-controls { display:flex; flex-wrap:wrap; align-items:center; gap:8px; font-size:.85em; } small { width:100%; opacity:.7; }</style>
