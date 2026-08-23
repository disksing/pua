// @ts-nocheck -- companion projection migrated unchanged and covered by tests.
import { activityPlaybackPlan } from "./schedule";
import { chordTonePool } from "./chords";

export function hashSessionId(value) {
  let hash = 2166136261;
  for (const character of String(value || "")) {
    hash ^= character.codePointAt(0);
    hash = Math.imul(hash, 16777619);
  }
  return hash >>> 0;
}

export class SessionToneAllocator {
  constructor(slotCount = chordTonePool().length) {
    this.slotCount = Math.max(1, Math.floor(Number(slotCount) || 1));
    this.assignments = new Map();
  }

  assign(sessionId) {
    const id = String(sessionId || "");
    if (this.assignments.has(id)) return this.assignments.get(id);
    const usage = Array(this.slotCount).fill(0);
    for (const slot of this.assignments.values()) usage[slot] += 1;
    const minimum = Math.min(...usage);
    const slot = usage.indexOf(minimum);
    this.assignments.set(id, slot);
    return slot;
  }

  release(sessionId) {
    this.assignments.delete(String(sessionId || ""));
  }

  retain(sessionIds) {
    const active = new Set([...sessionIds].map((value) => String(value || "")));
    for (const id of this.assignments.keys()) {
      if (!active.has(id)) this.assignments.delete(id);
    }
  }
}

export function quotaCycleItems(snapshot) {
  return (snapshot?.providers || []).flatMap((provider) => {
    const quota = (provider.quotas || [])[0];
    if (!quota) return [];
    return [{
      provider: provider.label || provider.provider,
      value: Math.round(Number(quota.remainingPercent) || 0),
      label: quota.kind || quota.label,
      status: quota.status || provider.status || "healthy",
      stale: Boolean(provider.stale || quota.stale),
    }];
  });
}

export function quotaVisibilityKey(provider, quota) {
  return JSON.stringify([
    String(provider?.provider || provider?.label || ""),
    String(quota?.kind || quota?.label || ""),
  ]);
}

// DEFAULT_BALANCE_TOTAL is the denominator used to display balance-style
// quotas (e.g. a DeepSeek credit balance) until a per-provider total is
// configured in the Activity settings.
export const DEFAULT_BALANCE_TOTAL = 100;

// applyBalanceTotals re-derives the percentages of balance-style quotas
// against per-provider balance totals from companion preferences. The daemon
// reports balance quotas with a raw Value and a remaining share computed
// against a default total of 100; this normalizes them for display without
// mutating the source snapshot.
export function applyBalanceTotals(snapshot, balanceTotals = {}) {
  const totals = {};
  for (const [provider, total] of Object.entries(balanceTotals || {})) {
    const numeric = Number(total);
    if (provider && Number.isFinite(numeric) && numeric > 0) totals[String(provider)] = numeric;
  }
  return {
    ...(snapshot || {}),
    providers: (snapshot?.providers || []).map((provider) => {
      const total = totals[provider.provider] ?? DEFAULT_BALANCE_TOTAL;
      const quotas = (provider.quotas || []).map((quota) => {
        if (quota.kind !== "balance" || quota.value == null) return quota;
        const remaining = Math.min(100, Math.max(0, (100 * quota.value) / total));
        const used = Math.min(total, Math.max(0, total - quota.value));
        return { ...quota, remainingPercent: remaining, usedPercent: 100 - remaining, used, limit: total };
      });
      return { ...provider, quotas };
    }),
  };
}

export function filterQuotaSnapshot(snapshot, hiddenQuotaKeys = []) {
  const hidden = new Set((hiddenQuotaKeys || []).map(String));
  return {
    ...(snapshot || {}),
    providers: (snapshot?.providers || []).flatMap((provider) => {
      const quotas = (provider.quotas || []).filter((quota) => !hidden.has(quotaVisibilityKey(provider, quota)));
      if (!quotas.length && !provider.error) return [];
      return [{ ...provider, quotas }];
    }),
  };
}

export function formatDuration(seconds) {
  const value = Math.max(0, Math.floor(Number(seconds) || 0));
  const days = Math.floor(value / 86400);
  const hours = Math.floor((value % 86400) / 3600);
  const minutes = Math.floor((value % 3600) / 60);
  if (days) return `${days}d ${hours}h`;
  if (hours) return `${hours}h ${minutes}m`;
  return `${minutes}m`;
}

