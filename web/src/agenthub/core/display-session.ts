import type { AgentHubSession } from "../types";

const STOP_REASON_LABELS: Record<string, string> = {
  requested: "requested",
  completed: "provider completed",
  provider_error: "provider error",
  startup_error: "startup error",
  daemon_recovery: "daemon recovery",
};

export function sessionStatusLabel(session: AgentHubSession | undefined): string {
  if (!session?.state) return "Unknown";
  const state = session.state === "waiting_approval" ? "Waiting approval" : titleCase(session.state.replaceAll("_", " "));
  const reason = session.state === "stopped" ? STOP_REASON_LABELS[session.stopReason || ""] : "";
  return reason ? `${state} · ${reason}` : state;
}

export function sessionStatusTone(session: AgentHubSession): string {
  if (session.state === "running") return "running";
  if (session.state === "waiting_approval") return "attention";
  if (session.state === "starting" || session.state === "stopping") return "transition";
  if (session.state === "stopped" && ["provider_error", "startup_error", "daemon_recovery"].includes(session.stopReason || "")) return "error";
  if (session.state === "archived") return "archived";
  if (session.state === "ready") return "ready";
  return "stopped";
}

export function formatDateTime(value: string | undefined): string {
  const date = new Date(value || "");
  if (Number.isNaN(date.valueOf())) return "—";
  return date.toLocaleString("en-US", { month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" });
}

export function relativeTime(value: string | undefined, now = Date.now()): string {
  const timestamp = new Date(value || "").valueOf();
  if (!Number.isFinite(timestamp)) return "—";
  const seconds = Math.max(0, Math.round((now - timestamp) / 1000));
  if (seconds < 10) return "just now";
  if (seconds < 60) return `${seconds}s ago`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m ago`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h ago`;
  const days = Math.floor(hours / 24);
  return `${days}d ago`;
}

function titleCase(value: string): string {
  return value ? value[0].toUpperCase() + value.slice(1) : value;
}
