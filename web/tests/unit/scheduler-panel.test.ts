import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import SchedulerPanel from "../../src/components/SchedulerPanel.svelte";
import type { SchedulerMutationCallbacks } from "../../src/models/detail";
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

function schedulerActions(overrides: Partial<SchedulerMutationCallbacks> = {}): SchedulerMutationCallbacks {
  return {
    validateTarget: (resourceId) => {
      const target = resourceId.trim();
      if (!target) return "Target resource is required.";
      return target === "workspace" || target === "scheduler" || target === "project1.task1"
        ? ""
        : "Target must be an open resource in the current Workspace.";
    },
    save: vi.fn(async () => true),
    setPaused: vi.fn(async () => true),
    remove: vi.fn(async () => true),
    ...overrides,
  };
}

function mountPanel(panelConfig = config, actions = schedulerActions()) {
  const target = document.createElement("section");
  document.body.append(target);
  const component = mount(SchedulerPanel, {
    target,
    props: {
      config: panelConfig,
      actions,
    },
  });
  mounted.push(component);
  return { target, actions };
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
  it("declares its owner boundary on each visual root", () => {
    const { target } = mountPanel();

    expect(Array.from(target.children).map((root) => root.getAttribute("data-component-owner"))).toEqual([
      "scheduler-panel",
      "scheduler-panel",
    ]);
  });

  it("marks an unknown target invalid and prevents the schedule request", async () => {
    const { target, actions } = mountPanel();
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
    expect(actions.save).not.toHaveBeenCalled();
  });

  it("accepts a resource resolved from the current Workspace", async () => {
    const save = vi.fn(async () => true);
    const { target } = mountPanel(config, schedulerActions({ save }));
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
    await vi.waitFor(() => expect(save).toHaveBeenCalledOnce());
    expect(save).toHaveBeenCalledWith({
      scheduleId: undefined,
      description: "Review the release",
      condition: "when the release is ready",
      target: "project1.task1",
    });
  });

  it("offers direct recovery for a schedule requiring attention", async () => {
    const setPaused = vi.fn(async () => true);
    const { target } = mountPanel({
      ...config,
      schedules: [{
        ...schedule,
        effectiveState: "attention_required",
        lastOutcome: "attention_required",
      }],
    }, schedulerActions({ setPaused }));
    await tick();

    const resume = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Resume");
    expect(resume).toBeDefined();
    resume!.click();
    await vi.waitFor(() => expect(setPaused).toHaveBeenCalledOnce());
    expect(setPaused).toHaveBeenCalledWith("schedule-0123456789abcdef01234567", false);
  });

  it("renders the last occurrence and outcome together", () => {
    const lastOccurrenceAt = "2026-08-23T09:00:00.123456789Z";
    const { target } = mountPanel({
      ...config,
      schedules: [{
        ...schedule,
        lastOccurrenceAt,
        lastOutcome: "completed",
      }],
    });

    const details = Object.fromEntries(Array.from(target.querySelectorAll("dl > div"), (row) => [
      row.querySelector("dt")?.textContent,
      row.querySelector("dd")?.textContent,
    ]));
    expect(details["Last occurrence"]).toBe(lastOccurrenceAt);
    expect(details["Last outcome"]).toBe("completed");
  });

  it("omits the last occurrence row when no occurrence is recorded", () => {
    const { target } = mountPanel({
      ...config,
      schedules: [{ ...schedule, lastOutcome: "pending" }],
    });

    const labels = Array.from(target.querySelectorAll("dt"), (label) => label.textContent);
    expect(labels).not.toContain("Last occurrence");
    expect(labels).toContain("Last outcome");
  });

  it("keeps a schedule row pending while its callback is unresolved", async () => {
    let resolve!: (completed: boolean) => void;
    const setPaused = vi.fn(() => new Promise<boolean>((done) => { resolve = done; }));
    const { target } = mountPanel({ ...config, schedules: [schedule] }, schedulerActions({ setPaused }));
    await tick();

    const pause = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Pause")!;
    pause.click();
    await vi.waitFor(() => expect(pause.disabled).toBe(true));
    pause.click();
    expect(setPaused).toHaveBeenCalledOnce();

    resolve(true);
    await vi.waitFor(() => expect(pause.disabled).toBe(false));
  });
});
