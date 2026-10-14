import { mount, tick, unmount } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import SchedulerPanel from "../../src/components/SchedulerPanel.svelte";
import type { SchedulerConfigRecord } from "../../src/models/workspace";

const confirmDialogMock = vi.hoisted(() => vi.fn(async () => true));
vi.mock("../../src/controllers/confirm-dialog-controller", () => ({ confirmDialog: confirmDialogMock }));

// jsdom lacks the native dialog methods; browser tests cover focus and Escape.
Object.defineProperties(HTMLDialogElement.prototype, {
  showModal: { configurable: true, value() { this.open = true; } },
  close: { configurable: true, value() { this.open = false; this.dispatchEvent(new Event("close")); } },
});

beforeEach(() => confirmDialogMock.mockReset().mockResolvedValue(true));

const mounted: Array<ReturnType<typeof mount>> = [];

const config: SchedulerConfigRecord = {
  schemaVersion: 1,
  agentBinding: { kind: "profile", name: "default" },
  wakeIntervalMinutes: 30,
  conditionMaxChars: 128,
  descriptionMaxChars: 32,
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
      resolveResourceTitle: (resourceId: string) => resourceId === "project1.task1" ? "Target task" : resourceId === "workspace" ? "Test workspace" : resourceId === "project1" ? "Test project" : null,
      onChanged: vi.fn(async () => undefined),
      onToast: vi.fn(),
    },
  });
  mounted.push(component);
  return target;
}

