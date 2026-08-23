<script lang="ts">
  import { onMount } from "svelte";
  import Icon from "../components/Icon.svelte";
  import { buildCreatePayload } from "./core/new-session";
  import type { AgentHubAgent, AgentHubProvider, AgentHubSession } from "./types";

  let { agents, providers, defaultCwd = "", submitting, error, onSubmit, onClose }: {
    agents: AgentHubAgent[];
    providers: AgentHubProvider[];
    defaultCwd?: string;
    submitting: boolean;
    error: string;
    onSubmit: (payload: Record<string, string>) => Promise<AgentHubSession | void>;
    onClose: () => void;
  } = $props();

  let title = $state("");
  let cwd = $state("");
  let agentName = $state("");
  let errors = $state<Record<string, string>>({});

  onMount(() => { cwd = defaultCwd; agentName = agents[0]?.name || ""; });

  function providerLabel(agent: AgentHubAgent): string {
    const provider = providers.find((item) => item.id === agent.providerId);
    return provider?.name || provider?.type || agent.providerId;
  }

  async function submit(): Promise<void> {
    const result = buildCreatePayload({ title, cwd, agentName, agents });
    errors = result.errors;
    if (!result.payload) return;
    await onSubmit(result.payload);
  }
</script>

<div class="modal-backdrop" role="presentation" onclick={(event) => { if (event.target === event.currentTarget && !submitting) onClose(); }} onkeydown={() => {}}>
  <div class="agenthub-dialog new-session-dialog" role="dialog" aria-modal="true" aria-labelledby="new-session-title">
    <header><div><span class="eyebrow">AgentHub</span><h2 id="new-session-title">Create Session</h2><p>Start an independent Provider Session in a local working directory.</p></div><button type="button" class="icon-button" aria-label="Close" disabled={submitting} onclick={onClose}><Icon name="x" /></button></header>
    <form onsubmit={(event) => { event.preventDefault(); void submit(); }}>
      <label><span>Working directory</span><input bind:value={cwd} aria-invalid={Boolean(errors.cwd)} placeholder="/absolute/path/to/project" />{#if errors.cwd}<small class="field-error">{errors.cwd}</small>{:else}<small>Absolute path of an existing local directory.</small>{/if}</label>
      <label><span>Agent</span><select bind:value={agentName} aria-invalid={Boolean(errors.agent)} disabled={!agents.length}>{#each agents as agent (agent.name)}<option value={agent.name}>{agent.name} · {providerLabel(agent)}</option>{/each}</select>{#if errors.agent}<small class="field-error">{errors.agent}</small>{:else if !agents.length}<small class="field-error">No available agents. Enable a Provider or add an Agent in Settings.</small>{/if}</label>
      <label><span>Title <em>optional</em></span><input bind:value={title} aria-invalid={Boolean(errors.title)} placeholder="New Session" />{#if errors.title}<small class="field-error">{errors.title}</small>{:else}<small>Leave empty to use AgentHub's default title.</small>{/if}</label>
      {#if error}<div class="dialog-error" role="alert">{error}</div>{/if}
      <footer><button type="button" class="secondary-button" disabled={submitting} onclick={onClose}>Cancel</button><button type="submit" class="primary-button" disabled={submitting || !agents.length}><Icon name="plus" /><span>{submitting ? "Creating…" : "Create Session"}</span></button></footer>
    </form>
  </div>
</div>
