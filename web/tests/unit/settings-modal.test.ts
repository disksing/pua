import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import { createModelChannel } from "../../src/components/model-channel";
import type { SettingsModel } from "../../src/components/models";
import SettingsModal from "../../src/components/SettingsModal.svelte";

const cleanups: Array<() => Promise<void>> = [];

afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
});

  function model(overrides: Partial<SettingsModel> = {}): SettingsModel {
  return {
    open: true,
    identity: "settings-1",
    dataVersion: 1,
    initialTab: "system",
    workspaces: [{ id: "workspace-a", name: "Workspace A", path: "/tmp/a" }],
    activeWorkspaceId: "workspace-a",
    workspaceIcons: [{ id: "", label: "PUA default", src: "/favicon.svg" }],
    workspaceIconSavingId: "",
    suggestedUserName: "ServerUser",
    system: {
      pua: { address: "127.0.0.1", port: "4936", configPath: "/tmp/pua/serve.json", workspaces: [{ name: "Workspace A", path: "/tmp/a" }], buildBranch: "master", buildCommit: "pua-commit" },
      agentHub: { mode: "embedded", address: "127.0.0.1", port: "4936", endpoint: "http://127.0.0.1:4936/agenthub", connected: true, compatible: true, version: "hub-commit", paths: { config: "/tmp/agenthub/config.json", sessions: "/tmp/agenthub/sessions", archive: "/tmp/agenthub/sessions/Archive", logs: "/tmp/agenthub/logs" }, error: "" },
    },
    appearance: { layout: "auto", fontScales: { sidebar: 1, details: 1, chat: 1 }, theme: "default", themeOptions: [{ id: "default", label: "Default", description: "The standard PUA appearance" }] },
    agentHub: {
      mode: "external",
      configuredEndpoint: "http://127.0.0.1:4646",
      connected: true,
      compatible: true,
      error: "",
      apiVersion: "v1",
      version: "1.2.3",
      capabilities: [],
      providers: [],
      agents: [],
      probes: [],
    },
    profiles: [{ key: "default", description: "Default", agentName: "codex" }],
    agents: [{ id: "codex", label: "Codex", summary: "Primary" }],
    notifications: { browser: false, sound: false, permission: "default", permissionError: "", soundError: "" },
    onClose: vi.fn(),
    onAddWorkspace: vi.fn(async () => undefined),
    onRemoveWorkspace: vi.fn(async () => undefined),
    onWorkspaceIcon: vi.fn(async () => undefined),
    onSaveWorkspaceName: vi.fn(async () => undefined),
    onLayoutPreference: vi.fn(),
    onFontScale: vi.fn(),
    onResetFontScales: vi.fn(),
    onThemePreference: vi.fn(),
    onSaveAgentHub: vi.fn(async () => undefined),
    onSetProviderCommand: vi.fn(async (providerId) => ({ id: providerId, name: providerId, type: providerId })),
    onBrowserNotifications: vi.fn(),
    onCompletionSound: vi.fn(),
    onToast: vi.fn(),
    ...overrides,
  };
}

function input(element: HTMLInputElement, value: string): void {
  element.value = value;
  element.dispatchEvent(new InputEvent("input", { bubbles: true }));
}

