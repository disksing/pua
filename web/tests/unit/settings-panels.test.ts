import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import { createSettingsDraft } from "../../src/components/settings-draft";
import type { SettingsModel } from "../../src/components/models";
import SettingsPanelHarness from "../fixtures/SettingsPanelHarness.svelte";

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
    initialTab: "workspace",
    workspaces: [{ id: "workspace-a", name: "Workspace A", path: "/tmp/a", icon: "blue" }],
    activeWorkspaceId: "workspace-a",
    workspaceIcons: [
      { id: "blue", label: "Blue", src: "/blue.png" },
      { id: "green", label: "Green", src: "/green.png" },
    ],
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
      capabilities: ["sessions"],
      providers: [{ id: "codex" }],
      agents: [{ name: "Codex", providerId: "codex", available: true }],
      probes: [],
    },
    profiles: [
      { key: "default", description: "Default", agentName: "codex" },
      { key: "custom", description: "Custom", agentName: "missing" },
    ],
    agents: [{ id: "codex", label: "Codex", summary: "Primary agent" }],
    notifications: { browser: false, sound: true, permission: "default", permissionError: "", soundError: "" },
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
    onToggleProvider: vi.fn(async (providerId, enabled) => ({ id: providerId, name: providerId, type: providerId, enabled })),
    onBrowserNotifications: vi.fn(),
    onCompletionSound: vi.fn(),
    onToast: vi.fn(),
    ...overrides,
  };
}

function deferred<T>() {
  let resolve!: (value: T | PromiseLike<T>) => void;
  let reject!: (reason?: unknown) => void;
  const promise = new Promise<T>((resolvePromise, rejectPromise) => {
    resolve = resolvePromise;
    reject = rejectPromise;
  });
  return { promise, resolve, reject };
}

function input(element: HTMLInputElement, value: string): void {
  element.value = value;
  element.dispatchEvent(new InputEvent("input", { bubbles: true }));
}

