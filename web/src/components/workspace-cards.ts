import type { ResourceRecord, WorkspaceTree } from "../models/workspace";
import { writable } from "svelte/store";

export const cardObservation = writable<{workspace:string; user:string; tree:WorkspaceTree|null} | null>(null);

export const styles = { stat: "Stat", gauge: "Gauge", pie: "Pie Chart", trend: "Trend", list: "List", html: "HTML", quota: "Quota" } as const;
export type Kind = keyof typeof styles;
export const sources: Record<Kind, string[]> = {
  stat: ["running", "running-tasks", "tasks"], trend: ["running", "running-tasks", "tasks"],
  list: ["running", "running-tasks", "tasks"], gauge: ["task-ratio"], pie: ["task-states"], html: [], quota: ["magpie-quota"],
};
export const labels: Record<string, string> = {
  running: "Running resources", "running-tasks": "Running tasks", tasks: "All tasks",
  "task-ratio": "Running share of tasks", "task-states": "Task states", agent: "Agent publication",
  "magpie-quota": "Magpie usage",
};
export interface Item { label: string; resource?: string; url?: string; value?: number }
export interface Point { time: string; value: number }
interface QuotaWindow {
  key?: string; windowPositionPercent?: number; resetInSeconds?: number; expired?: boolean;
  label: string; remainingPercent: number | null; display?: string; unlimited?: boolean;
  resetsAt?: string; stale?: boolean; used?: number; limit?: number; unit?: string;
}
export interface QuotaSnapshot {
  configured: boolean; connected: boolean; stale?: boolean;
  providers: {id?: string; label: string; accountLabel?: string; planLabel?: string; stale?: boolean; status: string; quotas: QuotaWindow[]}[];
}
export function quotaEntryKey(provider: QuotaSnapshot["providers"][number], window: QuotaWindow) {
  return JSON.stringify([provider.id ?? [provider.label, provider.accountLabel], window.key ?? window.label]);
}
export function visibleQuotaProviders(quota: QuotaSnapshot, hiddenKeys: string[]) {
  if (!hiddenKeys.length) return quota.providers;
  const hidden = new Set(hiddenKeys);
  return quota.providers.flatMap(provider => {
    if (!provider.quotas?.length) return [provider];
    const quotas = provider.quotas.filter(window => !hidden.has(quotaEntryKey(provider, window)));
    return quotas.length ? [{...provider, quotas}] : [];
  });
}
export function quotaStatus(quota: QuotaSnapshot) {
  if (!quota.configured) return "Magpie not configured";
  if (!quota.connected) return "Magpie disconnected";
  if (!quota.providers.length) return "No quota readings";
  return "";
}
export function quotaValue(window: QuotaWindow) {
  if (window.unlimited) return "Unlimited";
  if (window.remainingPercent != null) return `${Number(window.remainingPercent.toFixed(1))}% left`;
  if (window.display) return window.display;
  if (window.used != null && window.limit != null) return `${window.used} / ${window.limit} ${window.unit ?? ""}`.trim();
  return "Unavailable";
}
export interface Content { value?: number; max?: number; items?: Item[]; points?: Point[]; html?: string; quota?: QuotaSnapshot }
export interface Card {
  id: string; title: string; kind: Kind; source: string; version: number;
  collapsed: boolean; height?: number; content?: Content; updated?: string;
  hiddenQuotaKeys?: string[];
}
export function collapsedReadout(kind: Kind, data: Content | null, hiddenKeys: string[] = []) {
  if (!data) return "—";
  switch (kind) {
    case "stat":
    case "trend": return String(data.value ?? data.points?.at(-1)?.value ?? "—");
    case "gauge": return data.max && data.value != null ? `${Math.round(100 * data.value / data.max)}%` : "—";
    case "list": return String(data.items?.length ?? 0);
    case "pie": return String(data.items?.reduce((sum, item) => sum + (item.value ?? 0), 0) ?? 0);
    case "html": return "";
    case "quota": {
      if (!data.quota) return "Unavailable";
      const count = visibleQuotaProviders(data.quota, hiddenKeys).length;
      return quotaStatus(data.quota) || (count ? `${count} ${count === 1 ? "account" : "accounts"}` : "All entries hidden");
    }
  }
}
export function summarize(tree: WorkspaceTree) {
  const resources: ResourceRecord[] = [];
  function collect(item?: ResourceRecord) {
    if (!item || item.archived) return;
    resources.push(item);
    item.children?.forEach(collect);
  }
  collect(tree.workspace);
  collect(tree.scheduler);
  tree.projects.forEach(collect);
  const tasks = resources.filter(item => item.type === "task");
  const running = resources.filter(item => item.runtime?.activeTurn);
  const bySource: Record<string, ResourceRecord[]> = {tasks, running, "running-tasks": tasks.filter(item => item.runtime?.activeTurn)};
  const states = new Map<string, number>();
  for (const item of tasks) {
    const state = item.state || "unknown";
    states.set(state, (states.get(state) || 0) + 1);
  }
  return {bySource, states};
}
export type Summary = ReturnType<typeof summarize>;
export interface Sample { time: string; counts: Record<string, number> }
export function project(card: Card, summary: Summary | null, samples: Sample[]): Content | null {
  if (card.source === "agent") return card.content ?? null;
  if (!summary) return null;
  const {bySource, states} = summary;
  if (card.kind === "gauge") return {value: bySource["running-tasks"].length, max: bySource.tasks.length};
  if (card.kind === "pie") return {items: Array.from(states, ([label,value]) => ({label,value}))};
  const rows = bySource[card.source];
  if (!rows) return null;
  if (card.kind === "list") return {items: rows.map(item => ({label: item.title || item.id, resource: item.id}))};
  return {value: rows.length, points: samples.map(sample => ({time: sample.time, value: sample.counts[card.source]}))};
}
