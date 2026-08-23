<script lang="ts">
  import { onMount } from "svelte";
  import { ApiClient } from "../api/client";
  import type { ServiceStatus } from "../api/types";
  import Icon from "./Icon.svelte";
  import "./ServicesPanel.css";

  let { workspaceId, onToast }: { workspaceId: string; onToast: (message: string) => void } = $props();
  const client = new ApiClient();
  let services = $state<ServiceStatus[]>([]);
  let loading = $state(true);
  let pending = $state("");
  let logs = $state<{ id: string; text: string } | null>(null);

  async function refresh(): Promise<void> {
    loading = true;
    try {
      const value = await client.request<{ services: ServiceStatus[] }>(`/api/workspaces/${encodeURIComponent(workspaceId)}/services`);
      services = value.services || [];
    } catch (error) {
      onToast(error instanceof Error ? error.message : String(error));
    } finally {
      loading = false;
    }
  }

  async function action(service: ServiceStatus, name: "start" | "stop" | "restart"): Promise<void> {
    pending = `${name}:${service.id}`;
    try {
      await client.request(`/api/workspaces/${encodeURIComponent(workspaceId)}/services/${encodeURIComponent(service.id)}/${name}`, { method: "POST" });
      await refresh();
    } catch (error) {
      onToast(error instanceof Error ? error.message : String(error));
    } finally {
      pending = "";
    }
  }

  async function showLogs(service: ServiceStatus): Promise<void> {
    try {
      const text = await client.request<string>(`/api/workspaces/${encodeURIComponent(workspaceId)}/services/${encodeURIComponent(service.id)}/logs`);
      logs = { id: service.id, text };
    } catch (error) {
      onToast(error instanceof Error ? error.message : String(error));
    }
  }

  onMount(() => {
    void refresh();
    const timer = window.setInterval(() => void refresh(), 5000);
    return () => { window.clearInterval(timer); client.dispose(); };
  });
</script>

<div class="services-panel" data-component-owner="services-panel">
  <div class="services-heading"><div><h2>Services</h2><p>Workspace services are supervised by the owning <code>pua serve</code> process.</p></div><button class="secondary-button" type="button" onclick={() => void refresh()} disabled={loading}><Icon name="refresh-cw" /><span>Refresh</span></button></div>
  {#if loading && services.length === 0}<div class="services-empty"><Icon name="loader-circle" /><span>Loading services…</span></div>
  {:else if services.length === 0}<div class="services-empty"><Icon name="server-off" /><span>No services configured.</span></div>
  {:else}<div class="services-list">
    {#each services as service (service.id)}
      <section class="service-card" class:attention={service.attentionRequired}>
        <header><div><h3>{service.id}</h3><span class="service-state">{service.state}</span></div><div class="service-actions"><button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => void action(service, "start")}><Icon name="play" /><span>Start</span></button><button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => void action(service, "restart")}><Icon name="rotate-ccw" /><span>Restart</span></button><button type="button" class="secondary-button" disabled={Boolean(pending)} onclick={() => void action(service, "stop")}><Icon name="square" /><span>Stop</span></button><button type="button" class="secondary-button" onclick={() => void showLogs(service)}><Icon name="scroll-text" /><span>Logs</span></button></div></header>
        <dl><div><dt>Dependencies</dt><dd>{service.dependencies?.join(", ") || "None"}</dd></div><div><dt>Readiness</dt><dd>{service.readiness.ready ? "Ready" : service.readiness.configured ? (service.readiness.lastError || "Waiting") : "Not configured"}</dd></div><div><dt>Cleanup</dt><dd>{service.cleanup.configured ? (service.cleanup.succeeded ? "Succeeded" : service.cleanup.lastError || "Pending") : "Not configured"}</dd></div>{#if service.nextRetryAt}<div><dt>Next retry</dt><dd>{service.nextRetryAt}</dd></div>{/if}</dl>
        {#if service.lastError}<p class="service-error">{service.lastError}</p>{/if}
        {#if service.exports.secrets?.length}<p class="service-secrets">Secrets: {service.exports.secrets.map((secret) => secret.name).join(", ")}</p>{/if}
      </section>
    {/each}
  </div>{/if}
  {#if logs}<div class="service-log-view"><header><h3>{logs.id} logs</h3><button type="button" class="icon-button" aria-label="Close logs" onclick={() => logs = null}><Icon name="x" /></button></header><pre>{logs.text}</pre></div>{/if}
</div>
