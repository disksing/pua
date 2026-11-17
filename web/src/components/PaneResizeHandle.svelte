<script lang="ts">
  import "./PaneResizeHandle.css";

  import { onDestroy } from "svelte";

  import type { AppShellModel } from "./models";

  type PaneName = keyof AppShellModel["paneSizes"];

  let { id, kind, className, label, onPreview, onCommit }: {
    id: string;
    kind: PaneName;
    className: string;
    label: string;
    onPreview: (name: PaneName, value: number) => void;
    onCommit: (name: PaneName) => void;
  } = $props();
  let resizeCleanup: (() => void) | null = null;

  onDestroy(() => resizeCleanup?.());

  function beginResize(event: PointerEvent): void {
    // On mobile only the stacked details/chat divider stays draggable.
    if (kind !== "chatHeight" && window.matchMedia("(max-width: 980px)").matches) return;
    event.preventDefault();
    resizeCleanup?.();
    const handle = event.currentTarget as HTMLElement;
    const app = document.getElementById("app");
    const sidebar = document.getElementById("mobileSidebar");
    const workspace = document.querySelector<HTMLElement>(".workspace-panel");
    const chat = document.getElementById("agentPanel");
    if (!app || !sidebar || !workspace || !chat) return;
    // Two-column mode stacks details above chat in one column, so the sidebar
    // only has to leave room for the details minimum.
    const twoColumn = document.body.dataset.layout === "two";
    const startX = event.clientX;
    const startY = event.clientY;
    const startSidebar = sidebar.getBoundingClientRect().width;
    const startChat = chat.getBoundingClientRect().width;
    const startChatHeight = chat.getBoundingClientRect().height;
    const vertical = kind === "chatHeight";
    const bodyClass = vertical ? "resizing-y" : "resizing-x";
    handle.classList.add("dragging");
    document.body.classList.add(bodyClass);
    const move = (moveEvent: PointerEvent) => {
      if (kind === "sidebarWidth") {
        const reserved = twoColumn ? 360 : 360 + 8 + Math.max(320, chat.getBoundingClientRect().width);
        const max = Math.max(220, app.getBoundingClientRect().width - 8 - reserved);
        onPreview(kind, Math.min(max, Math.max(220, startSidebar + moveEvent.clientX - startX)));
      } else if (kind === "chatWidth") {
        const max = Math.max(320, workspace.getBoundingClientRect().width - 360 - 8);
        onPreview(kind, Math.min(max, Math.max(320, startChat - (moveEvent.clientX - startX))));
      } else {
        // Stacked panes shrink down to their 56px header band at most.
        const max = Math.max(56, workspace.getBoundingClientRect().height - 56 - 8);
        onPreview(kind, Math.min(max, Math.max(56, startChatHeight - (moveEvent.clientY - startY))));
      }
    };
    const done = () => {
      handle.classList.remove("dragging");
      document.body.classList.remove(bodyClass);
      window.removeEventListener("pointermove", move);
      window.removeEventListener("pointerup", done);
      window.removeEventListener("pointercancel", done);
      resizeCleanup = null;
      onCommit(kind);
    };
    resizeCleanup = done;
    window.addEventListener("pointermove", move);
    window.addEventListener("pointerup", done, { once: true });
    window.addEventListener("pointercancel", done, { once: true });
  }
</script>

<div {id} class={`resize-handle ${className}`} data-component-owner="pane-resize-handle" role="separator" aria-orientation={kind === "chatHeight" ? "horizontal" : "vertical"} aria-label={label} onpointerdown={beginResize}></div>