export const ACTIVITY_SESSION_RETENTION_MS = 5 * 60 * 1000;
export const TERMINAL_TONE_HOLD_MS = 10 * 1000;

export function activitySessionTerminal(session) {
  if (session?.turnTerminal) return session.turnTerminal;
  if (!session?.completed) return null;
  return {
    turnId: session.turnId || "",
    status: "completed",
    endedAt: session.lastEventAt,
  };
}

export function activitySessionNeedsTone(previous, incoming) {
  if (activitySessionTerminal(incoming)) return true;
  const previousTerminal = activitySessionTerminal(previous);
  if (!previousTerminal) return true;
  return Boolean(incoming?.turnId && incoming.turnId !== previousTerminal.turnId);
}

export function activitySessionHoldsTone(session, now = Date.now()) {
  return !activitySessionTerminal(session) || Number(session?.toneReleaseAt) > now;
}

export function activitySessions(current, frame, now = Date.now()) {
  let next = current;
  const mutable = () => {
    if (next === current) next = new Map(current);
    return next;
  };
  for (const session of frame?.sessions || []) {
    const previous = next.get(session.sessionId);
    const incomingTerminal = activitySessionTerminal(session);
    const previousTerminal = activitySessionTerminal(previous);
    const startsDifferentTurn = !incomingTerminal && previousTerminal && session.turnId
      && session.turnId !== previousTerminal.turnId;
    const preservesTerminal = previousTerminal && !incomingTerminal && !startsDifferentTurn;
    const turnTerminal = incomingTerminal || (preservesTerminal ? previousTerminal : null);
    mutable().set(session.sessionId, {
      ...previous,
      ...session,
      turnTerminal,
      lastActiveAt: now,
      expiresAt: incomingTerminal
        ? now + ACTIVITY_SESSION_RETENTION_MS
        : preservesTerminal ? previous.expiresAt : now + ACTIVITY_SESSION_RETENTION_MS,
      toneReleaseAt: incomingTerminal
        ? now + TERMINAL_TONE_HOLD_MS
        : preservesTerminal ? previous.toneReleaseAt : null,
    });
  }
  for (const [id, session] of next) {
    if (session.expiresAt <= now) mutable().delete(id);
  }
  return next;
}

export const ACTIVITY_VISIBLE_MS = 9000;
const ACTIVITY_LEAD_MS = 750;
const PULSE_SHAPE = [
  [420, 0], [280, 0.02], [180, 0.12], [105, 0.03], [52, 0], [24, -0.30],
  [0, 1.28], [-30, -0.42], [-70, -0.04], [-125, 0.08], [-220, 0.18],
  [-340, 0.04], [-460, 0],
];
const IDLE_PULSE_INTERVAL_MS = 2100;

function clamp(value, low, high) {
  return Math.min(high, Math.max(low, value));
}

function baselineMotion(sampleTimeMs, active) {
  const time = sampleTimeMs / 1000;
  const value = Math.sin(time * 2.1) * 0.018
    + Math.sin(time * 7.8 + 1.4) * 0.010
    + Math.sin(time * 18 + 0.6) * 0.005;
  return value * (active ? 1 : 0.35);
}

export function activityPulsesForFrame(frame, now = Date.now()) {
  const sessions = [...(frame?.sessions || [])].sort((a, b) => (
    (Number(a.toneSlot) || 0) - (Number(b.toneSlot) || 0)
    || String(a.sessionId).localeCompare(String(b.sessionId))
  ));
  return activityPlaybackPlan(sessions, frame?.sequence).map(({ item: session, delay }) => {
    const baseAmplitude = 0.88 + (hashSessionId(session.sessionId) % 18) / 100;
    return {
      at: now + delay * 1000,
      amplitude: activitySessionTerminal(session) ? 1.18 : baseAmplitude,
      sessionId: session.sessionId,
    };
  });
}

export function pruneActivityPulses(pulses, now = Date.now()) {
  const current = pulses || [];
  const next = current.filter((pulse) => now - pulse.at < ACTIVITY_VISIBLE_MS + ACTIVITY_LEAD_MS);
  return next.length === current.length ? current : next;
}

