<script lang="ts">
  import { untrack, type Snippet } from "svelte";
  import { ApiClient, ApiError, StaleResponseError } from "../api/client";
  import ApprovalCard from "./ApprovalCard.svelte";
  import { turnIsWorking } from "./generation-status";
  import { eventsFromSemanticFrame, projectConversationEvents } from "./timeline-events";
  import TurnWorkingIndicator from "./TurnWorkingIndicator.svelte";
  import type { AgentEvent, AgentSemanticFrame, DetailPanelModel, EventTimelineModel, ResourceMessageStatus, TimelineItem } from "./models";

  let { model, enabled, followTail, onLatest, children }: {
    model: DetailPanelModel;
    enabled: boolean;
    followTail: boolean;
    onLatest: () => void;
    children: Snippet<[Snippet<[Snippet | undefined, string, boolean]>, Snippet, ResourceMessageStatus | null]>;
  } = $props();

  type ApprovalReply = Parameters<EventTimelineModel["onApproval"]>[2];
  type BackwardPage = { hasMoreBefore?: boolean; nextBefore?: number };
  type ApprovalPage = BackwardPage & { events?: AgentEvent[]; frames?: AgentSemanticFrame[]; page?: BackwardPage };
  type StopResult = { status?: string; pendingSteerCancellationError?: string; taskStateError?: string };

  const client = new ApiClient();
  const base = $derived(`/api/workspaces/${encodeURIComponent(model.workspaceId)}/resources/${encodeURIComponent(model.resourceId)}`);
  const user = $derived(model.currentUserName);
  const context = $derived(enabled && user ? `${base}:${user}` : "");
  let status = $state.raw<ResourceMessageStatus | null>(null);
  let statusContext = $state("");
  const current = $derived(statusContext === context ? status : null);
  const generation = $derived(current?.generation?.generationId || "");
  const turn = $derived(activeTurnKey(current));
  const sessionState = $derived(current?.session?.state);
  let stoppedTurn = $state("");
  let stopping = $state(false);
  let statusError = $state("");
  let stopError = $state("");
  let contextRevision = 0;
  let refreshStatus: (force?: boolean) => Promise<void> = async () => {};

  let approvalCache = $state.raw<{ generation: string; items: Map<string, TimelineItem> }>({ generation: "", items: new Map() });
  let acknowledged = $state.raw<string[]>([]);
  let approvalRetry = $state(0);
  let approvalError = $state("");
  let approvalGroup = $state<HTMLElement>();
  const pendingIDs = $derived(stoppedTurn && turn === stoppedTurn
    ? []
    : [...new Set(current?.session?.pendingApprovalIds || [])].filter(id => !acknowledged.includes(id)));
  const lookup = $derived(JSON.stringify([generation, pendingIDs]));
  const approvals = $derived(approvalCache.generation === generation
    ? pendingIDs.flatMap(id => approvalCache.items.get(id) || [])
    : []);

  const waitingApproval = $derived(pendingIDs.length > 0 || (sessionState === "waiting_approval" && turn !== stoppedTurn));
  const working = $derived(turnIsWorking(current) && turn !== stoppedTurn && !waitingApproval);
  const canStop = $derived(Boolean(turn && turn !== stoppedTurn && current?.session?.id
    && !current?.archived && !current?.generation?.replacementPending
    && (sessionState === "running" || sessionState === "waiting_approval")));
  const hasTail = $derived(working || waitingApproval || canStop || stopping);

  function activeTurnKey(next: ResourceMessageStatus | null): string {
    const generationId = next?.generation?.generationId;
    const turnId = next?.session?.currentTurnId;
    return generationId && turnId ? `${generationId}:${turnId}` : "";
  }

  function errorText(error: unknown): string {
    return error instanceof Error ? error.message : String(error);
  }

  $effect(() => {
    const key = context;
    const path = base, name = user;
    contextRevision++;
    status = null;
    statusContext = key;
    stoppedTurn = "";
    stopping = false;
    statusError = "";
    stopError = "";
    acknowledged = [];
    approvalCache = { generation: "", items: new Map() };
    if (!key) return;

    let disposed = false, reading = false, revision = 0;
    async function refresh(force = false) {
      if (reading && !force) return;
      reading = true;
      const requestRevision = ++revision;
      try {
        const next = await client.latest<ResourceMessageStatus>(`${path}/status`, {
          scope: "execution-status", headers: { "X-PUA-User": name },
        });
        if (disposed || context !== key) return;
        const previousGeneration = status?.generation?.generationId;
        if (previousGeneration !== next.generation?.generationId) acknowledged = [];
        else acknowledged = acknowledged.filter(id => next.session?.pendingApprovalIds?.includes(id));
        if (stoppedTurn && (activeTurnKey(next) !== stoppedTurn || !["running", "waiting_approval"].includes(next.session?.state || ""))) stoppedTurn = "";
        status = next;
        statusError = "";
      } catch (error) {
        if (disposed || context !== key || error instanceof StaleResponseError) return;
        status = null;
        statusError = `Could not load execution status. ${errorText(error)}`;
      } finally {
        if (revision === requestRevision) reading = false;
      }
    }
    refreshStatus = refresh;
    void refresh();
    const timer = setInterval(refresh, 3000);
    return () => {
      disposed = true;
      clearInterval(timer);
      client.requests.abort("execution-status");
      refreshStatus = async () => {};
    };
  });

  $effect(() => {
    const [generationId, ids]: [string, string[]] = JSON.parse(lookup);
    void approvalRetry;
    const controller = new AbortController();
    const [path, name, cache] = untrack(() => [base, user, approvalCache] as const);
    approvalError = "";
    const items = new Map(cache.generation === generationId ? cache.items : []);
    const missing = new Set(ids.filter(id => !items.has(id)));
    if (!generationId || !missing.size) return;

    async function load() {
      const query = new URLSearchParams({ generationId, latest: "true", limit: "250" });
      try {
        while (missing.size && !controller.signal.aborted) {
          const page = await client.request<ApprovalPage>(`${path}/events?${query}`, {
            headers: { "X-PUA-User": name }, signal: controller.signal,
          });
          if (controller.signal.aborted) return;
          const events = page.frames?.flatMap(eventsFromSemanticFrame) ?? page.events ?? [];
          for (const item of projectConversationEvents(events)) {
            if (item.kind === "approval" && item.status === "pending" && item.approvalId && missing.delete(item.approvalId)) {
              items.set(item.approvalId, item);
            }
          }
          approvalCache = { generation: generationId, items: new Map(items) };
          const before = query.get("before");
          const { hasMoreBefore, nextBefore } = page.page ?? page;
          if (!hasMoreBefore || !nextBefore || (before && nextBefore >= Number(before))) break;
          query.delete("latest");
          query.set("before", String(nextBefore));
        }
        if (missing.size && !controller.signal.aborted) approvalError = "Some pending approval details could not be found.";
      } catch (error) {
        if (!controller.signal.aborted) approvalError = `Could not load pending approvals. ${errorText(error)}`;
      }
    }
    void load();
    return () => controller.abort();
  });

  function control<Result>(action: string, generationId: string, body?: unknown) {
    return client.request<Result>(`${base}/${action}?generationId=${encodeURIComponent(generationId)}`, {
      method: "POST", headers: { "X-PUA-User": user, "Content-Type": "application/json" },
      body: body === undefined ? undefined : JSON.stringify(body),
    });
  }

  async function stop(event: MouseEvent) {
    if (!canStop || stopping) return;
    const key = context, revision = contextRevision, generationId = generation, requestedTurn = turn;
    const isCurrent = () => key === context && revision === contextRevision;
    const button = event.currentTarget as HTMLButtonElement;
    stopping = true;
    stopError = "";
    try {
      const result = await control<StopResult>("turn/end", generationId);
      if (!isCurrent()) return;
      if (result?.status === "interrupted") stoppedTurn = requestedTurn;
      if (result?.pendingSteerCancellationError || result?.taskStateError) {
        stopError = [
          "Execution stopped.",
          result.pendingSteerCancellationError && `Could not cancel pending steer inputs. ${result.pendingSteerCancellationError}`,
          result.taskStateError && `Could not pause the task. ${result.taskStateError}`,
        ].filter(Boolean).join(" ");
      }
      if (document.activeElement === button) onLatest();
    } catch (error) {
      if (!isCurrent()) return;
      const rejected = error instanceof ApiError && error.status < 500;
      stopError = `${rejected ? "Could not stop execution." : "Could not confirm that execution stopped."} ${errorText(error)}`;
    } finally {
      if (isCurrent()) await refreshStatus(true);
      if (isCurrent()) stopping = false;
    }
  }

  async function resolveApproval(generationId: string, id: string, reply: ApprovalReply) {
    if (generationId !== generation || !pendingIDs.includes(id)) throw new Error("This approval is no longer pending.");
    const key = context;
    const revision = contextRevision;
    await control("approval", generationId, { requestId: id, ...reply });
    if (key === context && revision === contextRevision && generationId === generation) {
      acknowledged = [...acknowledged, id];
      void refreshStatus(true);
    }
  }

  function viewApprovals() {
    approvalGroup?.focus({ preventScroll: true });
    approvalGroup?.scrollIntoView?.({ block: "center" });
  }
