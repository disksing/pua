import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import ResourceSettingsPanel from "../../src/components/ResourceSettingsPanel.svelte";
import type { DetailPanelModel } from "../../src/components/models";

const cleanups: Array<() => Promise<void>> = [];

afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
  vi.unstubAllGlobals();
});

function model(overrides: Partial<DetailPanelModel> = {}): DetailPanelModel {
  return {
    identity: "ws-test:scheduler:scheduler",
    workspaceId: "ws-test",
    workspaceName: "Test workspace",
    resourceId: "scheduler",
    resourceType: "scheduler",
    resourceTitle: "Scheduler",
    parent: null,
    loading: false,
    detail: {
      id: "scheduler",
      type: "scheduler",
      title: "Scheduler",
      path: "scheduler",
      scheduler: {
        schemaVersion: 1,
        agentBinding: { kind: "profile", name: "default" },
        wakeIntervalMinutes: 30,
        conditionMaxChars: 128,
        descriptionMaxChars: 32,
        schedules: [],
      },
    },
    wiki: null,
    workspaceAgents: null,
    workspaceDefaults: { project: { kind: "profile", name: "default" }, task: { kind: "profile", name: "default" } },
    workspaceUsers: [],
    currentUserName: "User",
    generationPolicy: { budgetEnabled: true, maxTurns: 20, maxAccumulatedTurnMinutes: 120, inactivityEnabled: true, maxInactivityMinutes: 1440 },
    stallWatchdogPolicy: { enabled: true, timeoutMinutes: 30 },
    agentBinding: { kind: "profile", name: "default" },
    agentProfiles: [{ key: "default", description: "Default", agentName: "fake-agent" }],
    agents: [{ id: "fake-agent", label: "Fake Agent", summary: "fake" }],
    resolveResourceTitle: () => null,
    onNavigate: vi.fn(),
    onCreateTask: vi.fn(),
    onArchive: vi.fn(),
    onSaveWorkspaceAgents: vi.fn(async () => ({ path: "AGENTS.md", content: "", contentHash: "saved" })),
    onSaveMarkdownFile: vi.fn(async (path: string, content: string) => ({ path, content, contentHash: "saved" })),
    onDeleteArtifact: vi.fn(async () => undefined),
    onSaveAgentBinding: vi.fn(async () => undefined),
    onRenameResource: vi.fn(async () => undefined),
    onSaveDescription: vi.fn(async () => undefined),
    onSaveWorkspaceDefaults: vi.fn(async () => undefined),
    onSaveWorkspaceUserPreference: vi.fn(async () => undefined),
    onSwitchWorkspaceUser: vi.fn(async () => undefined),
    onAddWorkspaceUser: vi.fn(async () => undefined),
    onDeleteWorkspaceUser: vi.fn(async () => undefined),
    onSaveGenerationPolicy: vi.fn(async () => undefined),
    onSaveStallWatchdogPolicy: vi.fn(async () => undefined),
    onSaveTaskDefault: vi.fn(async () => undefined),
    onRefreshScheduler: vi.fn(async () => undefined),
    onToast: vi.fn(),
    ...overrides,
  };
}