describe("settings domain panels", () => {
  it("owns workspace add, icon, remove, pending deduplication, and failure reporting", async () => {
    const add = deferred<void>();
    const current = model({
      onAddWorkspace: vi.fn(() => add.promise),
      onRemoveWorkspace: vi.fn(async () => { throw new Error("remove failed"); }),
    });
    const draft = createSettingsDraft(current);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "workspace", model: current, initialDraft: draft } });
    cleanups.push(() => unmount(component));
    await tick();

    const pathInput = target.querySelector<HTMLInputElement>("#settingsWorkspacePath")!;
    const addButton = target.querySelector<HTMLButtonElement>("#settingsWorkspaceForm [type=\"submit\"]")!;
    expect(pathInput.required).toBe(true);
    expect(pathInput.getAttribute("aria-invalid")).toBe("true");
    expect(pathInput.getAttribute("aria-describedby")).toBe("settings-workspace-path-error");
    expect(target.querySelector("#settings-workspace-path-error")?.textContent).toContain("required");
    expect(addButton.disabled).toBe(true);
    addButton.click();
    expect(current.onAddWorkspace).not.toHaveBeenCalled();

    input(target.querySelector<HTMLInputElement>("#settingsWorkspacePath")!, " /tmp/new ");
    await tick();
    expect(pathInput.getAttribute("aria-invalid")).toBe("false");
    expect(pathInput.getAttribute("aria-describedby")).toBeNull();
    expect(target.querySelector("#settings-workspace-path-error")).toBeNull();
    expect(addButton.disabled).toBe(false);
    expect(target.querySelector("#settingsWorkspaceLanguage")).toBeNull();
    target.querySelector<HTMLInputElement>("#settingsWorkspaceCreate")!.click();
    await tick();
    const language = target.querySelector<HTMLSelectElement>("#settingsWorkspaceLanguage")!;
    language.value = "zh-CN";
    language.dispatchEvent(new Event("change", { bubbles: true }));
    target.querySelector<HTMLButtonElement>('[type="submit"]')!.click();
    target.querySelector<HTMLButtonElement>('[type="submit"]')!.click();
    await tick();
    expect(current.onAddWorkspace).toHaveBeenCalledTimes(1);
    expect(target.querySelector<HTMLButtonElement>('[type="submit"]')?.disabled).toBe(true);
    expect(current.onAddWorkspace).toHaveBeenCalledWith(expect.objectContaining({
      workspacePath: " /tmp/new ", createWorkspace: true, workspaceLanguage: "zh-CN",
    }));

    add.resolve();
    await vi.waitFor(() => expect(target.querySelector<HTMLInputElement>("#settingsWorkspacePath")?.value).toBe(""));
    expect(target.querySelector<HTMLButtonElement>('[type="submit"]')?.disabled).toBe(true);

    target.querySelector<HTMLButtonElement>('[title="Change workspace icon"]')!.click();
    await tick();
    target.querySelector<HTMLButtonElement>('[title="Green"]')!.click();
    await vi.waitFor(() => expect(current.onWorkspaceIcon).toHaveBeenCalledWith("workspace-a", "green", expect.any(Object)));
    await vi.waitFor(() => expect(target.querySelector<HTMLButtonElement>('[title="Remove workspace"]')?.disabled).toBe(false));

    target.querySelector<HTMLButtonElement>('[title="Remove workspace"]')!.click();
    await vi.waitFor(() => expect(current.onToast).toHaveBeenCalledWith("remove failed"));
    expect(target.textContent).toContain("Active");
  });

  it("renames a workspace inline, deduplicates pending saves, and keeps the editor open on failure", async () => {
    const save = deferred<void>();
    const current = model({
      onSaveWorkspaceName: vi.fn(() => save.promise),
    });
    const draft = createSettingsDraft(current);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "workspace", model: current, initialDraft: draft } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelector<HTMLButtonElement>('[title="Rename workspace"]')!.click();
    await tick();
    await tick();
    const nameInput = target.querySelector<HTMLInputElement>(".settings-workspace-name-form input")!;
    expect(nameInput).toBeTruthy();
    expect(nameInput.value).toBe("Workspace A");

    input(nameInput, "  My Workspace  ");
    nameInput.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }));
    await tick();
    expect(current.onSaveWorkspaceName).toHaveBeenCalledTimes(1);
    expect(current.onSaveWorkspaceName).toHaveBeenCalledWith("workspace-a", "My Workspace", expect.any(Object));
    expect(target.querySelector<HTMLButtonElement>(".settings-workspace-name-form [type=\"submit\"]")?.disabled).toBe(true);

    save.resolve();
    await vi.waitFor(() => expect(target.querySelector(".settings-workspace-name-form")).toBeNull());

    const failing = model({
      onSaveWorkspaceName: vi.fn(async () => { throw new Error("rename failed"); }),
    });
    const failingDraft = createSettingsDraft(failing);
    const failingTarget = document.body.appendChild(document.createElement("div"));
    const failingComponent = mount(SettingsPanelHarness, { target: failingTarget, props: { panel: "workspace", model: failing, initialDraft: failingDraft } });
    cleanups.push(() => unmount(failingComponent));
    await tick();

    failingTarget.querySelector<HTMLButtonElement>('[title="Rename workspace"]')!.click();
    await tick();
    const failingInput = failingTarget.querySelector<HTMLInputElement>(".settings-workspace-name-form input")!;
    input(failingInput, "Broken");
    failingTarget.querySelector<HTMLButtonElement>(".settings-workspace-name-form [type=\"submit\"]")!.click();
    await vi.waitFor(() => expect(failing.onToast).toHaveBeenCalledWith("rename failed"));
    expect(failingTarget.querySelector(".settings-workspace-name-form")).not.toBeNull();

    // Submitting an unchanged or whitespace-only name closes without saving.
    input(failingInput, "Workspace A");
    failingInput.dispatchEvent(new KeyboardEvent("keydown", { key: "Enter", bubbles: true, cancelable: true }));
    await tick();
    expect(failing.onSaveWorkspaceName).toHaveBeenCalledTimes(1);
    expect(failingTarget.querySelector(".settings-workspace-name-form")).toBeNull();
  });

  it("routes appearance layout and font scale changes through the settings model", async () => {
    const current = model({ appearance: { layout: "auto", fontScales: { sidebar: 1, details: 1.1, chat: 1 }, theme: "default", themeOptions: [{ id: "default", label: "Default", description: "The standard PUA appearance" }] } });
    const draft = createSettingsDraft(current);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "appearance", model: current, initialDraft: draft } });
    cleanups.push(() => unmount(component));
    await tick();

    const options = [...target.querySelectorAll<HTMLButtonElement>('[aria-label="Layout"] .layout-option')];
    expect(options.map((option) => option.getAttribute("aria-checked"))).toEqual(["true", "false", "false", "false"]);
    expect(options[0].classList.contains("active")).toBe(true);
    expect(target.querySelectorAll(".layout-diagram svg")).toHaveLength(4);

    // The split diagram keeps the collapsed sidebar drawer on the left edge.
    const splitDiagram = options[3].querySelectorAll(".layout-diagram rect");
    const splitDrawer = splitDiagram[splitDiagram.length - 1];
    expect(splitDrawer.classList.contains("d-fill-strong")).toBe(true);
    expect(Number(splitDrawer.getAttribute("x"))).toBe(6);

    options[2].click();
    expect(current.onLayoutPreference).toHaveBeenCalledWith("two");

    const detailsSlider = target.querySelector<HTMLInputElement>('input[aria-label="Details text size"]')!;
    expect(detailsSlider.value).toBe("110");
    input(detailsSlider, "120");
    expect(current.onFontScale).toHaveBeenCalledWith("details", 1.2);

    const reset = target.querySelector<HTMLButtonElement>(".appearance-reset")!;
    expect(reset.disabled).toBe(false);
    reset.click();
    expect(current.onResetFontScales).toHaveBeenCalledTimes(1);
  });

  it("routes theme selection through the settings model", async () => {
    const themeOptions = [
      { id: "default", label: "Default", description: "The standard PUA appearance" },
      { id: "riso", label: "Riso", description: "Risograph print" }
    ];
    const current = model({ appearance: { layout: "auto", fontScales: { sidebar: 1, details: 1, chat: 1 }, theme: "default", themeOptions } });
    const draft = createSettingsDraft(current);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "appearance", model: current, initialDraft: draft } });
    cleanups.push(() => unmount(component));
    await tick();

    const options = [...target.querySelectorAll<HTMLButtonElement>('[aria-label="Theme"] .theme-option')];
    expect(options).toHaveLength(2);
    expect(options.map((option) => option.getAttribute("aria-checked"))).toEqual(["true", "false"]);
    expect(options[0].classList.contains("active")).toBe(true);

    options[1].click();
    expect(current.onThemePreference).toHaveBeenCalledWith("riso");
  });

  it("disables the appearance reset while every column uses the default text size", async () => {
    const current = model();
    const draft = createSettingsDraft(current);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "appearance", model: current, initialDraft: draft } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector<HTMLButtonElement>(".appearance-reset")?.disabled).toBe(true);
  });

  it("owns AgentHub connection, provider switches, agent draft dirtiness, save pending, and errors", async () => {
    const save = deferred<void>();
    const onSaveAgentHub = vi.fn()
      .mockImplementationOnce(() => save.promise)
      .mockRejectedValueOnce(new Error("hub save failed"));
    const current = model({ onSaveAgentHub });
    const draft = createSettingsDraft(current);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "agenthub", model: current, initialDraft: draft } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.textContent).toContain("Compatible");
    expect(target.textContent).toContain("Codex");
    expect(target.textContent).toContain("1 providers · switches immediate, paths on Save All");
    expect(target.textContent).toContain("1 agents");
    expect(target.textContent).not.toContain("API v1 · AgentHub 1.2.3");
    expect(target.querySelector(".settings-capability-list")).toBeNull();

    input(target.querySelector<HTMLInputElement>('input[aria-label$="executable path"]')!, "/opt/homebrew/bin/codex");
    await tick();
    expect(target.querySelector(".settings-save-hint.visible")).toBeTruthy();

    input(target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")!, "http://127.0.0.1:5656");
    await tick();
    const saveButton = target.querySelector<HTMLButtonElement>("#settingsSaveButton")!;
    expect(target.querySelector(".settings-save-hint.visible")).toBeTruthy();
    saveButton.click();
    saveButton.click();
    await tick();
    expect(onSaveAgentHub).toHaveBeenCalledTimes(1);
    expect(onSaveAgentHub).toHaveBeenCalledWith(expect.objectContaining({
      endpoint: "http://127.0.0.1:5656",
      dirty: true,
      agentProviders: [expect.objectContaining({ id: "codex", command: "/opt/homebrew/bin/codex" })],
    }));
    expect(saveButton.disabled).toBe(true);

    save.resolve();
    await vi.waitFor(() => expect(target.querySelector(".settings-save-hint.visible")).toBeNull());
    input(target.querySelector<HTMLInputElement>("#settingsAgentHubEndpoint")!, "http://bad");
    await tick();
    saveButton.click();
    await vi.waitFor(() => expect(current.onToast).toHaveBeenCalledWith("hub save failed"));
    expect(target.querySelector(".settings-save-hint.visible")).toBeTruthy();
  });

  it("hides the external endpoint when PUA uses the embedded AgentHub", async () => {
    const current = model({ agentHub: { ...model().agentHub, mode: "embedded" } });
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "agenthub", model: current, initialDraft: createSettingsDraft(current) } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.querySelector("#settingsAgentHubEndpoint")).toBeNull();
    expect(target.textContent).toContain("not applicable in this mode");
  });

  it("toggles providers immediately and edits agent cards in the shared draft", async () => {
    const toggle = vi.fn(async (providerId: string, enabled: boolean) => ({ id: providerId, name: "Codex", type: "codex", enabled }));
    const current = model({ onToggleProvider: toggle });
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "agenthub", model: current, initialDraft: createSettingsDraft(current) } });
    cleanups.push(() => unmount(component));
    await tick();

    input(target.querySelector<HTMLInputElement>('input[aria-label$="executable path"]')!, "/opt/homebrew/bin/codex");
    await tick();
    target.querySelector<HTMLButtonElement>('[role="switch"]')!.click();
    await vi.waitFor(() => expect(toggle).toHaveBeenCalledWith("codex", false));
    await tick();
    expect(target.querySelector('[role="switch"]')?.getAttribute("aria-checked")).toBe("false");
    expect(target.querySelector<HTMLInputElement>('input[aria-label$="executable path"]')?.value).toBe("/opt/homebrew/bin/codex");

    target.querySelector<HTMLButtonElement>("#settingsAddAgent")!.click();
    await tick();
    expect(target.textContent).toContain("Agent");
    const names = target.querySelectorAll<HTMLInputElement>('[aria-label="Agent name"]');
    input(names[0]!, "Worker");
    await tick();
    expect(target.querySelector(".settings-save-hint.visible")).toBeTruthy();
    target.querySelector<HTMLButtonElement>('[aria-label="Delete Worker"]')!.click();
    await tick();
    expect(target.querySelectorAll(".settings-agent-card")).toHaveLength(1);
  });

  it("reorders agent cards via drag and keyboard", async () => {
    const current = model({
      agentHub: {
        ...model().agentHub,
        agents: [
          { name: "Codex", providerId: "codex", available: true },
          { name: "Review", providerId: "codex", available: true },
          { name: "Fast", providerId: "codex", available: true },
        ],
      },
    });
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "agenthub", model: current, initialDraft: createSettingsDraft(current) } });
    cleanups.push(() => unmount(component));
    await tick();

    const names = () => [...target.querySelectorAll<HTMLElement>(".settings-agent-card-title strong")].map((el) => el.textContent);
    const handles = () => [...target.querySelectorAll<HTMLButtonElement>(".settings-drag-handle")];
    const cards = () => [...target.querySelectorAll<HTMLElement>(".settings-agent-card")];
    expect(names()).toEqual(["Codex", "Review", "Fast"]);
    expect(target.querySelectorAll(".settings-order-button")).toHaveLength(0);

    // Keyboard reorder stays available through the drag handle.
    handles()[2]!.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowUp", bubbles: true, cancelable: true }));
    await tick();
    expect(names()).toEqual(["Codex", "Fast", "Review"]);
    expect(target.querySelector(".settings-save-hint.visible")).toBeTruthy();

    // Drag the middle card onto the last card, matching the Profiles panel.
    handles()[1]!.dispatchEvent(new Event("dragstart", { bubbles: true, cancelable: true }));
    await tick();
    cards()[2]!.dispatchEvent(new Event("dragover", { bubbles: true, cancelable: true }));
    cards()[2]!.dispatchEvent(new Event("drop", { bubbles: true, cancelable: true }));
    await tick();
    expect(names()).toEqual(["Codex", "Review", "Fast"]);
  });

  it("enforces system/custom profile rules, unavailable routes, and shared save pending", async () => {
    const save = deferred<void>();
    const current = model({ onSaveAgentHub: vi.fn(() => save.promise) });
    const draft = createSettingsDraft(current);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "profiles", model: current, initialDraft: draft } });
    cleanups.push(() => unmount(component));
    await tick();

    // Cards start collapsed; the system card is pinned without a drag handle.
    const toggles = () => [...target.querySelectorAll<HTMLButtonElement>(".settings-profile-card-toggle")];
    const names = () => [...target.querySelectorAll<HTMLElement>(".settings-profile-card-toggle strong")].map((el) => el.textContent);
    expect(names()).toEqual(["default", "custom"]);
    expect(target.querySelectorAll(".settings-drag-handle").length).toBe(1);
    expect(target.querySelector("#settingsNewProfileKey")).toBeNull();

    // Expand both cards: system fields stay locked, the custom card shows the unavailable route.
    toggles()[0]!.click();
    toggles()[1]!.click();
    await tick();
    const profileKeys = target.querySelectorAll<HTMLInputElement>('[aria-label="Profile key"]');
    expect(profileKeys[0]!.disabled).toBe(true);
    expect(target.textContent).toContain("System");
    expect(target.querySelectorAll<HTMLSelectElement>('[aria-label="AgentHub Agent"]')[1]?.textContent).toContain("missing (Unavailable)");

    target.querySelector<HTMLButtonElement>('[title="Delete Profile"]')!.click();
    await tick();
    expect(names()).toEqual(["default"]);
    expect(target.querySelector(".settings-save-hint.visible")).toBeTruthy();

    // The add button inserts an expanded card right after the system block.
    const addButton = [...target.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent?.includes("Add profile"))!;
    addButton.click();
    await tick();
    expect(names()).toEqual(["default", "profile-1"]);
    const keyFields = () => [...target.querySelectorAll<HTMLInputElement>('[aria-label="Profile key"]')];
    expect(keyFields().at(-1)?.value).toBe("profile-1");

    input(keyFields().at(-1)!, " Review ");
    input([...target.querySelectorAll<HTMLInputElement>('[aria-label="Summary"]')].at(-1)!, " Review work ");
    await tick();
    expect(names()).toEqual(["default", " Review "]);

    const saveButton = [...target.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent?.includes("Save All"))!;
    saveButton.click();
    saveButton.click();
    await tick();
    expect(current.onSaveAgentHub).toHaveBeenCalledTimes(1);
    expect(current.onSaveAgentHub).toHaveBeenCalledWith(expect.objectContaining({ dirty: true, profiles: [expect.objectContaining({ key: "default" }), expect.objectContaining({ key: " Review " })] }));
    expect(saveButton.disabled).toBe(true);
    save.resolve();
    await vi.waitFor(() => expect(target.querySelector(".settings-save-hint.visible")).toBeNull());
  });

  it("shows profile key errors inline and blocks invalid saves", async () => {
    const current = model();
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "profiles", model: current, initialDraft: createSettingsDraft(current) } });
    cleanups.push(() => unmount(component));
    await tick();

    target.querySelectorAll<HTMLButtonElement>(".settings-profile-card-toggle")[1]!.click();
    await tick();
    const keyInput = target.querySelector<HTMLInputElement>('[aria-label="Profile key"]')!;
    const saveButton = [...target.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent?.includes("Save All"))!;

    input(keyInput, "");
    await tick();
    expect(keyInput.getAttribute("aria-invalid")).toBe("true");
    expect(keyInput.getAttribute("aria-describedby")).toBe("settings-profile-1-key-error");
    expect(target.querySelector("#settings-profile-1-key-error")?.textContent).toBe("Profile key is required.");
    expect(saveButton.disabled).toBe(true);
    saveButton.click();
    expect(current.onSaveAgentHub).not.toHaveBeenCalled();

    input(keyInput, "review");
    await tick();
    expect(keyInput.getAttribute("aria-invalid")).toBe("false");
    expect(keyInput.getAttribute("aria-describedby")).toBeNull();
    expect(target.querySelector("#settings-profile-1-key-error")).toBeNull();
    expect(saveButton.disabled).toBe(false);
  });

  it("reorders profile cards via drag and keyboard and validates keys on save", async () => {
    const current = model({
      profiles: [
        { key: "default", description: "Default", agentName: "codex" },
        { key: "fast", description: "Fast", agentName: "codex" },
        { key: "reasoning", description: "Deep", agentName: "codex" },
      ],
    });
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "profiles", model: current, initialDraft: createSettingsDraft(current) } });
    cleanups.push(() => unmount(component));
    await tick();

    const names = () => [...target.querySelectorAll<HTMLElement>(".settings-profile-card-toggle strong")].map((el) => el.textContent);
    const handles = () => [...target.querySelectorAll<HTMLButtonElement>(".settings-drag-handle")];
    const cards = () => [...target.querySelectorAll<HTMLElement>(".settings-profile-card")];
    expect(names()).toEqual(["default", "fast", "reasoning"]);

    // Keyboard reorder: move "reasoning" above "fast".
    handles()[1]!.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowUp", bubbles: true, cancelable: true }));
    await tick();
    expect(names()).toEqual(["default", "reasoning", "fast"]);
    expect(target.querySelector(".settings-save-hint.visible")).toBeTruthy();

    // The first custom card cannot move into the pinned system block.
    handles()[0]!.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowUp", bubbles: true, cancelable: true }));
    await tick();
    expect(names()).toEqual(["default", "reasoning", "fast"]);

    // Drag reorder: drop "fast" (index 2) onto "reasoning" (index 1).
    handles()[1]!.dispatchEvent(new Event("dragstart", { bubbles: true, cancelable: true }));
    await tick();
    cards()[1]!.dispatchEvent(new Event("dragover", { bubbles: true, cancelable: true }));
    cards()[1]!.dispatchEvent(new Event("drop", { bubbles: true, cancelable: true }));
    await tick();
    expect(names()).toEqual(["default", "fast", "reasoning"]);

    // The system card is not a drop target: dragging "fast" onto it is a no-op.
    handles()[0]!.dispatchEvent(new Event("dragstart", { bubbles: true, cancelable: true }));
    await tick();
    cards()[0]!.dispatchEvent(new Event("dragover", { bubbles: true, cancelable: true }));
    cards()[0]!.dispatchEvent(new Event("drop", { bubbles: true, cancelable: true }));
    await tick();
    expect(names()).toEqual(["default", "fast", "reasoning"]);
    handles()[0]!.dispatchEvent(new Event("dragend", { bubbles: true }));
    await tick();

    // Duplicate keys are reported on the fields and block saving; a reserved
    // key collides with the system row as well.
    const toggles = [...target.querySelectorAll<HTMLButtonElement>(".settings-profile-card-toggle")];
    toggles[1]!.click();
    toggles[2]!.click();
    await tick();
    const keyInputs = () => [...target.querySelectorAll<HTMLInputElement>('[aria-label="Profile key"]')];
    const saveButton = [...target.querySelectorAll<HTMLButtonElement>("button")].find((button) => button.textContent?.includes("Save All"))!;
    input(keyInputs()[0]!, "DEFAULT");
    await tick();
    expect(keyInputs()[0]!.disabled).toBe(false);
    expect(keyInputs()[0]!.getAttribute("aria-invalid")).toBe("true");
    expect(target.querySelector(".settings-field-error")?.textContent).toContain("Profile default already exists.");
    expect(saveButton.disabled).toBe(true);
    expect(current.onSaveAgentHub).not.toHaveBeenCalled();

    input(keyInputs()[0]!, "reasoning");
    await tick();
    expect(keyInputs()[0]!.getAttribute("aria-invalid")).toBe("true");
    expect(target.querySelector(".settings-field-error")?.textContent).toContain("Profile reasoning already exists.");
    expect(saveButton.disabled).toBe(true);
    expect(current.onSaveAgentHub).not.toHaveBeenCalled();

    input(keyInputs()[0]!, "fast");
    await tick();
    expect(keyInputs()[0]!.getAttribute("aria-invalid")).toBe("false");
    expect(saveButton.disabled).toBe(false);
    saveButton.click();
    await vi.waitFor(() => expect(current.onSaveAgentHub).toHaveBeenCalledTimes(1));
  });

  it("projects notification permission and sound errors and forwards both toggles", async () => {
    const current = model({
      notifications: { browser: false, sound: true, permission: "denied", permissionError: "Notifications are blocked.", soundError: "Audio unavailable." },
    });
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(SettingsPanelHarness, { target, props: { panel: "notifications", model: current, initialDraft: createSettingsDraft(current) } });
    cleanups.push(() => unmount(component));
    await tick();

    expect(target.textContent).toContain("Notifications are blocked.");
    expect(target.textContent).toContain("Audio unavailable.");
    target.querySelector<HTMLInputElement>("#settingsBrowserNotifications")!.click();
    target.querySelector<HTMLInputElement>("#settingsCompletionSound")!.click();
    expect(current.onBrowserNotifications).toHaveBeenCalledWith(true);
    expect(current.onCompletionSound).toHaveBeenCalledWith(false);
  });
});
