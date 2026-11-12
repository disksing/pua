<script lang="ts">
  import Icon from "../components/Icon.svelte";
  import { api } from "./core/api";

  let { providerId, enabled, value, id = "", onChange }: { providerId: string; enabled: boolean; value: string; id?: string; onChange: (value: string) => void } = $props();
  let models = $state<Array<{ id?: string; name?: string; model?: string }>>([]);
  let status = $state<"idle" | "loading" | "ready" | "error">("idle");
  let error = $state("");

  $effect(() => {
    providerId;
    enabled;
    void load();
  });

  async function load(): Promise<void> {
    if (!providerId || !enabled) { models = []; status = "idle"; return; }
    status = "loading";
    error = "";
    try {
      const body = await api<{ models: Array<{ id?: string; name?: string; model?: string }> }>(`/v1/providers/${encodeURIComponent(providerId)}/models`);
      models = body.models || [];
      status = "ready";
    } catch (reason) {
      status = "error";
      error = reason instanceof Error ? reason.message : String(reason);
    }
  }

  function modelValue(model: { id?: string; name?: string; model?: string }): string {
    return model.id || model.model || model.name || "";
  }
</script>

<div class="model-select-row">
  <select {id} {value} disabled={!enabled || status === "loading"} onchange={(event) => onChange(event.currentTarget.value)}>
    <option value="">Provider default</option>
    {#if value && !models.some((model) => modelValue(model) === value)}<option value={value}>{value} · saved</option>{/if}
    {#each models as model (modelValue(model))}<option value={modelValue(model)}>{model.name || modelValue(model)}</option>{/each}
  </select>
  {#if status === "error"}<button type="button" class="icon-button" title={error} aria-label="Retry model list" onclick={load}><Icon name="refresh-cw" /></button>{/if}
</div>
