<script lang="ts">
  import "./AppShell.css";

  import { onMount, type Snippet } from "svelte";

  import Icon from "./Icon.svelte";
  import DoctorDialog from "./DoctorDialog.svelte";
  import type { ModelChannel } from "./model-channel";
  import type { AppShellModel } from "./models";
  import PaneResizeHandle from "./PaneResizeHandle.svelte";
  import ProjectTree from "./ProjectTree.svelte";
  import SchedulerNav from "./SchedulerNav.svelte";
  import WorkspaceSwitcher from "./WorkspaceSwitcher.svelte";
  import WorkspaceUserGate from "./WorkspaceUserGate.svelte";

  let { channel, details }: {
    channel: ModelChannel<AppShellModel>;
    details?: Snippet<[Snippet]>;
  } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  let appliedRouteRevision = $state(0);
  let doctorOpen = $state(false);
  let narrow = $state(false);
  let sidebar = $state<HTMLElement>();
  let navigationTrigger = $state<HTMLButtonElement>();
  let navigationClose = $state<HTMLButtonElement>();
  const sidebarOpen = $derived(narrow && model.mobile.sidebarOpen);
  let wasSidebarOpen = false;

  onMount(() => {
    const unsubscribe = channel.subscribe((next) => {
      model = next;
    });
    const keydown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !event.defaultPrevented && sidebarOpen) model.onMobileSidebar(false);
    };
    const popstate = () => {
      void model.onHistoryNavigation(window.location.pathname).catch((reason) => {
        model.onToast(reason instanceof Error ? reason.message : String(reason));
      });
    };
    const viewport = window.visualViewport;
    const viewportTimers = new Set<number>();
    const mobileQuery = typeof window.matchMedia === "function"
      ? window.matchMedia("(max-width: 980px)")
      : { matches: false, addEventListener: () => undefined, removeEventListener: () => undefined } as unknown as MediaQueryList;
    const syncViewport = () => {
      const root = document.documentElement;
      if (!narrow || !viewport) {
        root.style.removeProperty("--app-viewport-width");
        root.style.removeProperty("--app-viewport-height");
        root.style.removeProperty("--app-viewport-offset-top");
        root.style.removeProperty("--app-viewport-offset-left");
        return;
      }
      root.style.setProperty("--app-viewport-width", `${viewport.width}px`);
      root.style.setProperty("--app-viewport-height", `${viewport.height}px`);
      root.style.setProperty("--app-viewport-offset-top", `${viewport.offsetTop}px`);
      root.style.setProperty("--app-viewport-offset-left", `${viewport.offsetLeft}px`);
    };
    const resetViewport = () => {
      if (window.scrollX !== 0 || window.scrollY !== 0) window.scrollTo(0, 0);
      syncViewport();
    };
    const clearViewportTimers = () => {
      for (const timer of viewportTimers) window.clearTimeout(timer);
      viewportTimers.clear();
    };
    const scheduleViewportReset = (delay: number) => {
      const timer = window.setTimeout(() => {
        viewportTimers.delete(timer);
        resetViewport();
      }, delay);
      viewportTimers.add(timer);
    };
    const settleViewport = () => {
      clearViewportTimers();
      scheduleViewportReset(0);
      scheduleViewportReset(300);
    };
    const resize = () => {
      const nextNarrow = mobileQuery.matches;
      if (nextNarrow && !narrow && sidebar?.contains(document.activeElement)) {
        navigationTrigger?.focus({ preventScroll: true });
      }
      narrow = nextNarrow;
      if (!narrow && model.mobile.sidebarOpen) model.onMobileSidebar(false);
      model.onPaneViewport();
      syncViewport();
    };
    window.addEventListener("keydown", keydown);
    document.addEventListener("focusout", settleViewport);
    window.addEventListener("resize", resize);
    window.addEventListener("orientationchange", settleViewport);
    window.addEventListener("popstate", popstate);
    viewport?.addEventListener("resize", syncViewport);
    viewport?.addEventListener("scroll", syncViewport);
    mobileQuery.addEventListener?.("change", resize);
    resize();
    return () => {
      unsubscribe();
      window.removeEventListener("keydown", keydown);
      document.removeEventListener("focusout", settleViewport);
      window.removeEventListener("resize", resize);
      window.removeEventListener("orientationchange", settleViewport);
      window.removeEventListener("popstate", popstate);
      viewport?.removeEventListener("resize", syncViewport);
      viewport?.removeEventListener("scroll", syncViewport);
      mobileQuery.removeEventListener?.("change", resize);
      clearViewportTimers();
      document.body.classList.remove("mobile-sidebar-open", "resizing-x", "resizing-y");
    };
  });

  $effect(() => {
    const open = sidebarOpen;
    if (open === wasSidebarOpen) return;
    wasSidebarOpen = open;
    if (open) {
      navigationClose?.focus({ preventScroll: true });
      return;
    }
    const focused = document.activeElement;
    if (narrow && (focused === document.body || sidebar?.contains(focused))) {
      navigationTrigger?.focus({ preventScroll: true });
    } else if (!narrow && (focused === document.body || focused === navigationClose)) {
      sidebar?.focus({ preventScroll: true });
    }
  });

  $effect(() => {
    const route = model.route;
    if (!route.path || route.revision <= appliedRouteRevision) return;
    appliedRouteRevision = route.revision;
    if (window.location.pathname === route.path) return;
    window.history[route.replace ? "replaceState" : "pushState"]({}, "", route.path);
  });
