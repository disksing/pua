// @ts-nocheck -- WebAudio engine migrated unchanged and covered by tests.
import {
  chordTonePool,
  DEFAULT_BEEP_CHORD,
  DEFAULT_BEEP_PROGRESSION,
  noteForToneSlot,
  progressionChordValues,
} from "./chords";
export { activityPlaybackPlan } from "./schedule";

export const COMPLETION_SOUNDS = [
  { value: "completed-voice", label: "Completed Voice", file: "completed-voice.mp3" },
  { value: "done-for-you-girl", label: "Done For You Girl", file: "done-for-you-girl.mp3" },
  { value: "light-hearted-message", label: "Light Hearted Message", file: "light-hearted-message.mp3" },
  { value: "not-bad", label: "Not Bad", file: "not-bad.mp3" },
  { value: "slow-spring-board", label: "Slow Spring Board", file: "slow-spring-board.mp3" },
  { value: "smile", label: "Smile", file: "smile.mp3" },
];

const LEGACY_COMPLETION_SOUNDS = new Set(["chime", "bell", "ding", "marimba", "pop"]);

export function normalizeCompletionSound(value) {
  if (COMPLETION_SOUNDS.some((option) => option.value === value)) return value;
  if (LEGACY_COMPLETION_SOUNDS.has(value)) return "completed-voice";
  return value || "completed-voice";
}

export function completionSoundURL(value) {
  const selected = normalizeCompletionSound(value);
  const option = COMPLETION_SOUNDS.find((candidate) => candidate.value === selected) || COMPLETION_SOUNDS[0];
  return `/agenthub/completion-sounds/${option.file}`;
}

export class TonePlayer {
  constructor(
    AudioContextClass = globalThis.AudioContext || globalThis.webkitAudioContext,
    AudioClass = globalThis.Audio,
  ) {
    this.AudioContextClass = AudioContextClass;
    this.AudioClass = AudioClass;
    this.context = null;
    this.mediaPrimed = false;
    this.completionPlayers = new Set();
  }

  status() {
    return this.context?.state || "unavailable";
  }

  async resume() {
    let contextRunning = false;
    if (this.AudioContextClass) {
      if (!this.context) this.context = new this.AudioContextClass();
      if (this.context.state === "suspended") await this.context.resume();
      contextRunning = this.context.state === "running";
    }
    const mediaReady = await this.primeCompletionAudio();
    return contextRunning || mediaReady;
  }

  pulse(toneSlot, chord = DEFAULT_BEEP_CHORD, volume = 0.28, delay = 0) {
    return this.playFrequency(noteForToneSlot(toneSlot, chord).frequency, volume, delay, 0.1);
  }

  previewProgression(progression = DEFAULT_BEEP_PROGRESSION, volume = 0.28) {
    const chordValues = progressionChordValues(progression);
    const chordDuration = 0.72;
    return chordValues.flatMap((value, chordIndex) => chordTonePool(value).slice(0, 3).map((note, noteIndex) => (
      this.playFrequency(note.frequency, volume, chordIndex * chordDuration + noteIndex * 0.12, 0.16)
    )));
  }

  completion(sound = "completed-voice", volume = 0.28) {
    if (!this.AudioClass) return this.fallbackCompletion(volume);
    const player = new this.AudioClass(completionSoundURL(sound));
    player.volume = Math.max(0, Math.min(1, volume));
    const cleanup = () => this.completionPlayers.delete(player);
    player.addEventListener?.("ended", cleanup, { once: true });
    player.addEventListener?.("error", cleanup, { once: true });
    this.completionPlayers.add(player);
    try {
      const playback = player.play();
      playback?.catch(() => {
        cleanup();
        this.fallbackCompletion(volume);
      });
    } catch {
      cleanup();
      return this.fallbackCompletion(volume);
    }
    return player;
  }

  async primeCompletionAudio() {
    if (this.mediaPrimed || !this.AudioClass) return this.mediaPrimed;
    const player = new this.AudioClass(completionSoundURL("completed-voice"));
    player.volume = 0;
    try {
      await player.play();
      player.pause?.();
      try { player.currentTime = 0; } catch { /* Some test doubles expose a read-only value. */ }
      this.mediaPrimed = true;
    } catch {
      this.mediaPrimed = false;
    }
    return this.mediaPrimed;
  }

  fallbackCompletion(volume) {
    return [
      this.playFrequency(659.25, volume, 0, 0.16),
      this.playFrequency(987.77, volume, 0.11, 0.16),
      this.playFrequency(1318.51, volume, 0.22, 0.16),
    ];
  }

  playFrequency(frequency, volume, delay = 0, duration = 0.1) {
    if (!this.context || this.context.state !== "running") return false;
    const start = this.context.currentTime + Math.max(0, delay);
    const oscillator = this.context.createOscillator();
    const gain = this.context.createGain();
    oscillator.type = "sine";
    oscillator.frequency.setValueAtTime(frequency, start);
    gain.gain.setValueAtTime(0.0001, start);
    gain.gain.exponentialRampToValueAtTime(Math.max(0.0001, Math.min(1, volume)), start + 0.012);
    gain.gain.setValueAtTime(Math.max(0.0001, Math.min(1, volume)), start + Math.max(0.013, duration - 0.024));
    gain.gain.exponentialRampToValueAtTime(0.0001, start + duration);
    oscillator.connect(gain);
    gain.connect(this.context.destination);
    oscillator.start(start);
    oscillator.stop(start + duration + 0.01);
    return true;
  }
}
