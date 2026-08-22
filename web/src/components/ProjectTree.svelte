<script lang="ts">
  import "./ProjectTree.css";

  import { onDestroy, onMount, tick } from "svelte";

  import Icon from "./Icon.svelte";
  import StatusPresentation from "./StatusPresentation.svelte";
  import type { ShellDragTarget, ShellResourceItem, ShellStatusPresentation } from "./models";

  let {
    identity,
    loading,
    error,
    projects,
    editing,
    onCreate,
    onToggle,
    onSelect,
    onReorder,
    onDragState,
    onToggleEditing,
    onCreateFolder,
    onRenameFolder,
    onDeleteFolder,
    onToggleFolder,
    onToast,
  }: {
    identity: string;
    loading: boolean;
    error: string;
    projects: ShellResourceItem[];
    editing: boolean;
    onCreate: () => void;
    onToggle: (id: string) => Promise<void>;
    onSelect: (id: string) => Promise<void>;
    onReorder: (drag: ShellDragTarget, target: ShellDragTarget, after: boolean) => Promise<void>;
    onDragState: (drag: ShellDragTarget | null) => void;
    onToggleEditing: () => void;
    onCreateFolder: (projectId: string) => Promise<string>;
    onRenameFolder: (id: string, name: string) => Promise<void>;
    onDeleteFolder: (id: string) => Promise<void>;
    onToggleFolder: (id: string) => Promise<void>;
    onToast: (message: string) => void;
  } = $props();
  let drag = $state<ShellDragTarget | null>(null);
  let drop = $state<{ id: string; after: boolean; into: boolean } | null>(null);
  let treeRoot = $state<HTMLElement | null>(null);
  let stateTooltip = $state<{ resourceId: string; text: string; left: number; top: number; pinned: boolean } | null>(null);
  let renamingId = $state("");
  let renameDraft = $state("");
  // svelte-ignore state_referenced_locally
  let previousIdentity = $state(identity);
  // svelte-ignore state_referenced_locally
  let previousEditing = $state(editing);

  $effect(() => {
    if (identity === previousIdentity) return;
    previousIdentity = identity;
    hideStateTooltip();
    cancelRename();
    finishDrag();
  });

  $effect(() => {
    if (editing === previousEditing) return;
    previousEditing = editing;
    if (editing) return;
    hideStateTooltip();
    cancelRename();
    finishDrag();
  });

  $effect(() => {
    const current = stateTooltip;
    const items = projects;
    if (!current) return;
    const item = findTask(items, current.resourceId);
    queueMicrotask(() => {
      if (stateTooltip?.resourceId !== current.resourceId || stateTooltip.pinned !== current.pinned) return;
      const anchor = taskRowElement(current.resourceId);
      if (!item?.statusLabel || !anchor) {
        hideStateTooltip();
        return;
      }
      positionStateTooltip(anchor, item.statusLabel, stateTooltip.pinned, current.resourceId);
    });
  });

  onMount(() => {
    const onPointerDown = (event: PointerEvent) => {
      if (!stateTooltip?.pinned) return;
      const target = event.target instanceof Element ? event.target : null;
      if (target?.closest(".task-state-tooltip") || target?.closest(".task-state-icon")) return;
      hideStateTooltip();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") hideStateTooltip();
    };
    // Scrolls in unrelated containers (e.g. the chat timeline auto-scrolling
    // on new messages) do not move the tooltip anchor; only a viewport scroll
    // or a scroll inside a container holding the tree invalidates its
    // position.
    const onViewportChange = (event: Event) => {
      const target = event.target;
      if (target instanceof Element && (!treeRoot || (target !== treeRoot && !target.contains(treeRoot)))) return;
      hideStateTooltip();
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    window.addEventListener("resize", onViewportChange);
    window.addEventListener("scroll", onViewportChange, true);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
      window.removeEventListener("resize", onViewportChange);
      window.removeEventListener("scroll", onViewportChange, true);
    };
  });

  onDestroy(() => {
    hideStateTooltip();
    finishDrag();
  });

  function statusClass(status: ShellStatusPresentation): string {
    return [status.layoutClassName, status.className].filter(Boolean).join(" ");
  }

  function rowDropClass(id: string): string {
    if (!drop || drop.id !== id) return "";
    if (drop.into) return "drop-into";
    return drop.after ? "drop-after" : "drop-before";
  }

  function compatibleDrop(target: ShellDragTarget): boolean {
    if (!drag || !editing || drag.id === target.id) return false;
    if (drag.kind === "project") return target.kind === "project";
    // Tasks and folders stay inside their own Project.
    if (target.kind === "project") return target.id === drag.projectId;
    if (target.projectId !== drag.projectId) return false;
    if (drag.kind === "folder") {
      // Folders only live at the Project root, never inside other folders.
      return target.kind === "folder" || !target.containerId;
    }
    return true;
  }

  function beginDrag(event: DragEvent, target: ShellDragTarget): void {
    event.stopPropagation();
    drag = target;
    drop = null;
    onDragState(target);
    if (event.dataTransfer) {
      event.dataTransfer.effectAllowed = "move";
      event.dataTransfer.setData("text/plain", target.id);
    }
  }

  function updateDrop(event: DragEvent, target: ShellDragTarget): void {
    if (!compatibleDrop(target)) return;
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = "move";
    // A Task released on a folder row groups into the folder instead of
    // reordering; every other target uses the before/after midpoint rule.
    if (drag?.kind === "task" && target.kind === "folder") {
      drop = { id: target.id, after: false, into: true };
      return;
    }
    const rect = (event.currentTarget as HTMLElement).getBoundingClientRect();
    drop = { id: target.id, after: event.clientY > rect.top + rect.height / 2, into: false };
  }

  async function commitDrop(event: DragEvent, target: ShellDragTarget): Promise<void> {
    event.preventDefault();
    if (!drag || !compatibleDrop(target)) return;
    const current = drag;
    const after = drop?.id === target.id ? drop.after : false;
    finishDrag();
    try {
      await onReorder(current, target, after);
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

  function finishDrag(): void {
    if (drag) onDragState(null);
    drag = null;
    drop = null;
    hideStateTooltip();
  }

  function hideStateTooltip(): void {
    stateTooltip = null;
  }

  function findTask(items: ShellResourceItem[], resourceId: string): ShellResourceItem | null {
    for (const project of items) {
      for (const child of project.children) {
        if (child.id === resourceId) return child;
        const nested = child.children?.find((candidate) => candidate.id === resourceId);
        if (nested) return nested;
      }
    }
    return null;
  }

  function taskRowElement(resourceId: string): HTMLElement | null {
    if (!treeRoot) return null;
    for (const row of treeRoot.querySelectorAll<HTMLElement>("[data-task-id]")) {
      if (row.dataset.taskId === resourceId) return row;
    }
    return null;
  }

  function positionStateTooltip(anchor: Element, text: string, pinned: boolean, resourceId: string): void {
    const rect = anchor.getBoundingClientRect();
    const margin = 8;
    const maxWidth = Math.min(280, window.innerWidth - margin * 2);
    const left = Math.min(Math.max(rect.left, margin), window.innerWidth - maxWidth - margin);
    const below = rect.bottom + 6;
    const estimatedHeight = 44;
    const top = below + estimatedHeight > window.innerHeight - margin
      ? Math.max(margin, rect.top - estimatedHeight - 6)
      : below;
    if (stateTooltip?.resourceId === resourceId && stateTooltip.text === text && stateTooltip.left === left && stateTooltip.top === top && stateTooltip.pinned === pinned) return;
    stateTooltip = { resourceId, text, left, top, pinned };
  }

  function showStateTooltip(event: MouseEvent, item: ShellResourceItem): void {
    if (editing || !item.statusLabel || drag) return;
    positionStateTooltip(event.currentTarget as Element, item.statusLabel, false, item.id);
  }

  function leaveStateTooltip(item: ShellResourceItem): void {
    if (stateTooltip?.pinned || stateTooltip?.resourceId !== item.id) return;
    queueMicrotask(() => {
      if (stateTooltip?.pinned || stateTooltip?.resourceId !== item.id) return;
      const row = taskRowElement(item.id);
      if (row?.matches(":hover")) {
        positionStateTooltip(row, item.statusLabel, false, item.id);
        return;
      }
      hideStateTooltip();
    });
  }

  function toggleStateTooltip(event: MouseEvent, item: ShellResourceItem): void {
    event.preventDefault();
    event.stopPropagation();
    if (!item.statusLabel) return;
    const alreadyPinned = stateTooltip?.pinned && stateTooltip.resourceId === item.id;
    if (alreadyPinned) {
      hideStateTooltip();
      return;
    }
    positionStateTooltip(event.currentTarget as Element, item.statusLabel, true, item.id);
  }

  async function activate(event: MouseEvent, item: ShellResourceItem): Promise<void> {
    const target = event.target instanceof Element ? event.target : null;
    if (target?.closest(".drag-handle") || target?.closest(".task-state-icon") || target?.closest(".row-actions")) return;
    hideStateTooltip();
    try {
      if (item.type === "folder") {
        // Folders are not selectable resources; clicking toggles expansion.
        if (!editing || target?.closest("[data-folder-toggle]")) await onToggleFolder(item.id);
        return;
      }
      if (editing) {
        // Edit mode never navigates; the chevron still expands Projects.
        if (item.type === "project" && target?.closest("[data-project-toggle]")) await onToggle(item.id);
        return;
      }
      if (item.type === "project" && target?.closest("[data-project-toggle]")) {
        // Pointer clicks on the chevron focus the row button; drop that
        // focus so hover-only row actions do not stay pinned visible after
        // the pointer leaves the row.
        (event.currentTarget as HTMLElement | null)?.blur();
        await onToggle(item.id);
      }
      else {
        // Pointer clicks on the row itself focus the row button; drop that
        // focus so hover-only row actions do not stay pinned visible after
        // the pointer leaves the row. Keyboard activation (detail === 0)
        // keeps it.
        if (event.detail > 0) (event.currentTarget as HTMLElement | null)?.blur();
        await onSelect(item.id);
      }
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

  function actionKeydown(event: KeyboardEvent, run: (event: Event) => void): void {
    if (event.key !== "Enter" && event.key !== " ") return;
    event.preventDefault();
    run(event);
  }

  async function addFolder(event: Event, project: ShellResourceItem): Promise<void> {
    event.preventDefault();
    event.stopPropagation();
    try {
      if (!project.expanded) await onToggle(project.id);
      const id = await onCreateFolder(project.id);
      // The user may have started renaming another row while the create
      // round-trip was in flight; do not steal that edit.
      if (id && !renamingId) {
        renamingId = id;
        renameDraft = "";
        await tick();
        focusRenameInput(id);
      }
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

  function startRename(event: Event, folder: ShellResourceItem): void {
    event.preventDefault();
    event.stopPropagation();
    renamingId = folder.id;
    renameDraft = folder.title;
    void tick().then(() => focusRenameInput(folder.id));
  }

  function focusRenameInput(folderId: string): void {
    const input = treeRoot?.querySelector<HTMLInputElement>(`input[data-folder-rename="${folderId}"]`);
    input?.focus();
    input?.select();
  }

  function cancelRename(): void {
    renamingId = "";
    renameDraft = "";
  }

  async function commitRename(folder: ShellResourceItem): Promise<void> {
    if (renamingId !== folder.id) return;
    const name = renameDraft.trim();
    cancelRename();
    if (!name || name === folder.title) return;
    try {
      await onRenameFolder(folder.id, name);
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

  function renameKeydown(event: KeyboardEvent, folder: ShellResourceItem): void {
    if (event.key === "Enter") {
      event.preventDefault();
      void commitRename(folder);
    } else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      cancelRename();
    }
  }

  async function removeFolder(event: Event, folder: ShellResourceItem): Promise<void> {
    event.preventDefault();
    event.stopPropagation();
    try {
      await onDeleteFolder(folder.id);
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }
</script>

<section class="tree-section" class:editing data-component-owner="project-tree">
  <div class="section-title">
    <span class="section-label">Projects</span>
    <span class="section-actions">
      <button id="treeEditButton" type="button" class:active={editing} aria-pressed={editing} title={editing ? "Done reordering" : "Reorder projects"} onclick={onToggleEditing}><Icon name={editing ? "check" : "list-ordered"} /><span>{editing ? "Done" : "Reorder"}</span></button>
      <button id="newProjectButton" type="button" title="New project" onclick={onCreate}><Icon name="plus" /><span>New</span></button>
    </span>
  </div>
  <nav id="projectTree" class="project-tree" data-navigation-identity={identity} bind:this={treeRoot}>
    {#if loading}
      <div class="empty-state"><Icon name="loader-circle" className="empty-state-icon" /><strong>Loading workspace</strong><span>Refreshing navigation...</span></div>
    {:else if error}
      <div class="empty-state" role="alert"><Icon name="circle-alert" className="empty-state-icon" /><strong>Workspace unavailable</strong><span>{error}</span></div>
    {:else if projects.length === 0}
      <div class="empty-state"><Icon name="folder-search" className="empty-state-icon" /><strong>No workspace yet</strong><span>Add a workspace path to begin.</span></div>
    {:else}
      {#each projects as project (project.id)}
        <button type="button" class={`tree-item ${statusClass(project.status)} ${project.active ? "active" : ""} ${drag?.id === project.id ? "drag-source" : ""} ${rowDropClass(project.id)}`} aria-label={project.ariaLabel || undefined} onclick={(event) => activate(event, project)} ondragover={(event) => updateDrop(event, { kind: "project", id: project.id, projectId: "" })} ondrop={(event) => commitDrop(event, { kind: "project", id: project.id, projectId: "" })}>
          <span class="chevron" class:expanded={project.expanded} data-project-toggle={project.children.length ? project.id : undefined}>{#if project.children.length}<Icon name="chevron-right" />{/if}</span>
          {#if editing}
            <Icon name="folder" className="tree-icon" />
          {:else if project.status.hasTaskState}
            <StatusPresentation status={project.status} />
          {:else}
            <Icon name="folder" className="tree-icon" />
          {/if}
          <span class="name"><span class="name-text">{project.title}</span><span class="resource-ref">{project.ref}</span>{#if !editing && project.summary && !project.expanded}<span class="project-task-summary" aria-hidden="true"><span class="project-task-summary-count">{project.summary.taskLabel}</span><span class="project-task-summary-separator">·</span><span class="project-task-summary-running">{project.summary.runningLabel}</span></span>{/if}{#if !editing && project.unreadCount > 0}<span class="unread-badge" aria-label={`${project.unreadCount} unread ${project.unreadCount === 1 ? "Turn" : "Turns"}`}>{project.unreadCount > 99 ? "99+" : project.unreadCount}</span>{/if}</span>
          {#if editing}
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <span class="row-actions"><span class="row-action-button" role="button" tabindex="0" aria-label={`New folder in ${project.title}`} title="New folder" onclick={(event) => addFolder(event, project)} onkeydown={(event) => actionKeydown(event, (e) => void addFolder(e, project))}><Icon name="folder-plus" /></span></span>
            <!-- svelte-ignore a11y_no_static_element_interactions -->
            <span class="drag-handle" draggable="true" title="Drag to reorder" ondragstart={(event) => beginDrag(event, { kind: "project", id: project.id, projectId: "" })} ondragend={finishDrag}><Icon name="grip-vertical" className="drag-handle-icon" /></span>
          {/if}
        </button>
        {#if project.expanded}
          <div class="task-group">
            {#each project.children as child (child.id)}
              {#if child.type === "folder"}
                <!-- The folder row hosts a rename input in edit mode, which
                     is invalid inside a <button>; a div with button semantics
                     keeps the markup valid. -->
                <!-- svelte-ignore a11y_no_static_element_interactions -->
                <div role="button" tabindex="0" class={`tree-item folder-item ${drag?.id === child.id ? "drag-source" : ""} ${rowDropClass(child.id)}`} aria-label={child.ariaLabel || undefined} onclick={(event) => activate(event, child)} onkeydown={(event) => { if (event.key === "Enter" && event.target === event.currentTarget) void activate(event as unknown as MouseEvent, child); }} ondragover={(event) => updateDrop(event, { kind: "folder", id: child.id, projectId: project.id })} ondrop={(event) => commitDrop(event, { kind: "folder", id: child.id, projectId: project.id })}>
                  <span class="chevron" class:expanded={child.expanded} data-folder-toggle={child.id}><Icon name="chevron-right" /></span>
                  <Icon name="folders" className="tree-icon" />
                  {#if renamingId === child.id}
                    <span class="name"><input class="folder-rename-input" data-folder-rename={child.id} bind:value={renameDraft} maxlength="80" placeholder="Folder name" aria-label={`Rename ${child.title}`} onclick={(event) => event.stopPropagation()} onkeydown={(event) => renameKeydown(event, child)} onblur={() => void commitRename(child)} /></span>
                  {:else}
                    <span class="name"><span class="name-text">{child.title}</span><span class="folder-count" aria-hidden="true">{child.children.length}</span>{#if !editing && child.unreadCount > 0}<span class="unread-badge" aria-label={`${child.unreadCount} unread ${child.unreadCount === 1 ? "Turn" : "Turns"}`}>{child.unreadCount > 99 ? "99+" : child.unreadCount}</span>{/if}</span>
                  {/if}
                  {#if editing}
                    <!-- svelte-ignore a11y_no_static_element_interactions -->
                    <span class="row-actions">
                      <!-- svelte-ignore a11y_no_static_element_interactions -->
                      <span class="row-action-button" role="button" tabindex="0" aria-label={`Rename ${child.title}`} title="Rename folder" onclick={(event) => startRename(event, child)} onkeydown={(event) => actionKeydown(event, (e) => startRename(e, child))}><Icon name="pencil" /></span>
                      <!-- svelte-ignore a11y_no_static_element_interactions -->
                      <span class="row-action-button" role="button" tabindex="0" aria-label={`Delete ${child.title}`} title="Delete folder (tasks stay in the project)" onclick={(event) => removeFolder(event, child)} onkeydown={(event) => actionKeydown(event, (e) => void removeFolder(e, child))}><Icon name="trash-2" /></span>
                    </span>
                    <!-- svelte-ignore a11y_no_static_element_interactions -->
                    <span class="drag-handle" draggable="true" title="Drag to reorder" ondragstart={(event) => beginDrag(event, { kind: "folder", id: child.id, projectId: project.id })} ondragend={finishDrag}><Icon name="grip-vertical" className="drag-handle-icon" /></span>
                  {/if}
                </div>
                {#if child.expanded}
                  <div class="task-group folder-task-group">
                    {#each child.children as task (task.id)}
                      {@render taskRow(task, project, child.id)}
                    {/each}
                    {#if editing && child.children.length === 0}
                      <div class="folder-empty">Drag tasks here</div>
                    {/if}
                  </div>
                {/if}
              {:else}
                {@render taskRow(child, project, "")}
              {/if}
            {/each}
          </div>
        {/if}
      {/each}
    {/if}
  </nav>
  {#if stateTooltip}
    <div class="task-state-tooltip" role="tooltip" style={`left:${stateTooltip.left}px;top:${stateTooltip.top}px`}>{stateTooltip.text}</div>
  {/if}
</section>

{#snippet taskRow(task: ShellResourceItem, project: ShellResourceItem, containerId: string)}
  <button type="button" class={`tree-item task-item ${statusClass(task.status)} ${task.active ? "active" : ""} ${drag?.id === task.id ? "drag-source" : ""} ${rowDropClass(task.id)}`} data-task-id={task.id} aria-label={task.ariaLabel || undefined} onmouseenter={(event) => showStateTooltip(event, task)} onmouseleave={() => leaveStateTooltip(task)} onclick={(event) => activate(event, task)} ondragover={(event) => updateDrop(event, { kind: "task", id: task.id, projectId: project.id, containerId })} ondrop={(event) => commitDrop(event, { kind: "task", id: task.id, projectId: project.id, containerId })}>
    <span class="chevron"></span>
    {#if editing}
      <span class="tree-icon-placeholder"></span>
    {:else}
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <!-- svelte-ignore a11y_click_events_have_key_events -->
      <span class="task-state-icon" onclick={(event) => toggleStateTooltip(event, task)}><StatusPresentation status={task.status} /></span>
    {/if}
    <span class="name"><span class="name-text">{task.title}</span><span class="resource-ref">{task.ref}</span>{#if !editing && task.unreadCount > 0}<span class="unread-badge" aria-label={`${task.unreadCount} unread ${task.unreadCount === 1 ? "Turn" : "Turns"}`}>{task.unreadCount > 99 ? "99+" : task.unreadCount}</span>{/if}</span>
    {#if editing}
      <span class="row-actions"></span>
      <!-- svelte-ignore a11y_no_static_element_interactions -->
      <span class="drag-handle" draggable="true" title="Drag to reorder" ondragstart={(event) => beginDrag(event, { kind: "task", id: task.id, projectId: project.id, containerId })} ondragend={finishDrag}><Icon name="grip-vertical" className="drag-handle-icon" /></span>
    {/if}
  </button>
{/snippet}
