<script lang="ts">
  import "./DetailPanel.css";

  import { onDestroy, onMount, tick } from "svelte";

  import { ApiClient } from "../api/client";
  import { confirmDialog } from "../controllers/confirm-dialog-controller";
  import { RESOURCE_DOCUMENTS } from "../controllers/resource-documents";
  import type { ModelChannel } from "./model-channel";
  import DiffModal from "./DiffModal.svelte";
  import FileBrowser from "./FileBrowser.svelte";
  import FilePreviewModal from "./FilePreviewModal.svelte";
  import Icon from "./Icon.svelte";
  import ChannelEvents from "./ChannelEvents.svelte";
  import MarkdownDocument from "./MarkdownDocument.svelte";
  import SchedulerPanel from "./SchedulerPanel.svelte";
  import ResourceSettingsPanel from "./ResourceSettingsPanel.svelte";
  import type { DetailPanelModel, FilePreviewModel, ResourceFileModel, ResourceRepoModel } from "./models";

  let { channel }: { channel: ModelChannel<DetailPanelModel> } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  let identity = $state("");
  let activeTab = $state("");
  let expanded = $state(new Set<string>());
  let preview = $state<{ section: string; path: string; mode?: "edit" | "annotate" } | null>(null);
  let diffRepo = $state<ResourceRepoModel | null>(null);
  const tabMemory = new Map<string, string>();
  const client = new ApiClient();
  let refreshVersion = 0;

  type PreviewScrollState = { key: string; scrollTop: number; scrollLeft: number };

  const files = $derived((model.detail?.files || []).filter((file) => file.name !== "AGENTS.md"));
  const fileNames = $derived(new Set(files.map((file) => file.name)));
  const agentsFile = $derived(model.workspaceAgents && !model.workspaceAgents.error ? {
    name: "AGENTS.md",
    path: model.workspaceAgents.path || "AGENTS.md",
    content: model.workspaceAgents.content || "",
    contentHash: model.workspaceAgents.contentHash,
  } as ResourceFileModel : null);
  const tabs = $derived(resourceTabs());
  const activePreviewPath = $derived(preview ? `${preview.section}:${preview.path}` : "");
  const filePreviewEditable = $derived(model.resourceType === "workspace"
    ? Boolean(preview && (preview.path === "AGENTS.md" || RESOURCE_DOCUMENTS.some((document) => document.name === preview?.path.replace(/^\//, "")) || preview.path === ""))
    : !model.detail?.archived && (model.resourceType === "project" || model.resourceType === "task"));

  onMount(() => channel.subscribe((next) => {
    const previewScrollState = capturePreviewScrollState();
    const currentRefreshVersion = ++refreshVersion;
    model = next;
    if (next.identity !== identity) {
      if (identity && activeTab) tabMemory.set(identity, activeTab);
      identity = next.identity;
      preview = null;
      diffRepo = null;
      expanded = new Set();
      const remembered = tabMemory.get(identity);
      activeTab = remembered && remembered !== "work" ? remembered : initialTab(next);
      const content = document.getElementById("detailsContent");
      if (content) content.scrollTop = 0;
    } else if (tabs.length && !tabs.some((tab) => tab.id === activeTab)) {
      activeTab = tabs[0].id;
    }
    void tick().then(() => {
      if (currentRefreshVersion === refreshVersion) restorePreviewScrollState(previewScrollState);
    });
  }));

  onMount(() => {
    const keydown = (event: KeyboardEvent) => {
      if (event.key !== "Escape") return;
      if (diffRepo) { event.preventDefault(); diffRepo = null; }
      else if (preview) { event.preventDefault(); preview = null; }
    };
    document.addEventListener("keydown", keydown);
    return () => document.removeEventListener("keydown", keydown);
  });

  onDestroy(() => client.dispose());

  function initialTab(value: DetailPanelModel): string {
    if (value.resourceType === "scheduler") return "schedules";
    if (value.resourceType === "workspace" || value.resourceType === "project" || value.resourceType === "task") return "spec";
    return "execution-events";
  }

  function resourceTabs(): Array<{ id: string; label: string; icon: string }> {
    if (model.resourceType === "workspace") return [
      ...RESOURCE_DOCUMENTS,
	  { id: "agents", label: "AGENTS.md", icon: "file-text" },
	  { id: "wiki", label: "Wiki", icon: "book-open" },
	  { id: "execution-events", label: "Execution events", icon: "history" },
      { id: "settings", label: "Settings", icon: "settings" }
	];
    if (!model.detail) return [];
	if (model.resourceType === "scheduler") return [
	  { id: "schedules", label: "Schedules", icon: "calendar-clock" },
	  { id: "context", label: "Context", icon: "file-text" },
	  { id: "execution-events", label: "Execution events", icon: "history" },
      { id: "settings", label: "Settings", icon: "settings" }
	];
    const result: Array<{ id: string; label: string; icon: string }> = [...RESOURCE_DOCUMENTS];
    result.push({ id: "execution-events", label: "Execution events", icon: "history" }, { id: "artifacts", label: "Artifacts", icon: "paperclip" });
    if (model.resourceType === "task") result.push({ id: "worktrees", label: "Worktrees", icon: "folder-git-2" });
    result.push({ id: "settings", label: "Settings", icon: "settings" });
    return result;
  }

  function documentTab(file: ResourceFileModel): string {
	if (file.name === "scheduler.md") return "context";
    return RESOURCE_DOCUMENTS.find((document) => document.name === file.name)?.id || "";
  }

  function selectTab(tab: string): void {
    activeTab = tab;
    tabMemory.set(identity, tab);
  }

  function resourceReference(id: string): string {
    const segment = id.includes(".") ? id.slice(id.lastIndexOf(".") + 1) : id;
    const match = segment.match(/^(?:project|task)(\d+)$/);
    return `#${match ? match[1] : segment}`;
  }

  function toggleFile(key: string): void {
    const next = new Set(expanded);
    if (next.has(key)) next.delete(key); else next.add(key);
    expanded = next;
  }

  function rawURL(section: string, path: string, download = false): string {
    const suffix = download ? "&download=1" : "";
    return `/api/workspaces/${encodeURIComponent(model.workspaceId)}/files/raw?path=${encodeURIComponent(path)}${suffix}`;
  }

  function openPreview(section: string, path: string, mode?: "edit" | "annotate"): void {
    preview = { section, path, mode };
  }

  function openLinkedFile(path: string): void {
    openPreview("Files", path);
  }

  function openEditor(path: string): void {
    openPreview("Files", path, "edit");
  }

  function openAnnotator(path: string): void {
    openPreview("Files", path, "annotate");
  }

  function deleteArtifact(path: string): void {
    const name = path.split("/").pop() || path;
    void confirmDialog({ title: "Delete artifact", message: `Delete artifact "${name}"? This cannot be undone.`, confirmLabel: "Delete", danger: true }).then(async (confirmed) => {
      if (!confirmed) return;
      try {
        await model.onDeleteArtifact(path);
        if (preview && preview.section === "Artifacts" && preview.path === path) preview = null;
      } catch (err) {
        toastError(err instanceof Error ? err.message : String(err));
      }
    });
  }

  function saveMarkdownViaModal(path: string, content: string, expectedContentHash: string): Promise<FilePreviewModel> {
    if (model.resourceType === "workspace" && (path === "AGENTS.md" || path === "")) {
      return model.onSaveWorkspaceAgents(content, expectedContentHash);
    }
    return model.onSaveMarkdownFile(path, content, expectedContentHash);
  }

  function previewKey(value: { section: string; path: string }): string {
    return `${value.section}:${value.path}`;
  }

  function capturePreviewScrollState(): PreviewScrollState | null {
    if (!preview) return null;
    const scroller = document.querySelector<HTMLElement>("[data-preview-scroll]");
    if (!scroller) return null;
    return {
      key: previewKey(preview),
      scrollTop: scroller.scrollTop,
      scrollLeft: scroller.scrollLeft,
    };
  }

  function restorePreviewScrollState(snapshot: PreviewScrollState | null): void {
    if (!snapshot || !preview || snapshot.key !== previewKey(preview)) return;
    const scroller = document.querySelector<HTMLElement>("[data-preview-scroll]");
    if (!scroller) return;
    scroller.scrollTop = snapshot.scrollTop;
    scroller.scrollLeft = snapshot.scrollLeft;
  }

  function toastError(message: string): void {
    if (message) model.onToast(message);
  }
</script>

{#if !model.workspaceId}
  <div id="detailsContent" class="details-content"><div class="empty-state"><Icon name="folder-search" className="empty-state-icon" /><strong>No workspace selected</strong><span>Add an AgentWorkspace path in the sidebar.</span></div></div>
{:else if model.resourceType === "workspace"}
  <div class="details-header"><h1 class="details-title">{model.workspaceName}</h1></div>
  <div class="details-tabs" role="tablist" aria-label="Workspace details">
    {#each tabs as tab (tab.id)}<button type="button" class:active={activeTab === tab.id} class="details-tab" role="tab" aria-selected={activeTab === tab.id} onclick={() => selectTab(tab.id)}><Icon name={tab.icon} /><span>{tab.label}</span>{#if tab.id === "execution-events" && model.executionUnreadCount}<span class="event-unread-count" aria-label={`${model.executionUnreadCount} unread execution Turns`}>{model.executionUnreadCount}</span>{/if}</button>{/each}
  </div>
  <div id="detailsContent" class="details-content" class:details-events={activeTab === "execution-events"}>
    <div hidden={activeTab !== "agents"}>
      {#if agentsFile}<MarkdownDocument file={agentsFile} workspaceId={model.workspaceId} editable={true} resolveResourceTitle={model.resolveResourceTitle} onNavigate={model.onNavigate} onOpenFile={openLinkedFile} onEdit={openEditor} onAnnotate={openAnnotator} />
      {:else if model.workspaceAgents?.error}<div class="content-section"><div class="file-modal-empty error-preview wiki-status"><Icon name="triangle-alert" /><strong>AGENTS.md unavailable</strong><span>{model.workspaceAgents.error}</span></div></div>
      {:else}<div class="content-section"><div class="file-modal-empty wiki-status"><Icon name="loader-circle" /><strong>Loading AGENTS.md...</strong></div></div>{/if}
    </div>
    {#each RESOURCE_DOCUMENTS as document (document.id)}
      {@const file = model.workspaceDocuments?.[document.name]}
      <div hidden={activeTab !== document.id}>
        {#if file && !file.error}
          <MarkdownDocument file={{ ...file, name: document.name, content: file.content || "" }} workspaceId={model.workspaceId} editable={true} resolveResourceTitle={model.resolveResourceTitle} onNavigate={model.onNavigate} onOpenFile={openLinkedFile} onEdit={openEditor} onAnnotate={openAnnotator} />
        {:else if file?.error}
          <div class="content-section"><div class="file-modal-empty error-preview"><Icon name="triangle-alert" /><strong>{document.label} unavailable</strong><span>{file.error}</span></div></div>
        {:else}
          <div class="content-section"><div class="file-modal-empty"><Icon name="loader-circle" /><strong>Loading {document.name}…</strong></div></div>
        {/if}
      </div>
    {/each}
    <div hidden={activeTab !== "wiki"}>
      {#if model.wiki?.error}<div class="content-section"><div class="file-modal-empty error-preview wiki-status"><Icon name="triangle-alert" /><strong>Wiki unavailable</strong><span>{model.wiki.error}</span></div></div>
      {:else if !model.wiki?.exists}<div class="content-section"><div class="file-modal-empty wiki-status"><Icon name="book-open" /><strong>Wiki not initialized</strong><span>Run pua migrate to create wiki/index.md.</span></div></div>
      {:else}<FileBrowser title="Wiki" entries={model.wiki.entries || []} emptyMessage="No Wiki files yet." {expanded} activePath={activePreviewPath} onToggle={toggleFile} onPreview={openPreview} {rawURL} showHeading={false} />{/if}
    </div>
    <div hidden={activeTab !== "settings"}><ResourceSettingsPanel {model} /></div>
    {#if activeTab === "execution-events"}{#key `${model.identity}:${model.currentUserName}:${activeTab}`}<ChannelEvents {model} onOpenFile={openLinkedFile} onOpenLegacy={(path) => openPreview("Artifacts", path)} />{/key}{/if}
  </div>
{:else}
  <div class="details-header">
    <nav class="breadcrumb" aria-label="Location">
      <button type="button" class="breadcrumb-link" onclick={() => model.onNavigate("workspace")}>{model.workspaceName}</button>
      {#if model.parent}<span class="breadcrumb-separator">/</span><button type="button" class="breadcrumb-link" onclick={() => model.onNavigate(model.parent?.id || "workspace")}>{model.parent.title}</button>{/if}
    </nav>
    <h1 class="details-title">{model.resourceTitle}{#if model.resourceType !== "scheduler"}<code class="resource-ref-badge">{resourceReference(model.resourceId)}</code>{/if}</h1>
  </div>
  {#if model.loading || !model.detail}<div id="detailsContent" class="details-content"><div class="empty-state"><Icon name="loader-circle" className="empty-state-icon" /><strong>Loading details...</strong></div></div>
  {:else}
    <div class="details-tabs" role="tablist" aria-label="Resource details">
      {#each tabs as tab (tab.id)}<button type="button" class:active={activeTab === tab.id} class="details-tab" role="tab" aria-selected={activeTab === tab.id} onclick={() => selectTab(tab.id)}><Icon name={tab.icon} /><span>{tab.label}</span>{#if tab.id === "execution-events" && model.executionUnreadCount}<span class="event-unread-count" aria-label={`${model.executionUnreadCount} unread execution Turns`}>{model.executionUnreadCount}</span>{/if}</button>{/each}
    </div>
    <div id="detailsContent" class="details-content" class:details-events={activeTab === "execution-events"}>
      {#each files as file (file.path || file.name)}<div hidden={activeTab !== documentTab(file)}><MarkdownDocument {file} workspaceId={model.workspaceId} editable={!model.detail.archived && (model.resourceType === "project" || model.resourceType === "task")} resolveResourceTitle={model.resolveResourceTitle} onNavigate={model.onNavigate} onOpenFile={openLinkedFile} onEdit={openEditor} onAnnotate={openAnnotator} /></div>{/each}
      {#if model.resourceType !== "scheduler"}
        {#each RESOURCE_DOCUMENTS as document (document.id)}
          {#if !fileNames.has(document.name)}<div class="content-section" hidden={activeTab !== document.id}><div class="file-modal-empty detail-missing"><Icon name="file-text" /><strong>{document.label} is missing</strong><span>Run pua migrate to initialize {document.name}.</span></div></div>{/if}
        {/each}
      {/if}
      {#if model.resourceType === "scheduler" && model.detail.scheduler}<div hidden={activeTab !== "schedules"}><SchedulerPanel workspaceId={model.workspaceId} config={model.detail.scheduler} resolveResourceTitle={model.resolveResourceTitle} onChanged={model.onRefreshScheduler || (async () => undefined)} onToast={model.onToast} /></div>{/if}
      <div hidden={activeTab !== "settings"}><ResourceSettingsPanel {model} /></div>
      {#if activeTab === "execution-events"}{#key `${model.identity}:${model.currentUserName}:${activeTab}`}<ChannelEvents {model} onOpenFile={openLinkedFile} onOpenLegacy={(path) => openPreview("Artifacts", path)} />{/key}{/if}
      <div hidden={activeTab !== "artifacts"}><FileBrowser title="Artifacts" entries={model.detail.artifacts || []} emptyMessage="No artifacts." {expanded} activePath={activePreviewPath} onToggle={toggleFile} onPreview={openPreview} {rawURL} onDelete={model.detail.archived ? undefined : deleteArtifact} showHeading={false} /></div>
      <div hidden={activeTab !== "worktrees"}><div class="content-section"><div class="worktree-list">{#if model.detail.repos?.length}{#each model.detail.repos as repo (`${repo.name}:${repo.worktreePath}`)}<div class="worktree-row"><div class="worktree-main"><Icon name="git-branch" className="worktree-icon" /><div><strong>{repo.branch || "HEAD"}</strong><span>{repo.name || "repository"}{repo.targetBranch ? ` · base ${repo.targetBranch}` : ""}</span><small>{repo.worktreePath || ""}</small></div></div><button type="button" class="secondary-button" onclick={() => diffRepo = repo}><Icon name="git-compare-arrows" /><span>View Diff</span></button></div>{/each}{:else}<div class="empty-list-row"><Icon name="git-branch" /><span>No worktrees.</span></div>{/if}</div></div></div>
    </div>
  {/if}
{/if}

<FilePreviewModal {client} workspaceId={model.workspaceId} resourceId={model.resourceId} userName={model.currentUserName} selection={preview} editable={filePreviewEditable} resolveResourceTitle={model.resolveResourceTitle} onNavigate={model.onNavigate} onOpenFile={openLinkedFile} onSaveMarkdown={saveMarkdownViaModal} onClose={() => preview = null} onError={toastError} />
<DiffModal {client} workspaceId={model.workspaceId} resourceId={model.resourceId} repo={diffRepo} onClose={() => diffRepo = null} onError={toastError} />
