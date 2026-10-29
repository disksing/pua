<script lang="ts">
  import { untrack } from "svelte";
  import { ApiClient } from "../api/client";

  let { workspaceId, workspaceName, currentUserName = "" }: { workspaceId: string; workspaceName: string; currentUserName?: string } = $props();
  interface Settings {
    configured: boolean;
    enabled: boolean;
    botName: string;
    accounts: Array<{ user: string; telegramId: number }>;
    topicCount: number;
    lastError: string;
    lastPoll: string;
    deliveryProblems: Array<{ route: string; delivery: string; resource: string; user: string; status: string; error: string; held?: boolean; count?: number; pendingStatus?: string; since?: string }>;
    topicProblems?: Array<{ resource: string; user: string; reason: string; hint: string; since?: string }>;
  }
  const client = new ApiClient();
  let expanded = $state(false);
  let settings = $state<Settings | null>(null);
  let users = $state<Array<{ name: string }>>([]);
  let selectedUser = $state("");
  let token = $state("");
  let enabled = $state(false);
  let pending = $state("");
  let error = $state("");
  let notice = $state("");
  let pairing = $state<{ url: string; expiresAt: string } | null>(null);
  let confirmDisconnect = $state(false);
  let confirmUnbind = $state("");
  let reviewDelivery = $state<string | null>(null);
  const endpoint = $derived(`/api/workspaces/${encodeURIComponent(workspaceId)}/telegram`);
  // Parent models are republished during polling. Depend on the scalar identity,
  // not on the parent's object-backed prop getter, so drafts survive refreshes.
  const loadIdentity = $derived(expanded ? workspaceId : "");

  $effect(() => {
    const id = loadIdentity;
    if (!id) return;
    const preferredUser = untrack(() => currentUserName);
    let active = true;
    pending = "load"; settings = null; confirmUnbind = ""; confirmDisconnect = false; reviewDelivery = null;
    error = "";
    Promise.all([
      client.request<Settings>(`/api/workspaces/${encodeURIComponent(id)}/telegram`),
      client.request<{ users: Array<{ name: string }> }>(`/api/workspaces/${encodeURIComponent(id)}/users`),
    ]).then(([value, list]) => {
      if (!active) return;
      settings = value; enabled = value.enabled; users = list.users;
      selectedUser = list.users.some(user => user.name === preferredUser) ? preferredUser : list.users.length === 1 ? list.users[0].name : "";
      token = ""; pairing = null;
    }).catch((reason: unknown) => { if (active) error = reason instanceof Error ? reason.message : String(reason); })
      .finally(() => { if (active) pending = ""; });
    return () => { active = false; token = ""; pairing = null; };
  });

  async function action(key: string, body: object = {}): Promise<void> {
    if (pending) return;
    const identity = workspaceId;
    pending = key; error = ""; notice = "";
    try {
      if (key === "pair") {
        const result = await client.request<{ url: string; expiresAt: string }>(`${endpoint}/pair`, { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
        if (identity === workspaceId) pairing = result;
      } else {
        const path = key === "save" || key === "refresh" ? endpoint : `${endpoint}/${key}`;
        const result = await client.request<Settings>(path, key === "refresh" ? {} : { method: key === "save" ? "PUT" : "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify(body) });
        if (identity !== workspaceId) return;
        settings = result;
        if (key === "save" || key === "disconnect") { enabled = result.enabled; token = ""; }
        if (key === "disconnect" || key === "unbind") pairing = null;
        confirmDisconnect = false; confirmUnbind = ""; reviewDelivery = null;
        notice = key === "refresh" ? "Status refreshed." : "Telegram settings updated.";
      }
    } catch (reason) { if (identity === workspaceId) error = reason instanceof Error ? reason.message : String(reason); }
    finally { if (identity === workspaceId) pending = ""; }
  }
</script>

<details class="telegram-settings" bind:open={expanded}>
  <summary>Telegram <span>Connect {workspaceName} to a private bot</span></summary>
  {#if expanded}
    <div class="telegram-content">
      <p>Create a dedicated bot with <a href="https://t.me/BotFather" target="_blank" rel="noreferrer">BotFather</a> and enable Topics / Threaded Mode. Each resource opens in its own private topic.</p>
      {#if pending === "load"}<p role="status">Loading Telegram settings…</p>{/if}
      {#if error}<p class="error" role="alert">{error}</p>{/if}
      {#if notice}<p role="status">{notice}</p>{/if}
      {#if settings}
        <form onsubmit={(event) => { event.preventDefault(); void action("save", { token, enabled }); }}>
          <label>Bot token
            <input type="password" autocomplete="new-password" bind:value={token} placeholder={settings.configured ? "Saved securely · leave empty to keep" : "Token from BotFather"} disabled={Boolean(pending)} />
          </label>
          <label class="check"><input type="checkbox" bind:checked={enabled} disabled={Boolean(pending)} /> Enable Telegram connection</label>
          <p class="help">Tokens are stored on the PUA server and are never returned to this page. Pausing the connection keeps account bindings and topics.</p>
          <div class="actions">
            <button type="submit" disabled={Boolean(pending) || (enabled && !settings.configured && !token.trim())}>{pending === "save" ? "Checking…" : "Save connection"}</button>
            <button type="button" disabled={Boolean(pending)} onclick={() => action("refresh")}>Refresh status</button>
          </div>
        </form>
        <p class="status">{settings.enabled ? "Enabled" : "Disabled"}{settings.botName ? ` · @${settings.botName}` : ""} · {settings.topicCount} topics</p>
        {#if settings.lastPoll}<p class="help">Last successful connection: {new Date(settings.lastPoll).toLocaleString()}</p>{/if}
        {#if settings.lastError}<p class="error" role="alert">{settings.lastError}</p>{/if}
        {#if settings.enabled}
          <p class="help">Telegram and the web share history for the same Workspace user. {currentUserName ? `You are viewing web chats as ${currentUserName}.` : "Choose the same user you use for web chats."}</p>
          <form onsubmit={(event) => { event.preventDefault(); void action("pair", { user: selectedUser }); }}>
            <label>Workspace user
              <select bind:value={selectedUser} disabled={Boolean(pending)} onchange={() => pairing = null}><option value="" disabled>Choose a Workspace user</option>{#each users as user}<option value={user.name}>{user.name}</option>{/each}</select>
            </label>
            {#if currentUserName && selectedUser && selectedUser !== currentUserName}<p class="help">Pairing as {selectedUser} opens separate history from your web chats as {currentUserName}.</p>{/if}
            <button type="submit" disabled={Boolean(pending) || !selectedUser}>Create pairing link</button>
          </form>
          {#if pairing}
            <div class="pairing" role="status">
              <a href={pairing.url} target="_blank" rel="noreferrer">Open Telegram to pair this account</a>
              <p>This link grants access as the selected Workspace user. Use it yourself before {new Date(pairing.expiresAt).toLocaleTimeString()}. It works once.</p>
              <p>After pairing, choose Workspace, browse projects, or send /search followed by a task name. Refresh status here to see the connection.</p>
            </div>
          {/if}
        {/if}
        {#each settings.accounts as account (account.telegramId)}
          {#if currentUserName && account.user !== currentUserName}<p class="help">Telegram is linked as {account.user}, while web chats use {currentUserName}. To share history, unlink and pair as {currentUserName}, or switch the web user to {account.user}.</p>{/if}
          <div class="account"><span><strong>{account.user}</strong> · Telegram {account.telegramId}</span>
            <button type="button" disabled={Boolean(pending)} onclick={() => confirmUnbind = account.user}>Unlink account</button>
          </div>
          {#if confirmUnbind === account.user}
            <div class="confirmation"><p>Unlink {account.user}? Telegram input and pushes will stop, and saved delivery and review records for this user will be removed. PUA chat history will remain.</p>
              <button type="button" disabled={Boolean(pending)} onclick={() => action("unbind", { user: account.user })}>Unlink</button>
              <button type="button" onclick={() => confirmUnbind = ""}>Cancel</button>
            </div>
          {/if}
        {/each}
        {#each settings.topicProblems ?? [] as topic (`${topic.resource}/${topic.user}`)}
          <div class="delivery"><strong>{topic.resource} · {topic.user}</strong>
            <p>{topic.hint}</p>
            {#if topic.since}<p class="help">Paused since {new Date(topic.since).toLocaleString()}</p>{/if}
          </div>
        {/each}
        {#each settings.deliveryProblems as problem (problem.delivery)}
          <div class="delivery"><strong>{problem.resource} · {problem.user}</strong>
            {#if problem.held}
              <p>{problem.count || 0} earlier messages may not have been forwarded. They remain in PUA and will not be sent automatically.</p>
              {#if problem.pendingStatus}<p>A previous delivery was {problem.pendingStatus}; it may already be in Telegram. Check both chats before deciding.</p>{/if}
              {#if problem.since}<p class="help">Needs review since {new Date(problem.since).toLocaleString()}</p>{/if}
              {#if reviewDelivery === problem.delivery}
                <p>Clear this review notice? Earlier messages will not be resent, and the saved pending delivery will be removed.</p>
                <button type="button" disabled={Boolean(pending)} onclick={() => action("delivery", { route: problem.route, delivery: problem.delivery, action: "acknowledge" })}>Clear review notice</button>
                <button type="button" onclick={() => reviewDelivery = null}>Cancel</button>
              {:else}
                <button type="button" disabled={Boolean(pending)} onclick={() => reviewDelivery = problem.delivery}>Review notice</button>
              {/if}
            {:else}
              <p>{problem.error || problem.status}</p>
              {#if reviewDelivery === problem.delivery}
                <p>The message may already be in Telegram. Retrying can create a duplicate.</p>
                <button type="button" disabled={Boolean(pending)} onclick={() => action("delivery", { route: problem.route, delivery: problem.delivery, action: "retry" })}>Retry delivery</button>
                <button type="button" onclick={() => reviewDelivery = null}>Cancel</button>
              {:else}
                <button type="button" disabled={Boolean(pending)} onclick={() => reviewDelivery = problem.delivery}>Review retry</button>
              {/if}
              <button type="button" disabled={Boolean(pending)} onclick={() => action("delivery", { route: problem.route, delivery: problem.delivery, action: "skip" })}>Skip this message</button>
            {/if}
          </div>
        {/each}
        {#if settings.configured}
          {#if confirmDisconnect}
            <div class="confirmation"><p>Disconnect this bot and remove its saved token, pairings, topic mappings, and delivery and review records? PUA chat history and existing Telegram messages will remain.</p>
              <button type="button" disabled={Boolean(pending)} onclick={() => action("disconnect")}>Disconnect bot</button>
              <button type="button" onclick={() => confirmDisconnect = false}>Cancel</button>
            </div>
          {:else}
            <button type="button" class="disconnect" disabled={Boolean(pending)} onclick={() => confirmDisconnect = true}>Disconnect bot…</button>
          {/if}
        {/if}
      {/if}
    </div>
  {/if}
</details>

<style>
  .telegram-settings { margin-top: 12px; border: 1px solid var(--line, #d4d4d4); border-radius: 10px; font-size: 13px; }
  summary { padding: 12px 14px; cursor: pointer; font-weight: 600; }
  summary span { margin-left: 8px; font-weight: 400; color: var(--muted, #71717a); }
  .telegram-content { padding: 0 14px 14px; display: grid; gap: 12px; }
  p { margin: 0; line-height: 1.55; }
  form { display: grid; gap: 10px; }
  label { display: grid; gap: 6px; font-weight: 500; }
  input:not([type="checkbox"]), select { width: 100%; box-sizing: border-box; min-width: 0; padding: 8px 10px; color: inherit; background: var(--panel, transparent); border: 1px solid var(--line, #d4d4d4); border-radius: 6px; font: inherit; }
  .check { display: flex; align-items: center; }
  .actions, .account { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .account { justify-content: space-between; }
  button { border: 1px solid var(--line, #d4d4d4); border-radius: 6px; padding: 7px 10px; background: transparent; color: inherit; font: inherit; cursor: pointer; }
  button:disabled { opacity: .5; cursor: default; }
  .help, .status { color: var(--muted, #71717a); }
  .error { color: var(--red, #b42318); overflow-wrap: anywhere; }
  .pairing, .confirmation, .delivery { padding: 10px; border: 1px solid var(--line, #d4d4d4); border-radius: 6px; display: grid; gap: 8px; }
  .disconnect { justify-self: start; }
  @media(max-width: 600px) { summary span { display: block; margin: 6px 0 0; } }
</style>
