// @ts-nocheck -- deterministic chord model migrated unchanged and covered by tests.
export const DEFAULT_BEEP_CHORD = "c-major";
export const DEFAULT_BEEP_PROGRESSION = "canon-in-c";
export const BEEP_MIN_FRAMES_PER_CHORD = 1;
export const BEEP_MAX_FRAMES_PER_CHORD = 6;
// Prefer the lower octave bands before reaching the highest activity tones.
export const BEEP_OCTAVE_ORDER = [5, 4, 6, 3, 7];

const chord = (value, label, quality, pitchClasses, noteNames) => ({
  value,
  label,
  quality,
  pitchClasses,
  noteNames,
});

export const BEEP_CHORDS = [
  chord("c-major", "C Major", "major", [0, 4, 7], ["C", "E", "G"]),
  chord("db-major", "Db Major", "major", [1, 5, 8], ["Db", "F", "Ab"]),
  chord("d-major", "D Major", "major", [2, 6, 9], ["D", "F#", "A"]),
  chord("eb-major", "Eb Major", "major", [3, 7, 10], ["Eb", "G", "Bb"]),
  chord("e-major", "E Major", "major", [4, 8, 11], ["E", "G#", "B"]),
  chord("f-major", "F Major", "major", [5, 9, 0], ["F", "A", "C"]),
  chord("gb-major", "Gb Major", "major", [6, 10, 1], ["Gb", "Bb", "Db"]),
  chord("g-major", "G Major", "major", [7, 11, 2], ["G", "B", "D"]),
  chord("ab-major", "Ab Major", "major", [8, 0, 3], ["Ab", "C", "Eb"]),
  chord("a-major", "A Major", "major", [9, 1, 4], ["A", "C#", "E"]),
  chord("bb-major", "Bb Major", "major", [10, 2, 5], ["Bb", "D", "F"]),
  chord("b-major", "B Major", "major", [11, 3, 6], ["B", "D#", "F#"]),
  chord("c-minor", "C Minor", "minor", [0, 3, 7], ["C", "Eb", "G"]),
  chord("cs-minor", "C# Minor", "minor", [1, 4, 8], ["C#", "E", "G#"]),
  chord("d-minor", "D Minor", "minor", [2, 5, 9], ["D", "F", "A"]),
  chord("eb-minor", "Eb Minor", "minor", [3, 6, 10], ["Eb", "Gb", "Bb"]),
  chord("e-minor", "E Minor", "minor", [4, 7, 11], ["E", "G", "B"]),
  chord("f-minor", "F Minor", "minor", [5, 8, 0], ["F", "Ab", "C"]),
  chord("fs-minor", "F# Minor", "minor", [6, 9, 1], ["F#", "A", "C#"]),
  chord("g-minor", "G Minor", "minor", [7, 10, 2], ["G", "Bb", "D"]),
  chord("gs-minor", "G# Minor", "minor", [8, 11, 3], ["G#", "B", "D#"]),
  chord("a-minor", "A Minor", "minor", [9, 0, 4], ["A", "C", "E"]),
  chord("bb-minor", "Bb Minor", "minor", [10, 1, 5], ["Bb", "Db", "F"]),
  chord("b-minor", "B Minor", "minor", [11, 2, 6], ["B", "D", "F#"]),
];

const progression = (value, label, description, chords) => ({
  value,
  label,
  description,
  chords,
});

