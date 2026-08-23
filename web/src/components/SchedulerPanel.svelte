<script lang="ts">
  import "./SchedulerPanel.css";

  import { onDestroy } from "svelte";
  import { ApiClient } from "../api/client";
  import { confirmDialog } from "../controllers/confirm-dialog-controller";
  import type { ScheduleRecord, SchedulerConfigRecord } from "../models/workspace";
  import Icon from "./Icon.svelte";

  let { workspaceId, config, resolveResourceTitle, onChanged, onToast }: {
    workspaceId: string;
    config: SchedulerConfigRecord;
    resolveResourceTitle: (resourceId: string) => string | null;
    onChanged: () => Promise<void>;
    onToast: (message: string) => void;
  } = $props();
  type OperationLease = { identity: string; key: string; generation: number };
  const client = new ApiClient();
  let disposed = false;
  onDestroy(() => {
    disposed = true;
    client.dispose();
  });
  let editingId = $state("");
  let description = $state("");
  let condition = $state("");
  let target = $state("workspace");
  // The Scheduler resource is fixed, so its Workspace is the operation identity.
  // svelte-ignore state_referenced_locally
  let localIdentity = $state(workspaceId);
  let operationGeneration = 0;
  let pendingOperations = $state(new Map<string, OperationLease>());
  const saving = $derived(pendingOperations.has("save"));
  const targetError = $derived(validateTarget(target));

  $effect(() => {
    if (workspaceId === localIdentity) return;
    localIdentity = workspaceId;
    pendingOperations = new Map();
    clearForm();
  });

  function validateTarget(value: string): string {
    const resourceId = value.trim();
    if (!resourceId) return "Target resource is required.";
    if (resourceId === "workspace" || resourceId === "scheduler") return "";
    return resolveResourceTitle(resourceId)
      ? ""
      : "Target must be an open resource in the current Workspace.";
  }

  function edit(schedule: ScheduleRecord): void {
    editingId = schedule.id;
    description = schedule.description;
    condition = schedule.condition;
    target = schedule.target;
  }

  function clearForm(): void {
    editingId = "";
    description = "";
    condition = "";
    target = "workspace";
  }

  function scheduleOperationKey(schedule: ScheduleRecord): string {
    return `schedule:${schedule.id}`;
  }

  function beginOperation(key: string): OperationLease | null {
    if (pendingOperations.has(key)) return null;
    const lease = { identity: workspaceId, key, generation: ++operationGeneration };
    pendingOperations = new Map(pendingOperations).set(key, lease);
    return lease;
  }

  function operationIsCurrent(lease: OperationLease): boolean {
    return !disposed
      && workspaceId === lease.identity
      && pendingOperations.get(lease.key)?.generation === lease.generation;
  }

  function finishOperation(lease: OperationLease): boolean {
    if (!operationIsCurrent(lease)) return false;
    const next = new Map(pendingOperations);
    next.delete(lease.key);
    pendingOperations = next;
    return true;
  }

  function triggerLabel(schedule: ScheduleRecord): string {
    const trigger = schedule.trigger;
    if (!trigger) return "Needs compilation";
    if (trigger.type === "at") return `At ${trigger.at}`;
    if (trigger.type === "interval") return `Every ${trigger.everySeconds}s from ${trigger.anchorAt}`;
    return `${trigger.cron} (${trigger.timeZone})`;
  }

  async function saveSchedule(): Promise<void> {
    if (!description.trim() || !condition.trim() || targetError) return;
    const lease = beginOperation("save");
    if (!lease) return;
    const wasEditing = Boolean(editingId);
    const scheduleId = editingId;
    try {
      const path = `/api/workspaces/${encodeURIComponent(lease.identity)}/scheduler${scheduleId ? `/${encodeURIComponent(scheduleId)}` : ""}`;
      await client.request(path, {
        method: scheduleId ? "PUT" : "POST",
        body: JSON.stringify({ description, condition, target })
      });
      if (!operationIsCurrent(lease)) return;
      clearForm();
      await onChanged();
      if (!operationIsCurrent(lease)) return;
      onToast(wasEditing ? "Schedule update request sent." : "Schedule request sent.");
    } catch (reason) {
      if (operationIsCurrent(lease)) onToast(reason instanceof Error ? reason.message : String(reason));
    } finally {
      finishOperation(lease);
    }
  }

  async function remove(schedule: ScheduleRecord): Promise<void> {
    const lease = beginOperation(scheduleOperationKey(schedule));
    if (!lease) return;
    try {
      if (!(await confirmDialog({ title: "Remove schedule", message: `Remove schedule ${schedule.id}?`, confirmLabel: "Remove", danger: true }))) return;
      if (!operationIsCurrent(lease)) return;
      await client.request(`/api/workspaces/${encodeURIComponent(lease.identity)}/scheduler/${encodeURIComponent(schedule.id)}`, { method: "DELETE" });
      if (!operationIsCurrent(lease)) return;
      if (editingId === schedule.id) clearForm();
      await onChanged();
      if (!operationIsCurrent(lease)) return;
      onToast("Schedule removed.");
    } catch (reason) {
      if (operationIsCurrent(lease)) onToast(reason instanceof Error ? reason.message : String(reason));
    } finally {
      finishOperation(lease);
    }
  }

  async function setPaused(schedule: ScheduleRecord, paused: boolean): Promise<void> {
    const lease = beginOperation(scheduleOperationKey(schedule));
    if (!lease) return;
    try {
      await client.request(`/api/workspaces/${encodeURIComponent(lease.identity)}/scheduler/${encodeURIComponent(schedule.id)}/${paused ? "pause" : "resume"}`, { method: "POST" });
      if (!operationIsCurrent(lease)) return;
      await onChanged();
      if (!operationIsCurrent(lease)) return;
      onToast(paused ? "Schedule paused." : "Schedule resumed.");
    } catch (reason) {
      if (operationIsCurrent(lease)) onToast(reason instanceof Error ? reason.message : String(reason));
    } finally {
      finishOperation(lease);
    }
  }
