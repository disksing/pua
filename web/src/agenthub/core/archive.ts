// @ts-nocheck -- small compatibility helper covered by AgentHub UI tests.
// Archive helpers shared by the session list and the details panel. Kept
// free of React so the node test runner can exercise the rules directly.

export const ARCHIVED_STATE = "archived";

// stopped is the only trustworthy provider-release boundary. The daemon
// re-checks the turn and approval invariants before moving any files.
export const ARCHIVABLE_STATES = new Set(["stopped"]);

export function isArchived(session) {
  return session?.state === ARCHIVED_STATE;
}

export function isArchivable(session) {
  if (!session || isArchived(session)) return false;
  if (!ARCHIVABLE_STATES.has(session.state)) return false;
  if (session.currentTurnId) return false;
  if (session.pendingApprovalIds?.length) return false;
  return true;
}

// archiveDisabledReason explains why the Archive action is unavailable, for
// tooltips and aria descriptions. Returns "" when archiving is allowed.
export function archiveDisabledReason(session) {
  if (!session) return "No session is selected.";
  if (isArchived(session)) return "This session is already archived.";
  if (session.currentTurnId) return "Wait for the running turn to finish before archiving.";
  if (session.pendingApprovalIds?.length) return "Resolve the pending approval before archiving.";
  if (!ARCHIVABLE_STATES.has(session.state)) {
    return `Stop the session before archiving (current state: ${session.state || "unknown"}).`;
  }
  return "";
}

// sessionsQuery is the explicit list contract: the default list hides
// archived sessions; archivedOnly lists only them.
export function sessionsQuery({ archivedOnly = false, state = [], sourceApp = "", sourceInstanceId = "", sourceExternalId = "", limit = 50, cursor = "" }: {
  archivedOnly?: boolean;
  state?: string[];
  sourceApp?: string;
  sourceInstanceId?: string;
  sourceExternalId?: string;
  limit?: number;
  cursor?: string;
} = {}) {
  const query = new URLSearchParams();
  if (archivedOnly) query.set("archived", "true");
  if (state.length) query.set("state", state.join(","));
  if (sourceApp) query.set("sourceApp", sourceApp);
  if (sourceInstanceId) query.set("sourceInstanceId", sourceInstanceId);
  if (sourceExternalId) query.set("sourceExternalId", sourceExternalId);
  query.set("limit", String(limit));
  if (cursor) query.set("cursor", cursor);
  return `/v1/sessions?${query}`;
}

// pickActiveAfterArchive resolves the workspace selection after a session was
// archived from the list. Archiving a session that is not selected keeps the
// current selection; archiving the selected session moves to the first
// remaining active session, or to the empty state when none are left.
export function pickActiveAfterArchive(sessions, archivedId, activeId) {
  if (!archivedId || activeId !== archivedId) return activeId;
  const remaining = (sessions || []).filter((item) => item.id !== archivedId);
  return remaining[0]?.id || "";
}

// archiveListError formats the inline-list archive failure banner. It always
// names the session (falling back to the stable ID) and keeps the server
// reason so state conflicts stay actionable.
export function archiveListError(session, reason) {
  const label = session?.title || session?.id || "session";
  return `Failed to archive "${label}": ${reason || "unknown error"}`;
}

const STOP_REASON_LABELS = {
  requested: "requested",
  completed: "provider completed",
  provider_error: "provider error",
  startup_error: "startup error",
  daemon_recovery: "daemon recovery",
};

export function sessionStatusLabel(session) {
  if (!session?.state) return "No session yet";
  const state = session.state === "waiting_approval" ? "waiting approval" : session.state.replaceAll("_", " ");
  const reason = session.state === "stopped" ? STOP_REASON_LABELS[session.stopReason] : "";
  return reason ? `${state} · ${reason}` : state;
}
