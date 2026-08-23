<script lang="ts">
  import "./WorkspaceUserGate.css";
  import { sanitizeUserNameInput, validateUserName } from "../controllers/user-settings-controller";
  import type { AppShellModel } from "./models";
  import Icon from "./Icon.svelte";

  let { model }: { model: AppShellModel } = $props();
  let name = $state("");
  let pending = $state(false);
  let error = $state("");
  let adding = $state(false);
  let gateKey = $state("");

  $effect(() => {
    const nextKey = `${model.activeWorkspaceId}:${model.userGate.mode}`;
    if (nextKey === gateKey) return;
    gateKey = nextKey;
    name = model.userGate.suggestedUserName || "";
    pending = false;
    error = "";
    adding = false;
  });

  async function resolve(selected: string, create: boolean): Promise<void> {
    if (pending) return;
    error = "";
    try {
      const valid = validateUserName(selected);
      pending = true;
      await model.onResolveWorkspaceUser(valid, create);
    } catch (reason) {
      error = reason instanceof Error ? reason.message : String(reason);
      pending = false;
    }
  }
</script>

<section class="workspace-user-gate" aria-labelledby="workspace-user-gate-title">
  <div class="workspace-user-card">
    <div class="workspace-user-icon"><Icon name="user-round" /></div>
    {#if model.userGate.mode === "loading"}
      <div class="workspace-user-copy">
        <p class="workspace-user-eyebrow">{model.workspaceName}</p>
        <h1 id="workspace-user-gate-title">Switching user…</h1>
        <p>Loading this user’s preferences, read state, and inbox.</p>
      </div>
    {:else if model.userGate.mode === "create"}
      <div class="workspace-user-copy">
        <p class="workspace-user-eyebrow">{model.workspaceName}</p>
        <h1 id="workspace-user-gate-title">Create the first user</h1>
        <p>This Workspace has no users yet. Your preferences, read state, and inbox will be stored under this name.</p>
        {#if model.userGate.missingUserName}<p class="workspace-user-warning" role="status">Previously selected user <strong>{model.userGate.missingUserName}</strong> is no longer available.</p>{/if}
      </div>
      <form class="workspace-user-create" onsubmit={(event) => { event.preventDefault(); void resolve(name, true); }}>
        <label for="workspaceUserName">User name</label>
        <div class="workspace-user-input-row">
          <input id="workspaceUserName" value={name} disabled={pending} autocomplete="username" aria-invalid={error ? "true" : undefined} oninput={(event) => { name = sanitizeUserNameInput((event.currentTarget as HTMLInputElement).value); error = ""; }} />
          <button type="submit" disabled={pending || !name}><span>{pending ? "Creating…" : "Create and continue"}</span><Icon name="arrow-right" /></button>
        </div>
        <small>Letters, numbers, underscores, and hyphens only.</small>
      </form>
    {:else}
      <div class="workspace-user-copy">
        <p class="workspace-user-eyebrow">{model.workspaceName}</p>
        <h1 id="workspace-user-gate-title">Who are you in this Workspace?</h1>
        <p>Select your Workspace-local identity before loading personal read state, preferences, and inbox.</p>
        {#if model.userGate.missingUserName}<p class="workspace-user-warning" role="status">Previously selected user <strong>{model.userGate.missingUserName}</strong> is no longer available.</p>{/if}
      </div>
      <div class="workspace-user-options">
        {#each model.userGate.users as user (user.name)}
          <button type="button" disabled={pending} onclick={() => resolve(user.name, false)}>
            <span class="workspace-user-avatar">{user.name.slice(0, 1).toUpperCase()}</span>
            <span><strong>{user.name}</strong><small>{user.preference || "No preference set"}</small></span>
            <Icon name="chevron-right" />
          </button>
        {/each}
      </div>
      {#if adding}
        <form class="workspace-user-create workspace-user-add" onsubmit={(event) => { event.preventDefault(); void resolve(name, true); }}>
          <label for="workspaceNewUserName">New user name</label>
          <div class="workspace-user-input-row">
            <input id="workspaceNewUserName" value={name} disabled={pending} autocomplete="username" aria-invalid={error ? "true" : undefined} oninput={(event) => { name = sanitizeUserNameInput((event.currentTarget as HTMLInputElement).value); error = ""; }} />
            <button type="submit" disabled={pending || !name}><span>{pending ? "Creating…" : "Create and continue"}</span><Icon name="arrow-right" /></button>
          </div>
          <button class="workspace-user-cancel" type="button" disabled={pending} onclick={() => { adding = false; name = ""; error = ""; }}>Cancel</button>
        </form>
      {:else}
        <button class="workspace-user-add-toggle" type="button" disabled={pending} onclick={() => { adding = true; name = ""; error = ""; }}><Icon name="plus" /><span>Add a new user</span></button>
      {/if}
    {/if}
    {#if error}<p class="workspace-user-error" role="alert">{error}</p>{/if}
    <p class="workspace-user-footnote">User selection applies only to this Workspace in this browser.</p>
  </div>
</section>