function waveformY(sampleTime, pulseMotion, baseline, amplitude, active) {
  const motion = clamp(baselineMotion(sampleTime, active) + pulseMotion, -1.15, 1.15);
  return baseline - motion * amplitude;
}

function pulseShapeAt(ageOffset) {
  if (ageOffset > PULSE_SHAPE[0][0] || ageOffset < PULSE_SHAPE[PULSE_SHAPE.length - 1][0]) return 0;
  for (let index = 0; index < PULSE_SHAPE.length - 1; index += 1) {
    const [fromOffset, fromValue] = PULSE_SHAPE[index];
    const [toOffset, toValue] = PULSE_SHAPE[index + 1];
    if (ageOffset <= fromOffset && ageOffset >= toOffset) {
      const progress = (fromOffset - ageOffset) / (fromOffset - toOffset);
      return fromValue + (toValue - fromValue) * progress;
    }
  }
  return 0;
}

function deterministicUnit(value) {
  let hash = Math.imul(value ^ 0x9e3779b9, 0x85ebca6b);
  hash ^= hash >>> 13;
  hash = Math.imul(hash, 0xc2b2ae35);
  hash ^= hash >>> 16;
  return (hash >>> 0) / 4294967295;
}

function idlePulseMotion(sampleTime) {
  const bucket = Math.floor(sampleTime / IDLE_PULSE_INTERVAL_MS);
  let motion = 0;
  for (let index = bucket - 1; index <= bucket + 1; index += 1) {
    const position = 0.22 + deterministicUnit(index) * 0.56;
    const peakTime = index * IDLE_PULSE_INTERVAL_MS + position * IDLE_PULSE_INTERVAL_MS;
    const amplitude = 0.07 + deterministicUnit(index + 0x51f15e) * 0.07;
    motion += pulseShapeAt(peakTime - sampleTime) * amplitude;
  }
  return motion;
}

export function waveformSampleY(pulses = [], sampleTime = Date.now(), height = 86, active = true) {
  const baseline = height * 0.54;
  const amplitude = height * 0.32;
  let pulseMotion = pulses?.length ? 0 : idlePulseMotion(sampleTime);
  if (pulses?.length) {
    for (const pulse of pulses) {
      pulseMotion += pulseShapeAt(Number(pulse.at) - sampleTime) * (Number(pulse.amplitude) || 1);
    }
  }
  return waveformY(sampleTime, pulseMotion, baseline, amplitude, active);
}

function waveformPulseSegment(pulse, now, width, baseline, amplitude, active) {
  const pulseAge = now - pulse.at;
  const centerX = width * (1 - pulseAge / ACTIVITY_VISIBLE_MS);
  const points = PULSE_SHAPE.map(([ageOffset, value]) => ({
    x: centerX - ageOffset / ACTIVITY_VISIBLE_MS * width,
    y: waveformY(pulse.at - ageOffset, value * pulse.amplitude, baseline, amplitude, active),
  }));
  return {
    startX: Math.min(...points.map((point) => point.x)),
    endX: Math.max(...points.map((point) => point.x)),
    points,
  };
}

export function waveformCoordinates(pulses = [], now = Date.now(), width = 700, height = 86, active = true) {
  const baseline = height * 0.54;
  const amplitude = height * 0.32;
  const segments = pruneActivityPulses(pulses, now)
    .map((pulse) => waveformPulseSegment(pulse, now, width, baseline, amplitude, active))
    .filter((segment) => segment.endX >= -16 && segment.startX <= width + 16)
    .sort((a, b) => a.startX - b.startX);
  const points = [];
  let x = 0;
  let segmentIndex = 0;
  while (x <= width) {
    while (segmentIndex < segments.length && segments[segmentIndex].endX < x) segmentIndex += 1;
    if (segmentIndex < segments.length && segments[segmentIndex].startX <= x) {
      points.push(...segments[segmentIndex].points);
      x = Math.max(x + 2, segments[segmentIndex].endX + 2);
      segmentIndex += 1;
      continue;
    }
    const age = ACTIVITY_VISIBLE_MS * (1 - x / width);
    const sampleTime = now - age;
    const y = waveformY(sampleTime, 0, baseline, amplitude, active);
    points.push({ x, y });
    x += 2;
  }
  return points;
}

