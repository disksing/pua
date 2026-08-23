<script lang="ts">
  import { onMount } from "svelte";

  import Icon from "../components/Icon.svelte";
  import ActivityWaveform from "./ActivityWaveform.svelte";
  import { activityPlaybackPlan, COMPLETION_SOUNDS, TonePlayer } from "./companion/audio";
  import { DEFAULT_BEEP_CHORD, nextProgressionFrame, noteForToneSlot } from "./companion/chords";
  import { activityPulsesForFrame, activitySessionHoldsTone, activitySessionNeedsTone, activitySessions, activitySessionTerminal, applyBalanceTotals, filterQuotaSnapshot, formatDuration, pruneActivityPulses, quotaCycleItems, SessionToneAllocator } from "./companion/model";
  import { loadCompanionPreferences, saveCompanionPreferences, subscribeCompanionPreferences } from "./companion/preferences";
  import { agentHubPath, api } from "./core/api";

  let { standalone = false, pauseLiveUpdates = false, revision = 0, onOpenSettings }: { standalone?: boolean; pauseLiveUpdates?: boolean; revision?: number; onOpenSettings: () => void } = $props();
  let open = $state(false);
  let preferences = $state<any>(loadCompanionPreferences());
  let config = $state<any>({ onWatch: {} });
  let quota = $state<any>({ configured: false, connected: false, providers: [] });
  let quotaLoading = $state(true);
  let quotaIndex = $state(0);
  let activityState = $state<"paused" | "connecting" | "live">("connecting");
  let activityChord = $state(DEFAULT_BEEP_CHORD);
  let sessions = $state<Map<string, any>>(new Map());
  let pulses = $state<any[]>([]);
  let audioBlocked = $state(false);
  let controlError = $state("");
  let quotaScroll: HTMLDivElement | undefined = $state();
  let quotaGrid: HTMLDivElement | undefined = $state();
  let quotaDense = $state(false);
  let sequence = 0;
  let progressionFrame: any = null;
  const player = new TonePlayer();
  const allocator = new SessionToneAllocator();
  const expanded = $derived(standalone || open);
  const visibleQuota = $derived(filterQuotaSnapshot(applyBalanceTotals(quota, preferences.balanceTotals), preferences.hiddenQuotaKeys));
  const cycleItems = $derived(quotaCycleItems(visibleQuota));
  const cycleItem = $derived(cycleItems[quotaIndex % Math.max(1, cycleItems.length)]);
  const activeList = $derived([...sessions.values()].sort((left, right) => left.sessionId.localeCompare(right.sessionId)));

  function measureQuotaLayout(): void {
    if (!quotaScroll || !quotaGrid) return;
    const card = quotaGrid.closest<HTMLElement>(".companion-card");
    const providerCount = quotaGrid.children.length;
    const wideEnough = (card?.clientWidth || 0) >= 680;
    let singleHeight = quotaGrid.getBoundingClientRect().height;
    if (quotaDense) {
      const probe = quotaGrid.cloneNode(true) as HTMLDivElement;
      probe.classList.remove("dense");
      probe.setAttribute("aria-hidden", "true");
      probe.style.cssText = `position:absolute;visibility:hidden;pointer-events:none;width:${quotaGrid.clientWidth}px`;
      quotaGrid.parentElement?.append(probe);
      singleHeight = probe.getBoundingClientRect().height;
      probe.remove();
    }
    const currentHeight = quotaGrid.getBoundingClientRect().height;
    const content = quotaGrid.parentElement;
    const contentTop = content?.getBoundingClientRect().top || 0;
    const contentBottom = Math.max(contentTop, ...[...(content?.children || [])].map((element) => element.getBoundingClientRect().bottom));
    const paddingBottom = Number.parseFloat(content ? getComputedStyle(content).paddingBottom : "0") || 0;
    const projectedSingleContentHeight = contentBottom - contentTop + paddingBottom - currentHeight + singleHeight;
    quotaDense = providerCount > 1 && wideEnough && projectedSingleContentHeight > quotaScroll.clientHeight + 1;
  }

  $effect(() => {
    expanded;
    activeList.length;
    quotaLoading;
    (visibleQuota.providers || []).map((provider: any) => `${provider.provider}:${provider.quotas?.length || 0}`).join("|");
    const frame = window.requestAnimationFrame(measureQuotaLayout);
    return () => window.cancelAnimationFrame(frame);
  });

  $effect(() => {
    if (!quotaScroll || !quotaGrid) return;
    const resize = new ResizeObserver(measureQuotaLayout);
    resize.observe(quotaScroll);
    resize.observe(quotaGrid);
    measureQuotaLayout();
    return () => resize.disconnect();
  });

  onMount(() => {
    const unsubscribe = subscribeCompanionPreferences((next: any) => preferences = next);
    const housekeeping = window.setInterval(() => {
      const now = Date.now();
      const nextSessions = (activitySessions as any)(sessions, { sessions: [] }, now);
      const nextPulses = (pruneActivityPulses as any)(pulses, now);
      if (nextSessions !== sessions) sessions = nextSessions;
      if (nextPulses !== pulses) pulses = nextPulses;
      allocator.retain([...sessions].filter(([, session]) => activitySessionHoldsTone(session, now)).map(([id]) => id));
    }, 1000);
    const rotation = window.setInterval(() => { if (cycleItems.length > 1) quotaIndex = (quotaIndex + 1) % cycleItems.length; }, 3000);
    const unlock = () => { if (preferences.enableBeeping) void player.resume().then((running: boolean) => audioBlocked = !running); };
    window.addEventListener("pointerdown", unlock, { once: true });
    window.addEventListener("keydown", unlock, { once: true });
    return () => { unsubscribe(); window.clearInterval(housekeeping); window.clearInterval(rotation); window.removeEventListener("pointerdown", unlock); window.removeEventListener("keydown", unlock); };
  });

  $effect(() => {
    revision;
    if (pauseLiveUpdates) return;
    const controller = new AbortController();
    void api<any>("/v1/config", { signal: controller.signal }).then((body) => config = body.config || { onWatch: {} }).catch(() => {});
    return () => controller.abort();
  });

  $effect(() => {
    revision; config.onWatch?.enabled; config.onWatch?.refreshIntervalSeconds;
    if (pauseLiveUpdates) { quotaLoading = false; return; }
    let disposed = false;
    const load = async () => {
      quotaLoading = true;
      try { const body = await api<any>("/v1/quota"); if (!disposed) quota = body.quota || { configured: false, connected: false, providers: [] }; }
      catch (reason) { if (!disposed) quota = { ...quota, connected: false, stale: true, error: message(reason) }; }
      finally { if (!disposed) quotaLoading = false; }
    };
    void load();
    const timer = window.setInterval(load, Math.max(30, Number(config.onWatch?.refreshIntervalSeconds) || 60) * 1000);
    return () => { disposed = true; window.clearInterval(timer); };
  });

  $effect(() => {
    preferences.showActivity; preferences.enableBeeping; preferences.beepVolume; preferences.beepProgression; preferences.completionSound; pauseLiveUpdates;
    if (pauseLiveUpdates || !preferences.showActivity) { activityState = "paused"; return; }
    const source = new EventSource(agentHubPath("/v1/activity/events"));
    let disposed = false;
    activityState = "connecting"; progressionFrame = null;
    source.onopen = () => { if (!disposed) activityState = "live"; };
    source.onerror = () => { if (!disposed) activityState = "connecting"; };
    source.onmessage = (event) => {
      if (disposed) return;
      try {
        const frame = JSON.parse(event.data);
        const now = Date.now();
        if (sequence && frame.sequence !== sequence + 1) { sessions = new Map(); pulses = []; allocator.retain([]); progressionFrame = null; }
        sequence = frame.sequence;
        progressionFrame = nextProgressionFrame(progressionFrame, preferences.beepProgression, frame.sequence);
        activityChord = progressionFrame.chord;
        const incoming = [...(frame.sessions || [])].sort((a, b) => String(a.sessionId).localeCompare(String(b.sessionId))).map((session) => {
          const previous = sessions.get(session.sessionId);
          return { ...session, toneSlot: activitySessionNeedsTone(previous, session) ? allocator.assign(session.sessionId) : previous?.toneSlot };
        }).sort((a, b) => a.toneSlot - b.toneSlot || a.sessionId.localeCompare(b.sessionId));
        const assigned = { ...frame, sessions: incoming };
        sessions = (activitySessions as any)(sessions, assigned, now);
        pulses = (pruneActivityPulses as any)([...pulses, ...(activityPulsesForFrame as any)(assigned, now)], now);
        if (preferences.enableBeeping) (activityPlaybackPlan as any)(incoming, frame.sequence).forEach(({ item, delay, gain }: any) => {
          if (activitySessionTerminal(item)) player.completion(preferences.completionSound, preferences.beepVolume);
          else player.pulse(item.toneSlot, activityChord, preferences.beepVolume * gain, delay);
        });
        audioBlocked = preferences.enableBeeping && player.status() !== "running";
      } catch { activityState = "connecting"; }
    };
    return () => { disposed = true; source.close(); };
  });

  function savePreference(patch: Record<string, any>): void {
    try { preferences = saveCompanionPreferences({ ...preferences, ...patch }); controlError = ""; }
    catch (reason) { controlError = message(reason); }
  }
  async function toggleBeeping(): Promise<void> { const enabled = !preferences.enableBeeping; if (enabled) audioBlocked = !(await player.resume()); savePreference({ enableBeeping: enabled }); }
  async function preview(): Promise<void> { if (await player.resume()) { player.completion(preferences.completionSound, preferences.beepVolume); audioBlocked = false; } }
  function terminalClass(session: any): string { const terminal = activitySessionTerminal(session); if (!terminal) return ""; return terminal.status === "completed" ? "terminal-completed" : "terminal-error"; }
  function statusTone(status: string): string { return status === "critical" || status === "danger" ? "danger" : status === "warning" ? "warning" : "healthy"; }
  function updatedAgo(value: string): string { const seconds = Math.max(0, Math.floor((Date.now() - Date.parse(value || "")) / 1000)); if (!Number.isFinite(seconds)) return "not updated"; if (seconds < 60) return "just now"; if (seconds < 3600) return `${Math.floor(seconds / 60)}m ago`; return `${Math.floor(seconds / 3600)}h ago`; }
  function message(reason: unknown): string { return reason instanceof Error ? reason.message : String(reason); }
