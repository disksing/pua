<script lang="ts">
 import { onMount, onDestroy } from "svelte";
 import { ApiClient } from "../api/client";
 import type { DetailPanelModel, ResourceAgentBindingModel } from "./models";
 import AgentBindingSelector from "./AgentBindingSelector.svelte";
 import ResourceEventTimeline from "./ResourceEventTimeline.svelte";
 let { model, dialogue = false, onOpenFile, onOpenLegacy }: { model: DetailPanelModel; dialogue?: boolean; onOpenFile: (path: string) => void; onOpenLegacy: (path: string) => void } = $props();
 let binding = $state<ResourceAgentBindingModel>({ kind: "profile", name: "default" });
 let pending = $state(false);
 let error = $state("");
 const client = new ApiClient();
 onDestroy(() => client.dispose());
 const base = $derived(`/api/workspaces/${encodeURIComponent(model.workspaceId)}/resources/${encodeURIComponent(model.resourceId)}/chat`);
 onMount(() => { if (dialogue) void client.latest<{ binding: ResourceAgentBindingModel }>(base, { scope: "binding", headers: { "X-PUA-User": model.currentUserName } }).then(c => binding = c.binding).catch(e => error = String(e)); });
 async function save(value: ResourceAgentBindingModel) {
  pending = true; error = "";
  try {
   if (dialogue) { await client.request(`${base}/binding`, { method:"POST", headers:{ "Content-Type":"application/json", "X-PUA-User":model.currentUserName }, body:JSON.stringify({binding:value}) }); binding = value; }
   else await model.onSaveAgentBinding(value);
  } catch(e) { error = String(e); } finally { pending = false; }
 }
</script>
<div class="channel-controls">
 <strong>{dialogue ? "Dialog events" : "Execution events"}</strong>
 <AgentBindingSelector value={dialogue ? binding : model.agentBinding} profiles={model.agentProfiles} agents={model.agents} disabled={pending || Boolean(model.detail?.archived)} openUp={false} ariaLabel={dialogue ? "Dialogue model" : "Execution model"} onSelect={save} />
 <small>Model changes take effect after the current Turn.</small>
 {#if error}<span role="alert">{error}</span>{/if}
</div>
<div class="channel-timeline">
 {#key `${model.workspaceId}:${model.resourceId}:${model.currentUserName}:${dialogue}`}
  <ResourceEventTimeline {model} {dialogue} />
 {/key}
</div>
<style>.channel-controls { display:flex; align-items:center; gap:12px; flex-wrap:wrap; padding:12px; } small { opacity:.65; } .channel-timeline { flex:1; min-height:0; overflow:auto; padding:0 20px 16px; overflow-anchor:none; }</style>
