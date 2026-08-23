// @ts-nocheck -- migrated protocol projector; covered by behavior tests.
import { api } from "./api";

export class EventCursorGapError extends Error {
  constructor(expected, got) {
    super(`Event cursor gap: expected ${expected}, got ${got}.`);
    this.name = "EventCursorGapError";
    this.expected = expected;
    this.got = got;
  }
}

export async function catchUpFrames(sessionId, after = 0, request = api) {
  let cursor = after;
  let target = null;
  const frames = [];
  do {
    const body = await request(`/v1/sessions/${sessionId}/events?after=${cursor}&limit=1000`);
    if (body.schema !== "agenthub.semantic-events.v1") throw new Error(`Unsupported events schema: ${body.schema || "missing"}.`);
    if (target === null) target = body.latestCursor;
    const page = body.frames || [];
    for (const frame of page) {
      if (frame.cursor > target) break;
      if (frame.cursor !== cursor + 1) throw new EventCursorGapError(cursor + 1, frame.cursor);
      frames.push(frame);
      cursor = frame.cursor;
    }
    if (cursor < target && page.length === 0) throw new EventCursorGapError(cursor + 1, 0);
  } while (cursor < target);
  return { frames, cursor, latestCursor: target };
}

export async function projectLiveFrame({ sessionId, cursor, frame, request = api, project }) {
  if (frame.cursor <= cursor) {
    project([frame]);
    return cursor;
  }
  if (frame.cursor === cursor + 1) {
    project([frame]);
    return frame.cursor;
  }
  const caughtUp = await catchUpFrames(sessionId, cursor, request);
  project(caughtUp.frames);
  return caughtUp.cursor;
}

export function mergeIncomingFrames(current, incoming) {
  const next = [...current];
  const indexByCursor = new Map(next.map((frame, index) => [frame.cursor, index]));
  for (const frame of incoming) {
    const index = indexByCursor.get(frame.cursor);
    if (index === undefined) {
      indexByCursor.set(frame.cursor, next.length);
      next.push(frame);
    } else if (frame.mode === "append") {
      next[index] = appendFrame(next[index], frame);
    } else {
      next[index] = frame;
    }
  }
  return next.sort((left, right) => left.cursor - right.cursor);
}

export function flattenFrames(frames) {
  return (frames || []).flatMap((frame) => frame.events || []);
}

function appendFrame(existing, patch) {
  const currentEvents = [...(existing.events || [])];
  const byID = new Map(currentEvents.map((event, index) => [event.id, index]));
  for (const event of patch.events || []) {
    const index = byID.get(event.id);
    if (index === undefined) {
      byID.set(event.id, currentEvents.length);
      currentEvents.push(event);
      continue;
    }
    currentEvents[index] = appendSemanticEvent(currentEvents[index], event);
  }
  return { ...existing, source: { ...existing.source, ...patch.source }, events: currentEvents, mode: "replace" };
}

function appendSemanticEvent(existing, patch) {
  if (patch.type === "message.assistant.delta" || patch.type === "message.reasoning.delta") {
    return {
      ...existing,
      time: patch.time || existing.time,
      data: { ...existing.data, text: `${existing.data?.text || ""}${patch.data?.text || ""}` },
    };
  }
  if (patch.type === "tool.call" && patch.data?.output?.mode === "append") {
    const previous = existing.data?.output?.text || "";
    return {
      ...existing,
      time: patch.time || existing.time,
      data: {
        ...existing.data,
        ...patch.data,
        output: { ...patch.data.output, mode: "replace", text: previous + (patch.data.output.text || "") },
      },
    };
  }
  return { ...existing, ...patch, data: { ...existing.data, ...patch.data } };
}