</script>

<div class="schedule-editor" data-component-owner="scheduler-panel">
  <div class="schedule-editor-heading"><div><strong>{editingId ? "Edit schedule" : "Add schedule"}</strong><span>The Scheduler Agent compiles this natural-language request into a native trigger and asks when timing or timezone is ambiguous.</span></div>{#if editingId}<button type="button" class="secondary-button" onclick={clearForm}>Cancel edit</button>{/if}</div>
  <label><span>Description</span><input bind:value={description} placeholder="What should the Scheduler understand?" /></label>
  <label><span>Condition</span><textarea bind:value={condition} rows="3" placeholder="For example: when the release branch is green after 09:00 Shanghai time"></textarea></label>
  <label><span>Target resource ID</span><input bind:value={target} placeholder="workspace, scheduler, project1, or project1.task1" aria-invalid={Boolean(targetError)} aria-describedby={targetError ? "schedule-target-error" : undefined} />{#if targetError}<small id="schedule-target-error" class="schedule-field-error" role="alert">{targetError}</small>{/if}</label>
  <button type="button" class="primary-button" class:busy={saving} class:editing={Boolean(editingId)} disabled={saving || !description.trim() || !condition.trim() || Boolean(targetError)} onclick={saveSchedule}><span class="schedule-icon schedule-icon-busy"><Icon name="loader-circle" /></span><span class="schedule-icon schedule-icon-editing"><Icon name="save" /></span><span class="schedule-icon schedule-icon-add"><Icon name="plus" /></span><span>{editingId ? "Update schedule" : "Add schedule"}</span></button>
</div>

<div class="schedule-list" data-component-owner="scheduler-panel">
  {#if config.schedules.length}
    {#each config.schedules as schedule (schedule.id)}
      <article class:editing={editingId === schedule.id}>
        <header><div><strong>{schedule.description}</strong><code>{schedule.id} · r{schedule.revision}</code></div><div><button type="button" class="secondary-button" disabled={pendingOperations.has(scheduleOperationKey(schedule))} onclick={() => edit(schedule)}><Icon name="pencil" /><span>Edit</span></button>{#if schedule.effectiveState === "paused" || schedule.effectiveState === "attention_required"}<button type="button" class="secondary-button" disabled={pendingOperations.has(scheduleOperationKey(schedule))} onclick={() => setPaused(schedule, false)}><span>Resume</span></button>{:else if schedule.effectiveState === "active"}<button type="button" class="secondary-button" disabled={pendingOperations.has(scheduleOperationKey(schedule))} onclick={() => setPaused(schedule, true)}><span>Pause</span></button>{/if}<button type="button" class="danger-button" disabled={pendingOperations.has(scheduleOperationKey(schedule))} onclick={() => remove(schedule)}><Icon name="trash-2" /><span>Remove</span></button></div></header>
        <dl><div><dt>Trigger</dt><dd>{triggerLabel(schedule)}</dd></div><div><dt>Condition</dt><dd>{schedule.condition}</dd></div>{#if schedule.guard}<div><dt>Guard</dt><dd>{schedule.guard}</dd></div>{/if}<div><dt>Target</dt><dd><code>{schedule.target}</code></dd></div><div><dt>State</dt><dd>{schedule.effectiveState}</dd></div>{#if schedule.nextRunAt}<div><dt>Next run</dt><dd>{schedule.nextRunAt}</dd></div>{/if}{#if schedule.lastOutcome}<div><dt>Last outcome</dt><dd>{schedule.lastOutcome}</dd></div>{/if}{#if schedule.lastError}<div><dt>Error</dt><dd>{schedule.lastError}</dd></div>{/if}</dl>
      </article>
    {/each}
  {:else}
    <div class="empty-list-row"><Icon name="calendar-clock" /><span>No schedules. Native Scheduler timing is idle.</span></div>
  {/if}
</div>