export const BEEP_PROGRESSIONS = [
  progression(
    "canon-in-c",
    "Canon in C",
    "C · G · Am · Em · F · C · F · G · random 1–6 frames each",
    ["c-major", "g-major", "a-minor", "e-minor", "f-major", "c-major", "f-major", "g-major"],
  ),
  progression(
    "pop-axis",
    "Pop axis (I–V–vi–IV)",
    "C · G · Am · F · random 1–6 frames each",
    ["c-major", "g-major", "a-minor", "f-major"],
  ),
  progression(
    "doo-wop",
    "Doo-wop (I–vi–IV–V)",
    "C · Am · F · G · random 1–6 frames each",
    ["c-major", "a-minor", "f-major", "g-major"],
  ),
  progression(
    "three-chord",
    "Three-chord (I–IV–V)",
    "C · F · G · random 1–6 frames each",
    ["c-major", "f-major", "g-major"],
  ),
  progression(
    "jazz-turnaround",
    "Jazz turnaround (ii–V–I)",
    "Dm · G · C · random 1–6 frames each",
    ["d-minor", "g-major", "c-major"],
  ),
  progression(
    "andalusian",
    "Andalusian cadence (i–VII–VI–V)",
    "Am · G · F · E · random 1–6 frames each",
    ["a-minor", "g-major", "f-major", "e-major"],
  ),
  progression(
    "royal-road",
    "Royal road (IV–V–iii–vi)",
    "F · G · Em · Am · random 1–6 frames each",
    ["f-major", "g-major", "e-minor", "a-minor"],
  ),
  progression(
    "creep",
    "Creep (I–III–IV–iv)",
    "C · E · F · Fm · random 1–6 frames each",
    ["c-major", "e-major", "f-major", "f-minor"],
  ),
  progression(
    "blues-12-bar",
    "12-bar blues",
    "C · F · C · C · F · F · C · C · G · F · C · G · random 1–6 frames each",
    ["c-major", "f-major", "c-major", "c-major", "f-major", "f-major", "c-major", "c-major", "g-major", "f-major", "c-major", "g-major"],
  ),
];

const CHORD_BY_VALUE = new Map(BEEP_CHORDS.map((value) => [value.value, value]));
const PROGRESSION_BY_VALUE = new Map(BEEP_PROGRESSIONS.map((value) => [value.value, value]));

export function normalizeBeepProgression(value) {
  return String(value || DEFAULT_BEEP_PROGRESSION);
}

export function beepChord(value) {
  return CHORD_BY_VALUE.get(value) || CHORD_BY_VALUE.get(DEFAULT_BEEP_CHORD);
}

export function beepProgression(value) {
  return PROGRESSION_BY_VALUE.get(normalizeBeepProgression(value))
    || PROGRESSION_BY_VALUE.get(DEFAULT_BEEP_PROGRESSION);
}

export function progressionChordValues(value) {
  const chords = beepProgression(value).chords;
  return chords?.length ? [...chords] : [DEFAULT_BEEP_CHORD];
}

export function randomProgressionDuration(
  random = Math.random,
  minimum = BEEP_MIN_FRAMES_PER_CHORD,
  maximum = BEEP_MAX_FRAMES_PER_CHORD,
) {
  const low = Math.max(1, Math.floor(Number(minimum) || BEEP_MIN_FRAMES_PER_CHORD));
  const high = Math.max(low, Math.floor(Number(maximum) || BEEP_MAX_FRAMES_PER_CHORD));
  const sample = Math.max(0, Math.min(0.999999999999, Number(random()) || 0));
  return low + Math.floor(sample * (high - low + 1));
}

export function nextProgressionFrame(
  current,
  value,
  frameSequence = 0,
  random = Math.random,
) {
  const chords = progressionChordValues(value);
  const sequence = Math.max(0, Math.floor(Number(frameSequence) || 0));
  const key = `${beepProgression(value).value}:${chords.join(",")}`;
  if (chords.length === 1) {
    return { key, sequence, chord: chords[0], chordIndex: 0, duration: null, frameInChord: 1 };
  }
  const contiguous = current?.key === key && current.sequence + 1 === sequence;
  if (!contiguous) {
    return {
      key,
      sequence,
      chord: chords[0],
      chordIndex: 0,
      duration: randomProgressionDuration(random),
      frameInChord: 1,
    };
  }
  if (current.frameInChord >= current.duration) {
    const chordIndex = (current.chordIndex + 1) % chords.length;
    return {
      key,
      sequence,
      chord: chords[chordIndex],
      chordIndex,
      duration: randomProgressionDuration(random),
      frameInChord: 1,
    };
  }
  return {
    ...current,
    sequence,
    frameInChord: current.frameInChord + 1,
  };
}

export function chordTonePool(value) {
  const selected = beepChord(value);
  return BEEP_OCTAVE_ORDER.flatMap((octave) => selected.pitchClasses.map((pitchClass, index) => {
    const midi = (octave + 1) * 12 + pitchClass;
    return {
      name: `${selected.noteNames[index]}${octave}`,
      frequency: 440 * (2 ** ((midi - 69) / 12)),
      midi,
    };
  }));
}

export function noteForToneSlot(slot, value = DEFAULT_BEEP_CHORD) {
  const pool = chordTonePool(value);
  const index = Math.max(0, Math.floor(Number(slot) || 0)) % pool.length;
  return pool[index];
}
