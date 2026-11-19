<script lang="ts">
  import "./AppearanceSettingsPanel.css";

  import Icon from "./Icon.svelte";
  import type { SettingsModel } from "./models";

  type Appearance = SettingsModel["appearance"];
  type LayoutPreference = Appearance["layout"];
  type FontScaleColumn = keyof Appearance["fontScales"];

  let { appearance, onLayoutPreference, onFontScale, onResetFontScales, onThemePreference }: {
    appearance: Appearance;
    onLayoutPreference: SettingsModel["onLayoutPreference"];
    onFontScale: SettingsModel["onFontScale"];
    onResetFontScales: SettingsModel["onResetFontScales"];
    onThemePreference: SettingsModel["onThemePreference"];
  } = $props();

  const layoutOptions: Array<{ id: LayoutPreference; label: string; description: string }> = [
    { id: "auto", label: "Auto", description: "Follows the window width" },
    { id: "three", label: "Three columns", description: "Sidebar, resource details, and an empty right column" },
    { id: "two", label: "Two columns", description: "Sidebar and full-width resource details" },
    { id: "split", label: "Split", description: "Sidebar collapsed into a drawer" },
  ];

  const fontColumns: Array<{ id: FontScaleColumn; label: string }> = [
    { id: "sidebar", label: "Sidebar" },
    { id: "details", label: "Details" },
    { id: "chat", label: "Chat" },
  ];

  const percent = (value: number) => `${Math.round(value * 100)}%`;
  const fontScalesDefault = $derived(fontColumns.every((column) => appearance.fontScales[column.id] === 1));
</script>

<div class="settings-panel" data-component-owner="appearance-settings-panel" data-settings-panel>
  <div class="settings-panel-header"><h2>Appearance</h2><p>Choose the theme, the workspace layout and the text size of each column. Everything applies immediately and is stored only in this browser.</p></div>

  <section class="appearance-section" aria-label="Theme">
    <div class="settings-section-heading"><h3>Theme</h3></div>
    <div class="layout-options" role="radiogroup" aria-label="Theme">
      {#each appearance.themeOptions as option (option.id)}
        <button type="button" class="layout-option theme-option" class:active={appearance.theme === option.id} role="radio" aria-checked={appearance.theme === option.id} onclick={() => onThemePreference(option.id)}>
          <span class="layout-option-text"><strong>{option.label}</strong><small>{option.description}</small></span>
        </button>
      {/each}
    </div>
  </section>

  <section class="appearance-section" aria-label="Layout">
    <div class="settings-section-heading"><h3>Layout</h3></div>
    <div class="layout-options" role="radiogroup" aria-label="Workspace layout">
      {#each layoutOptions as option (option.id)}
        <button type="button" class="layout-option" class:active={appearance.layout === option.id} role="radio" aria-checked={appearance.layout === option.id} onclick={() => onLayoutPreference(option.id)}>
          <span class="layout-diagram">
            {#if option.id === "auto"}
              <svg viewBox="0 0 120 72" aria-hidden="true"><rect class="d-fill-strong" x="6" y="8" width="22" height="56" rx="3" /><rect class="d-dash" x="34" y="8" width="50" height="56" rx="3" /><rect class="d-dash" x="90" y="8" width="24" height="56" rx="3" /></svg>
            {:else if option.id === "three"}
              <svg viewBox="0 0 120 72" aria-hidden="true"><rect class="d-fill-strong" x="6" y="8" width="22" height="56" rx="3" /><rect class="d-fill-light" x="34" y="8" width="50" height="56" rx="3" /><rect class="d-fill-mid" x="90" y="8" width="24" height="56" rx="3" /></svg>
            {:else if option.id === "two"}
              <svg viewBox="0 0 120 72" aria-hidden="true"><rect class="d-fill-strong" x="6" y="8" width="22" height="56" rx="3" /><rect class="d-fill-light" x="34" y="8" width="80" height="32" rx="3" /><rect class="d-fill-mid" x="34" y="46" width="80" height="18" rx="3" /></svg>
            {:else}
              <svg viewBox="0 0 120 72" aria-hidden="true"><rect class="d-fill-light" x="6" y="8" width="70" height="56" rx="3" /><rect class="d-fill-mid" x="82" y="8" width="32" height="56" rx="3" /><rect class="d-fill-strong" x="6" y="8" width="18" height="56" rx="3" /></svg>
            {/if}
          </span>
          <span class="layout-option-text"><strong>{option.label}</strong><small>{option.description}</small></span>
        </button>
      {/each}
    </div>
  </section>

  <section class="appearance-section" aria-label="Text size">
    <div class="settings-section-heading"><h3>Text size</h3><button type="button" class="appearance-reset" disabled={fontScalesDefault} onclick={onResetFontScales}><Icon name="rotate-ccw" /><span>Reset</span></button></div>
    <div class="font-scale-rows">
      {#each fontColumns as column (column.id)}
        <div class="font-scale-row">
          <span class="font-scale-label">{column.label}</span>
          <input type="range" min="80" max="140" step="5" value={Math.round(appearance.fontScales[column.id] * 100)} aria-label={`${column.label} text size`} oninput={(event) => onFontScale(column.id, Number(event.currentTarget.value) / 100)} />
          <span class="font-scale-value">{percent(appearance.fontScales[column.id])}</span>
        </div>
      {/each}
    </div>
    <small class="appearance-hint">Scales the text of each column independently from 80% to 140%.</small>
  </section>
</div>
