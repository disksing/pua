<script lang="ts">
 import type { DetailPanelModel, ResourceAgentBindingModel } from "./models";
 import AgentBindingSelector from "./AgentBindingSelector.svelte";
 import ResourceEventTimeline from "./ResourceEventTimeline.svelte";
 let { model, dialogue = false, onOpenFile, onOpenLegacy }: { model: DetailPanelModel; dialogue?: boolean; onOpenFile: (path: string) => void; onOpenLegacy: (path: string) => void } = $props();
 let pending = $state(false);
 let error = $state("");
 async function save(value: ResourceAgentBindingModel) {
  pending = true; error = "";
  try {
   await model.onSaveAgentBinding(value);
  } catch(e) { error = String(e); } finally { pending = false; }
 }
</script>
<div class="channel-controls">
 <strong>{dialogue ? "Dialog events" : "Execution events"}</strong>
 {#if !dialogue}
 <AgentBindingSelector value={model.agentBinding} profiles={model.agentProfiles} agents={model.agents} disabled={pending || Boolean(model.detail?.archived)} openUp={false} ariaLabel="Execution model" onSelect={save} />
 <small>Model changes take effect after the current Turn.</small>
 {/if}
 {#if error}<span role="alert">{error}</span>{/if}
</div>
<div class="channel-timeline">
 {#key `${model.workspaceId}:${model.resourceId}:${model.currentUserName}:${dialogue}`}
  <ResourceEventTimeline {model} {dialogue} />
 {/key}
</div>
<style>.channel-controls { display:flex; align-items:center; gap:12px; flex-wrap:wrap; padding:12px; } small { opacity:.65; } .channel-timeline { flex:1; min-height:0; overflow:auto; padding:0 20px 16px; overflow-anchor:none; }</style>
