// @ts-nocheck -- product-specific projector retained independently from PUA.
// Timeline builder: turns AgentHub semantic events into UI-neutral
// timeline items. This module intentionally has no DOM, React, networking, or
// host-specific formatting dependencies.
//
// Rebuild from the complete ordered event sequence whenever a REST page fills
// a gap or an SSE event arrives. That keeps history and live projections
// identical and lets late tool updates settle earlier visible groups.

const MAX_PREVIEW = 400;
const MAX_OUTPUT = 12000;

function truncateText(value, max = MAX_PREVIEW) {
  const text = String(value ?? "");
  return text.length > max ? `${text.slice(0, max - 1)}…` : text;
}

function safePreview(data) {
  if (data === undefined || data === null) return "";
  try {
    return truncateText(JSON.stringify(data));
  } catch {
    return "";
  }
}

// Humanizes identifiers such as "mcpToolCall" or "web_search" into
// "Mcp tool call" style labels.
function humanizeName(value) {
  const text = String(value || "")
    .replace(/[_-]+/g, " ")
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .trim();
  if (!text) return "";
  return text.charAt(0).toUpperCase() + text.slice(1);
}

function joinCommand(value) {
  if (Array.isArray(value)) return value.filter((part) => typeof part === "string").join(" ");
  return typeof value === "string" ? value : "";
}

function firstString(...values) {
  for (const value of values) {
    if (typeof value === "string" && value.trim()) return value.trim();
  }
  return "";
}

const MESSAGE_ROLES = new Set(["user", "system", "agent", "assistant"]);

function normalizeMessageSender(value) {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const sender = {};
  for (const key of ["id", "name", "sessionId"]) {
    if (typeof value[key] === "string" && value[key].trim()) sender[key] = value[key].trim();
  }
  return Object.keys(sender).length ? sender : undefined;
}

function normalizeMessageRole(value) {
  const role = typeof value === "string" ? value.trim().toLowerCase() : "";
  return MESSAGE_ROLES.has(role) ? role : "user";
}

function normalizeToolStatus(status) {
  const value = String(status || "").toLowerCase();
  if (["completed", "complete", "done", "success", "succeeded"].includes(value)) return "completed";
  if (["failed", "failure", "error", "declined", "denied", "cancelled", "canceled"].includes(value)) return "failed";
  return "running";
}

function contentText(content) {
  if (!Array.isArray(content)) return "";
  const parts = [];
  for (const block of content) {
    if (typeof block?.text === "string") parts.push(block.text);
    else if (typeof block?.content?.text === "string") parts.push(block.content.text);
    else if (block?.type === "diff" && typeof block?.path === "string") parts.push(`Edit ${block.path}`);
  }
  return parts.filter(Boolean).join("\n");
}

function parseToolCall(event) {
  const data = event?.data ?? {};
  const output = data.output && typeof data.output === "object" ? data.output : {};
  const error = data.error && typeof data.error === "object" ? data.error : {};
  return {
    callId: firstString(data.callId),
    method: firstString(data.operation),
    time: event?.time || "",
    name: firstString(data.name, data.toolKind ? humanizeName(data.toolKind) : ""),
    status: normalizeToolStatus(data.status),
    summary: truncateText(firstString(data.summary).replace(/\s+/g, " ").trim(), 120),
    output: truncateText(firstString(output.text), MAX_OUTPUT),
    outputMode: output.mode === "append" ? "append" : "replace",
    error: firstString(error.message),
    operation: firstString(data.operation, "update"),
  };
}

function summarizeApproval(event) {
  const data = event?.data ?? {};
  const options = Array.isArray(data.options)
    ? data.options
      .map((option) => ({
        optionId: firstString(option?.optionId),
        name: firstString(option?.label),
        kind: firstString(option?.kind),
      }))
      .filter((option) => option.optionId)
    : [];
  return {
    title: firstString(data.title, "Approval requested"),
    detail: firstString(data.detail),
    question: firstString(data.question),
    options,
  };
}

const DECISION_LABELS = {
  accept: "Allowed",
  acceptForSession: "Allowed for this session",
  decline: "Declined",
  cancel: "Cancelled",
};

const NOTABLE_STATES = {
  failed: "Session failed",
  stopping: "Stopping provider",
  stopped: "Session stopped",
  archived: "Session archived",
};

