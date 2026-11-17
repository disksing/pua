<script lang="ts">
  import "./CreateDialog.css";

  import { onMount, tick } from "svelte";

  import { confirmDialogChannel } from "../controllers/confirm-dialog-controller";
  import type { ModelChannel } from "./model-channel";
  import Icon from "./Icon.svelte";
  import ProjectCreateForm from "./ProjectCreateForm.svelte";
  import TaskWizard from "./TaskWizard.svelte";
  import { isValidResourceSlug, type CreateDialogModel, type CreateDraft } from "./models";

  let { channel }: { channel: ModelChannel<CreateDialogModel> } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  // svelte-ignore state_referenced_locally
  let draft = $state(cloneDraft(model.draft));
  let identity = $state("");
  let dialogElement: HTMLElement | undefined = $state();
  let origin: HTMLElement | null = null;

  const isTask = $derived(draft.type === "task");
  const projectDescriptionMissing = $derived(!isTask && !draft.description.trim());
  const projectSlugInvalid = $derived(!isTask && !isValidResourceSlug(draft.slug));

  onMount(() => channel.subscribe((next) => {
    if (next.open && !model.open) {
      origin = document.activeElement instanceof HTMLElement ? document.activeElement : null;
      void tick().then(() => {
        if (model.open && !dialogElement?.contains(document.activeElement)) dialogElement?.focus({ preventScroll: true });
      });
    } else if (!next.open && model.open) {
      if (!model.submitting && origin?.isConnected) origin.focus({ preventScroll: true });
      origin = null;
    }
    model = next;
    if (next.identity !== identity) {
      identity = next.identity;
      draft = cloneDraft(next.draft);
    }
  }));

  onMount(() => {
    const keydown = (event: KeyboardEvent) => {
      if (!model.open || event.defaultPrevented || confirmDialogChannel.current().open) return;
      if (event.key === "Escape") {
        event.preventDefault();
        if (!model.submitting) model.onClose();
        return;
      }
      if (event.key !== "Tab" || !dialogElement) return;
      const focusable = [...dialogElement.querySelectorAll<HTMLElement>("button:not([disabled]), input:not([disabled]), textarea:not([disabled]), select:not([disabled])")];
      if (!focusable.length) return;
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (document.activeElement === dialogElement) { event.preventDefault(); (event.shiftKey ? last : first).focus(); }
      else if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    };
    document.addEventListener("keydown", keydown);
    return () => document.removeEventListener("keydown", keydown);
  });

  function cloneDraft(value: CreateDraft): CreateDraft {
    return { ...value, templateFields: { ...value.templateFields }, startBinding: { ...value.startBinding } };
  }

  async function submit(event: SubmitEvent): Promise<void> {
    event.preventDefault();
    if (!model.submitting && !projectDescriptionMissing && !projectSlugInvalid) await model.onSubmit(cloneDraft(draft));
  }
</script>

{#if model.open}
  <div class="create-dialog-layer" role="presentation">
    <button class="create-dialog-backdrop modal-enter" type="button" aria-label="Close" onclick={model.onClose}></button>
    <div bind:this={dialogElement} class:create-task-dialog={isTask} class="create-dialog modal-enter" role="dialog" aria-modal="true" aria-label={isTask ? "Create task" : "Create project"} tabindex="-1">
      <header class="create-dialog-header">
        <div>
          <strong>{isTask ? "Create task" : "Create project"}</strong>
          {#if isTask}<span>{draft.projectId}</span>{/if}
        </div>
        <button class="icon-button" type="button" title="Close" aria-label="Close" disabled={model.submitting} onclick={model.onClose}><Icon name="x" /></button>
      </header>
      {#key model.identity}
        {#if isTask}
          <TaskWizard {draft} {model} />
        {:else}
          <form id="createDialogForm" class="details-form create-dialog-form" onsubmit={submit}>
            <ProjectCreateForm {draft} />
            <div class="form-actions">
              <button type="submit" class="primary-button" disabled={model.submitting || projectDescriptionMissing || projectSlugInvalid}>{model.submitting ? "Creating..." : "Create"}</button>
              <button type="button" class="secondary-button" disabled={model.submitting} onclick={model.onClose}>Cancel</button>
            </div>
          </form>
        {/if}
      {/key}
    </div>
  </div>
{/if}
