<script lang="ts">
  import { onDestroy } from "svelte";
  import Icon from "./Icon.svelte";
  import { positionTimelinePreview, type TimelineSource } from "./shared-chat-timeline";

  let { source, expanded, controls, onToggle, onInspect, status }: {
    source: TimelineSource;
    expanded?: boolean;
    controls?: string;
    onToggle?: (button: HTMLButtonElement) => void;
    onInspect: () => void;
    status?: string;
  } = $props();

  const id = $props.id();
  let button: HTMLButtonElement;
  let preview: HTMLDivElement;
  let linger: ReturnType<typeof setTimeout> | undefined;
  let pinned = false;
  const queued = $derived(expanded === undefined);
  const sender = $derived(source.sender && source.sender.toLowerCase() !== source.label.toLowerCase() ? source.sender : "");
  const heading = $derived([source.label, sender, source.resumed && "Resumed"].filter(Boolean).join(" · "));
  const name = $derived(`${queued ? "Waiting" : "Turn"} · ${heading}${status ? ` · ${status}` : ""}`);
  const previewText = $derived(source.text.length > 480 ? `${source.text.slice(0, 479)}…` : source.text);
  const supportsPopover = typeof HTMLElement !== "undefined" && "showPopover" in HTMLElement.prototype;

  function isPreviewOpen() {
    return supportsPopover && preview.matches(":popover-open");
  }

  function show() {
    clearTimeout(linger);
    if (expanded || !supportsPopover || !preview || isPreviewOpen()) return;
    preview.showPopover();
    positionTimelinePreview(preview);
  }

  function hide() {
    clearTimeout(linger);
    pinned = false;
    if (isPreviewOpen()) preview.hidePopover();
  }

  function leave() {
    if (!pinned && !button.matches(":focus-visible")) linger = setTimeout(hide, 160);
  }

  function activate() {
    if (!queued) {
      hide();
      onToggle?.(button);
    } else if (pinned) {
      hide();
    } else {
      pinned = true;
      onInspect();
      show();
    }
  }

  onDestroy(() => clearTimeout(linger));
</script>

<button
  bind:this={button}
  type="button"
  class="timeline-source-button"
  class:queued
  data-source={source.role}
  aria-label={name}
  aria-describedby={id}
  title={supportsPopover ? undefined : `${name}\n${previewText}`}
  aria-expanded={expanded}
  aria-controls={controls}
  onclick={activate}
  onpointerenter={(event) => { if (event.pointerType !== "touch") show(); }}
  onpointerleave={leave}
  onfocus={() => {
    if (button.matches(":focus-visible")) {
      onInspect();
      show();
    }
  }}
  onblur={hide}
  onkeydown={(event) => {
    if (event.key === "Escape" && isPreviewOpen()) {
      event.preventDefault();
      event.stopPropagation();
      hide();
    }
  }}
>
  <Icon name={source.icon} />
  {#if source.resumed}<span class="resume-marker"><Icon name="rotate-ccw" /></span>{/if}
  {#if queued}<span class="waiting-marker"></span>{/if}
</button>
<div bind:this={preview} popover="auto" role="tooltip" id={id} class="timeline-preview" onpointerenter={() => clearTimeout(linger)} onpointerleave={leave}>
  <strong>{heading}</strong>
  <p>{previewText || "No message preview available."}</p>
</div>

<style>
  .timeline-source-button {
    position: relative;
    display: inline-grid;
    place-items: center;
    flex: none;
    width: 34px;
    height: 34px;
    padding: 0;
    border: 1px solid var(--line);
    border-radius: 8px;
    background: var(--panel);
    color: var(--muted-strong);
  }
  .timeline-source-button[data-source="agent"],
  .timeline-source-button[data-source="notification"] { color: var(--blue-deep); background: var(--blue-soft); }
  .timeline-source-button[data-source="system"] { color: var(--amber-deep); background: var(--amber-soft); }
  .timeline-source-button[data-source="scheduler"] { color: var(--green-deep); background: var(--green-soft); }
  .timeline-source-button:is(:hover, [aria-expanded="true"]) { border-color: currentColor; }
  .timeline-source-button:focus-visible { outline: 2px solid var(--blue); outline-offset: 2px; }
  .timeline-source-button :global(svg) { width: 17px; height: 17px; }
  .resume-marker {
    position: absolute;
    top: 1px;
    right: 1px;
    display: grid;
    place-items: center;
    width: 13px;
    height: 13px;
    border-radius: 50%;
    background: inherit;
  }
  .timeline-source-button .resume-marker :global(svg) { width: 10px; height: 10px; stroke-width: 2.5; }
  .queued { border-style: dashed; }
  .waiting-marker {
    position: absolute;
    right: 3px;
    bottom: 3px;
    width: 5px;
    height: 5px;
    border: 1px solid currentColor;
    border-radius: 50%;
  }
  .timeline-preview {
    position: fixed;
    inset: auto;
    margin: 0;
    box-sizing: border-box;
    width: max-content;
    max-width: min(320px, calc(100vw - 16px));
    max-height: min(260px, 55vh);
    overflow: auto;
    padding: 8px 10px;
    border: 1px solid var(--line);
    border-radius: 8px;
    background: var(--panel);
    color: var(--text);
    box-shadow: 0 4px 14px rgb(0 0 0 / 14%);
    font: 13px / 1.45 Inter, ui-sans-serif, system-ui, sans-serif;
    overflow-wrap: anywhere;
  }
  .timeline-preview p { margin: 3px 0 0; white-space: pre-wrap; }
  @supports not selector(:popover-open) {
    .timeline-preview { display: none; }
  }
  @media (pointer: coarse) {
    .timeline-source-button { width: 44px; height: 44px; }
  }
</style>
