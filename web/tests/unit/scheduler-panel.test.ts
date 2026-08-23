import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import DetailPanel from "../../src/components/DetailPanel.svelte";
import SchedulerPanel from "../../src/components/SchedulerPanel.svelte";
import { createModelChannel } from "../../src/components/model-channel";
import type { DetailPanelModel } from "../../src/components/models";
import type { ScheduleRecord, SchedulerConfigRecord } from "../../src/models/workspace";

const mounted: Array<ReturnType<typeof mount>> = [];

const config: SchedulerConfigRecord = {
  schemaVersion: 2,
  agentBinding: { kind: "profile", name: "default" },
  schedules: [],
};

const schedule: ScheduleRecord = {
  id: "schedule-0123456789abcdef01234567",
  revision: 1,
  description: "Recover target",
  condition: "once",
  target: "project1.task1",
  state: "active",
  trigger: { type: "at", at: "2026-08-23T09:00:00Z" },
  createdAt: "2026-08-23T08:00:00Z",
  updatedAt: "2026-08-23T08:00:00Z",
  effectiveState: "active",
};

function schedulerModel(workspaceId: string, onRefreshScheduler: () => Promise<void>, onToast: (message: string) => void): DetailPanelModel {
  return {
    identity: `${workspaceId}:scheduler:scheduler`,
    workspaceId,
    workspaceName: workspaceId,
    resourceId: "scheduler",
    resourceType: "scheduler",
    resourceTitle: "Scheduler",
    parent: null,
    loading: false,
    detail: { id: "scheduler", type: "scheduler", title: "Scheduler", path: "scheduler", scheduler: { ...config, schedules: [schedule] } },
    wiki: null,
    workspaceAgents: null,
    workspaceDefaults: { project: { kind: "profile", name: "default" }, task: { kind: "profile", name: "default" } },
    workspaceUsers: [],
    currentUserName: "User",
    generationPolicy: { enabled: true, maxTurns: 20, maxAccumulatedTurnMinutes: 120 },
    stallWatchdogPolicy: { enabled: true, timeoutMinutes: 30 },
    agentBinding: { kind: "profile", name: "default" },
    agentProfiles: [],
    agents: [],
    resolveResourceTitle: (resourceId) => resourceId === "project1.task1" ? "Target task" : null,
    onNavigate: vi.fn(),
    onCreateTask: vi.fn(),
    onArchive: vi.fn(),
    onSaveWorkspaceAgents: vi.fn(async () => ({ path: "AGENTS.md", content: "", contentHash: "saved" })),
    onSaveMarkdownFile: vi.fn(async (path, content) => ({ path, content, contentHash: "saved" })),
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
    onRefreshScheduler,
    onToast,
  };
}

function mountPanel(panelConfig = config) {
  const target = document.createElement("section");
  document.body.append(target);
  const component = mount(SchedulerPanel, {
    target,
    props: {
      workspaceId: "ws-test",
      config: panelConfig,
      resolveResourceTitle: (resourceId: string) => resourceId === "project1.task1" ? "Target task" : resourceId === "workspace" ? "Test workspace" : null,
      onChanged: vi.fn(async () => undefined),
      onToast: vi.fn(),
    },
  });
  mounted.push(component);
  return target;
}

function inputValue(element: HTMLInputElement | HTMLTextAreaElement, value: string): void {
  element.value = value;
  element.dispatchEvent(new Event("input", { bubbles: true }));
}

