<script lang="ts">
  import {quotaStatus, quotaValue, type QuotaSnapshot} from "./workspace-cards";

  let {quota}: {quota: QuotaSnapshot} = $props();
  const status = $derived(quotaStatus(quota));
</script>

{#if status}<p class="status">{status}</p>{/if}
{#each quota.providers as provider}
  <section>
    <h3>{provider.label}</h3>
    <small>{[provider.accountLabel, provider.planLabel].filter(Boolean).join(" · ")}</small>
    {#if provider.stale}<small>Stale reading</small>{/if}
    {#each provider.quotas as window}
      <div class="window">
        <div class="reading"><span>{window.label}</span><strong>{quotaValue(window)}</strong></div>
        {#if window.remainingPercent != null}
          <meter min="0" max="100" value={window.remainingPercent} aria-label={`${provider.label} ${window.label} remaining`}></meter>
        {/if}
        {#if window.resetsAt}
          <small>{window.stale ? "Last reported reset" : "Resets"} <time datetime={window.resetsAt}>{new Date(window.resetsAt).toLocaleString()}</time></small>
        {/if}
      </div>
    {:else}<small>No quota reading</small>{/each}
  </section>
{/each}

<style>
  section { min-width:0; padding-block:8px; overflow-wrap:anywhere }
  section + section { border-top:1px solid var(--card-line) }
  h3,.status { margin:0; font-size:13px }
  small { display:block; font-size:11px; color:var(--card-muted) }
  .status { color:var(--card-muted); padding-block:4px }
  .window { display:grid; gap:3px; margin-top:8px }
  .reading { display:flex; flex-wrap:wrap; justify-content:space-between; align-items:baseline; gap:2px 8px; font-size:12px }
  strong { color:var(--card-accent); font-variant-numeric:tabular-nums }
  meter { appearance:none; width:100%; height:5px; border:0; border-radius:3px; background:var(--card-line) }
  meter::-webkit-meter-bar { height:5px; border:0; border-radius:3px; background:var(--card-line) }
  meter::-webkit-meter-optimum-value { border-radius:3px; background:var(--card-accent) }
  meter::-moz-meter-bar { border-radius:3px; background:var(--card-accent) }
</style>
