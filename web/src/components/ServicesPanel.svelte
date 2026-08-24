<script lang="ts">
  import { onMount } from "svelte";
  import { ApiClient, StaleResponseError } from "../api/client";
  import type { ServiceStatus } from "../api/types";
  import Icon from "./Icon.svelte";
  import "./ServicesPanel.css";

  interface ServiceRow {
    workspaceId: string;
    service: ServiceStatus;
  }

  let { workspaceId, onToast }: { workspaceId: string; onToast: (message: string) => void } = $props();
  const client = new ApiClient();
  let activeWorkspaceId = $state("");
  let rows = $state<ServiceRow[]>([]);
  let loading = $state(true);
  let pending = $state("");
  let logs = $state<{ id: string; text: string } | null>(null);
  let listVersion = 0;
  let actionVersion = 0;
  let logsVersion = 0;

  function requestScope(requestWorkspaceId: string, operation: "list" | "mutation" | "logs"): string {
    return `workspace-services:${requestWorkspaceId}:${operation}`;
  }

  function isCurrentWorkspace(requestWorkspaceId: string): boolean {
    return Boolean(requestWorkspaceId) && activeWorkspaceId === requestWorkspaceId && workspaceId === requestWorkspaceId;
  }

  function abortWorkspaceRequests(requestWorkspaceId: string): void {
    client.requests.abort(requestScope(requestWorkspaceId, "list"));
    client.requests.abort(requestScope(requestWorkspaceId, "mutation"));
    client.requests.abort(requestScope(requestWorkspaceId, "logs"));
  }

  function isStale(error: unknown, requestWorkspaceId: string): boolean {
    return error instanceof StaleResponseError || !isCurrentWorkspace(requestWorkspaceId);
  }

  async function refresh(requestWorkspaceId: string): Promise<void> {
    if (!isCurrentWorkspace(requestWorkspaceId)) return;
    const version = ++listVersion;
    loading = true;
    try {
      const value = await client.latest<{ services: ServiceStatus[] }>(`/api/workspaces/${encodeURIComponent(requestWorkspaceId)}/services`, {
        scope: requestScope(requestWorkspaceId, "list"),
      });
      if (!isCurrentWorkspace(requestWorkspaceId) || version !== listVersion) return;
      rows = (value.services || []).map((service) => ({ workspaceId: requestWorkspaceId, service }));
    } catch (error) {
      if (isStale(error, requestWorkspaceId) || version !== listVersion) return;
      onToast(error instanceof Error ? error.message : String(error));
    } finally {
      if (isCurrentWorkspace(requestWorkspaceId) && version === listVersion) loading = false;
    }
  }

  async function action(row: ServiceRow, name: "enable" | "disable" | "start" | "stop" | "restart"): Promise<void> {
    const requestWorkspaceId = row.workspaceId;
    if (!isCurrentWorkspace(requestWorkspaceId) || !rows.includes(row) || pending) return;
    const version = ++actionVersion;
    pending = `${requestWorkspaceId}:${name}:${row.service.id}`;
    try {
      await client.latest(`/api/workspaces/${encodeURIComponent(requestWorkspaceId)}/services/${encodeURIComponent(row.service.id)}/${name}`, {
        method: "POST",
        scope: requestScope(requestWorkspaceId, "mutation"),
      });
      if (!isCurrentWorkspace(requestWorkspaceId) || version !== actionVersion) return;
      await refresh(requestWorkspaceId);
    } catch (error) {
      if (isStale(error, requestWorkspaceId) || version !== actionVersion) return;
      onToast(error instanceof Error ? error.message : String(error));
    } finally {
      if (isCurrentWorkspace(requestWorkspaceId) && version === actionVersion) pending = "";
    }
  }

  async function showLogs(row: ServiceRow): Promise<void> {
    const requestWorkspaceId = row.workspaceId;
    if (!isCurrentWorkspace(requestWorkspaceId) || !rows.includes(row)) return;
    const version = ++logsVersion;
    try {
      const text = await client.latest<string>(`/api/workspaces/${encodeURIComponent(requestWorkspaceId)}/services/${encodeURIComponent(row.service.id)}/logs`, {
        scope: requestScope(requestWorkspaceId, "logs"),
      });
      if (!isCurrentWorkspace(requestWorkspaceId) || version !== logsVersion) return;
      logs = { id: row.service.id, text };
    } catch (error) {
      if (isStale(error, requestWorkspaceId) || version !== logsVersion) return;
      onToast(error instanceof Error ? error.message : String(error));
    }
  }

  $effect(() => {
    const nextWorkspaceId = workspaceId;
    if (nextWorkspaceId === activeWorkspaceId) return;
    if (activeWorkspaceId) abortWorkspaceRequests(activeWorkspaceId);
    activeWorkspaceId = nextWorkspaceId;
    listVersion += 1;
    actionVersion += 1;
    logsVersion += 1;
    rows = [];
    loading = Boolean(nextWorkspaceId);
    pending = "";
    logs = null;
    if (nextWorkspaceId) void refresh(nextWorkspaceId);
  });

  onMount(() => {
    const timer = window.setInterval(() => {
      const requestWorkspaceId = activeWorkspaceId;
      if (requestWorkspaceId) void refresh(requestWorkspaceId);
    }, 5000);
    return () => { window.clearInterval(timer); client.dispose(); };
  });
