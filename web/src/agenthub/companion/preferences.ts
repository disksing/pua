// @ts-nocheck -- browser preference model migrated unchanged and covered by tests.
import { COMPLETION_SOUNDS, normalizeCompletionSound } from "./audio";
import {
  BEEP_PROGRESSIONS,
  DEFAULT_BEEP_PROGRESSION,
  normalizeBeepProgression,
} from "./chords";

export const COMPANION_PREFERENCES_STORAGE_KEY = "agenthub.companion.preferences.v1";
export const COMPANION_PREFERENCES_EVENT = "agenthub:companion-preferences";

export const DEFAULT_COMPANION_PREFERENCES = Object.freeze({
  showActivity: true,
  enableBeeping: true,
  beepVolume: 0.28,
  beepProgression: DEFAULT_BEEP_PROGRESSION,
  completionSound: "completed-voice",
  hiddenQuotaKeys: [],
  balanceTotals: {},
});

export function normalizeCompanionPreferences(value = {}) {
  const volume = Number(value?.beepVolume);
  const beepProgression = normalizeBeepProgression(value?.beepProgression);
  const completionSound = normalizeCompletionSound(String(value?.completionSound ?? DEFAULT_COMPANION_PREFERENCES.completionSound));
  const hiddenQuotaKeys = [...new Set(
    (Array.isArray(value?.hiddenQuotaKeys) ? value.hiddenQuotaKeys : [])
      .map((key) => String(key || ""))
      .filter(Boolean),
  )].sort();
  const balanceTotals = {};
  for (const [provider, total] of Object.entries(value?.balanceTotals || {})) {
    const numeric = Number(total);
    if (provider && Number.isFinite(numeric) && numeric > 0) balanceTotals[String(provider)] = numeric;
  }
  return {
    showActivity: value?.showActivity == null ? DEFAULT_COMPANION_PREFERENCES.showActivity : Boolean(value.showActivity),
    enableBeeping: value?.enableBeeping == null ? DEFAULT_COMPANION_PREFERENCES.enableBeeping : Boolean(value.enableBeeping),
    beepVolume: Number.isFinite(volume) && volume >= 0 && volume <= 1 ? volume : DEFAULT_COMPANION_PREFERENCES.beepVolume,
    beepProgression: BEEP_PROGRESSIONS.some((option) => option.value === beepProgression) ? beepProgression : DEFAULT_COMPANION_PREFERENCES.beepProgression,
    completionSound: COMPLETION_SOUNDS.some((option) => option.value === completionSound) ? completionSound : DEFAULT_COMPANION_PREFERENCES.completionSound,
    hiddenQuotaKeys,
    balanceTotals,
  };
}

export function validateCompanionPreferences(value) {
  const errors = [];
  const push = (field, message) => errors.push({ section: "activity", index: 0, field, message });
  if (!Number.isFinite(value?.beepVolume) || value.beepVolume < 0 || value.beepVolume > 1) {
    push("beepVolume", "Volume must be between 0 and 1");
  }
  if (!BEEP_PROGRESSIONS.some((option) => option.value === value?.beepProgression)) {
    push("beepProgression", "Select a supported chord progression");
  }
  if (!COMPLETION_SOUNDS.some((option) => option.value === value?.completionSound)) {
    push("completionSound", "Select a supported completion sound");
  }
  for (const [provider, total] of Object.entries(value?.balanceTotals || {})) {
    if (!Number.isFinite(Number(total)) || Number(total) < 0) {
      push("balanceTotals", `Balance total for "${provider}" must be a non-negative number`);
    }
  }
  return errors;
}

export function companionPreferencesEqual(left, right) {
  return JSON.stringify(normalizeCompanionPreferences(left)) === JSON.stringify(normalizeCompanionPreferences(right));
}

function browserStorage() {
  try {
    return typeof window === "undefined" ? undefined : window.localStorage;
  } catch {
    return undefined;
  }
}

export function loadCompanionPreferences(storage = browserStorage()) {
  try {
    const raw = storage?.getItem(COMPANION_PREFERENCES_STORAGE_KEY);
    return normalizeCompanionPreferences(raw ? JSON.parse(raw) : undefined);
  } catch {
    return normalizeCompanionPreferences();
  }
}

export function saveCompanionPreferences(value, storage = browserStorage()) {
  const normalized = normalizeCompanionPreferences(value);
  storage?.setItem(COMPANION_PREFERENCES_STORAGE_KEY, JSON.stringify(normalized));
  if (storage && storage === browserStorage() && typeof window.dispatchEvent === "function") {
    window.dispatchEvent(new CustomEvent(COMPANION_PREFERENCES_EVENT, { detail: normalized }));
  }
  return normalized;
}

export function subscribeCompanionPreferences(listener) {
  if (typeof window === "undefined" || typeof window.addEventListener !== "function") return () => {};
  const onLocalChange = (event) => listener(normalizeCompanionPreferences(event.detail));
  const onStorage = (event) => {
    if (event.key === COMPANION_PREFERENCES_STORAGE_KEY) listener(loadCompanionPreferences());
  };
  window.addEventListener(COMPANION_PREFERENCES_EVENT, onLocalChange);
  window.addEventListener("storage", onStorage);
  return () => {
    window.removeEventListener(COMPANION_PREFERENCES_EVENT, onLocalChange);
    window.removeEventListener("storage", onStorage);
  };
}