const STOP_REASON_TIMELINE = {
  requested: "requested",
  completed: "provider completed",
  provider_error: "provider error",
  startup_error: "startup error",
  daemon_recovery: "daemon recovery",
};

// Low-value provider notifications and internal delivery facts remain in the
// durable event log but are intentionally omitted from the conversation
// timeline. In addition to being noisy, projecting them would split otherwise
// consecutive tool calls or surface transport state as a user message.
// provider.turn.* mirrors the manager-level turn.started/turn.completed
// lifecycle events, which are the ones rendered as timeline notes.
function isActivityType(type) {
  return type === "semantic.empty" || type === "message.delivery" || type === "provider.event" || type === "provider.metadata" || type === "plan.event" ||
    type === "provider.stderr" || type === "provider.turn.started" || type === "provider.turn.completed" ||
    type.startsWith("provider.process.");
}

function mergeToolCall(previous, update) {
  const next = { ...previous };
  if (update.name) next.name = update.name;
  if (update.summary) next.summary = update.summary;
  if (update.status) next.status = update.status;
  if (update.error) next.error = update.error;
  if (update.outputMode === "append") {
    next.output = truncateText((next.output || "") + (update.output || ""), MAX_OUTPUT);
  } else if (update.output) {
    next.output = update.output;
  }
  next.time = update.time || previous.time;
  next.key = previous.key;
  return next;
}

function newToolCall(update, event) {
  return {
    key: event.id,
    callId: update.callId || "",
    name: update.name || "Tool",
    summary: update.summary || "",
    status: update.status || "completed",
    output: update.output || "",
    error: update.error || "",
    method: update.method || "",
    time: update.time || event.time || "",
  };
}