</script>

<div class:standalone class:open={expanded} class="companion-layer">
  {#if !expanded}
    <button type="button" class="companion-pill" title="Open activity Companion" onclick={async () => { open = true; if (preferences.enableBeeping) audioBlocked = !(await player.resume()); }}>
      <svg class="companion-spark" viewBox="0 0 52 22" preserveAspectRatio="none" aria-hidden="true"><polyline points="0,13 6,13 9,10 12,13 19,13 22,4 25,19 28,13 36,13 39,10 42,13 47,13 49,7 51,13" /></svg>
      <span class:active={activeList.length > 0} class="companion-live-dot"></span><span class={`companion-cycle ${cycleItem ? statusTone(cycleItem.status) : ""}`}>{cycleItem ? `${cycleItem.provider} ${cycleItem.value}%` : "No quota data"}</span><Icon name={preferences.enableBeeping ? "volume-2" : "volume-x"} />
    </button>
  {:else}
    <section class="companion-card" aria-label="Activity and provider quota companion">
      <header class="companion-card-header"><span class:connected={quota.connected} class="companion-connection"><i></i>OnWatch · {!quota.configured ? "Not configured" : quota.connected ? "Connected" : "Disconnected"}</span><span>{quotaLoading ? "updating…" : updatedAgo(quota.updatedAt)}</span><div><button type="button" aria-label="Open settings" title="Open settings" onclick={onOpenSettings}><Icon name="settings" /></button>{#if !standalone}<a class="companion-header-action" href="/agenthub/beeper" target="_blank" rel="noreferrer" aria-label="Open Beeper in a new window" title="Open Beeper in a new window"><Icon name="external-link" /><span>Beeper</span></a><button type="button" class="companion-header-action" aria-label="Collapse Companion" title="Collapse Companion" onclick={() => open = false}><Icon name="x" /><span>Collapse</span></button>{/if}</div></header>
      <div bind:this={quotaScroll} class="companion-scroll"><div class="companion-dark">
        <div class="companion-cap-row"><span class="companion-cap">Activity Monitor</span><span class={`companion-live-state ${activityState}`}>{activityState === "live" ? "AgentHub Live" : activityState === "paused" ? "Paused" : "Connecting"}</span></div>
        <div class="companion-thread-stat"><strong>{activeList.length}</strong><span>active threads · last 5 min</span></div>
        <ActivityWaveform {pulses} live={activityState === "live"} />
        <div class="companion-thread-list">{#each activeList as session (session.sessionId)}<div class={`companion-thread-row ${terminalClass(session)}`}>{#if !activitySessionTerminal(session)}{#key session.lastActiveAt}<span class="companion-thread-highlight" aria-hidden="true"><span class="companion-thread-title">{session.title || session.sessionId.slice(0, 8)}</span><span class="companion-thread-note">{noteForToneSlot(session.toneSlot, activityChord).name}</span></span>{/key}{/if}<span class="companion-thread-title">{session.title || session.sessionId.slice(0, 8)}</span><span class="companion-thread-note">{noteForToneSlot(session.toneSlot, activityChord).name}</span></div>{:else}<span class="idle">Waiting for activity</span>{/each}</div>
        <div class="companion-controls"><label><span><strong>Enable beeping</strong><small>{audioBlocked ? "Click to enable audio" : "Beep while agents are active"}</small></span><button type="button" role="switch" aria-label="Enable activity beeping" aria-checked={preferences.enableBeeping} class:on={preferences.enableBeeping} class="companion-switch" onclick={toggleBeeping}><span></span></button></label><label><strong>On finish</strong><span class="companion-sound-controls"><select value={preferences.completionSound} onchange={(event) => savePreference({ completionSound: event.currentTarget.value })}>{#each COMPLETION_SOUNDS as option}<option value={option.value}>{option.label}</option>{/each}</select><button type="button" aria-label="Preview completion sound" onclick={preview}><Icon name="play" /></button></span></label>{#if controlError}<p role="alert">{controlError}</p>{/if}</div>
        <div class="companion-quota-heading"><span class="companion-cap">Provider Quota</span><small>All data from OnWatch</small></div>
        {#if quota.error}<div class="companion-quota-error">{quota.error}</div>{/if}
        <div bind:this={quotaGrid} class:dense={quotaDense} class="companion-provider-grid">{#each visibleQuota.providers || [] as provider (provider.provider)}<section class="companion-provider"><header><strong>{provider.label}</strong><em class={statusTone(provider.status)}>{provider.stale ? "Stale" : provider.status}</em></header>{#each provider.quotas || [] as item}<div class={`companion-quota-row ${statusTone(item.status)}`}><div><span>{item.label || item.kind}</span><strong>{Math.round(Number(item.remainingPercent) || 0)}% <small>left</small></strong></div><div class="companion-quota-track"><span style={`width:${Math.max(0, Math.min(100, Number(item.remainingPercent) || 0))}%`}></span>{#if item.windowPositionPercent != null}<i style={`left:${item.windowPositionPercent}%`}></i>{/if}</div><small>{item.resetInSeconds != null ? `resets in ${formatDuration(item.resetInSeconds)}` : `${Math.round(item.usedPercent || 0)}% used`}</small></div>{/each}</section>{/each}</div>
        {#if !quotaLoading && !(visibleQuota.providers || []).length}<p class="companion-empty-quota">No visible quota data</p>{/if}
      </div></div>
    </section>
  {/if}
</div>
