<script lang="ts">
  import { onMount } from "svelte";

  import Icon from "../components/Icon.svelte";
  import { COMPLETION_SOUNDS, TonePlayer } from "./companion/audio";
  import { BEEP_PROGRESSIONS } from "./companion/chords";
  import { applyBalanceTotals, DEFAULT_BALANCE_TOTAL, quotaVisibilityKey } from "./companion/model";
  import { companionPreferencesEqual, loadCompanionPreferences, saveCompanionPreferences, validateCompanionPreferences } from "./companion/preferences";
  import { api } from "./core/api";
  import ModelSelect from "./ModelSelect.svelte";
  import { buildPayload, createDraft, isDirty, normalizeAgentOptions, providerOptionSchema, reorderAgents, uniqueAgentName, validateDraft } from "./settings/config-model";
  import { applyProviderUpdate, buildProviderRows, requestProviderCommand } from "./settings/provider-rows";

  let { onClose, onSaved }: { onClose: () => void; onSaved: () => void } = $props();
  let phase = $state<"loading" | "ready" | "error">("loading");
  let section = $state<"general" | "activity" | "providers" | "agents">("general");
  let draft = $state<any>(null);
  let snapshot = $state<any>(null);
  let activity = $state<any>(null);
  let activitySnapshot = $state<any>(null);
  let probes = $state<any[]>([]);
  let quota = $state<any>({ providers: [] });
  let error = $state("");
  let saving = $state(false);
  let pendingProvider = $state("");
  let saved = $state(false);
  let testing = $state(false);
  let testMessage = $state("");
  const player = new TonePlayer();

  const errors = $derived(draft && activity ? [...validateDraft(draft), ...validateCompanionPreferences(activity)] : []);
  const dirty = $derived(Boolean(draft && snapshot && (isDirty(draft, snapshot) || !companionPreferencesEqual(activity, activitySnapshot))));
  const effectiveQuota = $derived(activity ? applyBalanceTotals(quota, activity.balanceTotals) : quota);
  const quotaEntries = $derived((effectiveQuota?.providers || []).flatMap((provider: any) => (
    (provider.quotas || []).map((item: any) => ({ provider, item, key: quotaVisibilityKey(provider, item) }))
  )));
  const balanceProviders = $derived((quota?.providers || []).filter((provider: any) => (
    (provider.quotas || []).some((item: any) => item.kind === "balance")
  )));

  onMount(() => {
    void load();
    const key = (event: KeyboardEvent) => { if (event.key === "Escape") requestClose(); };
    document.addEventListener("keydown", key);
    return () => document.removeEventListener("keydown", key);
  });

  function clone<T>(value: T): T { return JSON.parse(JSON.stringify(value)); }
  function mutate(recipe: (next: any) => void): void { const next = clone(draft); recipe(next); draft = next; saved = false; }
  function mutateActivity(patch: Record<string, any>): void { activity = { ...activity, ...patch }; saved = false; }
  function fieldError(target: string, index = 0): string { return errors.find((item: any) => item.section === section && item.index === index && item.field === target)?.message || ""; }

  async function load(): Promise<void> {
    phase = "loading"; error = "";
    try {
      const configBody = await api<any>("/v1/config");
      draft = createDraft(configBody.config || {});
      snapshot = clone(draft);
      activity = loadCompanionPreferences();
      activitySnapshot = clone(activity);
      phase = "ready";
      void Promise.allSettled([api<any>("/v1/agents"), api<any>("/v1/quota")]).then(([agentsResult, quotaResult]) => {
        if (agentsResult.status === "fulfilled") probes = agentsResult.value.probes || [];
        if (quotaResult.status === "fulfilled") quota = quotaResult.value.quota || { providers: [] };
      });
    } catch (reason) {
      error = message(reason); phase = "error";
    }
  }

  function requestClose(): void {
    if (dirty && !window.confirm("You have unsaved changes. Close settings and discard them?")) return;
    onClose();
  }

  let commandEditing = $state("");
  let commandDraft = $state("");
  let commandError = $state("");

  function startCommandEdit(provider: any): void {
    commandEditing = provider.id;
    commandDraft = provider.command || "";
    commandError = "";
  }

  function cancelCommandEdit(): void {
    commandEditing = "";
    commandDraft = "";
    commandError = "";
  }

  async function confirmProviderCommand(provider: any): Promise<void> {
    if (pendingProvider) return;
    pendingProvider = provider.id; commandError = "";
    try {
      const updated = await requestProviderCommand(api, provider.id, commandDraft.trim());
      draft = applyProviderUpdate(draft, updated);
      snapshot = applyProviderUpdate(snapshot, updated);
      const body = await api<any>("/v1/agents");
      probes = body.probes || [];
      cancelCommandEdit();
      onSaved();
    } catch (reason) { commandError = message(reason); }
    finally { pendingProvider = ""; }
  }

  async function testOnWatch(): Promise<void> {
    testing = true; testMessage = "";
    try {
      const result = await api<any>("/v1/onwatch/test", { method: "POST", body: JSON.stringify({ onWatch: draft.onWatch }) });
      testMessage = `Connected · ${(result.providers || []).length} providers`;
    } catch (reason) { testMessage = message(reason); }
    finally { testing = false; }
  }

  async function save(force = false): Promise<void> {
    if (!dirty || saving) return;
    if (errors.length) { section = errors[0].section; error = errors[0].message; return; }
    saving = true; error = "";
    try {
      if (isDirty(draft, snapshot) && !force) {
        const current = await api<any>("/v1/config");
        if (JSON.stringify(createDraft(current.config)) !== JSON.stringify(createDraft(snapshot))) {
          if (!window.confirm("Configuration changed in another client. Overwrite it?")) return;
        }
      }
      if (!companionPreferencesEqual(activity, activitySnapshot)) {
        activity = saveCompanionPreferences(activity);
        activitySnapshot = clone(activity);
      }
      if (isDirty(draft, snapshot)) {
        const payload = buildPayload(draft);
        await api("/v1/config", { method: "PUT", body: JSON.stringify({ config: payload }) });
        draft = createDraft(payload); snapshot = clone(draft);
      }
      saved = true; onSaved();
    } catch (reason) { error = message(reason); }
    finally { saving = false; }
  }

  function addAgent(): void {
    mutate((next) => next.agents.push({ name: uniqueAgentName("Agent", next.agents.map((agent: any) => agent.name)), providerId: next.agentProviders[0]?.id || "" }));
  }

  function removeAgent(index: number): void { mutate((next) => next.agents.splice(index, 1)); }
  function moveAgent(index: number, delta: number): void { const target = index + delta; if (target < 0 || target >= draft.agents.length) return; mutate((next) => { next.agents = reorderAgents(next.agents, index, target); }); }
  function toggleQuota(key: string, visible: boolean): void {
    const hidden = new Set<string>(activity.hiddenQuotaKeys || []);
    if (visible) hidden.delete(key); else hidden.add(key);
    mutateActivity({ hiddenQuotaKeys: [...hidden].sort() });
  }
  function setBalanceTotal(providerId: string, raw: string): void {
    const totals = { ...(activity.balanceTotals || {}) };
    const numeric = Number(raw);
    if (Number.isFinite(numeric) && numeric > 0) totals[providerId] = numeric;
    else delete totals[providerId];
    mutateActivity({ balanceTotals: totals });
  }
  function balanceTotalFor(providerId: string): number {
    const numeric = Number(activity.balanceTotals?.[providerId]);
    return Number.isFinite(numeric) && numeric > 0 ? numeric : DEFAULT_BALANCE_TOTAL;
  }
  function changeProvider(index: number, providerId: string): void {
    mutate((next) => {
      const agent = next.agents[index]; agent.providerId = providerId;
      const provider = next.agentProviders.find((item: any) => item.id === providerId);
      agent.options = normalizeAgentOptions(provider?.type || "", agent.options || {});
    });
  }
  function changeOption(index: number, key: string, value: string): void { mutate((next) => { const options = { ...(next.agents[index].options || {}) }; if (value) options[key] = value; else delete options[key]; next.agents[index].options = options; }); }
  function envText(agent: any): string { return Object.entries(agent.environment || {}).map(([key, value]) => `${key}=${value}`).join("\n"); }
  function changeEnv(index: number, value: string): void { mutate((next) => { const environment: Record<string, string> = {}; value.split("\n").forEach((line) => { const at = line.indexOf("="); if (at > 0) environment[line.slice(0, at).trim()] = line.slice(at + 1); }); next.agents[index].environment = environment; }); }
  function providerRows(): any[] { return (buildProviderRows as any)(draft, probes); }
  function providerAvailable(providerId: string): boolean {
    const probe = probes.find((item: any) => item.providerId === providerId);
    return probe ? probe.available !== false : true;
  }
  function optionFields(agent: any): any[] { return (providerOptionSchema as any)(draft.agentProviders.find((item: any) => item.id === agent.providerId)?.type || ""); }
  function message(reason: unknown): string { return reason instanceof Error ? reason.message : String(reason); }
