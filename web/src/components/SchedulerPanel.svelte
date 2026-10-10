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
  const client = new ApiClient();
  onDestroy(() => client.dispose());
  let editingId = $state("");
  let description = $state("");
  let condition = $state("");
  let target = $state("workspace");
  let saving = $state(false);
  const targetError = $derived(validateTarget(target));
  const conditionCount = $derived(Array.from(condition.trim()).length);
  const conditionLimit = $derived(config.conditionMaxChars ?? 128);
  const conditionError = $derived(conditionCount > conditionLimit ? `Condition must be at most ${conditionLimit} characters.` : "");

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

  async function saveSchedule(): Promise<void> {
    if (!description.trim() || !condition.trim() || conditionError || targetError || saving) return;
    saving = true;
    const wasEditing = Boolean(editingId);
    try {
      const path = `/api/workspaces/${encodeURIComponent(workspaceId)}/scheduler${editingId ? `/${encodeURIComponent(editingId)}` : ""}`;
      await client.request(path, {
        method: editingId ? "PUT" : "POST",
        body: JSON.stringify({ description, condition, target })
      });
      clearForm();
      await onChanged();
      onToast(wasEditing ? "Schedule updated." : "Schedule added.");
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    } finally {
      saving = false;
    }
  }

  async function remove(schedule: ScheduleRecord): Promise<void> {
    if (!(await confirmDialog({ title: "Remove schedule", message: `Remove schedule ${schedule.id}?`, confirmLabel: "Remove", danger: true }))) return;
    try {
      await client.request(`/api/workspaces/${encodeURIComponent(workspaceId)}/scheduler/${encodeURIComponent(schedule.id)}`, { method: "DELETE" });
      if (editingId === schedule.id) clearForm();
      await onChanged();
      onToast("Schedule removed.");
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }
</script>

<div class="schedule-editor">
  <div class="schedule-editor-heading"><div><strong>{editingId ? "Edit schedule" : "Add schedule"}</strong><span>Conditions are natural language interpreted by the Scheduler Agent.</span></div>{#if editingId}<button type="button" class="secondary-button" onclick={clearForm}>Cancel edit</button>{/if}</div>
  <label><span>Description</span><input bind:value={description} placeholder="What should the Scheduler understand?" /></label>
  <label><span>Condition ({conditionCount}/{conditionLimit})</span><textarea bind:value={condition} rows="3" aria-label="Condition" placeholder="For example: when the release branch is green after 09:00 Shanghai time" aria-invalid={Boolean(conditionError)} aria-describedby={conditionError ? "schedule-condition-error" : undefined}></textarea>{#if conditionError}<small id="schedule-condition-error" class="schedule-field-error" role="alert">{conditionError}</small>{/if}</label>
  <label><span>Target resource ID</span><input bind:value={target} placeholder="workspace, scheduler, project1, or project1.task1" aria-invalid={Boolean(targetError)} aria-describedby={targetError ? "schedule-target-error" : undefined} />{#if targetError}<small id="schedule-target-error" class="schedule-field-error" role="alert">{targetError}</small>{/if}</label>
  <button type="button" class="primary-button" class:busy={saving} class:editing={Boolean(editingId)} disabled={saving || !description.trim() || !condition.trim() || Boolean(conditionError) || Boolean(targetError)} onclick={saveSchedule}><span class="schedule-icon schedule-icon-busy"><Icon name="loader-circle" /></span><span class="schedule-icon schedule-icon-editing"><Icon name="save" /></span><span class="schedule-icon schedule-icon-add"><Icon name="plus" /></span><span>{editingId ? "Update schedule" : "Add schedule"}</span></button>
</div>

<div class="schedule-list">
  {#if config.schedules.length}
    {#each config.schedules as schedule (schedule.id)}
      <article class:editing={editingId === schedule.id}>
        <header><div><strong>{schedule.description}</strong><code>{schedule.id}</code></div><div><button type="button" class="secondary-button" onclick={() => edit(schedule)}><Icon name="pencil" /><span>Edit</span></button><button type="button" class="danger-button" onclick={() => remove(schedule)}><Icon name="trash-2" /><span>Remove</span></button></div></header>
        <dl><div><dt>Condition</dt><dd>{schedule.condition}</dd></div><div><dt>Target</dt><dd><code>{schedule.target}</code></dd></div></dl>
      </article>
    {/each}
  {:else}
    <div class="empty-list-row"><Icon name="calendar-clock" /><span>No schedules. The Server will not create empty Scheduler Turns.</span></div>
  {/if}
</div>
