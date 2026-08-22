<script lang="ts">
  import "./WorkspaceSettingsPanel.css";

  import Icon from "./Icon.svelte";
  import type { SettingsDraft, SettingsModel } from "./models";
  import { cloneSettingsDraft, settingsErrorMessage } from "./settings-draft";
  import { sanitizeUserNameInput, validateUserName } from "../controllers/user-settings-controller";

  let {
    workspaces,
    activeWorkspaceId,
    workspaceIcons,
    draft = $bindable(),
    pending = $bindable(),
    onAddWorkspace,
    onRemoveWorkspace,
    onWorkspaceIcon,
    onSaveWorkspaceName,
    onToast,
  }: {
    workspaces: SettingsModel["workspaces"];
    activeWorkspaceId: string;
    workspaceIcons: SettingsModel["workspaceIcons"];
    draft: SettingsDraft;
    pending: string;
    onAddWorkspace: SettingsModel["onAddWorkspace"];
    onRemoveWorkspace: SettingsModel["onRemoveWorkspace"];
    onWorkspaceIcon: SettingsModel["onWorkspaceIcon"];
    onSaveWorkspaceName: SettingsModel["onSaveWorkspaceName"];
    onToast: SettingsModel["onToast"];
  } = $props();

  let iconPicker = $state("");
  let nameEditing = $state("");
  let nameDraft = $state("");
  let nameInput = $state<HTMLInputElement | null>(null);
  const workspaceItems = $derived(workspaces || []);

  const workspacePathMissing = $derived(!draft.workspacePath.trim());
  const initialUserError = $derived.by(() => {
    if (!draft.createWorkspace) return "";
    try { validateUserName(draft.initialUserName); return ""; }
    catch (error) { return settingsErrorMessage(error); }
  });

  $effect(() => {
    if (nameEditing && nameInput) nameInput.focus();
  });

  async function addWorkspace(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (workspacePathMissing || initialUserError || pending) return;
    pending = "workspace";
    try {
      await onAddWorkspace(cloneSettingsDraft(draft));
      draft.workspacePath = "";
      draft.createWorkspace = false;
      draft.workspaceLanguage = "en";
    } catch (error) {
      onToast(settingsErrorMessage(error));
    } finally {
      pending = "";
    }
  }

  async function removeWorkspace(id: string): Promise<void> {
    if (pending) return;
    pending = `remove:${id}`;
    try {
      await onRemoveWorkspace(id, cloneSettingsDraft(draft));
    } catch (error) {
      onToast(settingsErrorMessage(error));
    } finally {
      pending = "";
    }
  }

  async function saveIcon(id: string, icon: string): Promise<void> {
    if (pending) return;
    pending = `icon:${id}`;
    iconPicker = "";
    try {
      await onWorkspaceIcon(id, icon, cloneSettingsDraft(draft));
    } catch (error) {
      onToast(settingsErrorMessage(error));
    } finally {
      pending = "";
    }
  }

  function startNameEdit(workspace: SettingsModel["workspaces"][number]): void {
    if (pending) return;
    iconPicker = "";
    nameDraft = workspace.name;
    nameEditing = workspace.id;
  }

  function cancelNameEdit(): void {
    nameEditing = "";
    nameDraft = "";
  }

  async function saveName(workspace: SettingsModel["workspaces"][number]): Promise<void> {
    const name = nameDraft.trim();
    if (name === workspace.name) {
      cancelNameEdit();
      return;
    }
    pending = `name:${workspace.id}`;
    try {
      await onSaveWorkspaceName(workspace.id, name, cloneSettingsDraft(draft));
      cancelNameEdit();
    } catch (error) {
      onToast(settingsErrorMessage(error));
    } finally {
      pending = "";
    }
  }

  function nameKeydown(event: KeyboardEvent, workspace: SettingsModel["workspaces"][number]): void {
    if (event.key === "Enter") {
      event.preventDefault();
      void saveName(workspace);
    } else if (event.key === "Escape") {
      event.preventDefault();
      cancelNameEdit();
    }
  }

  function workspaceIcon(id: string): { id: string; label: string; src: string } {
    const workspace = workspaceItems.find((item) => item.id === id);
    return workspaceIcons.find((item) => item.id === (workspace?.icon || "")) || workspaceIcons[0];
  }

</script>

