<script lang="ts">
  import "./FilePreviewFullscreen.css";

  import { ApiClient } from "../api/client";
  import { puaRoutePath } from "../controllers/route-controller";
  import type { TaskTemplate } from "./models";
  import FilePreviewModal from "./FilePreviewModal.svelte";
  import Icon from "./Icon.svelte";
  import { clearFilePreviewHandoff, markdownEditorSessions, readFilePreviewHandoff } from "./markdown-editor-session";
  import type { FilePreviewModel } from "./models";

  // The full-screen page is a separate browsing context: parse its own URL and
  // restore any editor state handed over from the dialog window.
  const params = new URLSearchParams(window.location.search);
  const workspaceId = params.get("workspaceId") || "";
  const resourceId = params.get("resourceId") || "";
  const section = params.get("section") || "Files";
  const path = params.get("path") || "";
  const urlMode = params.get("mode") as "preview" | "edit" | "annotate" | null;
  const urlEditable = params.get("editable");
  const editable = urlEditable != null
    ? urlEditable === "1"
    : resourceId === "workspace"
      ? path === "AGENTS.md" || path === "chat.md" || path === ""
      : resourceId !== "scheduler";

  const handoff = readFilePreviewHandoff();
  const restoredMode: "preview" | "edit" | "annotate" = urlMode ?? handoff?.mode ?? "preview";
  if (handoff && handoff.workspaceId === workspaceId && handoff.resourceId === resourceId && handoff.section === section && handoff.path === path) {
    if (restoredMode === "edit" || restoredMode === "annotate") {
      const identity = `${workspaceId}:${resourceId}:${path}:${restoredMode}`;
      if (typeof handoff.baseline === "string" && typeof handoff.draft === "string") {
        markdownEditorSessions.set(identity, {
          baseline: handoff.baseline,
          baselineHash: handoff.baselineHash || "",
          draft: handoff.draft,
          annotations: (handoff.annotations || []).map((annotation) => ({ ...annotation })),
        });
      } else if (restoredMode === "annotate" && handoff.annotations?.length) {
        markdownEditorSessions.set(identity, {
          baseline: "",
          baselineHash: "",
          draft: "",
          annotations: handoff.annotations.map((annotation) => ({ ...annotation })),
        });
      }
    }
  }
  clearFilePreviewHandoff();

  const client = new ApiClient();
  let selection = $state<{ section: string; path: string; mode?: "edit" | "annotate" } | null>(
    workspaceId && path ? { section, path, mode: restoredMode === "edit" || restoredMode === "annotate" ? restoredMode : undefined } : null
  );
  const invalid = $derived(!workspaceId || !path || !selection);

  function fullscreenURL(): string {
    const next = selection;
    const params = new URLSearchParams({ workspaceId, resourceId, section: next?.section || "Files", path: next?.path || "", mode: "preview" });
    return `/file?${params.toString()}`;
  }

  function openFile(nextPath: string): void {
    selection = { section: "Files", path: nextPath };
    history.replaceState(null, "", fullscreenURL());
  }

  function navigateToResource(target: string): void {
    const route = puaRoutePath(workspaceId, target);
    if (route) window.location.assign(route);
  }

  async function saveMarkdown(nextPath: string, content: string, expectedContentHash: string): Promise<FilePreviewModel> {
    if (resourceId === "workspace" && (nextPath === "AGENTS.md" || nextPath === "")) {
      return client.request<FilePreviewModel>(`/api/workspaces/${encodeURIComponent(workspaceId)}/files?path=${encodeURIComponent("AGENTS.md")}`, {
        method: "PUT",
        body: JSON.stringify({ content, expectedContentHash }),
      });
    }
    if (nextPath.includes("/templates/")) {
      const name = nextPath.split("/").pop()?.replace(/\.(md|markdown|mdown|mkdn)$/i, "") || "template";
      const validation = await client.request<TaskTemplate>(`/api/workspaces/${encodeURIComponent(workspaceId)}/templates/validate`, {
        method: "POST",
        body: JSON.stringify({ name, content }),
      });
      if (!validation.valid) throw new Error(validation.errors?.[0]?.message || "The task template is invalid.");
    }
    return client.request<FilePreviewModel>(`/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/documents?path=${encodeURIComponent(nextPath)}`, {
      method: "PUT",
      body: JSON.stringify({ content, expectedContentHash }),
    });
  }
</script>

{#if invalid}
  <div class="file-fullscreen-state" data-component-owner="file-preview-fullscreen">
    <div class="file-modal-empty error-preview"><Icon name="triangle-alert" /><strong>File preview unavailable</strong><span>Missing workspace or file parameters in the full-screen URL.</span><a class="secondary-button" href="/">Back to PUA</a></div>
  </div>
{:else}
  <FilePreviewModal {client} {workspaceId} {resourceId} selection={selection} {editable} fullscreen resolveResourceTitle={(target) => target} onNavigate={navigateToResource} onOpenFile={openFile} onSaveMarkdown={saveMarkdown} onClose={() => window.close()} onError={(message) => console.warn("Full-screen preview:", message)} />
{/if}
