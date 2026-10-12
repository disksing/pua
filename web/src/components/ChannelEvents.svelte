<script lang="ts">
 import { onMount } from "svelte";
 import { ApiClient } from "../api/client";
 import type { DetailPanelModel, ResourceAgentBindingModel } from "./models";
 import AgentBindingSelector from "./AgentBindingSelector.svelte";
 import HistoryTimeline from "./HistoryTimeline.svelte";
 let { model, dialogue = false, onOpenFile, onOpenLegacy }: { model: DetailPanelModel; dialogue?: boolean; onOpenFile: (path: string) => void; onOpenLegacy: (path: string) => void } = $props();
 let binding = $state<ResourceAgentBindingModel>({ kind: "profile", name: "default" });
 let pending = $state(false);
 let error = $state("");
 const client = new ApiClient();
 const base = $derived(`/api/workspaces/${encodeURIComponent(model.workspaceId)}/resources/${encodeURIComponent(model.resourceId)}/chat`);
 onMount(() => { if (dialogue) void client.request<{ binding: ResourceAgentBindingModel }>(base, { headers: { "X-PUA-User": model.currentUserName } }).then(c => binding = c.binding).catch(e => error = String(e)); });
 async function save(value: ResourceAgentBindingModel) {
  pending = true; error = "";
  try {
   if (dialogue) { await client.request(`${base}/binding`, { method:"POST", headers:{ "Content-Type":"application/json", "X-PUA-User":model.currentUserName }, body:JSON.stringify({binding:value}) }); binding = value; }
   else await model.onSaveAgentBinding(value);
  } catch(e) { error = String(e); } finally { pending = false; }
 }
</script>
<div class="channel-controls">
 <strong>{dialogue ? "Dialogue events" : "Execution events"}</strong>
 <AgentBindingSelector value={dialogue ? binding : model.agentBinding} profiles={model.agentProfiles} agents={model.agents} disabled={pending || Boolean(model.detail?.archived)} openUp={false} ariaLabel={dialogue ? "Dialogue model" : "Execution model"} onSelect={save} />
 <small>Model changes take effect after the current Turn.</small>
 {#if error}<span role="alert">{error}</span>{/if}
</div>
<HistoryTimeline workspaceId={model.workspaceId} resourceId={model.resourceId} userName={model.currentUserName} {dialogue} artifacts={model.detail?.artifacts || []} resolveResourceTitle={model.resolveResourceTitle} onNavigate={model.onNavigate} {onOpenFile} {onOpenLegacy} />
<style>.channel-controls { display:flex; align-items:center; gap:12px; flex-wrap:wrap; padding:12px; } small { opacity:.65; }</style>
