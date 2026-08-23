<script lang="ts">
  import "./SystemInfoPanel.css";

  import Icon from "./Icon.svelte";
  import type { SystemInfo } from "./models";

  let { system }: { system: SystemInfo | null } = $props();

  function value(text: string): string {
    return text || "—";
  }
</script>

<div class="settings-panel system-info-panel" data-component-owner="system-info-panel" data-settings-panel>
  <div class="settings-panel-header">
    <h2>System Information</h2>
    <p>Runtime and storage details for this PUA Server and its AgentHub connection.</p>
  </div>

  {#if !system}
    <div class="system-info-loading" role="status">Loading system information…</div>
  {:else}
    <div class="system-info-grid">
      <section class="system-info-card" aria-labelledby="pua-system-heading">
        <div class="system-info-card-heading">
          <span class="system-info-icon"><Icon name="server" /></span>
          <div><h3 id="pua-system-heading">PUA Server</h3><span>Running instance</span></div>
        </div>
        <dl>
          <div><dt>Address</dt><dd>{value(system.pua.address)}</dd></div>
          <div><dt>Port</dt><dd>{value(system.pua.port)}</dd></div>
          <div><dt>Config file</dt><dd class="system-info-path">{value(system.pua.configPath)}</dd></div>
          <div><dt>Branch</dt><dd>{value(system.pua.buildBranch)}</dd></div>
          <div><dt>Commit</dt><dd class="system-info-path">{value(system.pua.buildCommit)}</dd></div>
          <div class="system-info-workspaces">
            <dt>Managed workspaces</dt>
            <dd>
              {#if system.pua.workspaces.length}
                <ul>
                  {#each system.pua.workspaces as workspace (workspace.path)}
                    <li><strong>{workspace.name || "Workspace"}</strong><span class="system-info-path">{workspace.path}</span></li>
                  {/each}
                </ul>
              {:else}
                <span>None</span>
              {/if}
            </dd>
          </div>
        </dl>
      </section>

      <section class="system-info-card" aria-labelledby="agenthub-system-heading">
        <div class="system-info-card-heading">
          <span class="system-info-icon"><Icon name="network" /></span>
          <div><h3 id="agenthub-system-heading">AgentHub</h3><span>{system.agentHub.mode || "Unknown mode"}</span></div>
          <span class:connected={system.agentHub.connected && system.agentHub.compatible} class="system-info-status">
            {system.agentHub.connected ? (system.agentHub.compatible ? "Connected" : "Incompatible") : "Unavailable"}
          </span>
        </div>
        <dl>
          <div><dt>Address</dt><dd>{value(system.agentHub.address)}</dd></div>
          <div><dt>Port</dt><dd>{value(system.agentHub.port)}</dd></div>
          <div><dt>Endpoint</dt><dd class="system-info-path">{value(system.agentHub.endpoint)}</dd></div>
          <div><dt>Version</dt><dd class="system-info-path">{value(system.agentHub.version)}</dd></div>
          <div><dt>Config</dt><dd class="system-info-path">{value(system.agentHub.paths.config)}</dd></div>
          <div><dt>Sessions</dt><dd class="system-info-path">{value(system.agentHub.paths.sessions)}</dd></div>
          <div><dt>Archive</dt><dd class="system-info-path">{value(system.agentHub.paths.archive)}</dd></div>
          <div><dt>Logs</dt><dd class="system-info-path">{value(system.agentHub.paths.logs)}</dd></div>
        </dl>
        {#if system.agentHub.error}<p class="system-info-error" role="status">{system.agentHub.error}</p>{/if}
      </section>
    </div>
  {/if}
</div>