describe("ResourceSettingsPanel", () => {
  it("switches and adds Workspace users without auto-switching the new user", async () => {
    const onSwitchWorkspaceUser = vi.fn(async () => undefined);
    const onAddWorkspaceUser = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, {
      target,
      props: { model: model({ resourceType: "workspace", resourceId: "workspace", workspaceUsers: [{ version: 1, name: "Alice", preference: "" }, { version: 1, name: "Bob", preference: "" }], currentUserName: "Alice", onSwitchWorkspaceUser, onAddWorkspaceUser }) },
    });
    cleanups.push(() => unmount(component));
    await tick();

    const selector = target.querySelector<HTMLSelectElement>("#workspaceCurrentUser")!;
    selector.value = "Bob";
    selector.dispatchEvent(new Event("change", { bubbles: true }));
    await vi.waitFor(() => expect(onSwitchWorkspaceUser).toHaveBeenCalledWith("Bob"));

    const input = target.querySelector<HTMLInputElement>("#workspaceNewUser")!;
    await vi.waitFor(() => expect(input.disabled).toBe(false));
    input.value = "Carol";
    input.dispatchEvent(new InputEvent("input", { bubbles: true }));
    input.form!.requestSubmit();
    await vi.waitFor(() => expect(onAddWorkspaceUser).toHaveBeenCalledWith("Carol"));
    expect(onSwitchWorkspaceUser).toHaveBeenCalledTimes(1);
  });

  it("keeps the Workspace Generation Save disabled until the policy changes", async () => {
    const onSaveGenerationPolicy = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, {
      target,
      props: {
        model: model({
          identity: "ws-test:workspace:workspace",
          resourceId: "workspace",
          resourceType: "workspace",
          resourceTitle: "Test workspace",
          detail: null,
          onSaveGenerationPolicy,
        }),
      },
    });
    cleanups.push(() => unmount(component));
    await tick();

    const save = target.querySelector<HTMLButtonElement>(".resource-settings-generation-controls .secondary-button")!;
    expect(save.textContent).toContain("Save");
    expect(save.disabled).toBe(true);
    expect(save.hasAttribute("disabled")).toBe(true);

    const maxTurns = target.querySelector<HTMLInputElement>('[aria-label="Maximum Turns per Generation"]')!;
    maxTurns.value = "25";
    maxTurns.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    expect(save.disabled).toBe(false);
  });

  it("saves independent Workspace Generation rotation policies", async () => {
    const onSaveGenerationPolicy = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, {
      target,
      props: { model: model({ resourceType: "workspace", resourceId: "workspace", onSaveGenerationPolicy }) },
    });
    cleanups.push(() => unmount(component));
    await tick();

    const budgetEnabled = target.querySelector<HTMLInputElement>('[aria-label="Enable usage-based Generation rotation"]')!;
    const inactivityEnabled = target.querySelector<HTMLInputElement>('[aria-label="Enable inactivity-based Generation rotation"]')!;
    const inactivityMinutes = target.querySelector<HTMLInputElement>('[aria-label="Maximum inactivity minutes per Generation"]')!;
    expect(budgetEnabled.checked).toBe(true);
    expect(inactivityEnabled.checked).toBe(true);
    expect(inactivityMinutes.value).toBe("1440");

    budgetEnabled.click();
    inactivityMinutes.value = "2880";
    inactivityMinutes.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    target.querySelector<HTMLButtonElement>(".resource-settings-generation-controls button")!.click();
    await vi.waitFor(() => expect(onSaveGenerationPolicy).toHaveBeenCalledWith({
      budgetEnabled: false,
      maxTurns: 20,
      maxAccumulatedTurnMinutes: 120,
      inactivityEnabled: true,
      maxInactivityMinutes: 2880,
    }));
  });

  it("saves the Workspace Turn stall watchdog policy", async () => {
    const onSaveStallWatchdogPolicy = vi.fn(async () => undefined);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, {
      target,
      props: {
        model: model({
          identity: "ws-test:workspace:workspace",
          resourceId: "workspace",
          resourceType: "workspace",
          resourceTitle: "Test workspace",
          detail: null,
          onSaveStallWatchdogPolicy,
        }),
      },
    });
    cleanups.push(() => unmount(component));
    await tick();

    const enabled = target.querySelector<HTMLInputElement>('[aria-label="Enable Turn stall watchdog"]')!;
    const timeout = target.querySelector<HTMLInputElement>('[aria-label="Turn stall watchdog timeout in minutes"]')!;
    const save = target.querySelector<HTMLButtonElement>(".resource-settings-stall-watchdog-controls button")!;
    expect(enabled.checked).toBe(true);
    expect(timeout.value).toBe("30");
    expect(save.disabled).toBe(true);

    enabled.click();
    timeout.value = "45";
    timeout.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    save.click();
    await vi.waitFor(() => expect(onSaveStallWatchdogPolicy).toHaveBeenCalledWith({ enabled: false, timeoutMinutes: 45 }));
  });

  it("shows range feedback and disables Save for an invalid Scheduler interval", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, { target, props: { model: model() } });
    cleanups.push(() => unmount(component));
    await tick();

    const input = target.querySelector<HTMLInputElement>('[aria-label="Scheduler wake interval in minutes"]')!;
    const save = target.querySelector<HTMLButtonElement>(".resource-settings-interval button")!;
    expect(input.getAttribute("aria-invalid")).toBeNull();
    expect(save.disabled).toBe(true);

    input.value = "0";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();

    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(input.getAttribute("aria-describedby")).toBe("schedulerIntervalError");
    expect(target.querySelector("#schedulerIntervalError")?.textContent).toContain("between 1 and 10080 minutes");
    expect(save.disabled).toBe(true);
    expect(fetch).not.toHaveBeenCalled();

    input.value = "10081";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    expect(input.getAttribute("aria-invalid")).toBe("true");
    expect(save.disabled).toBe(true);

    input.value = "45";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    expect(input.getAttribute("aria-invalid")).toBeNull();
    expect(target.querySelector("#schedulerIntervalError")).toBeNull();
    expect(save.disabled).toBe(false);
  });
  it("saves the Scheduler condition limit beside the wake interval and validates the range", async () => {
    const fetch = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetch);
    const panelModel = model();
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, { target, props: { model: panelModel } });
    cleanups.push(() => unmount(component));
    await tick();
    const input = target.querySelector<HTMLInputElement>('[aria-label="Scheduler condition character limit"]')!;
    const save = target.querySelector<HTMLButtonElement>(".resource-settings-condition-limit button")!;
    expect(input.value).toBe("128");
    expect(save.disabled).toBe(true);
    for (const value of ["0", "65537", "1.5", ""]) {
      input.value = value;
      input.dispatchEvent(new Event("input", { bubbles: true }));
      await tick();
      expect(input.getAttribute("aria-invalid")).toBe("true");
      expect(target.querySelector("#schedulerConditionLimitError")?.textContent).toContain("between 1 and 65536 characters");
      expect(save.disabled).toBe(true);
    }
    // An unsaved interval draft must not be saved by the condition-limit button.
    const interval = target.querySelector<HTMLInputElement>('[aria-label="Scheduler wake interval in minutes"]')!;
    interval.value = "45";
    interval.dispatchEvent(new Event("input", { bubbles: true }));
    const descriptionLimit = target.querySelector<HTMLInputElement>('[aria-label="Scheduler description character limit"]')!;
    descriptionLimit.value = "8";
    descriptionLimit.dispatchEvent(new Event("input", { bubbles: true }));
    input.value = "256";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    expect(save.disabled).toBe(false);
    save.click();
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toEqual({
      agentBinding: { kind: "profile", name: "default" }, wakeIntervalMinutes: 30, conditionMaxChars: 256, descriptionMaxChars: 32,
    });
    await vi.waitFor(() => expect(panelModel.onRefreshScheduler).toHaveBeenCalledOnce());
  });

  it("preserves the saved condition limit when saving the wake interval", async () => {
    const fetch = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetch);
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, { target, props: { model: model() } });
    cleanups.push(() => unmount(component));
    await tick();
    const interval = target.querySelector<HTMLInputElement>('[aria-label="Scheduler wake interval in minutes"]')!;
    interval.value = "45";
    interval.dispatchEvent(new Event("input", { bubbles: true }));
    const limit = target.querySelector<HTMLInputElement>('[aria-label="Scheduler condition character limit"]')!;
    limit.value = "256";
    limit.dispatchEvent(new Event("input", { bubbles: true }));
    const descriptionLimit = target.querySelector<HTMLInputElement>('[aria-label="Scheduler description character limit"]')!;
    descriptionLimit.value = "8";
    descriptionLimit.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    target.querySelector<HTMLButtonElement>(".resource-settings-interval button")!.click();
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toEqual({
      agentBinding: { kind: "profile", name: "default" }, wakeIntervalMinutes: 45, conditionMaxChars: 128, descriptionMaxChars: 32,
    });
  });

  it("saves the Scheduler description limit without overwriting the other settings and validates the range", async () => {
    let settle!: (response: Response) => void;
    const fetch = vi.fn((_input: RequestInfo | URL, _init?: RequestInit) => new Promise<Response>((resolve) => { settle = resolve; }));
    vi.stubGlobal("fetch", fetch);
    const panelModel = model();
    const target = document.body.appendChild(document.createElement("div"));
    const component = mount(ResourceSettingsPanel, { target, props: { model: panelModel } });
    cleanups.push(() => unmount(component));
    await tick();
    const input = target.querySelector<HTMLInputElement>('[aria-label="Scheduler description character limit"]')!;
    const save = target.querySelector<HTMLButtonElement>(".resource-settings-description-limit button")!;
    const interval = target.querySelector<HTMLInputElement>('[aria-label="Scheduler wake interval in minutes"]')!;
    const conditionLimit = target.querySelector<HTMLInputElement>('[aria-label="Scheduler condition character limit"]')!;
    expect(input.value).toBe("32");
    expect(save.disabled).toBe(true);
    for (const value of ["0", "65537", "1.5", ""]) {
      input.value = value;
      input.dispatchEvent(new Event("input", { bubbles: true }));
      await tick();
      expect(input.getAttribute("aria-invalid")).toBe("true");
      expect(target.querySelector("#schedulerDescriptionLimitError")?.textContent).toContain("between 1 and 65536 characters");
      expect(save.disabled).toBe(true);
    }
    interval.value = "45";
    interval.dispatchEvent(new Event("input", { bubbles: true }));
    conditionLimit.value = "256";
    conditionLimit.dispatchEvent(new Event("input", { bubbles: true }));
    input.value = "16";
    input.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    expect(save.disabled).toBe(false);
    save.click();
    await tick();
    expect(interval.disabled).toBe(true);
    expect(conditionLimit.disabled).toBe(true);
    expect(input.disabled).toBe(true);
    settle(new Response("{}", { status: 200 }));
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toEqual({
      agentBinding: { kind: "profile", name: "default" }, wakeIntervalMinutes: 30, conditionMaxChars: 128, descriptionMaxChars: 16,
    });
    await vi.waitFor(() => expect(panelModel.onRefreshScheduler).toHaveBeenCalledOnce());
    await vi.waitFor(() => expect(input.disabled).toBe(false));
  });

});
