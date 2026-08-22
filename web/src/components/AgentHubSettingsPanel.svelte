<script lang="ts">
  import "./AgentHubSettingsPanel.css";

  import Icon from "./Icon.svelte";
  import {
    cloneAgentHubProvider,
    normalizeAgentOptions,
    providerOptionFields,
    summarizeAgentOptions,
    uniqueAgentName,
    validateAgentHubAgents,
  } from "./agenthub-config";
  import type { AgentHubConfigAgent, AgentHubConfigProvider, SettingsDraft, SettingsModel } from "./models";
  import { cloneSettingsDraft, settingsErrorMessage } from "./settings-draft";

  let {
    agentHub,
    draft = $bindable(),
    pending = $bindable(),
    onDirty,
    onSaveAgentHub,
    onToggleProvider,
    onToast,
  }: {
    agentHub: SettingsModel["agentHub"];
    draft: SettingsDraft;
    pending: string;
    onDirty: () => void;
    onSaveAgentHub: SettingsModel["onSaveAgentHub"];
    onToggleProvider: SettingsModel["onToggleProvider"];
    onToast: SettingsModel["onToast"];
  } = $props();

  let rowIdCounter = 0;
  let rowIds = $state<number[]>(draft.agentConfigs.map(() => rowIdCounter++));
  let expanded = $state<Set<number>>(new Set());
  // dragIndex/dropIndex track an in-flight reorder gesture: dragIndex is the
  // card being dragged and dropIndex the card currently highlighted as the
  // drop target (dropIndex is used only for visual feedback).
  let dragIndex = $state<number | null>(null);
  let dropIndex = $state<number | null>(null);

  $effect(() => {
    if (rowIds.length !== draft.agentConfigs.length) {
      rowIds = draft.agentConfigs.map(() => rowIdCounter++);
      expanded = new Set();
      dragIndex = null;
      dropIndex = null;
    }
  });

  const validationErrors = $derived.by(() => validateAgentHubAgents(draft.agentConfigs, draft.agentProviders));

  function errorFor(index: number, field: string): string {
    return validationErrors.find((error) => error.index === index && error.field === field)?.message || "";
  }

  function providerByID(id: string): AgentHubConfigProvider | undefined {
    return draft.agentProviders.find((provider) => provider.id === id);
  }

  function probeFor(id: string) {
    return agentHub.probes.find((probe) => probe.providerId === id);
  }

  function toggleAgent(index: number): void {
    const id = rowIds[index];
    const next = new Set(expanded);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    expanded = next;
  }

  function updateAgent(index: number, patch: Partial<AgentHubConfigAgent>): void {
    draft.agentConfigs[index] = { ...draft.agentConfigs[index], ...patch };
    onDirty();
  }

  function updateOption(index: number, key: string, value: string): void {
    const agent = draft.agentConfigs[index];
    const options = { ...(agent.options || {}) };
    if (value.trim()) options[key] = value.trim();
    else delete options[key];
    updateAgent(index, { options: normalizeAgentOptions(providerByID(agent.providerId)?.type || "", options) });
  }

  function changeProvider(index: number, providerId: string): void {
    const agent = draft.agentConfigs[index];
    updateAgent(index, {
      providerId,
      options: normalizeAgentOptions(providerByID(providerId)?.type || "", agent.options),
    });
  }

  function updateEnvironment(index: number, environment: Record<string, string>): void {
    updateAgent(index, { environment: Object.keys(environment).length ? environment : undefined });
  }

  function changeEnvironmentKey(index: number, oldKey: string, newKey: string): void {
    const environment: Record<string, string> = {};
    for (const [key, value] of Object.entries(draft.agentConfigs[index].environment || {})) {
      environment[key === oldKey ? newKey : key] = value;
    }
    updateEnvironment(index, environment);
  }

  function changeEnvironmentValue(index: number, key: string, value: string): void {
    const environment = { ...(draft.agentConfigs[index].environment || {}) };
    environment[key] = value;
    updateEnvironment(index, environment);
  }

  function addEnvironment(index: number): void {
    const environment = { ...(draft.agentConfigs[index].environment || {}) };
    let key = "VARIABLE";
    let suffix = 2;
    while (Object.prototype.hasOwnProperty.call(environment, key)) key = `VARIABLE_${suffix++}`;
    environment[key] = "";
    updateEnvironment(index, environment);
  }

  function removeEnvironment(index: number, key: string): void {
    const environment = { ...(draft.agentConfigs[index].environment || {}) };
    delete environment[key];
    updateEnvironment(index, environment);
  }

  function moveAgent(from: number, to: number): void {
    if (to < 0 || to >= draft.agentConfigs.length || from === to) return;
    const agents = [...draft.agentConfigs];
    const ids = [...rowIds];
    agents.splice(to, 0, agents.splice(from, 1)[0]);
    ids.splice(to, 0, ids.splice(from, 1)[0]);
    draft.agentConfigs = agents;
    rowIds = ids;
    onDirty();
  }

  function removeAgent(index: number): void {
    const removedID = rowIds[index];
    draft.agentConfigs.splice(index, 1);
    rowIds.splice(index, 1);
    const nextExpanded = new Set(expanded);
    nextExpanded.delete(removedID);
    expanded = nextExpanded;
    onDirty();
  }

  function addAgent(): void {
    if (!draft.agentProviders.length) {
      onToast("Add a provider before configuring an agent.");
      return;
    }
    const provider = draft.agentProviders[0];
    const agent: AgentHubConfigAgent = {
      name: uniqueAgentName("Agent", draft.agentConfigs.map((item) => item.name)),
      providerId: provider.id,
    };
    draft.agentConfigs = [...draft.agentConfigs, agent];
    rowIds = [...rowIds, rowIdCounter++];
    expanded = new Set(expanded).add(rowIds[rowIds.length - 1]);
    onDirty();
  }

  async function toggleProvider(provider: AgentHubConfigProvider): Promise<void> {
    if (pending) return;
    pending = `provider:${provider.id}`;
    try {
      const updated = await onToggleProvider(provider.id, !provider.enabled);
      const index = draft.agentProviders.findIndex((item) => item.id === updated.id);
      if (index >= 0) draft.agentProviders[index] = cloneAgentHubProvider(updated);
      else draft.agentProviders = [...draft.agentProviders, cloneAgentHubProvider(updated)];
    } catch (error) {
      onToast(settingsErrorMessage(error));
    } finally {
      pending = "";
    }
  }

  async function saveAgentHub(): Promise<void> {
    if (!draft.dirty || pending) return;
    if (validationErrors.length) {
      const first = validationErrors[0];
      onToast(first.message);
      expanded = new Set(expanded).add(rowIds[first.index]);
      return;
    }
    pending = "agenthub";
    try {
      await onSaveAgentHub(cloneSettingsDraft(draft));
      draft.dirty = false;
    } catch (error) {
      onToast(settingsErrorMessage(error));
    } finally {
      pending = "";
    }
  }