</script>

{#snippet tail(trigger: Snippet | undefined, label: string, closed: boolean)}
  {#if approvals.length}
    <section class="public-chat-approvals" aria-label="Pending approvals" tabindex="-1" bind:this={approvalGroup}>
      {#each approvals as item (`${generation}:${item.approvalId}`)}
        <ApprovalCard {item} generationId={generation} contextIdentity={context} onApproval={resolveApproval} onToast={model.onToast} />
      {/each}
    </section>
  {/if}
  {#if !closed && (hasTail || trigger)}
    <div class="public-chat-execution" role="group" aria-label="Execution controls">
      {@render trigger?.()}
      {#if working}
        <TurnWorkingIndicator label={label || "Agent is working…"} />
      {:else if waitingApproval}
        <span role="status">Waiting for approval</span>
      {/if}
      {#if canStop || stopping}
        <button type="button" class="public-chat-stop ghost-button" disabled={stopping} onclick={stop}>{stopping ? "Stopping…" : "Stop execution"}</button>
      {/if}
    </div>
  {/if}
{/snippet}

{#snippet notices()}
  {#if pendingIDs.length || !followTail}
    <div class="public-chat-shortcuts">
      {#if approvals.length}
        <button type="button" class="ghost-button" onclick={viewApprovals}>View {pendingIDs.length === 1 ? "pending approval" : `${pendingIDs.length} pending approvals`}</button>
      {:else if pendingIDs.length && !approvalError}
        <span role="status">Loading pending approvals…</span>
      {/if}
      {#if !followTail}
        <button type="button" class="ghost-button" onclick={onLatest}>Jump to latest</button>
      {/if}
    </div>
  {/if}
  {#if approvalError}
    <div class="public-chat-error" role="alert">
      <span>{approvalError}</span>
      <button type="button" class="ghost-button" onclick={() => approvalRetry++}>Retry approvals</button>
    </div>
  {/if}
  {#each [statusError, stopError, current?.configError, current?.lastError].filter(Boolean) as message}
    <div class="public-chat-error" role="alert">{message}</div>
  {/each}
{/snippet}

{@render children(tail, notices, current)}

<style>
  .public-chat-execution, .public-chat-shortcuts {
    display: flex;
    align-items: center;
    flex-wrap: wrap;
    gap: 4px 10px;
    color: var(--muted-strong);
    font-size: 12px;
    --turn-working-margin: 0;
  }
  .public-chat-execution { flex: 0 0 100%; padding-bottom: 2px; }
  .public-chat-stop { color: var(--red-deep); font-weight: 650; }
  .public-chat-approvals { display: grid; flex: 0 1 min(100%, 720px); min-width: 0; gap: 6px; scroll-margin-bottom: 110px; }
  .public-chat-shortcuts button, .public-chat-stop { min-height: 32px; padding: 4px 8px; }
  .public-chat-shortcuts:empty { display: none; }
  .public-chat-error {
    padding: 5px 8px;
    border-left: 2px solid var(--red);
    color: var(--red-deep);
    overflow-wrap: anywhere;
    font-size: 12px;
  }
  @container shared-chat (max-width: 500px) {
    .public-chat-shortcuts button, .public-chat-stop { min-height: 44px; }
  }
</style>