describe("SettingsModal coordination", () => {
  it("keeps the close control in a header row outside the scrollable panel body", async () => {
    const channel = createModelChannel(model());
    const target = document.body.appendChild(document.createElement("div"));
    target.dataset.componentOwner = "settings";
    const component = mount(SettingsModal, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const close = target.querySelector<HTMLButtonElement>(".settings-close")!;
    expect(close.parentElement?.classList.contains("settings-header")).toBe(true);
    expect(close.closest(".settings-body")).toBeNull();
    const body = target.querySelector(".settings-body")!;
    expect(body.querySelector("h2")?.textContent).toBe("System Information");
    expect(body.querySelectorAll("input, select, textarea, [type=submit]")).toHaveLength(0);
    expect(body.textContent).toContain("/tmp/pua/serve.json");
    expect(body.textContent).toContain("/tmp/agenthub/sessions");
  });

  it("composes all domain panels and preserves a dirty focused draft across data refreshes", async () => {
    const initial = model();
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    target.dataset.componentOwner = "settings";
    const component = mount(SettingsModal, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    const tabs = [...target.querySelectorAll<HTMLButtonElement>(".settings-tab")];
    expect(tabs.map((tab) => tab.textContent?.trim())).toEqual(["System", "Workspace", "Appearance", "Agents", "Profiles", "Notifications"]);
    tabs.find((tab) => tab.textContent?.includes("Agents"))!.click();
    await tick();

    const endpoint = target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")!;
    input(endpoint, "http://127.0.0.1:5656");
    endpoint.focus();
    await tick();
    expect(target.querySelectorAll(".settings-tab.dirty")).toHaveLength(2);

    channel.publish({
      ...initial,
      dataVersion: 2,
      agentHub: { ...initial.agentHub, configuredEndpoint: "http://127.0.0.1:9999" },
    });
    await tick();

    const preserved = target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")!;
    expect(preserved).toBe(endpoint);
    expect(preserved.value).toBe("http://127.0.0.1:5656");
    expect(document.activeElement).toBe(preserved);

    tabs.find((tab) => tab.textContent?.includes("Profiles"))!.click();
    await tick();
    expect(target.querySelector('[data-component-owner="profiles-settings-panel"]')).toBeTruthy();
    target.querySelector<HTMLButtonElement>(".settings-close")!.click();
    expect(initial.onClose).toHaveBeenCalledWith(true);
  });

  it("deduplicates shared save pending, refreshes clean drafts, and resets dirty state on identity changes", async () => {
    let resolveSave!: () => void;
    const save = new Promise<void>((resolve) => { resolveSave = resolve; });
    const onSaveAgentHub = vi.fn(() => save);
    const initial = model({ initialTab: "agenthub", onSaveAgentHub });
    const channel = createModelChannel(initial);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsModal, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    input(target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")!, "http://127.0.0.1:5656");
    await tick();
    const saveButton = target.querySelector<HTMLButtonElement>("#settingsSaveButton")!;
    saveButton.click();
    saveButton.click();
    await tick();
    expect(onSaveAgentHub).toHaveBeenCalledTimes(1);
    expect(saveButton.disabled).toBe(true);

    resolveSave();
    await vi.waitFor(() => expect(target.querySelectorAll(".settings-tab.dirty")).toHaveLength(0));
    channel.publish({
      ...initial,
      dataVersion: 2,
      agentHub: { ...initial.agentHub, configuredEndpoint: "http://127.0.0.1:7777" },
    });
    await tick();
    expect(target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")?.value).toBe("http://127.0.0.1:7777");

    input(target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")!, "http://dirty");
    await tick();
    channel.publish({
      ...initial,
      identity: "settings-2",
      dataVersion: 3,
      agentHub: { ...initial.agentHub, configuredEndpoint: "http://identity-reset" },
    });
    await tick();
    expect(target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")?.value).toBe("http://identity-reset");
    expect(target.querySelectorAll(".settings-tab.dirty")).toHaveLength(0);
  });

  it("routes overlay and Escape closes through the parent with current dirty state", async () => {
    const current = model();
    const channel = createModelChannel(current);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsModal, { target, props: { channel } });
    cleanups.push(() => unmount(component));
    await tick();

    [...target.querySelectorAll<HTMLButtonElement>(".settings-tab")].find((tab) => tab.textContent?.includes("Agents"))!.click();
    await tick();
    input(target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")!, "http://dirty");
    await tick();
    target.querySelector<HTMLButtonElement>(".settings-overlay")!.click();
    document.dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true }));
    expect(current.onClose).toHaveBeenNthCalledWith(1, true);
    expect(current.onClose).toHaveBeenNthCalledWith(2, true);
  });
});