</script>

{#snippet navigation()}
  <button
    bind:this={navigationTrigger}
    id="mobileMenuButton"
    class="mobile-navigation-button"
    type="button"
    title="Open navigation"
    aria-label="Open navigation"
    aria-controls="mobileSidebar"
    aria-expanded={sidebarOpen}
    onclick={() => model.onMobileSidebar(true)}
  ><Icon name="menu" /></button>
{/snippet}

<div data-component-owner="app-shell" class="app-shell">
<aside bind:this={sidebar} id="mobileSidebar" class="sidebar" class:sidebar-open={sidebarOpen} aria-label="Navigation" tabindex="-1" inert={narrow && !sidebarOpen}>
  <div class="brand-band">
    <button
      bind:this={navigationClose}
      id="mobileSidebarClose"
      class="mobile-navigation-button"
      type="button"
      title="Close navigation"
      aria-label="Close navigation"
      onclick={() => model.onMobileSidebar(false)}
    ><Icon name="x" /></button>
    <div class="brand-mark"><img src="/workspace-icons/pua-yellow.png" alt="" aria-hidden="true" /></div>
    <div class="brand-copy"><strong>PUA</strong><span>{model.version}</span></div>
    {#if model.doctor.summary.errors + model.doctor.summary.warnings > 0 || model.doctor.error}
      <button
        id="doctorButton"
        class:has-errors={model.doctor.summary.errors > 0}
        class="brand-doctor"
        type="button"
        title="Workspace problems"
        aria-label={`${model.doctor.summary.errors} errors and ${model.doctor.summary.warnings} warnings`}
        onclick={() => { model.onMobileSidebar(false); doctorOpen = true; }}
      >
        <Icon name={model.doctor.summary.errors > 0 ? "circle-alert" : "triangle-alert"} />
        <span>{model.doctor.summary.errors + model.doctor.summary.warnings}</span>
      </button>
    {/if}
    <button id="systemSettingsButton" class="brand-settings" type="button" title="Settings" aria-label="Settings" onclick={() => { model.onMobileSidebar(false); model.onOpenSettings(); }}><Icon name="settings" /></button>
  </div>
  <div class="workspace-card">
    <WorkspaceSwitcher identity={model.identity} mobileSidebarOpen={model.mobile.sidebarOpen} activeWorkspaceId={model.activeWorkspaceId} workspaces={model.workspaces} onSwitch={model.onSwitchWorkspace} onOpen={() => model.onSelectResource("workspace")} onAdd={model.onAddWorkspace} onToast={model.onToast} />
    {#if !model.userGate?.mode}<SchedulerNav item={model.scheduler || null} onSelect={model.onSelectResource} onToast={model.onToast} />{/if}
  </div>
  {#if !model.userGate?.mode}
    <ProjectTree identity={model.identity} hasWorkspace={Boolean(model.activeWorkspaceId)} loading={model.loading} error={model.error} projects={model.projects} editing={model.treeEditing} drawerOpen={model.mobile.sidebarOpen} filter={model.treeFilter} onFilter={model.onTreeFilter} onCreate={model.onCreateProject} onCreateTask={model.onCreateTask} onArchive={model.onArchive} onToggle={model.onToggleProject} onSelect={model.onSelectResource} onReorder={model.onReorder} onDragState={model.onDragState} onToggleEditing={model.onToggleTreeEditing} onCreateFolder={model.onCreateFolder} onRenameFolder={model.onRenameFolder} onDeleteFolder={model.onDeleteFolder} onToggleFolder={model.onToggleFolder} onToast={model.onToast} />
  {/if}
</aside>
<PaneResizeHandle id="sidebarResize" kind="sidebarWidth" className="sidebar-resize" label="Resize sidebar" onPreview={model.onPanePreview} onCommit={model.onPaneCommit} />
<main class="workspace-panel" inert={sidebarOpen}>
  {#if model.userGate?.mode}
    <WorkspaceUserGate {model} {navigation} />
  {:else}
  <section id="detailsPanel" class="details-panel" data-component-owner="detail-panel">{#if details}{@render details(navigation)}{/if}</section>
  <PaneResizeHandle id="detailsResize" kind="chatWidth" className="details-resize" label="Resize right column" onPreview={model.onPanePreview} onCommit={model.onPaneCommit} />
  <div id="agentPanel" class="agent-panel" aria-hidden="true"></div>
  {/if}
</main>
</div>
{#if doctorOpen}<DoctorDialog snapshot={model.doctor} onClose={() => { doctorOpen = false; }} onRefresh={model.onRefreshDoctor} />{/if}
