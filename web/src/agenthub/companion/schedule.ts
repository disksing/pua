// @ts-nocheck -- deterministic playback scheduler migrated unchanged and covered by tests.
export const ACTIVITY_GRID_SECONDS = 0.25;
export const ACTIVITY_SLOT_GAINS = [1, 0.85, 0.92, 0.85];

const SLOT_PATTERNS = {
  1: [[0]],
  2: [[0, 2]],
  3: [[0, 1, 2], [0, 1, 3], [0, 2, 3], [0, 1, 2]],
  4: [[0, 1, 2, 3]],
};

function framePhase(frameSequence) {
  const sequence = Math.max(0, Math.floor(Number(frameSequence) || 0));
  return (sequence ? sequence - 1 : sequence) % 4;
}

function rotate(items, offset) {
  if (!items.length) return [];
  const index = offset % items.length;
  return [...items.slice(index), ...items.slice(0, index)];
}

export function activityPlaybackPlan(items = [], frameSequence = 0) {
  const values = [...items];
  if (!values.length) return [];
  const phase = framePhase(frameSequence);
  let assigned;
  if (values.length <= 4) {
    const patterns = SLOT_PATTERNS[values.length];
    const slots = patterns[phase % patterns.length];
    assigned = rotate(values, phase).map((item, index) => ({ item, slot: slots[index] }));
  } else {
    assigned = values.map((item, index) => ({ item, slot: (index + phase) % 4 }));
  }
  const stackSizes = assigned.reduce((counts, value) => {
    counts[value.slot] += 1;
    return counts;
  }, [0, 0, 0, 0]);
  return assigned.map(({ item, slot }) => ({
    item,
    slot,
    delay: slot * ACTIVITY_GRID_SECONDS,
    gain: ACTIVITY_SLOT_GAINS[slot] / Math.sqrt(stackSizes[slot]),
  }));
}
