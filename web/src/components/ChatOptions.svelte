<script lang="ts">
  import { tick } from "svelte";
  import Icon from "./Icon.svelte";

  let { disabled, onCloseChat }: { disabled: boolean; onCloseChat: () => Promise<boolean> } = $props();
  let open = $state(false);
  let trigger: HTMLButtonElement;
  let closeOption = $state<HTMLButtonElement>();
  let root: HTMLDivElement;

  async function toggle() {
    open = !open;
    if (!open) return;
    await tick();
    if (open) closeOption?.focus();
  }

  async function keydown(event: KeyboardEvent) {
    if (!open || event.key !== "Escape") return;
    event.preventDefault();
    event.stopPropagation();
    open = false;
    await tick();
    trigger?.focus({ preventScroll: true });
  }

  function focusout(event: FocusEvent) {
    if (open && event.relatedTarget instanceof Node && !root.contains(event.relatedTarget)) open = false;
  }

  function pointerdown(event: PointerEvent) {
    if (open && event.target instanceof Node && !root.contains(event.target)) open = false;
  }

  async function closeChat() {
    open = false;
    if (await onCloseChat()) return;
    await tick();
    if (trigger?.isConnected) trigger.focus({ preventScroll: true });
  }
</script>

<svelte:window onpointerdown={pointerdown} />
<div class="chat-options" bind:this={root} onfocusout={focusout}>
  <button type="button" class="chat-options-trigger ghost-button" bind:this={trigger}
    aria-label="Chat options" title="Chat options" aria-expanded={open} {disabled} onclick={toggle} onkeydown={keydown}>
    <Icon name="ellipsis" />
  </button>
  {#if open}
    <div class="chat-options-panel">
      <button type="button" bind:this={closeOption} {disabled} onclick={closeChat} onkeydown={keydown}>Close chat</button>
    </div>
  {/if}
</div>

<style>
  .chat-options { position: relative; flex: 0 0 44px; height: 32px; }
  .chat-options-trigger { width: 44px; height: 44px; padding: 0; margin-top: -6px; }
  .chat-options-trigger :global(svg) { width: 17px; height: 17px; }
  .chat-options-panel {
    position: absolute;
    z-index: 3;
    bottom: calc(100% + 8px);
    right: 0;
    width: max-content;
    padding: 3px;
    border: 1px solid var(--line);
    border-radius: 8px;
    background: var(--panel);
    box-shadow: 0 4px 14px var(--ink-shadow);
  }
  .chat-options-panel button {
    display: block;
    width: 100%;
    min-height: 44px;
    padding: 6px 12px;
    color: var(--text);
    text-align: start;
  }
  .chat-options-panel button:hover { background: var(--panel-sunken); }
</style>
