import type { ConversationBlock, ResourceMessageStatus } from "./models";

const ACTIVE_GENERATION_STATUSES = new Set([
  "starting",
  "running",
  "waiting_approval",
  "stopping",
  "recovering",
]);

const RESTING_GENERATION_STATUSES = new Set([
  "idle",
  "idle-suspended",
  "stopped",
  "failed",
  "archived",
]);

function normalized(value: string | undefined): string {
  return String(value || "").trim();
}

export function turnIsWorking(status: ResourceMessageStatus | null): boolean {
  return status?.session?.state === "running" && Boolean(status.session.currentTurnId);
}

/**
 * Projects one Generation's status for the live conversation.
 *
 * Historical Generations keep the status captured by History. The current
 * Generation also has a live resource status; use it to repair a cached
 * History value when it disagrees about whether the resource is active.
 */
export function effectiveGenerationStatus(block: Pick<ConversationBlock, "generation">, status: ResourceMessageStatus | null): string {
  const historyStatus = normalized(block.generation.status) || "unknown";
  const liveGeneration = status?.generation;
  if (!liveGeneration || liveGeneration.generationId !== block.generation.generationId) return historyStatus;

  const liveStatus = normalized(liveGeneration.status);
  if (!status?.sessionState) return liveStatus || historyStatus;

  switch (status.sessionState) {
    case "working":
      return ACTIVE_GENERATION_STATUSES.has(liveStatus) ? liveStatus : "running";
    case "attention_required":
      return liveStatus === "waiting_approval" ? liveStatus : "waiting_approval";
    case "idle":
      return RESTING_GENERATION_STATUSES.has(liveStatus) ? liveStatus : "idle";
    case "archived":
      return "archived";
    case "unavailable":
      return liveStatus === "failed" || liveStatus === "recovering" ? liveStatus : "failed";
    default:
      return liveStatus || historyStatus;
  }
}