export function buildTimeline(events) {
  const items = [];
  const approvalsById = new Map();
  const openToolCalls = new Map();
  // The thinking block the agent is currently reasoning into. Unlike tool
  // calls it is not tied to the tail of `items`: a steered message may be
  // appended while the agent keeps thinking, and later reasoning deltas must
  // still merge into the same block instead of starting a second one.
  let openThinking = null;

  const settleOpenToolCalls = (status, time) => {
    for (const { call, group } of openToolCalls.values()) {
      call.status = status;
      call.time = time || call.time;
      group.time = time || group.time;
    }
    openToolCalls.clear();
  };

  for (const event of events || []) {
    const type = event?.type || "";
    const data = event?.data ?? {};
    const time = event?.time || "";
    switch (type) {
      case "message.input": {
        const item = {
          kind: "message",
          role: normalizeMessageRole(data.role),
          key: event.id,
          time,
          steer: data.steer === true,
          text: typeof data.text === "string" ? data.text : "",
        };
        if (event.turnId) item.turnId = event.turnId;
        if (Object.prototype.hasOwnProperty.call(data, "payload")) item.payload = data.payload;
        // A message that starts a new turn ends any thinking still open from
        // the previous one. A message steered into the current turn (same
        // turnId) does not: the agent keeps reasoning and later deltas merge
        // back into the open block.
        if (openThinking && event.turnId && openThinking.turnId !== event.turnId) {
          openThinking = null;
        }
        const sender = normalizeMessageSender(data.sender);
        if (sender) item.sender = sender;
        items.push(item);
        break;
      }
      case "message.user":
      case "message.user.steer":
        if (openThinking && event.turnId && openThinking.turnId !== event.turnId) {
          openThinking = null;
        }
        items.push({
          kind: "message", role: "user", key: event.id, time,
          steer: type === "message.user.steer",
          text: typeof data.text === "string" ? data.text : "",
        });
        break;
      case "message.assistant.delta": {
        // Assistant text ends the reasoning phase.
        openThinking = null;
        const last = items.at(-1);
        const text = typeof data.text === "string" ? data.text : "";
        // Deltas that arrive after a turn terminal (for example a late
        // provider response to a timed-out prompt) carry no turnId. Normalize
        // the missing value so consecutive chunks still merge into one
        // message instead of one bubble per delta.
        const turnId = event.turnId || "";
        if (last?.kind === "message" && last.role === "assistant" && last.turnId === turnId) {
          last.text += text;
          last.time = time;
        } else if (text) {
          items.push({ kind: "message", role: "assistant", key: event.id, turnId, text, time });
        }
        break;
      }
      case "message.reasoning.delta": {
        const text = typeof data.text === "string" ? data.text : "";
        const turnId = event.turnId || "";
        if (openThinking && openThinking.turnId !== turnId) openThinking = null;
        if (openThinking) {
          openThinking.text += text;
          openThinking.time = time;
          openThinking.count += 1;
        } else if (text) {
          // startTime pins the first delta so the collapsed header can show
          // how long the agent reasoned; time keeps tracking the last delta.
          openThinking = {
            kind: "thinking", key: event.id, turnId, text,
            time, startTime: event.startTime || time, active: false, count: 1,
          };
          items.push(openThinking);
        }
        break;
      }
      case "tool.call": {
        const update = parseToolCall(event);
        if (!update) break;
        // Tool activity ends the reasoning phase that preceded it.
        openThinking = null;
        const last = items.at(-1);
        const group = last?.kind === "tools" ? last : null;
        const callKey = update.callId ? `${event.turnId || ""}:${update.callId}` : "";
        const existing = callKey ? openToolCalls.get(callKey) : null;
        if (existing) {
          Object.assign(existing.call, mergeToolCall(existing.call, update));
          existing.group.time = time;
          if (existing.call.status !== "running") {
            openToolCalls.delete(callKey);
          }
        } else {
          // A client may intentionally project only a recent tail of a long
          // session. In that case a command that started before the loaded
          // window can still emit output deltas inside it. A delta is only an
          // update to an existing call, never a standalone tool call; ignore
          // it when its start is not present instead of creating noisy
          // synthetic "Tool" rows that can also split assistant text.
          if (update.operation === "update" && update.outputMode === "append") break;
          const targetGroup = group || { kind: "tools", key: event.id, calls: [], time };
          const call = newToolCall(update, event);
          targetGroup.calls.push(call);
          targetGroup.time = time;
          if (!group) items.push(targetGroup);
          if (call.callId && call.status === "running") {
            openToolCalls.set(callKey, { call, group: targetGroup });
          }
        }
        break;
      }
      case "approval.requested": {
        const { title, detail, question, options } = summarizeApproval(event);
        const item = {
          kind: "approval", key: event.id, time,
          approvalId: firstString(data.approvalId),
          title, detail, question, options,
          status: "pending",
          decision: "",
          reply: "",
        };
        if (item.approvalId) approvalsById.set(item.approvalId, item);
        items.push(item);
        break;
      }
      case "approval.resolved": {
        const approvalId = firstString(data.approvalId);
        const decision = firstString(data.decision) || "decline";
        const optionId = firstString(data.optionId);
        const text = firstString(data.text);
        const target = approvalId ? approvalsById.get(approvalId) : null;
        const label = (item) => {
          if (decision === "text") return "Replied";
          if (optionId) {
            const option = item?.options?.find((entry) => entry.optionId === optionId);
            return `Answered: ${option?.name || optionId}`;
          }
          return DECISION_LABELS[decision] || humanizeName(decision);
        };
        const accepted = decision === "accept" || decision === "acceptForSession" || decision === "text";
        if (target) {
          target.status = accepted ? "accepted" : "declined";
          target.decision = label(target);
          target.reply = decision === "text" ? text : "";
          target.time = time;
        } else {
          items.push({
            kind: "approval", key: event.id, time, approvalId,
            title: "Approval resolved",
            detail: "",
            question: "",
            options: [],
            status: accepted ? "accepted" : "declined",
            decision: label(null),
            reply: decision === "text" ? text : "",
          });
        }
        break;
      }
      case "provider.error":
        {
          const message = firstString(data.message, "The provider reported an error");
          const details = firstString(data.details);
          const text = details && details !== message ? `${message} · ${details}` : message;
          if (data.retryable === true) {
            items.push({
              kind: "lifecycle", tone: "info", key: event.id, time,
              text,
            });
          } else {
            items.push({ kind: "error", key: event.id, time, text });
          }
        }
        break;
      case "turn.started":
        openThinking = null;
        items.push({ kind: "lifecycle", tone: "muted", key: event.id, time, text: "Turn started" });
        break;
      case "turn.completed":
        openThinking = null;
        settleOpenToolCalls("completed", time);
        items.push({ kind: "lifecycle", tone: "ok", key: event.id, time, text: "Turn completed" });
        break;
      case "turn.failed":
        openThinking = null;
        settleOpenToolCalls("failed", time);
        items.push({
          kind: "lifecycle", tone: "danger", key: event.id, time,
          text: `Turn failed${firstString(data.error, data.message) ? `: ${firstString(data.error, data.message)}` : ""}`,
        });
        break;
      case "turn.cancelled":
        openThinking = null;
        settleOpenToolCalls("failed", time);
        items.push({ kind: "lifecycle", tone: "muted", key: event.id, time, text: "Turn interrupted" });
        break;
      case "session.created":
        items.push({ kind: "lifecycle", tone: "muted", key: event.id, time, text: "Session created" });
        break;
      case "session.provider": {
        const agent = firstString(data.agentName);
        const providerName = firstString(data.provider);
        const parts = ["Agent connected"];
        if (agent) parts.push(agent);
        if (providerName) parts.push(`via ${providerName}`);
        items.push({ kind: "lifecycle", tone: "muted", key: event.id, time, text: parts.join(" · ") });
        break;
      }
      case "session.state": {
        let label = NOTABLE_STATES[data.state];
        if (data.state === "failed") {
          openThinking = null;
          settleOpenToolCalls("failed", time);
        } else if (data.state === "stopped") {
          openThinking = null;
          settleOpenToolCalls(data.reason === "completed" ? "completed" : "failed", time);
        }
        if (data.state === "stopped" && STOP_REASON_TIMELINE[data.reason]) {
          label += ` · ${STOP_REASON_TIMELINE[data.reason]}`;
        }
        const danger = data.state === "failed" || data.reason === "provider_error" || data.reason === "startup_error";
        if (label) items.push({ kind: "lifecycle", tone: danger ? "danger" : "muted", key: event.id, time, text: label });
        break;
      }
      case "session.archived":
        openThinking = null;
        items.push({ kind: "lifecycle", tone: "muted", key: event.id, time, text: "Session archived" });
        break;
      default: {
        if (isActivityType(type)) {
          break;
        } else {
          items.push({
            kind: "unknown", key: event.id, time, type: type || "unknown",
            preview: safePreview(data),
          });
        }
      }
    }
  }

  // A thinking block still open at the end of the projected window means the
  // agent is still reasoning. It does not have to be the last item: a steered
  // message may have been appended after it.
  if (openThinking) openThinking.active = true;

  return groupActivities(items);
}

