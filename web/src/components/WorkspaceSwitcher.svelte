<script lang="ts">
  import "./WorkspaceSwitcher.css";

  import { onMount, tick } from "svelte";

  import Icon from "./Icon.svelte";
  import StatusPresentation from "./StatusPresentation.svelte";
  import type { ShellWorkspaceItem } from "./models";

  let {
    identity,
    mobileSidebarOpen,
    activeWorkspaceId,
    workspaces,
    onSwitch,
    onOpen,
    onAdd,
    onToast,
  }: {
    identity: string;
    mobileSidebarOpen: boolean;
    activeWorkspaceId: string;
    workspaces: ShellWorkspaceItem[];
    onSwitch: (id: string) => Promise<void>;
    onOpen: () => Promise<void>;
    onAdd: () => void;
    onToast: (message: string) => void;
  } = $props();
  let menuOpen = $state(false);
  let switchingId = $state("");
  let activeId = $state("");
  let menu: HTMLDivElement | undefined = $state();
  let opener: HTMLButtonElement | undefined = $state();
  // svelte-ignore state_referenced_locally
  let previousIdentity = $state(identity);

  const workspaceItems = $derived(workspaces || []);
  const activeWorkspace = $derived(workspaceItems.find((workspace) => workspace.id === activeWorkspaceId) ?? null);

  $effect(() => {
    if (identity === previousIdentity) return;
    previousIdentity = identity;
    menuOpen = false;
    switchingId = "";
  });

  $effect(() => {
    if (!menuOpen || !menu) return;
    void tick().then(() => {
      const option = optionElements().find((el) => el.dataset.workspaceId === activeId) ?? optionElements()[0];
      option?.focus();
    });
  });

  onMount(() => {
    const outside = (event: MouseEvent) => {
      const target = event.target instanceof Element ? event.target : null;
      if (menuOpen && !target?.closest(".workspace-select-row")) menuOpen = false;
    };
    const keydown = (event: KeyboardEvent) => {
      if (event.key !== "Escape" || mobileSidebarOpen) return;
      if (menuOpen) opener?.focus();
      menuOpen = false;
    };
    document.addEventListener("mousedown", outside);
    document.addEventListener("keydown", keydown);
    return () => {
      document.removeEventListener("mousedown", outside);
      document.removeEventListener("keydown", keydown);
    };
  });

  async function openWorkspace(event: MouseEvent): Promise<void> {
    // Pointer clicks focus the button; drop that focus so the head does
    // not stay highlighted after the pointer leaves. Keyboard activation
    // (detail === 0) keeps it.
    if (event.detail > 0) (event.currentTarget as HTMLElement | null)?.blur();
    try {
      await onOpen();
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    }
  }

  async function switchWorkspace(id: string): Promise<void> {
    if (!id || switchingId) return;
    switchingId = id;
    menuOpen = false;
    try {
      await onSwitch(id);
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    } finally {
      switchingId = "";
    }
  }

  function optionElements(): HTMLButtonElement[] {
    return Array.from(menu?.querySelectorAll<HTMLButtonElement>('[role="option"]:not([disabled])') ?? []);
  }

  function workspaceLabel(workspace: ShellWorkspaceItem): string {
    const name = workspace.name || workspace.id;
    return workspace.path ? `${name} — ${workspace.path}` : name;
  }

  function keydown(event: KeyboardEvent): void {
    const options = optionElements();
    if (!options.length) return;
    let index = options.findIndex((el) => el.dataset.workspaceId === activeId);
    if (index < 0) index = options.findIndex((el) => el.dataset.workspaceId === activeWorkspaceId);
    if (index < 0) index = 0;

    switch (event.key) {
      case "ArrowDown":
        event.preventDefault();
        index = (index + 1) % options.length;
        break;
      case "ArrowUp":
        event.preventDefault();
        index = (index - 1 + options.length) % options.length;
        break;
      case "Home":
        event.preventDefault();
        index = 0;
        break;
      case "End":
        event.preventDefault();
        index = options.length - 1;
        break;
      case "Enter":
      case " ": {
        event.preventDefault();
        event.stopPropagation();
        const option = options[index];
        if (option) void switchWorkspace(option.dataset.workspaceId || "");
        return;
      }
      case "Escape":
        event.preventDefault();
        event.stopPropagation();
        menuOpen = false;
        opener?.focus();
        return;
      default:
        return;
    }
    event.stopPropagation();
    const option = options[index];
    if (option) {
      activeId = option.dataset.workspaceId ?? activeWorkspaceId;
      option.focus();
    }
  }
</script>

<section class="workspace-switcher" data-component-owner="workspace-switcher">
  <div class="workspace-select-row">
    <div class="workspace-switcher-head">
      <button id="workspaceOpen" class="workspace-open" type="button" title="Open workspace" aria-label={activeWorkspace?.statusLabel ? `Open workspace. ${activeWorkspace.statusLabel}` : "Open workspace"} onclick={openWorkspace}>
        <span class="workspace-avatar" id="workspaceAvatar">{#if activeWorkspace?.status?.hasTaskState}<StatusPresentation status={activeWorkspace.status} className="workspace-runtime-status" />{:else}<img src={activeWorkspace?.iconSrc || "/workspace-icons/pua-yellow.png"} alt="" aria-hidden="true" />{/if}</span>
        <span class="workspace-switcher-name" id="workspaceSwitcherName">{activeWorkspace?.name || "Workspace"}</span>
        <Icon name="arrow-up-right" className="workspace-open-icon" />
      </button>
      <button id="workspaceSwitcher" bind:this={opener} class="workspace-switcher-menu-button" class:busy={Boolean(switchingId)} type="button" title="Switch workspace" aria-haspopup="listbox" aria-expanded={menuOpen} onclick={(event) => { event.stopPropagation(); menuOpen = !menuOpen; if (menuOpen) activeId = activeWorkspaceId; }}>
        <span class="workspace-switcher-icon workspace-switcher-icon-idle"><Icon name="chevrons-up-down" className="select-icon" /></span><span class="workspace-switcher-icon workspace-switcher-icon-busy"><Icon name="loader-circle" className="select-icon" /></span>
      </button>
    </div>
    {#if menuOpen}
      <div id="workspaceMenu" class="workspace-menu" role="listbox" tabindex="-1" bind:this={menu} onkeydown={keydown}>
        <div class="workspace-menu-title">Switch Workspace</div>
        {#each workspaceItems as workspace (workspace.id)}
          <button type="button" class="workspace-menu-row" role="option" aria-label={workspaceLabel(workspace)} title={workspaceLabel(workspace)} aria-selected={workspace.id === activeId} tabindex={workspace.id === activeId ? 0 : -1} data-workspace-id={workspace.id} disabled={Boolean(switchingId)} onclick={() => switchWorkspace(workspace.id)}>
            <span class="workspace-avatar"><img src={workspace.iconSrc} alt="" aria-hidden="true" /></span>
            <span class="workspace-menu-main"><strong>{workspace.name || workspace.id}</strong><small>{workspace.path}</small></span>
            {#if workspace.id === activeId}<Icon name="check" className="workspace-menu-check" />{/if}
          </button>
        {/each}
        <div class="workspace-menu-footer"><button type="button" id="workspaceMenuAdd" onclick={() => { menuOpen = false; onAdd(); }}><Icon name="plus" /><span>Add workspace...</span></button></div>
      </div>
    {/if}
  </div>
</section>