</script>

<div class="settings-panel settings-agent-panel" data-component-owner="agenthub-settings-panel" data-settings-panel data-settings-section="agenthub">
  <div class="settings-panel-header"><h2>AgentHub</h2><p>Manage the AgentHub connection, provider switches, and the agents used by PUA. Provider switches save immediately; agent changes are saved together below.</p></div>

  <section class="settings-agent-section settings-connection-section">
    <div class="settings-section-heading"><h3>Connection</h3><span class="settings-pill" class:pill-warning={!agentHub.connected || !agentHub.compatible}>{agentHub.connected && agentHub.compatible ? "Compatible" : agentHub.connected ? "Incompatible" : "Unavailable"}</span></div>
    {#if agentHub.mode === "external"}
      <label class="settings-field-label"><span>External AgentHub endpoint</span><input id="settingsAgentHubEndpoint" bind:value={draft.endpoint} oninput={onDirty} /></label>
      <small>External mode is active. The address is used when the PUA server starts; restart the service after changing it.</small>
    {:else}
      <small class="settings-embedded-note">Embedded AgentHub is managed by PUA. The AgentHub address setting is not applicable in this mode.</small>
    {/if}
    {#if agentHub.error}<small>{agentHub.error}</small>{/if}
  </section>

  <section class="settings-agent-section">
    <div class="settings-section-heading"><h3>Providers</h3><span>{draft.agentProviders.length} providers · switches save immediately</span></div>
    <div class="settings-provider-list">
      {#each draft.agentProviders as provider (provider.id)}
        {@const probe = probeFor(provider.id)}
        {@const available = probe ? probe.available !== false : provider.enabled}
        <div class="settings-service-row" class:settings-service-disabled={!provider.enabled}>
          <div class="settings-provider-main"><span class="settings-agent-mark">{(provider.name || provider.id || "P").slice(0, 1).toUpperCase()}</span><span><strong>{provider.name || provider.id}</strong><small>{provider.type || provider.id} · {provider.enabled ? available ? "Available" : "Unavailable" : "Disabled"}</small></span></div>
          <button type="button" class="settings-switch" role="switch" aria-checked={provider.enabled} aria-label={`${provider.enabled ? "Disable" : "Enable"} ${provider.name || provider.id}`} disabled={Boolean(pending)} onclick={() => toggleProvider(provider)}><span></span><strong>{provider.enabled ? "On" : "Off"}</strong></button>
        </div>
      {:else}
        <div class="settings-empty">No AgentHub providers are configured.</div>
      {/each}
    </div>
  </section>

  <section class="settings-agent-section">
    <div class="settings-section-heading"><h3>Agents</h3><span>{draft.agentConfigs.length} agents</span></div>
    <p class="settings-section-description">Agents are named configurations built on a provider. Names must be unique, and provider-specific options are kept with each agent.</p>
    <div class="settings-agent-list">
      {#each draft.agentConfigs as agent, index (rowIds[index])}
        {@const open = expanded.has(rowIds[index])}
        {@const provider = providerByID(agent.providerId)}
        {@const agentNameError = errorFor(index, "name")}
        {@const providerError = errorFor(index, "providerId")}
        <article
          class="settings-agent-card"
          class:settings-agent-card-open={open}
          class:dragging={dragIndex === index}
          class:drop-target={dropIndex === index}
          ondragover={(event) => {
            if (dragIndex === null || dragIndex === index) return;
            event.preventDefault();
            if (event.dataTransfer) event.dataTransfer.dropEffect = "move";
            if (dropIndex !== index) dropIndex = index;
          }}
          ondrop={(event) => {
            event.preventDefault();
            if (dragIndex === null || dragIndex === index) return;
            moveAgent(dragIndex, index);
            dragIndex = null;
            dropIndex = null;
          }}
        >
          <div class="settings-agent-card-head">
            <button
              type="button"
              class="settings-drag-handle"
              aria-label={`Reorder agent ${agent.name || "Unnamed agent"}`}
              title="Drag to reorder (or focus and use Arrow keys)"
              draggable="true"
              ondragstart={(event) => {
                dragIndex = index;
                if (event.dataTransfer) {
                  event.dataTransfer.effectAllowed = "move";
                  event.dataTransfer.setData("text/plain", String(index));
                }
              }}
              ondragend={() => { dragIndex = null; dropIndex = null; }}
              onkeydown={(event) => {
                if (event.key === "ArrowUp" && index > 0) {
                  event.preventDefault();
                  moveAgent(index, index - 1);
                } else if (event.key === "ArrowDown" && index < draft.agentConfigs.length - 1) {
                  event.preventDefault();
                  moveAgent(index, index + 1);
                }
              }}
            ><Icon name="grip-vertical" /></button>
            <button type="button" class="settings-agent-card-toggle" aria-expanded={open} aria-controls={`settings-agent-${index}-body`} onclick={() => toggleAgent(index)}><span class="settings-card-caret" class:open><Icon name="chevron-right" /></span><span class="settings-agent-card-title"><strong>{agent.name || "Unnamed agent"}</strong><small>{provider?.name || agent.providerId || "No provider"}{summarizeAgentOptions(agent.options).length ? ` · ${summarizeAgentOptions(agent.options).join(" · ")}` : ""}</small></span></button>
            <button type="button" class="settings-delete-button" title="Delete Agent" aria-label={`Delete ${agent.name || "agent"}`} onclick={() => removeAgent(index)}><Icon name="trash" /></button>
          </div>
          {#if open}
            <div id={`settings-agent-${index}-body`} class="settings-agent-card-body">
              <label class="settings-field-label"><span>Name</span><input aria-label="Agent name" value={agent.name} aria-invalid={Boolean(agentNameError)} oninput={(event) => updateAgent(index, { name: event.currentTarget.value })} />{#if agentNameError}<small class="settings-field-error">{agentNameError}</small>{/if}</label>
              <label class="settings-field-label"><span>Provider</span><select aria-label="AgentHub Provider" value={agent.providerId} aria-invalid={Boolean(providerError)} onchange={(event) => changeProvider(index, event.currentTarget.value)}><option value="">Choose a provider</option>{#each draft.agentProviders as option (option.id)}<option value={option.id}>{option.name || option.id}{option.enabled ? "" : " (Disabled)"}</option>{/each}</select>{#if providerError}<small class="settings-field-error">{providerError}</small>{/if}</label>
              {#if provider}
                <div class="settings-agent-subsection"><div class="settings-subsection-heading"><strong>Provider options</strong><span>{provider.type}</span></div>{#each providerOptionFields(provider.type) as field (field.key)}<label class="settings-field-label"><span>{field.label}</span>{#if field.kind === "select"}<select aria-label={field.label} value={agent.options?.[field.key] || ""} onchange={(event) => updateOption(index, field.key, event.currentTarget.value)}><option value="">Default</option>{#each field.options || [] as option}<option value={option}>{option}</option>{/each}</select>{:else}<input aria-label={field.label} value={agent.options?.[field.key] || ""} oninput={(event) => updateOption(index, field.key, event.currentTarget.value)} />{/if}</label>{/each}</div>
              {/if}
              <div class="settings-agent-subsection"><div class="settings-subsection-heading"><strong>Environment</strong><button type="button" class="settings-inline-button" onclick={() => addEnvironment(index)}>+ Add variable</button></div>{#each Object.entries(agent.environment || {}) as [key, value] (key)}<div class="settings-environment-row"><input aria-label="Environment variable name" value={key} oninput={(event) => changeEnvironmentKey(index, key, event.currentTarget.value)} /><input aria-label="Environment variable value" value={value} oninput={(event) => changeEnvironmentValue(index, key, event.currentTarget.value)} /><button type="button" class="settings-delete-button" title="Remove variable" aria-label={`Remove ${key || "environment variable"}`} onclick={() => removeEnvironment(index, key)}><Icon name="trash" /></button></div>{/each}{#if errorFor(index, "environment")}<small class="settings-field-error">{errorFor(index, "environment")}</small>{/if}</div>
            </div>
          {/if}
        </article>
      {:else}
        <div class="settings-empty">No agents configured. Add one to make it available to PUA workflows.</div>
      {/each}
    </div>
    <button id="settingsAddAgent" type="button" class="settings-add-agent" disabled={!draft.agentProviders.length} onclick={addAgent}><Icon name="plus" /><span>Add agent</span></button>
  </section>

  <div class="settings-form-actions settings-save-bar"><span class:visible={draft.dirty} class="settings-save-hint">{draft.dirty ? "Unsaved changes" : ""}</span><button id="settingsSaveButton" type="button" class="primary-button" disabled={!draft.dirty || Boolean(pending)} onclick={saveAgentHub}><Icon name="save" /><span>Save All</span></button></div>
</div>
