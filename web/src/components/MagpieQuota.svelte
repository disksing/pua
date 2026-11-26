<script lang="ts">
  import {formatDuration} from "../agenthub/companion/model";
  import {quotaEntryKey, quotaStatus, quotaValue, visibleQuotaProviders, type QuotaSnapshot} from "./workspace-cards";

  let {quota, hiddenKeys, busy, onFilter}: {
    quota: QuotaSnapshot; hiddenKeys: string[]; busy: boolean; onFilter: (keys: string[]) => Promise<boolean | undefined>;
  } = $props();
  const status = $derived(quotaStatus(quota));
  const visible = $derived(visibleQuotaProviders(quota, hiddenKeys));
</script>

{#if status}<p class="status">{status}</p>{/if}
{#if quota.providers.some(provider => provider.quotas?.length)}
  <details>
    <summary>Filter entries</summary>
    {#each quota.providers as provider}
      {#each provider.quotas as window}
        {@const key = quotaEntryKey(provider, window)}
        <label><input type="checkbox" checked={!hiddenKeys.includes(key)} disabled={busy}
          onchange={async (event) => {
            const input = event.currentTarget;
            const keys = input.checked ? hiddenKeys.filter(item => item !== key) : [...hiddenKeys, key];
            if (!await onFilter(keys)) input.checked = !hiddenKeys.includes(key);
          }}/>
          {[provider.label, provider.accountLabel, window.label].filter(Boolean).join(" · ")}
        </label>
      {/each}
    {/each}
  </details>
{/if}
{#if !visible.length && quota.providers.length}<small>All entries hidden</small>{/if}
{#each visible as provider}
  <section>
    <h3>{provider.label}</h3>
    <small>{[provider.accountLabel, provider.planLabel].filter(Boolean).join(" · ")}</small>
    {#if provider.stale}<small>Stale reading</small>{/if}
    {#each provider.quotas as window}
      {@const stale = window.stale || !quota.connected}
      <div class="window">
        <div class="reading"><span>{window.label}</span><strong>{quotaValue(window)}</strong></div>
        {#if window.remainingPercent != null}
          <div class="track">
            <meter min="0" max="100" value={window.remainingPercent} aria-label={`${provider.label} ${window.label} remaining`}></meter>
            {#if !stale && !window.expired && window.windowPositionPercent != null}
              {@const position = `${window.windowPositionPercent}% of reset window remaining`}
              <span class="cursor" role="img" aria-label={position} title={position} style:left={`${window.windowPositionPercent}%`}></span>
            {/if}
          </div>
        {/if}
        {#if window.expired}<small>Reset due · awaiting new reading</small>
        {:else if !stale && window.resetInSeconds != null}<small>Resets in {formatDuration(window.resetInSeconds)}</small>
        {:else if window.resetsAt}
          <small>{stale ? "Last reported reset" : "Resets"} <time datetime={window.resetsAt}>{new Date(window.resetsAt).toLocaleString()}</time></small>
        {:else if window.stale}<small>Stale reading</small>
        {/if}
      </div>
    {:else}<small>No quota reading</small>{/each}
  </section>
{/each}

<style>
  details { font-size:12px; margin-block:4px 8px; overflow-wrap:anywhere }
  summary,label { cursor:pointer }
  summary { color:var(--card-accent) }
  label { display:flex; align-items:baseline; gap:6px; margin-top:4px }
  input { accent-color:var(--card-accent) }
  .track { position:relative; margin-block:3px }
  .cursor { position:absolute; width:2px; height:11px; top:50%; transform:translate(-50%,-50%); border-radius:2px; background:currentColor; box-shadow:0 0 0 1px var(--card-surface) }
  section { min-width:0; padding-block:8px; overflow-wrap:anywhere }
  section + section { border-top:1px solid var(--card-line) }
  h3,.status { margin:0; font-size:13px }
  small { display:block; font-size:11px; color:var(--card-muted) }
  .status { color:var(--card-muted); padding-block:4px }
  .window { display:grid; gap:3px; margin-top:8px }
  .reading { display:flex; flex-wrap:wrap; justify-content:space-between; align-items:baseline; gap:2px 8px; font-size:12px }
  strong { color:var(--card-accent); font-variant-numeric:tabular-nums }
  meter { display:block; appearance:none; width:100%; height:5px; border:0; border-radius:3px; background:var(--card-line) }
  meter::-webkit-meter-bar { height:5px; border:0; border-radius:3px; background:var(--card-line) }
  meter::-webkit-meter-optimum-value { border-radius:3px; background:var(--card-accent) }
  meter::-moz-meter-bar { border-radius:3px; background:var(--card-accent) }
</style>
