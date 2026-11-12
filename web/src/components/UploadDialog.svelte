<script lang="ts">
  import "./UploadDialog.css";

  import { onMount } from "svelte";

  import type { ModelChannel } from "./model-channel";
  import Icon from "./Icon.svelte";
  import type { UploadDialogModel } from "./models";

  type UploadStatus = "queued" | "uploading" | "success" | "error";
  interface UploadItem {
    id: number;
    file: File;
    name: string;
    size: number;
    progress: number;
    status: UploadStatus;
    path: string;
    error: string;
  }

  let { channel }: { channel: ModelChannel<UploadDialogModel> } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  let identity = $state("");
  let items = $state<UploadItem[]>([]);
  let nextId = 1;
  let input: HTMLInputElement = $state()!;
  const requests = new Map<number, XMLHttpRequest>();
  const busy = $derived(items.some((item) => item.status === "queued" || item.status === "uploading"));
  const succeeded = $derived(items.filter((item) => item.status === "success").length);
  const failed = $derived(items.filter((item) => item.status === "error").length);

  onMount(() => {
    const unsubscribe = channel.subscribe((next) => {
      model = next;
      if (next.identity !== identity) {
        abortAll();
        identity = next.identity;
        items = [];
        nextId = 1;
        if (next.open) queueMicrotask(() => document.getElementById("agentUploadDropZone")?.focus({ preventScroll: true }));
      }
    });
    const paste = (event: ClipboardEvent) => {
      if (!model.open) return;
      const files = clipboardFiles(event.clipboardData);
      if (!files.length) return;
      event.preventDefault();
      enqueue(files);
    };
    document.addEventListener("paste", paste);
    const keydown = (event: KeyboardEvent) => {
      if (model.open && event.key === "Escape" && !busy) {
        event.preventDefault();
        close();
      }
    };
    document.addEventListener("keydown", keydown);
    return () => {
      unsubscribe();
      document.removeEventListener("paste", paste);
      document.removeEventListener("keydown", keydown);
      abortAll();
    };
  });

  function abortAll(): void {
    for (const request of requests.values()) request.abort();
    requests.clear();
  }

  function clipboardFiles(data: DataTransfer | null): File[] {
    const itemFiles = Array.from(data?.items || []).filter((item) => item.kind === "file").map((item) => item.getAsFile()).filter((file): file is File => Boolean(file));
    return itemFiles.length ? itemFiles : Array.from(data?.files || []);
  }

  function clipboardName(file: File, index: number): string {
    const extensions: Record<string, string> = { "image/png": "png", "image/jpeg": "jpg", "image/gif": "gif", "image/webp": "webp", "application/pdf": "pdf" };
    return `clipboard-${Date.now()}-${index + 1}.${extensions[file.type] || "bin"}`;
  }

  function enqueue(files: File[] | FileList): void {
    const selected = Array.from(files || []);
    if (!model.open || !selected.length) return;
    const added = selected.map((file, index): UploadItem => ({
      id: nextId++, file, name: file.name || clipboardName(file, index), size: file.size || 0,
      progress: 0, status: "queued", path: "", error: "",
    }));
    items = [...items, ...added];
    for (const item of added) upload(item, model.identity, model.workspaceId, model.resourceId);
  }

  function updateItem(id: number, update: Partial<UploadItem>): void {
    items = items.map((item) => item.id === id ? { ...item, ...update } : item);
  }

  function upload(item: UploadItem, requestIdentity: string, workspaceId: string, resourceId: string): void {
    updateItem(item.id, { status: "uploading" });
    const request = new XMLHttpRequest();
    requests.set(item.id, request);
    request.open("POST", `/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/uploads`);
    if (model.currentUserName) request.setRequestHeader("X-PUA-User", model.currentUserName);
    request.responseType = "json";
    request.upload.addEventListener("progress", (event) => {
      if (model.identity !== requestIdentity || !event.lengthComputable) return;
      updateItem(item.id, { progress: Math.min(99, Math.round(event.loaded / event.total * 100)) });
    });
    request.addEventListener("load", () => {
      requests.delete(item.id);
      if (model.identity !== requestIdentity || model.workspaceId !== workspaceId || model.resourceId !== resourceId) return;
      const response = request.response || {};
      if (request.status >= 200 && request.status < 300) updateItem(item.id, { status: "success", progress: 100, path: response.path || "", name: response.name || item.name });
      else updateItem(item.id, { status: "error", error: response.error || `${request.status} ${request.statusText}` });
    });
    request.addEventListener("error", () => {
      requests.delete(item.id);
      if (model.identity === requestIdentity) updateItem(item.id, { status: "error", error: "Network error while uploading." });
    });
    const body = new FormData();
    body.append("file", item.file, item.name);
    request.send(body);
  }

  function close(): void {
    if (busy) return;
    model.onDone(items.filter((item) => item.status === "success" && item.path).map((item) => item.path), { workspaceId: model.workspaceId, resourceId: model.resourceId });
  }

  function formatBytes(size: number): string {
    if (size < 1024) return `${size} B`;
    if (size < 1024 * 1024) return `${(size / 1024).toFixed(1)} KB`;
    return `${(size / 1024 / 1024).toFixed(1)} MB`;
  }

  function presentation(item: UploadItem): { icon: string; label: string } {
    if (item.status === "queued") return { icon: "clock-3", label: "Queued" };
    if (item.status === "uploading") return { icon: "loader-circle", label: `Uploading ${item.progress}%` };
    if (item.status === "success") return { icon: "circle-check", label: "Uploaded" };
    return { icon: "triangle-alert", label: "Failed" };
  }
