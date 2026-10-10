import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import SchedulerPanel from "../../src/components/SchedulerPanel.svelte";
import type { SchedulerConfigRecord } from "../../src/models/workspace";

const mounted: Array<ReturnType<typeof mount>> = [];

const config: SchedulerConfigRecord = {
  schemaVersion: 1,
  agentBinding: { kind: "profile", name: "default" },
  wakeIntervalMinutes: 30,
  conditionMaxChars: 128,
  schedules: [],
};

function mountPanel(panelConfig: SchedulerConfigRecord = config) {
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

describe("SchedulerPanel validation", () => {
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

  it("counts Unicode characters and blocks oversized conditions using the configured limit", async () => {
    const fetch = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel({ ...config, conditionMaxChars: 2 });
    await tick();
    inputValue(target.querySelector<HTMLInputElement>("input[placeholder^='What should']")!, "Check status");
    const condition = target.querySelector<HTMLTextAreaElement>("textarea")!;
    const save = target.querySelector<HTMLButtonElement>(".schedule-editor > button")!;
    inputValue(condition, "\u4e2d😀\u4e2d");
    await tick();
    expect(condition.getAttribute("aria-invalid")).toBe("true");
    expect(target.querySelector("#schedule-condition-error")?.textContent).toContain("at most 2 characters");
    expect(save.disabled).toBe(true);
    save.click();
    expect(fetch).not.toHaveBeenCalled();
    inputValue(condition, " \u4e2d😀 ");
    await tick();
    expect(target.textContent).toContain("Condition (2/2)");
    expect(condition.getAttribute("aria-invalid")).toBe("false");
    expect(save.disabled).toBe(false);
    save.click();
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
  });

  it("requires an existing oversized condition to be shortened before saving an edit", async () => {
    const target = mountPanel({ ...config, conditionMaxChars: 2, schedules: [{
      id: "schedule-legacy", description: "Legacy", condition: "long condition", target: "workspace", createdAt: "", updatedAt: "",
    }] });
    await tick();
    Array.from(target.querySelectorAll<HTMLButtonElement>(".schedule-list button")).find((button) => button.textContent?.includes("Edit"))!.click();
    await tick();
    expect(target.querySelector<HTMLTextAreaElement>("textarea")?.value).toBe("long condition");
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor > button")?.disabled).toBe(true);
    inputValue(target.querySelector<HTMLTextAreaElement>("textarea")!, "ok");
    await tick();
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor > button")?.disabled).toBe(false);
  });
});