// groupActivities is deliberately the last projection pass. Thinking and tool
// parsing retain their precise provider-specific behavior above; this pass only
// wraps each uninterrupted visible run while preserving its child order and
// details for hosts to expand directly.
function groupActivities(items) {
  const grouped = [];
  for (const item of items) {
    if (item?.kind !== "thinking" && item?.kind !== "tools") {
      grouped.push(item);
      continue;
    }
    const previous = grouped.at(-1);
    const activity = previous?.kind === "activity"
      ? previous
      : {
          kind: "activity",
          key: item.key,
          items: [],
          time: item.time || "",
          startTime: item.startTime || item.time || "",
          thinkingCount: 0,
          reasoningUpdateCount: 0,
          toolCallCount: 0,
          active: false,
        };
    activity.items.push(item);
    activity.time = laterTime(activity.time, item.time);
    if (item.kind === "thinking") {
      activity.thinkingCount += 1;
      activity.reasoningUpdateCount += Math.max(1, Number(item.count) || 1);
      activity.active ||= item.active === true;
    } else {
      activity.toolCallCount += item.calls?.length || 0;
      activity.active ||= item.calls?.some((call) => call.status === "running") || false;
    }
    if (previous !== activity) grouped.push(activity);
  }
  // A completed tool frame can be the newest event while the Turn is still
  // open. Keep that tail activity expanded until a visible boundary (reply,
  // approval, lifecycle terminal, etc.) arrives; running child state alone
  // would fold it prematurely between provider frames.
  if (grouped.at(-1)?.kind === "activity") grouped.at(-1).active = true;
  return grouped;
}

function laterTime(current, candidate) {
  if (!current) return candidate || "";
  if (!candidate) return current;
  const currentTime = Date.parse(current);
  const candidateTime = Date.parse(candidate);
  if (!Number.isFinite(currentTime) || !Number.isFinite(candidateTime)) return candidate;
  return candidateTime >= currentTime ? candidate : current;
}