</script>

{#if model.open}
  <div class="upload-dialog-layer" role="presentation">
    <button class="upload-dialog-backdrop modal-enter" type="button" aria-label="Close" onclick={close}></button>
    <div class="upload-dialog modal-enter" role="dialog" aria-modal="true" aria-label="Upload files">
      <header class="upload-dialog-header">
        <div><strong>Upload files</strong><span>Files are saved in this resource's artifacts/upload/ directory.</span></div>
        <button class="icon-button" type="button" title="Close" aria-label="Close" disabled={busy} onclick={close}><Icon name="x" /></button>
      </header>
      <div class="upload-dialog-content">
        <input id="agentUploadInput" bind:this={input} type="file" multiple hidden onchange={() => input.files && enqueue(input.files)} />
        <div id="agentUploadDropZone" class="upload-drop-zone" tabindex="0" role="button"
          ondragover={(event) => { event.preventDefault(); event.currentTarget.classList.add("dragging"); }}
          ondragleave={(event) => event.currentTarget.classList.remove("dragging")}
          ondrop={(event) => { event.preventDefault(); event.currentTarget.classList.remove("dragging"); if (event.dataTransfer?.files) enqueue(event.dataTransfer.files); }}
          onkeydown={(event) => { if (event.key === "Enter" || event.key === " ") { event.preventDefault(); input.click(); } }}>
          <Icon name="clipboard-paste" /><strong>Paste files from the clipboard</strong><span>or choose one or more files from this device</span>
          <button id="agentUploadChooseButton" type="button" class="secondary-button" onclick={() => input.click()}><Icon name="folder-open" /><span>Choose files</span></button>
        </div>
        <div class="upload-list" aria-live="polite">
          {#if !items.length}<div class="upload-empty">Selected or pasted files upload automatically.</div>{/if}
          {#each items as item (item.id)}
            {@const shown = presentation(item)}
            <div class:upload-item-success={item.status === "success"} class:upload-item-error={item.status === "error"} class:upload-item-uploading={item.status === "uploading"} class="upload-item">
              <div class="upload-item-heading"><span class="upload-item-status-icon"><span class="upload-item-status upload-item-status-queued"><Icon name="clock-3" /></span><span class="upload-item-status upload-item-status-uploading"><Icon name="loader-circle" /></span><span class="upload-item-status upload-item-status-success"><Icon name="circle-check" /></span><span class="upload-item-status upload-item-status-error"><Icon name="triangle-alert" /></span></span><span><strong>{item.name}</strong><small>{formatBytes(item.size)}</small></span><em>{shown.label}</em></div>
              <div class="upload-progress" role="progressbar" aria-label={item.name} aria-valuemin="0" aria-valuemax="100" aria-valuenow={item.progress}><span style:width={`${item.progress}%`}></span></div>
              {#if item.status === "success"}<small class="upload-result-path">{item.path}</small>{/if}
              {#if item.status === "error"}<small class="upload-error">{item.error || "Upload failed"}</small>{/if}
            </div>
          {/each}
        </div>
      </div>
      <footer class="upload-dialog-footer">
        <span>{busy ? "Wait for uploads to finish before closing." : items.length ? `${succeeded} uploaded${failed ? ` · ${failed} failed` : ""}. Successful paths will be added to the chat input.` : "No files selected."}</span>
        <button type="button" disabled={busy} onclick={close}>Done</button>
      </footer>
    </div>
  </div>
{/if}
