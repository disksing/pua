<script lang="ts">
  import "./SchedulerPanel.css";

  import { onDestroy, tick } from "svelte";
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
  let saveError = $state("");
  let editorOpen = $state(false);
  let dialog: HTMLDialogElement | undefined = $state();
  const targetError = $derived(validateTarget(target));
  const descriptionCount = $derived(Array.from(description.trim()).length);
  const descriptionLimit = $derived(config.descriptionMaxChars ?? 32);
  const descriptionError = $derived(descriptionCount > descriptionLimit ? `Description must be at most ${descriptionLimit} characters.` : "");
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

  async function openEditor(schedule?: ScheduleRecord): Promise<void> {
    if (saving) return;
    clearForm();
    if (schedule) {
      editingId = schedule.id;
      description = schedule.description;
      condition = schedule.condition;
      target = schedule.target;
    }
    editorOpen = true;
    await tick();
    dialog?.showModal();
    dialog?.querySelector<HTMLInputElement>("input")?.focus();
  }

  function closeEditor(): void {
    if (saving) return;
    dialog?.close();
  }

  function editorClosed(): void {
    editorOpen = false;
    clearForm();
  }

  function targetTitle(resourceId: string): string | null {
    return resolveResourceTitle(resourceId) || (resourceId === "scheduler" ? "Scheduler" : null);
  }

  function clearForm(): void {
    saveError = "";
    editingId = "";
    description = "";
    condition = "";
    target = "workspace";
  }

  async function saveSchedule(): Promise<void> {
    if (!description.trim() || !condition.trim() || descriptionError || conditionError || targetError || saving) return;
    saving = true;
    saveError = "";
    const wasEditing = Boolean(editingId);
    try {
      const path = `/api/workspaces/${encodeURIComponent(workspaceId)}/scheduler${editingId ? `/${encodeURIComponent(editingId)}` : ""}`;
      await client.request(path, {
        method: editingId ? "PUT" : "POST",
        body: JSON.stringify({ description, condition, target })
      });
      dialog?.close();
      editorOpen = false;
      clearForm();
      await onChanged();
      onToast(wasEditing ? "Schedule updated." : "Schedule added.");
    } catch (reason) {
      const message = reason instanceof Error ? reason.message : String(reason);
      if (editorOpen) saveError = message;
      onToast(message);
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

<div class="schedule-list">
  <header class="schedule-list-heading">
    <div><h3>Schedules <span>{config.schedules.length}</span></h3></div>
    <button type="button" class="primary-button" disabled={saving} onclick={() => openEditor()}><Icon name="plus" /><span>Add schedule</span></button>
  </header>
  {#if config.schedules.length}
    <!-- svelte-ignore a11y_no_noninteractive_tabindex (Keyboard users must be able to scroll the table.) -->
    <div class="schedule-table-scroll" role="region" aria-label="Schedules table" tabindex="0">
      <table class="schedule-table" aria-label="Schedules">
        <thead><tr><th scope="col">Target</th><th scope="col">Schedule / Condition</th><th scope="col" class="schedule-actions-cell">Actions</th></tr></thead>
        <tbody>
          {#each config.schedules as schedule (schedule.id)}
            <tr class:editing={editingId === schedule.id}>
              <td class="schedule-target-cell">
                <strong class:unavailable={!targetTitle(schedule.target)}>{targetTitle(schedule.target) || "Title unavailable"}</strong>
                <code>{schedule.target}</code>
              </td>
              <td class="schedule-description-cell"><strong>{schedule.description}</strong><code>{schedule.id}</code><p>{schedule.condition}</p></td>
              <td class="schedule-actions-cell"><div class="schedule-row-actions"><button type="button" class="secondary-button" onclick={() => openEditor(schedule)}><Icon name="pencil" /><span>Edit</span></button><button type="button" class="danger-button" onclick={() => remove(schedule)}><Icon name="trash-2" /><span>Remove</span></button></div></td>
            </tr>
          {/each}
        </tbody>
      </table>
    </div>
  {:else}
    <div class="empty-list-row"><Icon name="calendar-clock" /><span>No schedules. The Server will not create empty Scheduler Turns.</span></div>
  {/if}
</div>

<dialog bind:this={dialog} class="schedule-dialog" aria-label={editingId ? "Edit schedule" : "Add schedule"} onclose={editorClosed} oncancel={(event) => { if (saving) event.preventDefault(); }}>
{#if editorOpen}
<div class="schedule-editor">
  <div class="schedule-editor-heading"><div><strong>{editingId ? "Edit schedule" : "Add schedule"}</strong><span>Conditions are natural language interpreted by the Scheduler Agent.</span></div><button type="button" class="secondary-button" disabled={saving} onclick={closeEditor}>Cancel</button></div>
  <label><span>Description ({descriptionCount}/{descriptionLimit})</span><input bind:value={description} disabled={saving} placeholder="What should the Scheduler understand?" aria-label="Description" aria-invalid={Boolean(descriptionError)} aria-describedby={descriptionError ? "schedule-description-error" : undefined} />{#if descriptionError}<small id="schedule-description-error" class="schedule-field-error" role="alert">{descriptionError}</small>{/if}</label>
  <label><span>Condition ({conditionCount}/{conditionLimit})</span><textarea bind:value={condition} disabled={saving} rows="3" aria-label="Condition" placeholder="For example: when the release branch is green after 09:00 Shanghai time" aria-invalid={Boolean(conditionError)} aria-describedby={conditionError ? "schedule-condition-error" : undefined}></textarea>{#if conditionError}<small id="schedule-condition-error" class="schedule-field-error" role="alert">{conditionError}</small>{/if}</label>
  <label><span>Target resource ID</span><input bind:value={target} disabled={saving} placeholder="workspace, scheduler, project1, or project1.task1" aria-invalid={Boolean(targetError)} aria-describedby={targetError ? "schedule-target-error" : undefined} />{#if targetError}<small id="schedule-target-error" class="schedule-field-error" role="alert">{targetError}</small>{/if}</label>
  {#if saveError}<small class="schedule-field-error" role="alert">{saveError}</small>{/if}
  <button type="button" class="primary-button" class:busy={saving} class:editing={Boolean(editingId)} disabled={saving || !description.trim() || !condition.trim() || Boolean(descriptionError) || Boolean(conditionError) || Boolean(targetError)} onclick={saveSchedule}><span class="schedule-icon schedule-icon-busy"><Icon name="loader-circle" /></span><span class="schedule-icon schedule-icon-editing"><Icon name="save" /></span><span class="schedule-icon schedule-icon-add"><Icon name="plus" /></span><span>{editingId ? "Update schedule" : "Add schedule"}</span></button>
</div>

{/if}
</dialog>
