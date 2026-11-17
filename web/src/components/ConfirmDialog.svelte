<script lang="ts">
  import "./ConfirmDialog.css";

  import { onMount } from "svelte";

  import type { ConfirmDialogModel } from "../models/common";
  import type { ModelChannel } from "./model-channel";
  import Icon from "./Icon.svelte";

  let { channel }: { channel: ModelChannel<ConfirmDialogModel> } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  let confirmButton: HTMLButtonElement = $state()!;
  let cancelButton: HTMLButtonElement = $state()!;

  onMount(() => {
    let origin: HTMLElement | null = null;
    const unsubscribe = channel.subscribe((next) => {
      const wasOpen = model.open;
      model = next;
      if (next.open && !wasOpen) {
        origin = next.defaultToCancel && document.activeElement instanceof HTMLElement ? document.activeElement : null;
        queueMicrotask(() => (next.defaultToCancel ? cancelButton : confirmButton)?.focus({ preventScroll: true }));
      } else if (!next.open && wasOpen) {
        origin?.focus({ preventScroll: true });
        origin = null;
      }
    });
    const keydown = (event: KeyboardEvent) => {
      if (!model.open || event.defaultPrevented) return;
      if (event.key === "Escape") {
        event.preventDefault();
        model.onResult(false);
      } else if (model.defaultToCancel && event.key === "Tab") {
        event.preventDefault();
        (document.activeElement === cancelButton ? confirmButton : cancelButton)?.focus();
      } else if (!model.defaultToCancel && event.key === "Enter") {
        event.preventDefault();
        model.onResult(true);
      }
    };
    document.addEventListener("keydown", keydown);
    return () => {
      unsubscribe();
      document.removeEventListener("keydown", keydown);
    };
  });
</script>

{#if model.open}
  <div class="confirm-dialog-layer" role="presentation">
    <button class="confirm-dialog-backdrop modal-enter" type="button" aria-label={model.cancelLabel} onclick={() => model.onResult(false)}></button>
    <div class="confirm-dialog modal-enter" role="alertdialog" aria-modal="true" aria-label={model.title}>
      <header class="confirm-dialog-header">
        <span class="confirm-dialog-icon" class:confirm-dialog-icon-danger={model.danger}><Icon name={model.danger ? "triangle-alert" : "circle-help"} /></span>
        <strong>{model.title}</strong>
      </header>
      <div class="confirm-dialog-content"><p>{model.message}</p></div>
      <footer class="confirm-dialog-footer">
        <button bind:this={cancelButton} type="button" class="secondary-button" onclick={() => model.onResult(false)}>{model.cancelLabel}</button>
        <button bind:this={confirmButton} type="button" class="primary-button" class:danger-button={model.danger} onclick={() => model.onResult(true)}>{model.confirmLabel}</button>
      </footer>
    </div>
  </div>
{/if}
