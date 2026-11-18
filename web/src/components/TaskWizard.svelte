<script lang="ts">
  import "./TaskWizard.css";

  import AgentBindingSelector from "./AgentBindingSelector.svelte";
  import Icon from "./Icon.svelte";
  import type { CreateDialogModel, CreateDraft, ResourceAgentBindingModel } from "./models";

  let { draft, model }: { draft: CreateDraft; model: CreateDialogModel } = $props();

  let step = $state(0);
  // A freshly opened dialog carries an empty start binding; preselect the
  // binding the new task would resolve to (project task default, else the
  // workspace task default) once the model provides it.
  const bindingInitialized = $derived(Boolean(draft.startBinding.name));
  const bindingDiffers = $derived(Boolean(draft.startBinding.name && model.defaultTaskBinding.name) &&
    (draft.startBinding.kind !== model.defaultTaskBinding.kind ||
      draft.startBinding.name.trim().toLowerCase() !== model.defaultTaskBinding.name.trim().toLowerCase()));

  const titleStep = 0;
  const detailsStep = 1;
  const startStep = 2;
  const stepLabels = ["Title & slug", "Details", "Start options"];
  const summaryTitle = $derived(draft.title.trim());

  $effect(() => {
    if (!bindingInitialized && model.defaultTaskBinding.name) draft.startBinding = { ...model.defaultTaskBinding };
  });

  function canProceed(): boolean {
    if (model.submitting) return false;
    if (step === titleStep) return Boolean(draft.title.trim());
    if (step === startStep && draft.startAfterCreate) return Boolean(draft.startPrompt.trim());
    return true;
  }

  function back(): void {
    if (model.submitting || step === 0) return;
    step--;
  }

  function next(): void {
    if (!canProceed()) return;
    if (step < startStep) {
      step++;
    }
  }

  async function create(): Promise<void> {
    if (model.submitting || !canProceed()) return;
    await model.onSubmit({ ...draft, startBinding: { ...draft.startBinding } });
  }

  function submitForm(event: SubmitEvent): void {
    event.preventDefault();
    // Enter in a single-line input advances the wizard; on the final step it
    // confirms the dialog.
    if (step < startStep) next();
    else void create();
  }

  function selectStartMode(startAfterCreate: boolean): void {
    draft.startAfterCreate = startAfterCreate;
  }

  function selectStartBinding(binding: ResourceAgentBindingModel): void {
    draft.startBinding = binding;
  }
</script>

<form class="task-wizard" data-component-owner="task-wizard" aria-label="Create task wizard" onsubmit={submitForm}>
  <ol class="wizard-steps">
    {#each stepLabels as label, index (label)}
      <li class:active={index === step} class:done={index < step} aria-current={index === step ? "step" : undefined}>
        <span class="wizard-step-num">{#if index < step}<Icon name="check" />{:else}{index + 1}{/if}</span>
        <span class="wizard-step-label">{label}</span>
      </li>
    {/each}
  </ol>

  <div class="wizard-body">
    {#if step === titleStep}
      <section class="create-section create-task-basic" aria-label="Basic information">
        <div class="create-section-title">Basic information</div>
        <label class="wizard-field">
          <span>Task title <small class="create-required">*</small></span>
          <input name="title" required bind:value={draft.title} placeholder="Task title" />
        </label>
        <label class="wizard-field create-task-slug-field">
          <span>Slug <small>(optional)</small></span>
          <span class="create-task-slug-wrap">
            <span class="create-task-slug-prefix" aria-hidden="true">#</span>
            <input name="slug" bind:value={draft.slug} placeholder="optional-slug" />
          </span>
        </label>
      </section>
    {/if}

    {#if step === detailsStep}
      <section class="create-section create-task-details" aria-label="Details">
        <div class="create-section-title">Details</div>
        <textarea name="detail" bind:value={draft.detail} placeholder="Task detail"></textarea>
      </section>
    {/if}

    {#if step === startStep}
      <section class="create-section" aria-label="Start options">
        <div class="create-section-title">Start options</div>
        <div class="wizard-start-cards" role="radiogroup" aria-label="Start options">
          <button type="button" role="radio" aria-checked={!draft.startAfterCreate} class="wizard-start-card" class:selected={!draft.startAfterCreate} disabled={model.submitting} onclick={() => selectStartMode(false)}>
            <span class="wizard-start-card-dot"></span>
            <span><strong>Create only</strong><small>Create the task and select it in the tree; start it later yourself.</small></span>
          </button>
          <button type="button" role="radio" aria-checked={draft.startAfterCreate} class="wizard-start-card" class:selected={draft.startAfterCreate} disabled={model.submitting} onclick={() => selectStartMode(true)}>
            <span class="wizard-start-card-dot"></span>
            <span><strong>Create and start</strong><small>Pick an agent, send a prompt, and the task starts running right away.</small></span>
          </button>
        </div>

        {#if draft.startAfterCreate}
          <div class="wizard-start-config">
            <div class="wizard-field">
              <span>Agent</span>
              <AgentBindingSelector value={draft.startBinding} profiles={model.agentProfiles} agents={model.agents} disabled={model.submitting} ariaLabel="Start agent" openUp={false} onSelect={selectStartBinding} />
            </div>
            <label class="wizard-field">
              <span>Prompt <small>(sent as the first message)</small></span>
              <textarea name="startPrompt" value={draft.startPrompt} oninput={(event) => draft.startPrompt = event.currentTarget.value}></textarea>
            </label>
            {#if bindingDiffers}
              <p class="wizard-binding-note" data-binding-note>The selected agent differs from the default — the task's agent binding will be switched before the prompt is sent.</p>
            {/if}
          </div>
        {/if}

        <dl class="wizard-summary" data-summary>
          <div><dt>Title</dt><dd>{summaryTitle}</dd></div>
          <div><dt>Slug</dt><dd>{draft.slug.trim() || "(auto)"}</dd></div>
        </dl>
      </section>
    {/if}
  </div>

  <div class="wizard-footer">
    <span class="wizard-step-pos">Step {step + 1} of {stepLabels.length}</span>
    <span class="wizard-footer-spacer"></span>
    {#if step > 0}<button type="button" class="secondary-button" disabled={model.submitting} onclick={back}>Back</button>{/if}
    <button type="button" class="secondary-button" disabled={model.submitting} onclick={model.onClose}>Cancel</button>
    <button type="submit" class="wizard-primary primary-button" disabled={!canProceed()}>{model.submitting ? "Creating..." : step === startStep ? (draft.startAfterCreate ? "Create & start" : "Create task") : "Next"}</button>
  </div>
</form>
