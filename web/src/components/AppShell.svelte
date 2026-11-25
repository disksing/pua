<script lang="ts">
  import "./AppShell.css";

  import { onMount, tick, type Snippet } from "svelte";

  import Icon from "./Icon.svelte";
  import DoctorDialog from "./DoctorDialog.svelte";
  import type { ModelChannel } from "./model-channel";
  import type { AppShellModel } from "./models";
  import PaneResizeHandle from "./PaneResizeHandle.svelte";
  import ProjectTree from "./ProjectTree.svelte";
  import SchedulerNav from "./SchedulerNav.svelte";
  import WorkspaceSwitcher from "./WorkspaceSwitcher.svelte";
  import WorkspaceUserGate from "./WorkspaceUserGate.svelte";
  import WorkspaceCards from "./WorkspaceCards.svelte";

  let { channel, details }: {
    channel: ModelChannel<AppShellModel>;
    details?: Snippet<[Snippet, Snippet]>;
  } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  let appliedRouteRevision = $state(0);
  let doctorOpen = $state(false);
  let cardsCollapsed = $state(false);
  let cardsDrawerOpen = $state(false);
  let cardsNarrow = $state(false);
  let narrow = $state(false);
  let sidebar = $state<HTMLElement>();
  let navigationTrigger = $state<HTMLButtonElement>();
  let navigationClose = $state<HTMLButtonElement>();
  let cardsPanel = $state<HTMLElement>();
  let cardsTrigger = $state<HTMLButtonElement>();
  let cardsDismiss = $state<HTMLButtonElement>();
  const sidebarOpen = $derived(narrow && model.mobile.sidebarOpen);
  const cardsAvailable = $derived(Boolean(model.cardUser && model.activeWorkspaceId && !model.userGate?.mode));
  const cardsDocked = $derived(cardsAvailable && !cardsNarrow && !cardsCollapsed);
  const cardsOverlay = $derived(cardsAvailable && cardsNarrow && cardsDrawerOpen);
  let wasSidebarOpen = false;

  async function focusCardsControl(): Promise<void> {
    await tick();
    (cardsDocked || cardsOverlay ? cardsDismiss : cardsTrigger)?.focus({ preventScroll: true });
  }

  function openCards(): void {
    if (cardsNarrow) cardsDrawerOpen = true;
    else cardsCollapsed = false;
    void focusCardsControl();
  }

  function closeCardsDrawer(): void {
    if (!cardsOverlay) return;
    cardsDrawerOpen = false;
    void focusCardsControl();
  }

  function dismissCards(): void {
    if (cardsNarrow) {
      closeCardsDrawer();
    } else {
      cardsCollapsed = true;
      void focusCardsControl();
    }
  }

  onMount(() => {
    const unsubscribe = channel.subscribe((next) => {
      model = next;
    });
    const keydown = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || event.defaultPrevented) return;
      if (cardsOverlay) closeCardsDrawer();
      else if (sidebarOpen) model.onMobileSidebar(false);
    };
    const popstate = () => {
      void model.onHistoryNavigation(window.location.pathname).catch((reason) => {
        model.onToast(reason instanceof Error ? reason.message : String(reason));
      });
    };
    const viewport = window.visualViewport;
    const viewportTimers = new Set<number>();
    const mobileQuery = window.matchMedia?.("(max-width: 980px)");
    const cardsQuery = window.matchMedia?.("(max-width: 1440px)");
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
      const nextNarrow = mobileQuery?.matches ?? false;
      if (nextNarrow && !narrow && sidebar?.contains(document.activeElement)) {
        navigationTrigger?.focus({ preventScroll: true });
      }
      narrow = nextNarrow;
      if (!narrow && model.mobile.sidebarOpen) model.onMobileSidebar(false);
      const nextCardsNarrow = cardsQuery?.matches ?? false;
      if (cardsNarrow !== nextCardsNarrow) {
        const focused = document.activeElement;
        const restoreFocus = cardsPanel?.contains(focused) || (focused === cardsTrigger && !cardsCollapsed);
        cardsNarrow = nextCardsNarrow;
        if (!cardsNarrow) cardsDrawerOpen = false;
        if (restoreFocus) void focusCardsControl();
      }
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
    mobileQuery?.addEventListener?.("change", resize);
    cardsQuery?.addEventListener?.("change", resize);
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
      mobileQuery?.removeEventListener?.("change", resize);
      cardsQuery?.removeEventListener?.("change", resize);
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

{#snippet cardsLauncher()}
  {#if cardsAvailable && !cardsDocked}
    <button
      bind:this={cardsTrigger}
      class="cards-launch"
      type="button"
      title="Open workspace cards"
      aria-label="Open workspace cards"
      aria-controls="agentPanel"
      aria-expanded={cardsOverlay}
      onclick={openCards}
    ><Icon name="panel-right-open" /></button>
  {/if}
{/snippet}

<div data-component-owner="app-shell" class="app-shell" class:cards-docked={cardsDocked}>
<aside bind:this={sidebar} id="mobileSidebar" class="sidebar" class:sidebar-open={sidebarOpen} aria-label="Navigation" tabindex="-1" inert={cardsOverlay || (narrow && !sidebarOpen)}>
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
  <section id="detailsPanel" class="details-panel" data-component-owner="detail-panel" inert={cardsOverlay}>{#if details}{@render details(navigation, cardsLauncher)}{/if}</section>
  {#if cardsDocked}
    <PaneResizeHandle id="detailsResize" kind="chatWidth" className="details-resize" label="Resize right column" onPreview={model.onPanePreview} onCommit={model.onPaneCommit} />
  {/if}
  {#if cardsOverlay}<div class="cards-backdrop" aria-hidden="true"></div>{/if}
  <div
    bind:this={cardsPanel}
    id="agentPanel"
    class="agent-panel"
    class:cards-drawer={cardsNarrow}
    class:cards-open={cardsOverlay}
    role={cardsNarrow ? "dialog" : "complementary"}
    aria-label="Workspace cards"
    aria-modal={cardsNarrow ? true : undefined}
    inert={!cardsDocked && !cardsOverlay}
  >
    {#if model.cardUser && model.activeWorkspaceId}
      {#key JSON.stringify([model.activeWorkspaceId, model.cardUser])}
        <WorkspaceCards workspace={model.activeWorkspaceId} user={model.cardUser} tree={model.cardTree ?? null} treeError={model.error} onSelect={model.onSelectResource} onClose={closeCardsDrawer}>
          {#snippet panelAction()}
            {@const label = cardsNarrow ? "Close workspace cards" : "Collapse workspace cards"}
            <button bind:this={cardsDismiss} class="cards-dismiss" type="button" title={label} aria-label={label} onclick={dismissCards}>
              <Icon name={cardsNarrow ? "x" : "arrow-left"} />
            </button>
          {/snippet}
        </WorkspaceCards>
      {/key}
    {/if}
  </div>
  {/if}
</main>
</div>
{#if doctorOpen}<DoctorDialog snapshot={model.doctor} onClose={() => { doctorOpen = false; }} onRefresh={model.onRefreshDoctor} />{/if}
