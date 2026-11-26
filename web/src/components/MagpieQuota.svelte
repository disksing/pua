<script lang="ts">
  import {formatDuration} from "../agenthub/companion/model";
  import {quotaStatus, quotaValue, visibleQuotaProviders, type QuotaSnapshot} from "./workspace-cards";

  let {quota, hiddenKeys}: {
    quota: QuotaSnapshot; hiddenKeys: string[];
  } = $props();
  const status = $derived(quotaStatus(quota));
  const visible = $derived(visibleQuotaProviders(quota, hiddenKeys));
</script>

{#if status}<p class="status">{status}</p>{/if}
{#if !visible.length && quota.providers.length}<small>All entries hidden</small>{/if}
{#each visible as provider}
  <section>
    <h3>{provider.label}
      {#if provider.accountLabel && visible.some(other => other !== provider && other.label === provider.label)}
        <small>{provider.accountLabel}</small>
      {/if}
    </h3>
    {#each provider.quotas as window}
      {@const stale = window.stale || !quota.connected}
      <div class="window">
        <span>{window.label}</span><strong>{quotaValue(window)}</strong>
        {#if window.remainingPercent != null}
          <div class="track">
            <meter min="0" max="100" value={window.remainingPercent} aria-label={`${provider.label} ${window.label} remaining`}></meter>
            {#if !stale && !window.expired && window.windowPositionPercent != null}
              {@const position = `${window.windowPositionPercent}% of reset window remaining`}
              <span class="cursor" role="img" aria-label={position} title={position} style:left={`${window.windowPositionPercent}%`}></span>
            {/if}
          </div>
        {/if}
        {#if window.expired}<small title="Awaiting new reading">Reset due</small>
        {:else if !stale && window.resetInSeconds != null}<small title="Time until reset">in {formatDuration(window.resetInSeconds)}</small>
        {:else if window.resetsAt}
          <small title={`${stale ? "Last reported reset" : "Resets"} ${new Date(window.resetsAt).toLocaleString()}`}><time datetime={window.resetsAt}>{new Date(window.resetsAt).toLocaleString(undefined, {month:"short", day:"numeric", hour:"numeric", minute:"2-digit"})}</time></small>
        {/if}
      </div>
    {:else}<small>No quota reading</small>{/each}
  </section>
{/each}

<style>
  .track { position:relative; grid-column:3 }
  .cursor { position:absolute; width:2px; height:11px; top:50%; transform:translate(-50%,-50%); border-radius:2px; background:currentColor; box-shadow:0 0 0 1px var(--card-surface) }
  section { min-width:0; padding-block:4px; overflow-wrap:anywhere }
  section + section { border-top:1px solid var(--card-line) }
  h3,.status { margin:0; font-size:13px }
  small { font-size:11px; color:var(--card-muted) }
  h3 small { font-weight:normal }
  .status { color:var(--card-muted); padding-block:4px }
  .window { display:grid; grid-template-columns:minmax(0,1fr) fit-content(34%) minmax(24px,.8fr) fit-content(35%); align-items:center; gap:0 6px; padding-block:2px; font-size:12px; line-height:1.3 }
  .window small { grid-column:4; text-align:right; font-variant-numeric:tabular-nums }
  strong { color:var(--card-accent); font-variant-numeric:tabular-nums }
  meter { display:block; appearance:none; width:100%; height:5px; border:0; border-radius:3px; background:var(--card-line) }
  meter::-webkit-meter-bar { height:5px; border:0; border-radius:3px; background:var(--card-line) }
  meter::-webkit-meter-optimum-value { border-radius:3px; background:var(--card-accent) }
  meter::-moz-meter-bar { border-radius:3px; background:var(--card-accent) }
</style>