export function waveformPoints(pulses = [], now = Date.now(), width = 700, height = 86, active = true) {
  return waveformCoordinates(pulses, now, width, height, active)
    .map((point) => `${point.x.toFixed(1)},${point.y.toFixed(1)}`)
    .join(" ");
}

export function normalizeCompanionPosition(value) {
  const x = Number(value?.x);
  const y = Number(value?.y);
  return {
    x: Number.isFinite(x) ? clamp(x, 0, 1) : 1,
    y: Number.isFinite(y) ? clamp(y, 0, 1) : 1,
  };
}

export function companionPositionPixels(position, viewport, pill, gap = 12) {
  const normalized = normalizeCompanionPosition(position);
  const rangeX = Math.max(0, viewport.width - pill.width - gap * 2);
  const rangeY = Math.max(0, viewport.height - pill.height - gap * 2);
  return { x: gap + normalized.x * rangeX, y: gap + normalized.y * rangeY };
}

export function companionPositionFromPixels(pixels, viewport, pill, gap = 12) {
  const rangeX = Math.max(0, viewport.width - pill.width - gap * 2);
  const rangeY = Math.max(0, viewport.height - pill.height - gap * 2);
  return normalizeCompanionPosition({
    x: rangeX ? (clamp(pixels.x, gap, gap + rangeX) - gap) / rangeX : 0,
    y: rangeY ? (clamp(pixels.y, gap, gap + rangeY) - gap) / rangeY : 0,
  });
}

export const DEFAULT_COMPANION_SIZE = { width: 380, height: 520 };
export const MIN_COMPANION_SIZE = { width: 280, height: 260 };

export function normalizeCompanionSize(value) {
  const width = Number(value?.width);
  const height = Number(value?.height);
  return {
    width: Number.isFinite(width) ? Math.max(MIN_COMPANION_SIZE.width, Math.round(width)) : DEFAULT_COMPANION_SIZE.width,
    height: Number.isFinite(height) ? Math.max(MIN_COMPANION_SIZE.height, Math.round(height)) : DEFAULT_COMPANION_SIZE.height,
  };
}

export function companionPlacement(anchor, viewport, pill, cardSize = DEFAULT_COMPANION_SIZE, gap = 12) {
  const vertical = anchor.y + pill.height / 2 <= viewport.height / 2 ? "down" : "up";
  const horizontal = anchor.x + pill.width / 2 <= viewport.width / 2 ? "right" : "left";
  const maxHeight = vertical === "down"
    ? Math.max(0, viewport.height - gap - anchor.y)
    : Math.max(0, anchor.y + pill.height - gap);
  const maxWidth = horizontal === "right"
    ? Math.max(0, viewport.width - gap - anchor.x)
    : Math.max(0, anchor.x + pill.width - gap);
  const desired = normalizeCompanionSize(cardSize);
  const width = clamp(desired.width, Math.min(MIN_COMPANION_SIZE.width, maxWidth), maxWidth);
  const height = clamp(desired.height, Math.min(MIN_COMPANION_SIZE.height, maxHeight), maxHeight);
  return {
    vertical,
    horizontal,
    left: horizontal === "right" ? anchor.x : anchor.x + pill.width - width,
    top: vertical === "down" ? anchor.y : null,
    bottom: vertical === "up" ? viewport.height - anchor.y - pill.height : null,
    width,
    height,
    maxWidth,
    maxHeight,
  };
}

export function resizeCompanionSize(size, delta, placement) {
  const current = normalizeCompanionSize(size);
  const horizontalDelta = (Number(delta?.x) || 0) * (placement.horizontal === "right" ? 1 : -1);
  const verticalDelta = (Number(delta?.y) || 0) * (placement.vertical === "down" ? 1 : -1);
  return {
    width: Math.round(clamp(
      current.width + horizontalDelta,
      Math.min(MIN_COMPANION_SIZE.width, placement.maxWidth),
      placement.maxWidth,
    )),
    height: Math.round(clamp(
      current.height + verticalDelta,
      Math.min(MIN_COMPANION_SIZE.height, placement.maxHeight),
      placement.maxHeight,
    )),
  };
}