async function openAdd(target: HTMLElement): Promise<void> {
  target.querySelector<HTMLButtonElement>(".schedule-list-heading button")!.click();
  await vi.waitFor(() => expect(target.querySelector<HTMLDialogElement>("dialog")?.open).toBe(true));
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
  const schedule = { id: "schedule-1", description: "Check release", condition: "when checks pass\nonly once", target: "project1.task1", createdAt: "", updatedAt: "" };

  it("renders one target-first table row per schedule with titles, full IDs, and complete conditions", async () => {
    const target = mountPanel({ ...config, schedules: [
      schedule,
      { ...schedule, id: "schedule-project", target: "project1" },
      { ...schedule, id: "schedule-workspace", target: "workspace" },
      { ...schedule, id: "schedule-scheduler", target: "scheduler" },
      { ...schedule, id: "schedule-missing", target: "project1.task999" },
    ] });
    await tick();
    expect(target.querySelector(".schedule-editor")).toBeNull();
    expect(Array.from(target.querySelectorAll("thead th"), (cell) => cell.textContent)).toEqual(["Target", "Schedule / Condition", "Actions"]);
    const rows = target.querySelectorAll("tbody tr");
    expect(rows).toHaveLength(5);
    expect(Array.from(rows, (row) => row.querySelector("td strong")?.textContent)).toEqual(["Target task", "Test project", "Test workspace", "Scheduler", "Title unavailable"]);
    expect(rows[4].querySelector("td code")?.textContent).toBe("project1.task999");
    expect(rows[0].querySelector(".schedule-description-cell p")?.textContent).toBe(schedule.condition);
    expect(Array.from(rows[0].querySelectorAll("button"), (button) => button.textContent?.trim())).toEqual(["Edit", "Remove"]);
  });

  it("cancels an edit without a request and clears the draft before adding", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel({ ...config, schedules: [schedule] });
    await tick();
    target.querySelector<HTMLButtonElement>("tbody button")!.click();
    await tick();
    await vi.waitFor(() => expect(target.querySelector<HTMLDialogElement>("dialog")?.open).toBe(true));
    expect(target.querySelector<HTMLInputElement>("input")?.value).toBe(schedule.description);
    target.querySelector<HTMLButtonElement>(".schedule-editor-heading button")!.click();
    await tick();
    expect(target.querySelector<HTMLDialogElement>("dialog")?.open).toBe(false);
    expect(fetch).not.toHaveBeenCalled();
    await openAdd(target);
    expect(target.querySelector<HTMLInputElement>("input")?.value).toBe("");
    expect(target.querySelector<HTMLInputElement>("input[placeholder^='workspace']")?.value).toBe("workspace");
  });

  it("updates the selected schedule and closes the editor after success", async () => {
    const fetch = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel({ ...config, schedules: [schedule] });
    await tick();
    target.querySelector<HTMLButtonElement>("tbody button")!.click();
    await tick();
    inputValue(target.querySelector<HTMLInputElement>("input")!, "Updated release check");
    await tick();
    target.querySelector<HTMLButtonElement>(".schedule-editor > button")!.click();
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(fetch.mock.calls[0]).toEqual(["/api/workspaces/ws-test/scheduler/schedule-1", expect.objectContaining({ method: "PUT", body: JSON.stringify({description: "Updated release check", condition: schedule.condition, target: schedule.target}) })]);
    await vi.waitFor(() => expect(target.querySelector<HTMLDialogElement>("dialog")?.open).toBe(false));
  });

  it("keeps the editor draft open when saving fails", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({error: "Save failed"}), { status: 500 })));
    const target = mountPanel({ ...config, schedules: [schedule] });
    await tick();
    target.querySelector<HTMLButtonElement>("tbody button")!.click();
    await tick();
    target.querySelector<HTMLButtonElement>(".schedule-editor > button")!.click();
    await vi.waitFor(() => expect(target.querySelector(".schedule-editor [role='alert']")?.textContent).toContain("Save failed"));
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor > button")?.disabled).toBe(false);
    expect(target.querySelector<HTMLDialogElement>("dialog")?.open).toBe(true);
    expect(target.querySelector<HTMLInputElement>("input")?.value).toBe(schedule.description);
  });

  it("removes a row only after confirming the existing removal dialog", async () => {
    const fetch = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel({ ...config, schedules: [schedule] });
    await tick();
    confirmDialogMock.mockResolvedValueOnce(false);
    const remove = target.querySelector<HTMLButtonElement>("tbody .danger-button")!;
    remove.click();
    await tick();
    expect(fetch).not.toHaveBeenCalled();
    remove.click();
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
    expect(confirmDialogMock).toHaveBeenLastCalledWith(expect.objectContaining({title: "Remove schedule", danger: true}));
    expect(fetch.mock.calls[0]).toEqual(["/api/workspaces/ws-test/scheduler/schedule-1", expect.objectContaining({method: "DELETE"})]);
  });

  it("keeps a pending save modal open and disables draft edits until the request settles", async () => {
    let settle!: (response: Response) => void;
    vi.stubGlobal("fetch", vi.fn(() => new Promise<Response>((resolve) => { settle = resolve; })));
    const target = mountPanel({ ...config, schedules: [schedule] });
    await tick();
    target.querySelector<HTMLButtonElement>("tbody button")!.click();
    await vi.waitFor(() => expect(target.querySelector<HTMLDialogElement>("dialog")?.open).toBe(true));
    target.querySelector<HTMLButtonElement>(".schedule-editor > button")!.click();
    await tick();
    expect(target.querySelector<HTMLInputElement>("input")?.disabled).toBe(true);
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor-heading button")?.disabled).toBe(true);
    const cancel = new Event("cancel", { cancelable: true });
    target.querySelector("dialog")!.dispatchEvent(cancel);
    expect(cancel.defaultPrevented).toBe(true);
    settle(new Response("{}", { status: 200 }));
    await vi.waitFor(() => expect(target.querySelector<HTMLDialogElement>("dialog")?.open).toBe(false));
  });

  it("marks an unknown target invalid and prevents the schedule request", async () => {
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel();
    await openAdd(target);

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
    await openAdd(target);

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
    await openAdd(target);
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

  it("counts Unicode characters and blocks oversized descriptions using the configured limit", async () => {
    const fetch = vi.fn(async () => new Response("{}", { status: 200 }));
    vi.stubGlobal("fetch", fetch);
    const target = mountPanel({ ...config, descriptionMaxChars: 2 });
    await openAdd(target);
    const description = target.querySelector<HTMLInputElement>('input[aria-label="Description"]')!;
    const save = target.querySelector<HTMLButtonElement>(".schedule-editor > button")!;
    inputValue(target.querySelector<HTMLTextAreaElement>("textarea")!, "ok");
    inputValue(description, "\u4e2d😀\u4e2d");
    await tick();
    expect(description.getAttribute("aria-invalid")).toBe("true");
    expect(target.querySelector("#schedule-description-error")?.textContent).toContain("at most 2 characters");
    expect(save.disabled).toBe(true);
    save.click();
    expect(fetch).not.toHaveBeenCalled();
    inputValue(description, " \u4e2d😀 ");
    await tick();
    expect(target.textContent).toContain("Description (2/2)");
    expect(description.getAttribute("aria-invalid")).toBe("false");
    expect(save.disabled).toBe(false);
    save.click();
    await vi.waitFor(() => expect(fetch).toHaveBeenCalledOnce());
  });

  it("requires an existing oversized description to be shortened before saving an edit", async () => {
    const target = mountPanel({ ...config, descriptionMaxChars: 2, schedules: [{
      id: "schedule-legacy", description: "Legacy", condition: "ok", target: "workspace", createdAt: "", updatedAt: "",
    }] });
    await tick();
    Array.from(target.querySelectorAll<HTMLButtonElement>(".schedule-list button")).find((button) => button.textContent?.includes("Edit"))!.click();
    await tick();
    expect(target.querySelector<HTMLInputElement>('input[aria-label="Description"]')?.value).toBe("Legacy");
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor > button")?.disabled).toBe(true);
    inputValue(target.querySelector<HTMLInputElement>('input[aria-label="Description"]')!, "ok");
    await tick();
    expect(target.querySelector<HTMLButtonElement>(".schedule-editor > button")?.disabled).toBe(false);
  });
});