afterEach(async () => {
  while (mounted.length) await unmount(mounted.pop()!);
  document.body.replaceChildren();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("SchedulerPanel", () => {
  it("rejects a late pause acknowledgement after the Workspace changes", async () => {
    const pending = new Map<string, (response: Response) => void>();
    vi.stubGlobal("fetch", vi.fn((input: RequestInfo | URL) => new Promise<Response>((resolve) => pending.set(String(input), resolve))));
    const oldChanged = vi.fn(async () => undefined);
    const oldToast = vi.fn();
    const currentChanged = vi.fn(async () => undefined);
    const currentToast = vi.fn();
    const oldModel = schedulerModel("workspace-old", oldChanged, oldToast);
    const channel = createModelChannel(oldModel);
    const target = document.createElement("section");
    document.body.append(target);
    mounted.push(mount(DetailPanel, { target, props: { channel } }));
    await tick();

    const pauseButton = () => Array.from(target.querySelectorAll<HTMLButtonElement>("button"))
      .find((button) => button.textContent?.trim() === "Pause")!;
    pauseButton().click();
    await vi.waitFor(() => expect(pending.has("/api/workspaces/workspace-old/scheduler/schedule-0123456789abcdef01234567/pause")).toBe(true));

    channel.publish(schedulerModel("workspace-current", currentChanged, currentToast));
    await tick();
    pauseButton().click();
    await vi.waitFor(() => expect(pending.has("/api/workspaces/workspace-current/scheduler/schedule-0123456789abcdef01234567/pause")).toBe(true));

    pending.get("/api/workspaces/workspace-old/scheduler/schedule-0123456789abcdef01234567/pause")!(new Response(JSON.stringify({}), {
      status: 200,
      headers: { "content-type": "application/json" },
    }));
    await new Promise((resolve) => setTimeout(resolve, 0));
    await tick();

    expect(oldChanged).not.toHaveBeenCalled();
    expect(oldToast).not.toHaveBeenCalled();
    expect(currentChanged).not.toHaveBeenCalled();
    expect(currentToast).not.toHaveBeenCalled();
    expect(pauseButton().disabled).toBe(true);

    pending.get("/api/workspaces/workspace-current/scheduler/schedule-0123456789abcdef01234567/pause")!(new Response(JSON.stringify({}), {
      status: 200,
      headers: { "content-type": "application/json" },
    }));
    await vi.waitFor(() => expect(currentChanged).toHaveBeenCalledOnce());
    expect(currentToast).toHaveBeenCalledWith("Schedule paused.");
  });

  it("marks an unknown target invalid and prevents the schedule request", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel();
    await tick();

    inputValue(target.querySelector<HTMLInputElement>("input[placeholder^='What should']")!, "Review the release");
    inputValue(target.querySelector<HTMLTextAreaElement>("textarea")!, "when the release is ready");
    const targetInput = target.querySelector<HTMLInputElement>("input[placeholder^='workspace']")!;
    inputValue(targetInput, "not-a-resource");
    await tick();

    expect(targetInput.getAttribute("aria-invalid")).toBe("true");
    expect(targetInput.getAttribute("aria-describedby")).toBe("schedule-target-error");
    expect(target.querySelector("#schedule-target-error")?.textContent).toContain("open resource in the current Workspace");
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor > button")?.disabled).toBe(true);

    target.querySelector<HTMLButtonElement>(".schedule-editor > button")!.click();
    expect(fetch).not.toHaveBeenCalled();
  });

  it("accepts a resource resolved from the current Workspace", async () => {
    const fetch = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(JSON.stringify({}), {
      status: 201,
      headers: { "content-type": "application/json" },
    }));
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel();
    await tick();

    inputValue(target.querySelector<HTMLInputElement>("input[placeholder^='What should']")!, "Review the release");
    inputValue(target.querySelector<HTMLTextAreaElement>("textarea")!, "when the release is ready");
    const targetInput = target.querySelector<HTMLInputElement>("input[placeholder^='workspace']")!;
    inputValue(targetInput, "project1.task1");
    await tick();

    expect(targetInput.getAttribute("aria-invalid")).toBe("false");
    const addButton = target.querySelector<HTMLButtonElement>(".schedule-editor > button")!;
    expect(addButton.disabled).toBe(false);
    addButton.click();
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(JSON.parse(String(fetch.mock.calls[0][1]?.body))).toEqual({
      description: "Review the release",
      condition: "when the release is ready",
      target: "project1.task1",
    });
  });

  it("offers direct recovery for a schedule requiring attention", async () => {
    const fetch = vi.fn(async (_input: RequestInfo | URL, _init?: RequestInit) => new Response(JSON.stringify({}), {
      status: 200,
      headers: { "content-type": "application/json" },
    }));
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel({
      ...config,
      schedules: [{
        ...schedule,
        effectiveState: "attention_required",
        lastOutcome: "attention_required",
      }],
    });
    await tick();

    const resume = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Resume");
    expect(resume).toBeDefined();
    resume!.click();
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(String(fetch.mock.calls[0][0])).toContain("/scheduler/schedule-0123456789abcdef01234567/resume");
    expect(fetch.mock.calls[0][1]?.method).toBe("POST");
  });
});
