import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import ProjectCreateForm from "../../src/components/ProjectCreateForm.svelte";
import TaskWizard from "../../src/components/TaskWizard.svelte";
import type { CreateDialogModel, CreateDraft } from "../../src/components/models";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
});

function draft(overrides: Partial<CreateDraft> = {}): CreateDraft {
  return {
    type: "task", projectId: "project1", title: "",
    description: "", detail: "", slug: "",
    startAfterCreate: false, startBinding: { kind: "profile", name: "" }, startPrompt: "Default prompt.",
    ...overrides,
  };
}

function model(currentDraft: CreateDraft, overrides: Partial<CreateDialogModel> = {}): CreateDialogModel {
  return {
    open: true, identity: "dialog-1:task:project1", workspaceId: "workspace-a", draft: currentDraft,
    submitting: false,
    agents: [], agentProfiles: [], defaultTaskBinding: { kind: "profile", name: "default" },
    onClose: vi.fn(), onSubmit: vi.fn(),
    ...overrides,
  };
}

function target(): HTMLElement {
  return document.body.appendChild(document.createElement("div"));
}

function mountWizard(current: CreateDraft, currentModel: CreateDialogModel) {
  const host = target();
  const component = mount(TaskWizard, { target: host, props: { draft: current, model: currentModel } });
  cleanups.push(() => unmount(component));
  return host;
}

async function advance(host: HTMLElement): Promise<void> {
  host.querySelector<HTMLButtonElement>('.wizard-footer button[type="submit"]')!.click();
  await tick();
}

describe("CreateDialog child components", () => {
  it("keeps Project description and slug in the shared draft", async () => {
    const current = draft({ type: "project" });
    const host = target();
    const component = mount(ProjectCreateForm, { target: host, props: { draft: current } });
    cleanups.push(() => unmount(component));

    const description = host.querySelector<HTMLTextAreaElement>('textarea[name="description"]')!;
    description.value = "A focused project";
    description.dispatchEvent(new InputEvent("input", { bubbles: true }));
    const slug = host.querySelector<HTMLInputElement>('input[name="slug"]')!;
    slug.value = "focused-project";
    slug.dispatchEvent(new InputEvent("input", { bubbles: true }));
    await tick();

    expect(current.description).toBe("A focused project");
    expect(current.slug).toBe("focused-project");
  });

  it("hides the Back button on the first step and shows it afterwards", async () => {
    const current = draft({ title: "Review" });
    const host = mountWizard(current, model(current));
    await tick();

    const footerTexts = () => [...host.querySelectorAll<HTMLButtonElement>(".wizard-footer button")].map((button) => button.textContent?.trim());
    expect(footerTexts()).toEqual(["Cancel", "Next"]);

    await advance(host); // -> details
    expect(footerTexts()).toEqual(["Back", "Cancel", "Next"]);
  });

  it("starts on the title and slug step", async () => {
    const current = draft();
    const host = mountWizard(current, model(current));
    await tick();

    expect(host.querySelector('[aria-label="Template"]')).toBeNull();
    expect(host.querySelector('[aria-label="Basic information"]')).toBeTruthy();
    expect(host.querySelector(".wizard-step-pos")?.textContent?.trim()).toBe("Step 1 of 3");
    expect([...host.querySelectorAll(".wizard-step-label")].map((item) => item.textContent?.trim())).toEqual([
      "Title & slug", "Details", "Start options",
    ]);

    const nextButton = () => host.querySelector<HTMLButtonElement>('.wizard-footer button[type="submit"]')!;
    expect(nextButton().textContent?.trim()).toBe("Next");
    expect(nextButton().disabled).toBe(true);
    current.title = "Blank task";
    host.querySelector<HTMLFormElement>("form")!.dispatchEvent(new SubmitEvent("submit", { bubbles: true, cancelable: true }));
    await tick();
    expect(host.querySelector('[aria-label="Details"]')).toBeTruthy();
    expect(host.querySelector(".wizard-step-pos")?.textContent?.trim()).toBe("Step 2 of 3");
  });

  it("preselects the resolved default binding for the start step", async () => {
    const current = draft();
    mountWizard(current, model(current, {
      defaultTaskBinding: { kind: "agent", name: "agent-b" },
      agents: [{ id: "agent-b", label: "Agent B", summary: "" }],
    }));
    await tick();

    expect(current.startBinding).toEqual({ kind: "agent", name: "agent-b" });
  });
});