<div class="settings-panel" data-component-owner="workspace-settings-panel" data-settings-panel>
  <div class="settings-panel-header">
    <h2>Workspaces</h2>
    <p>Add existing AgentWorkspace folders or create and initialize a new PUA workspace.</p>
  </div>
  <form id="settingsWorkspaceForm" class="settings-path-form" onsubmit={addWorkspace}>
    <input
      id="settingsWorkspacePath"
      bind:value={draft.workspacePath}
      required
      aria-invalid={workspacePathMissing}
      aria-describedby={workspacePathMissing ? "settings-workspace-path-error" : undefined}
      placeholder="/Users/me/Documents/AgentWorkspace"
    />
    {#if workspacePathMissing}<p id="settings-workspace-path-error" class="settings-field-error" role="alert">Workspace path is required.</p>{/if}
    <label class="settings-check">
      <input id="settingsWorkspaceCreate" type="checkbox" bind:checked={draft.createWorkspace} />
      <span>Create directory and run pua init</span>
    </label>
    {#if draft.createWorkspace}
      <label class="settings-language settings-initial-user" for="settingsWorkspaceInitialUser">
        <span>Initial user name</span>
        <input id="settingsWorkspaceInitialUser" value={draft.initialUserName} aria-invalid={initialUserError ? "true" : undefined} aria-describedby={initialUserError ? "settings-workspace-user-error" : undefined} disabled={Boolean(pending)} oninput={(event) => draft.initialUserName = sanitizeUserNameInput((event.currentTarget as HTMLInputElement).value)} />
      </label>
      {#if initialUserError}<p id="settings-workspace-user-error" class="settings-field-error" role="alert">{initialUserError}</p>{/if}
      <p class="settings-field-help">When available, this is prefilled from the account running PUA Server. You can change it.</p>
      <label class="settings-language" for="settingsWorkspaceLanguage">
        <span>Generated content language</span>
        <select id="settingsWorkspaceLanguage" bind:value={draft.workspaceLanguage} disabled={Boolean(pending)}>
          <option value="en">English</option>
          <option value="zh-CN">简体中文</option>
        </select>
      </label>
    {/if}
    <button type="submit" disabled={Boolean(pending) || workspacePathMissing || Boolean(initialUserError)}><Icon name="plus" /><span>{draft.createWorkspace ? "Create" : "Add"}</span></button>
  </form>
  <div class="settings-list">
    {#each workspaceItems as workspace (workspace.id)}
      {@const shownIcon = workspaceIcon(workspace.id)}
      <div class="settings-workspace-entry">
        <div class="settings-list-row">
          <div class="settings-row-main">
            <span class="settings-workspace-mark"><img src={shownIcon.src} alt="" aria-hidden="true" /></span>
            <span><strong>{workspace.name}</strong><small>{workspace.path}</small></span>
          </div>
          <div class="settings-row-actions">
            {#if workspace.id === activeWorkspaceId}<span class="settings-pill">Active</span>{/if}
            <button
              type="button"
              class="settings-workspace-icon-button"
              aria-expanded={iconPicker === workspace.id}
              title="Change workspace icon"
              disabled={Boolean(pending)}
              onclick={() => { nameEditing = ""; iconPicker = iconPicker === workspace.id ? "" : workspace.id; }}
            >
              <img src={shownIcon.src} alt="" />
              <span>{pending === `icon:${workspace.id}` ? "Saving..." : shownIcon.label}</span>
              <Icon name="chevron-down" />
            </button>
            <button type="button" class="settings-workspace-rename-button" title="Rename workspace" aria-label={`Rename ${workspace.name}`} disabled={Boolean(pending)} onclick={() => startNameEdit(workspace)}><Icon name="pencil" /></button>
            <button type="button" class="icon-button danger" title="Remove workspace" disabled={Boolean(pending)} onclick={() => removeWorkspace(workspace.id)}><Icon name="trash-2" /></button>
          </div>
        </div>
        {#if nameEditing === workspace.id}
          <form class="settings-workspace-name-form" onsubmit={(event) => { event.preventDefault(); void saveName(workspace); }}>
            <input bind:this={nameInput} bind:value={nameDraft} placeholder={workspace.path} aria-label={`Name for ${workspace.name}`} disabled={pending === `name:${workspace.id}`} onkeydown={(event) => nameKeydown(event, workspace)} />
            <button type="submit" disabled={Boolean(pending)}><Icon name="check" /><span>{pending === `name:${workspace.id}` ? "Saving..." : "Save"}</span></button>
            <button type="button" disabled={Boolean(pending)} onclick={cancelNameEdit}><Icon name="x" /><span>Cancel</span></button>
            <small class="settings-workspace-name-hint">Leave empty to use the directory name.</small>
          </form>
        {/if}
        {#if iconPicker === workspace.id}
          <div class="settings-workspace-icon-picker" role="radiogroup" aria-label={`Icon for ${workspace.name}`}>
            {#each workspaceIcons as option (option.id)}
              <button type="button" role="radio" aria-checked={option.id === shownIcon.id} class:selected={option.id === shownIcon.id} title={option.label} onclick={() => saveIcon(workspace.id, option.id)}>
                <img src={option.src} alt="" /><span>{option.label}</span>{#if option.id === shownIcon.id}<Icon name="check" />{/if}
              </button>
            {/each}
          </div>
        {/if}
      </div>
    {:else}
      <div class="settings-empty">No workspaces managed by PUA.</div>
    {/each}
  </div>
</div>