</script>

<div class="services-panel" data-component-owner="services-panel">
  <div class="services-heading"><div><h2>Services</h2><p>Workspace services are supervised by the owning <code>pua serve</code> process.</p></div><button class="secondary-button" type="button" onclick={() => void refresh(activeWorkspaceId)} disabled={loading}><Icon name="refresh-cw" /><span>Refresh</span></button></div>
  {#if loading && rows.length === 0}<div class="services-empty"><Icon name="loader-circle" /><span>Loading services…</span></div>
  {:else if rows.length === 0}<div class="services-empty"><Icon name="server-off" /><span>No services configured.</span></div>
  {:else}<div class="services-list">
    {#each rows as row (`${row.workspaceId}:${row.service.id}`)}
      {@const service = row.service}
      <section class="service-card" class:attention={service.attentionRequired}>
        <header><div><h3>{service.id}</h3><span class="service-state">{service.state}</span></div><div class="service-actions">{#if service.enabled}<button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => void action(row, "start")}><Icon name="play" /><span>Start</span></button><button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => void action(row, "restart")}><Icon name="rotate-ccw" /><span>Restart</span></button><button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => void action(row, "stop")}><Icon name="square" /><span>Stop</span></button><button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => void action(row, "disable")}><Icon name="power" /><span>Disable</span></button>{:else}<button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => void action(row, "enable")}><Icon name="power" /><span>Enable</span></button>{/if}<button type="button" class="secondary-button" onclick={() => void showLogs(row)}><Icon name="scroll-text" /><span>Logs</span></button></div></header>
        <dl><div><dt>Dependencies</dt><dd>{service.dependencies?.join(", ") || "None"}</dd></div><div><dt>Readiness</dt><dd>{service.readiness.ready ? "Ready" : service.readiness.configured ? (service.readiness.lastError || "Waiting") : "Not configured"}</dd></div><div><dt>Cleanup</dt><dd>{service.cleanup.configured ? (service.cleanup.succeeded ? "Succeeded" : service.cleanup.lastError || "Pending") : "Not configured"}</dd></div>{#if service.nextRetryAt}<div><dt>Next retry</dt><dd>{service.nextRetryAt}</dd></div>{/if}</dl>
        {#if service.lastError}<p class="service-error">{service.lastError}</p>{/if}
        {#if service.exports.secrets?.length}<p class="service-secrets">Secrets: {service.exports.secrets.map((secret) => secret.name).join(", ")}</p>{/if}
      </section>
    {/each}
  </div>{/if}
  {#if logs}<div class="service-log-view"><header><h3>{logs.id} logs</h3><button type="button" class="icon-button" aria-label="Close logs" onclick={() => logs = null}><Icon name="x" /></button></header><pre>{logs.text}</pre></div>{/if}
</div>
