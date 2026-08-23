import { readFileSync } from "node:fs";
import { resolve } from "node:path";

import { describe, expect, it, vi } from "vitest";

import { sessionsQuery } from "../../src/agenthub/core/archive";
import { buildCreatePayload } from "../../src/agenthub/core/new-session";
import { TonePlayer } from "../../src/agenthub/companion/audio";
import { ACTIVITY_LEAD_MS, ACTIVITY_WAVEFORMS, activityPulsesForFrame, activitySessions, activityWaveformIndex, activityWaveformPatchRange, applyBalanceTotals, filterQuotaSnapshot, pruneActivityPulses, quotaVisibilityKey, SessionToneAllocator, TERMINAL_TONE_HOLD_MS, waveformSampleY } from "../../src/agenthub/companion/model";
import { activityPlaybackPlan } from "../../src/agenthub/companion/schedule";
import { buildProviderSwitches } from "../../src/agenthub/settings/provider-switches";

describe("AgentHub audit application", () => {
  it("builds an explicit paginated current/archived inventory query", () => {
    expect(sessionsQuery({ limit: 25, state: ["running", "ready"], sourceApp: "pua", cursor: "next page" }))
      .toBe("/v1/sessions?state=running%2Cready&sourceApp=pua&limit=25&cursor=next+page");
    expect(sessionsQuery({ archivedOnly: true, limit: 50 }))
      .toBe("/v1/sessions?archived=true&limit=50");
  });

  it("validates new Sessions against the existing agent catalog", () => {
    const agents = [{ name: "Codex", providerId: "codex", available: true }];
    expect(buildCreatePayload({ cwd: "relative", agentName: "Missing", title: "", agents }).errors).toMatchObject({ cwd: expect.any(String), agent: expect.any(String) });
    expect(buildCreatePayload({ cwd: "/tmp/project", agentName: "Codex", title: "Audit", agents }).payload)
      .toEqual({ cwd: "/tmp/project", agentName: "Codex", title: "Audit" });
  });

  it("keeps provider switches fixed to the four daemon integrations", () => {
    const switches = (buildProviderSwitches as any)({ agentProviders: [{ id: "codex", name: "Codex", type: "codex", enabled: true }] }, [{ providerId: "codex", available: true, command: "/bin/codex" }]);
    expect(switches.map((item: any) => item.id)).toEqual(["codex", "kimi", "pi", "opencode"]);
    expect(switches[0]).toMatchObject({ enabled: true, availability: "CLI available" });
  });

  it("retains a terminal activity row for five minutes but its tone for ten seconds", () => {
    const now = 100_000;
    const sessions = (activitySessions as any)(new Map(), { sessions: [{ sessionId: "s1", turnId: "t1", completed: true, lastEventAt: new Date(now).toISOString() }] }, now);
    const session = sessions.get("s1") as any;
    expect(session.toneReleaseAt).toBe(now + TERMINAL_TONE_HOLD_MS);
    expect(session.expiresAt).toBe(now + 5 * 60 * 1000);
  });

  it("preserves activity collection identity when housekeeping has nothing to prune", () => {
    const sessions = new Map([["s1", { sessionId: "s1", expiresAt: 20_000 }]]);
    const pulses = [{ sessionId: "s1", at: 9_000 }];
    expect((activitySessions as any)(sessions, { sessions: [] }, 10_000)).toBe(sessions);
    expect((pruneActivityPulses as any)(pulses, 10_000)).toBe(pulses);
  });

  it("uses the approved four-subdivision activity patterns and stable tone slots", () => {
    expect((activityPlaybackPlan as any)(["a", "b", "c"], 1).map((entry: any) => entry.slot)).toEqual([0, 1, 2]);
    expect((activityPlaybackPlan as any)(["a", "b", "c"], 2).map((entry: any) => entry.slot)).toEqual([0, 1, 3]);
    const allocator = new SessionToneAllocator(3);
    expect([allocator.assign("a"), allocator.assign("b"), allocator.assign("a")]).toEqual([0, 1, 0]);
  });

  it("delays audio-aligned pulses by 300ms and stably binds Sessions to tuned waveforms", () => {
    const now = 40_000;
    const frame = { sequence: 1, sessions: [{ sessionId: "alpha", toneSlot: 0 }, { sessionId: "beta", toneSlot: 1 }] };
    const pulses = (activityPulsesForFrame as any)(frame, now);
    expect(ACTIVITY_LEAD_MS).toBe(300);
    expect(pulses[0].at).toBe(now + ACTIVITY_LEAD_MS);
    expect(pulses[0]).toMatchObject({ amplitude: 1, waveformIndex: (activityWaveformIndex as any)("alpha") });
    expect((activityPulsesForFrame as any)(frame, now + 10_000).map((pulse: any) => pulse.waveformIndex))
      .toEqual(pulses.map((pulse: any) => pulse.waveformIndex));
    expect(ACTIVITY_WAVEFORMS).toHaveLength(4);
    expect(ACTIVITY_WAVEFORMS.map((shape: any) => [shape[0], shape.at(-1)])).toEqual([
      [[200, 0], [-200, 0]],
      [[200, 0], [-200, 0]],
      [[200, 0], [-200, 0]],
      [[200, 0], [-200, 0]],
    ]);
    expect(ACTIVITY_WAVEFORMS.map((shape: any) => Math.max(...shape.map((point: any) => point[1])))).toEqual([1.28, 1.12, 1.309, 1.38]);
  });

  it("clips pulse patches to the offscreen write fence", () => {
    expect((activityWaveformPatchRange as any)(1300, 1000, 0, 9000)).toEqual({ from: 1100, to: 1500 });
    expect((activityWaveformPatchRange as any)(900, 1000, 0, 9000)).toEqual({ from: 1000, to: 1100 });
    expect((activityWaveformPatchRange as any)(700, 1000, 0, 9000)).toBeNull();
  });

  it("delays completion playback until the synchronized visual peak", () => {
    vi.useFakeTimers();
    const play = vi.fn(() => Promise.resolve());
    class FakeAudio {
      volume = 0;
      constructor(_url: string) {}
      addEventListener() {}
      play() { return play(); }
    }
    const player = new TonePlayer(undefined, FakeAudio as any);
    player.completion("smile", 0.4, ACTIVITY_LEAD_MS / 1000);
    expect(play).not.toHaveBeenCalled();
    vi.advanceTimersByTime(ACTIVITY_LEAD_MS - 1);
    expect(play).not.toHaveBeenCalled();
    vi.advanceTimersByTime(1);
    expect(play).toHaveBeenCalledOnce();
    vi.useRealTimers();
  });

  it("applies balance totals and persisted quota visibility preferences", () => {
    const snapshot = { providers: [{ provider: "deepseek", label: "DeepSeek", quotas: [{ kind: "balance", label: "Credit", value: 40, remainingPercent: 40 }] }] };
    const quota = (applyBalanceTotals as any)(snapshot, { deepseek: 200 });
    expect(quota.providers[0].quotas[0]).toMatchObject({ remainingPercent: 20, limit: 200 });
    const hiddenKey = (quotaVisibilityKey as any)(quota.providers[0], quota.providers[0].quotas[0]);
    expect((filterQuotaSnapshot as any)(quota, [hiddenKey]).providers).toEqual([]);
  });

  it("combines simultaneous activity into one waveform and adds deterministic idle peaks", () => {
    const sampleTime = 10_000;
    const baselinePulse = [{ sessionId: "future", at: sampleTime + 10_000, amplitude: 1, waveformIndex: 0 }];
    const baseline = (waveformSampleY as any)(baselinePulse, sampleTime, 86, true);
    const one = (waveformSampleY as any)([{ sessionId: "one", at: sampleTime, amplitude: 0.25, waveformIndex: 0 }], sampleTime, 86, true);
    const two = (waveformSampleY as any)([
      { sessionId: "one", at: sampleTime, amplitude: 0.25, waveformIndex: 0 },
      { sessionId: "two", at: sampleTime, amplitude: 0.25, waveformIndex: 0 },
    ], sampleTime, 86, true);
    expect(Math.abs(two - baseline)).toBeGreaterThan(Math.abs(one - baseline) * 1.8);

    const idle = Array.from({ length: 40 }, (_, index) => (waveformSampleY as any)([], sampleTime + index * 100, 86, true));
    const idleAgain = Array.from({ length: 40 }, (_, index) => (waveformSampleY as any)([], sampleTime + index * 100, 86, true));
    expect(idleAgain).toEqual(idle);
    expect(new Set(idle.map((value) => value.toFixed(2))).size).toBeGreaterThan(8);
    expect((waveformSampleY as any)([], sampleTime, 86, false)).toBe((waveformSampleY as any)([], sampleTime, 86, true));
    expect((waveformSampleY as any)(baselinePulse, sampleTime, 86, true)).toBe((waveformSampleY as any)([], sampleTime, 86, true));
  });

  it("keeps inventory controls visible and restores the full-screen animated Beeper contract", () => {
    const source = (name: string) => readFileSync(resolve(process.cwd(), "src/agenthub", name), "utf8");
    const css = source("agenthub.css");
    const inventory = source("SessionInventory.svelte");
    const companion = source("Companion.svelte");
    const waveform = source("ActivityWaveform.svelte");
    const settings = source("SettingsDialog.svelte");
    expect(css).toContain(".session-table-wrap { min-height: 0; flex: 1; overflow: auto;");
    expect(css).toContain(".companion-layer.standalone .companion-card { width: 100%; height: 100%; max-height: none;");
    expect(css).toContain("animation: companion-thread-highlight-fade 10s");
    expect(css).toContain("animation: companion-thread-highlight-fade 10s linear forwards");
    expect(css).toContain(".companion-ecg { width: 100%; max-width: 900px;");
    expect(css).toContain(".companion-provider-grid.dense");
    expect(css).toContain("@keyframes companion-scan { from { transform:");
    expect(inventory).not.toContain("Agent activity and history");
    expect(companion).toContain("{#each activeList as session (session.sessionId)}");
    expect(companion).toContain("{#key session.lastActiveAt}");
    expect(waveform.match(/<canvas/g)).toHaveLength(2);
    expect(waveform).not.toContain("$state");
    expect(waveform).toContain("context.lineTo");
    expect(waveform).toContain("track.animate(");
    expect(waveform).toContain("activityWaveformPatchRange");
    expect(waveform).toContain("const writeFence = visibleRightTime() + pixelFenceMs");
    expect(waveform).toContain("track.append(expired.canvas)");
    expect(waveform).toContain("context.lineWidth = 1.9");
    expect(waveform).toContain("context.shadowBlur = 12");
    expect(waveform).not.toContain("live;");
    expect(waveform).not.toContain("requestAnimationFrame");
    expect(waveform).toContain("document.hidden");
    expect(companion).toContain("Open Beeper in a new window");
    expect(companion).toContain("delay + ACTIVITY_LEAD_MS / 1000");
    expect(companion).toContain("preferences.beepVolume, synchronizedDelay");
    expect(companion).toContain("Collapse Companion");
    expect(companion).toContain("projectedSingleContentHeight > quotaScroll.clientHeight");
    expect(companion).toContain("class:dense={quotaDense}");
    expect(settings).toContain("Quota visibility");
    expect(settings).toContain("Balance totals");
  });
});