</script>

<div class="modal-backdrop" role="presentation" onclick={(event) => { if (event.target === event.currentTarget) requestClose(); }}>
  <div class="settings-dialog" role="dialog" aria-modal="true" aria-labelledby="settings-title">
    <header class="settings-header"><div><span class="eyebrow">AgentHub</span><h2 id="settings-title">Settings</h2><p>Configure OnWatch, activity feedback, providers, and agents.</p></div><button type="button" class="icon-button" aria-label="Close settings" onclick={requestClose}><Icon name="x" /></button></header>
    {#if phase === "loading"}<div class="settings-status"><span class="spinner"></span>Loading configuration…</div>
    {:else if phase === "error"}<div class="settings-status"><p role="alert">{error}</p><button type="button" class="secondary-button" onclick={load}>Retry</button></div>
    {:else}
      <div class="settings-layout">
        <nav class="settings-nav" aria-label="Settings sections">
          {#each [["general","General"],["activity","Activity"],["providers","Providers"],["agents","Agents"]] as item}
            <button type="button" class:active={section === item[0]} onclick={() => section = item[0] as typeof section}>{item[1]}{#if errors.filter((entry: any) => entry.section === item[0]).length}<span>{errors.filter((entry: any) => entry.section === item[0]).length}</span>{/if}</button>
          {/each}
        </nav>
        <div class="settings-main">
          <div class="settings-content">
            {#if section === "general"}
              <section class="settings-card"><header><div><h3>OnWatch Integration</h3><p>Connect the Companion to the existing local quota service.</p></div><label class="switch"><input type="checkbox" checked={draft.onWatch.enabled} onchange={(event) => mutate((next) => next.onWatch.enabled = event.currentTarget.checked)} /><span></span></label></header>
                <label><span>Server URL</span><input value={draft.onWatch.serverUrl} aria-invalid={Boolean(fieldError("serverUrl"))} oninput={(event) => mutate((next) => next.onWatch.serverUrl = event.currentTarget.value)} />{#if fieldError("serverUrl")}<small class="field-error">{fieldError("serverUrl")}</small>{/if}</label>
                <div class="settings-grid-two"><label><span>Authentication</span><select value={draft.onWatch.authMode} onchange={(event) => mutate((next) => next.onWatch.authMode = event.currentTarget.value)}><option value="trusted_proxy">Trusted proxy</option><option value="basic">Basic Auth</option><option value="none">None</option></select></label><label><span>Refresh</span><select value={draft.onWatch.refreshIntervalSeconds} onchange={(event) => mutate((next) => next.onWatch.refreshIntervalSeconds = Number(event.currentTarget.value))}><option value="30">30 seconds</option><option value="60">60 seconds</option><option value="300">5 minutes</option></select></label></div>
                {#if draft.onWatch.authMode !== "none"}<label><span>{draft.onWatch.authMode === "basic" ? "Username" : "Forwarded user"}</span><input value={draft.onWatch.username} oninput={(event) => mutate((next) => next.onWatch.username = event.currentTarget.value)} /></label>{/if}
                {#if draft.onWatch.authMode === "basic"}<label><span>Password</span><input type="password" value={draft.onWatch.password} autocomplete="new-password" oninput={(event) => mutate((next) => next.onWatch.password = event.currentTarget.value)} /><small>Leave blank to keep the stored password.</small></label>{/if}
                <div class="settings-inline"><button type="button" class="secondary-button" disabled={testing} onclick={testOnWatch}>{testing ? "Testing…" : "Test connection"}</button>{#if testMessage}<span>{testMessage}</span>{/if}</div>
              </section>
            {:else if section === "activity"}
              <section class="settings-stack">
                <section class="settings-card"><header><div><h3>Activity monitor</h3><p>Browser-local Companion and Beeper preferences.</p></div><label class="switch"><input type="checkbox" checked={activity.showActivity} onchange={(event) => mutateActivity({ showActivity: event.currentTarget.checked })} /><span></span></label></header>
                  <div class="setting-row"><span><strong>Enable beeping</strong><small>Play a quantized tone for active Sessions.</small></span><label class="switch"><input type="checkbox" checked={activity.enableBeeping} onchange={(event) => mutateActivity({ enableBeeping: event.currentTarget.checked })} /><span></span></label></div>
                  <label><span>Volume · {Math.round(activity.beepVolume * 100)}%</span><input type="range" min="0" max="1" step="0.01" value={activity.beepVolume} oninput={(event) => mutateActivity({ beepVolume: Number(event.currentTarget.value) })} /></label>
                  <label><span>Chord progression</span><select value={activity.beepProgression} onchange={(event) => mutateActivity({ beepProgression: event.currentTarget.value })}>{#each BEEP_PROGRESSIONS as option}<option value={option.value}>{option.label}</option>{/each}</select></label>
                  <label><span>On finish</span><div class="model-select-row"><select value={activity.completionSound} onchange={(event) => mutateActivity({ completionSound: event.currentTarget.value })}>{#each COMPLETION_SOUNDS as option}<option value={option.value}>{option.label}</option>{/each}</select><button type="button" class="icon-button" aria-label="Preview sound" onclick={async () => { await player.resume(); player.completion(activity.completionSound, activity.beepVolume); }}><Icon name="play" /></button></div></label>
                </section>
                <section class="settings-card"><header><div><h3>Quota visibility</h3><p>Choose which current quota rows appear in Beeper and its collapsed rotation.</p></div></header>
                  {#each quotaEntries as entry (entry.key)}
                    <div class="quota-pref"><span><strong>{entry.provider.label || entry.provider.provider} / {entry.item.label || entry.item.kind}</strong><small>{Math.round(Number(entry.item.remainingPercent) || 0)}% remaining</small></span><label class="switch"><input type="checkbox" checked={!new Set(activity.hiddenQuotaKeys || []).has(entry.key)} aria-label={`Show ${entry.provider.label || entry.provider.provider} / ${entry.item.label || entry.item.kind}`} onchange={(event) => toggleQuota(entry.key, event.currentTarget.checked)} /><span></span></label></div>
                  {:else}<div class="settings-empty">{quota?.error || "No current quota data."}</div>{/each}
                </section>
                {#if balanceProviders.length}
                  <section class="settings-card"><header><div><h3>Balance totals</h3><p>Set the denominator used to calculate the remaining percentage of balance quota entries.</p></div></header>
                    {#each balanceProviders as provider (provider.provider)}
                      {@const balance = (provider.quotas || []).find((item: any) => item.kind === "balance")}
                      {@const total = balanceTotalFor(provider.provider)}
                      <label><span>{provider.label || provider.provider} / {balance?.label || "Balance"}</span><input type="number" min="0" step="any" value={activity.balanceTotals?.[provider.provider] ?? DEFAULT_BALANCE_TOTAL} oninput={(event) => setBalanceTotal(provider.provider, event.currentTarget.value)} /><small>Current balance {balance?.value != null ? Number(balance.value).toFixed(2) : "—"} out of {total}. Clear to reset to {DEFAULT_BALANCE_TOTAL}.</small></label>
                    {/each}
                  </section>
                {/if}
              </section>
            {:else if section === "providers"}
              <section class="settings-stack"><header class="settings-section-heading"><h3>Providers</h3><p>Built-in integrations are enabled automatically when their executable is detected. Existing Sessions are not interrupted.</p></header>
                {#each providerRows() as provider (provider.id)}
                  {@const editing = commandEditing === provider.id || (!provider.available && !provider.command)}
                  <article class="settings-card provider-card">
                    <header>
                      <div><h3>{provider.name}</h3><p>{provider.description}</p><small class={provider.tone}>{provider.status}{provider.available && provider.path ? ` · ${provider.path}` : ""}</small>{#if !provider.available && provider.error}<small class="danger">{provider.error}</small>{/if}</div>
                    </header>
                    {#if editing}
                      <label><span>Executable path</span><input value={commandEditing === provider.id ? commandDraft : provider.command} placeholder={`Detect from PATH: ${provider.id}`} aria-label={`${provider.name} executable path`} oninput={(event) => { commandEditing = provider.id; commandDraft = event.currentTarget.value; commandError = ""; }} /><small>Leave blank to detect the executable automatically.</small></label>
                      {#if commandError && commandEditing === provider.id}<small class="field-error">{commandError}</small>{/if}
                      <div class="settings-inline">
                        <button type="button" class="secondary-button" disabled={Boolean(pendingProvider)} aria-label={`Confirm ${provider.name} executable path`} onclick={() => confirmProviderCommand(provider)}>Confirm</button>
                        {#if commandEditing === provider.id && (provider.available || provider.command)}
                          <button type="button" class="secondary-button" disabled={Boolean(pendingProvider)} onclick={cancelCommandEdit}>Cancel</button>
                        {/if}
                      </div>
                    {:else}
                      {#if !provider.available && provider.command}<p class="provider-path-invalid"><code>{provider.command}</code></p>{/if}
                      <div class="settings-inline"><button type="button" class="secondary-button" disabled={Boolean(pendingProvider)} aria-label={`Change ${provider.name} executable path`} onclick={() => startCommandEdit(provider)}>Change path</button></div>
                    {/if}
                  </article>
                {/each}
              </section>
            {:else}
              <section class="settings-stack"><header class="settings-section-heading action"><div><h3>Agents</h3><p>Named configurations used when a Session starts.</p></div><button type="button" class="secondary-button" onclick={addAgent}><Icon name="plus" />Add agent</button></header>
                {#each draft.agents as agent, index}<article class="settings-card agent-card"><header><strong>{agent.name || "Unnamed agent"}</strong><div><button type="button" class="icon-button" disabled={index === 0} aria-label="Move up" onclick={() => moveAgent(index, -1)}><Icon name="chevron-up" /></button><button type="button" class="icon-button" disabled={index === draft.agents.length - 1} aria-label="Move down" onclick={() => moveAgent(index, 1)}><Icon name="chevron-down" /></button><button type="button" class="icon-button danger" aria-label="Delete agent" onclick={() => removeAgent(index)}><Icon name="trash-2" /></button></div></header>
                  <div class="settings-grid-two"><label><span>Name</span><input value={agent.name} aria-invalid={Boolean(fieldError("name", index))} oninput={(event) => mutate((next) => next.agents[index].name = event.currentTarget.value)} />{#if fieldError("name", index)}<small class="field-error">{fieldError("name", index)}</small>{/if}</label><label><span>Provider</span><select value={agent.providerId} onchange={(event) => changeProvider(index, event.currentTarget.value)}>{#each draft.agentProviders as provider}<option value={provider.id}>{provider.name || provider.id}</option>{/each}</select></label></div>
                  {#each optionFields(agent) as field}<label><span>{field.label}</span>{#if field.kind === "model"}<ModelSelect providerId={agent.providerId} enabled={providerAvailable(agent.providerId)} value={agent.options?.[field.key] || ""} onChange={(value) => changeOption(index, field.key, value)} />{:else}<select value={agent.options?.[field.key] || field.fallback || ""} onchange={(event) => changeOption(index, field.key, event.currentTarget.value)}>{#each field.options as option}<option value={option.value}>{option.label}</option>{/each}</select>{/if}</label>{/each}
                  <label><span>Environment variables</span><textarea rows="3" value={envText(agent)} placeholder="NAME=value" oninput={(event) => changeEnv(index, event.currentTarget.value)}></textarea><small>One NAME=value entry per line.</small></label>
                </article>{:else}<div class="settings-empty">No Agents configured.</div>{/each}
              </section>
            {/if}
          </div>
          <footer class="settings-savebar">{#if error}<p role="alert">{error}</p>{/if}<span>{saved ? "Saved" : dirty ? "Unsaved changes" : "Up to date"}</span><div><button type="button" class="secondary-button" disabled={saving} onclick={requestClose}>Cancel</button><button type="button" class="primary-button" disabled={!dirty || saving || errors.length > 0} onclick={() => save()}>{saving ? "Saving…" : "Save all"}</button></div></footer>
        </div>
      </div>
    {/if}
  </div>
</div>
