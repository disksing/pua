<script lang="ts">
  import {quotaEntryKey, type QuotaSnapshot} from "./workspace-cards";

  let {providers, hiddenKeys, busy, onFilter}: {
    providers: QuotaSnapshot["providers"]; hiddenKeys: string[]; busy: boolean;
    onFilter: (keys: string[]) => Promise<boolean | undefined>;
  } = $props();
</script>

<fieldset>
  <legend>Filter entries</legend>
  {#each providers as provider}
    {#each provider.quotas as window}
      {@const key = quotaEntryKey(provider, window)}
      <label>
        <input type="checkbox" checked={!hiddenKeys.includes(key)} disabled={busy}
          onchange={async (event) => {
            const input = event.currentTarget;
            const keys = input.checked ? hiddenKeys.filter(item => item !== key) : [...hiddenKeys, key];
            if (!await onFilter(keys)) input.checked = !hiddenKeys.includes(key);
          }}/>
        {[provider.label, provider.accountLabel, window.label].filter(Boolean).join(" · ")}
      </label>
    {/each}
  {/each}
</fieldset>

<style>
  fieldset { min-width:0; margin:4px 0; padding:0 4px 4px; border:0 }
  legend { padding:0; color:var(--card-muted); font-size:12px }
  label { display:flex; align-items:baseline; gap:6px; padding:4px 0; font-size:12px; overflow-wrap:anywhere; cursor:pointer }
  input { flex:none; margin:0; accent-color:var(--card-accent) }
</style>
