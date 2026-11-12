import { mount, tick, unmount } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import SettingsDialog from "../../src/agenthub/SettingsDialog.svelte";
import { createDraft } from "../../src/agenthub/settings/config-model";

const { api } = vi.hoisted(() => ({ api: vi.fn() }));
vi.mock("../../src/agenthub/core/api", () => ({ api }));

let config: any;
let probes: any[];
let target: HTMLElement;
let component: ReturnType<typeof mount>;

const initial = () => createDraft({
  agentProviders: [{ id: "codex", name: "Codex", type: "codex" }, { id: "pi", name: "Pi", type: "pi" }],
  agents: [
    { name: "Alpha", providerId: "codex", options: { model: "old" }, environment: { TOKEN: "" } },
    { name: "Beta", providerId: "pi" },
  ],
});

async function render() {
  target = document.body.appendChild(document.createElement("div"));
  component = mount(SettingsDialog, { target, props: { onClose: vi.fn(), onSaved: vi.fn() } });
  await tick();
  await vi.waitFor(() => expect(target.querySelector(".settings-nav")).not.toBeNull());
  const agents = [...target.querySelectorAll<HTMLButtonElement>(".settings-nav button")].find((button) => button.textContent?.includes("Agents"))!;
  agents.click();
  await tick();
}

function click(label: string) {
  const button = [...target.querySelectorAll<HTMLButtonElement>("button")].find((item) => item.getAttribute("aria-label") === label || item.textContent?.trim() === label);
  expect(button, label).toBeDefined();
  button!.click();
  return tick();
}

beforeEach(() => {
  config = initial();
  probes = [{ providerId: "codex", available: true, command: "/bin/codex" }, { providerId: "pi", available: true, command: "/bin/pi" }];
  api.mockImplementation(async (url: string, options?: { method?: string; body?: string }) => {
    if (url === "/v1/config" && options?.method === "PUT") { config = JSON.parse(options.body!).config; return {}; }
    if (url === "/v1/config") return { config: structuredClone(config) };
    if (url === "/v1/agents") return { probes };
    if (url === "/v1/quota") return { quota: { providers: [] } };
    if (url.includes("/models")) return { models: [{ id: "old" }] };
    if (url.startsWith("/v1/config/providers/") && options?.method === "PUT") {
      const id = decodeURIComponent(url.split("/").at(-1)!);
      const command = JSON.parse(options.body!).command;
      const provider = { ...(config.agentProviders.find((item: any) => item.id === id) || { id, name: id, type: id }), command };
      const index = config.agentProviders.findIndex((item: any) => item.id === id);
      if (index >= 0) config.agentProviders[index] = provider;
      else config.agentProviders.push(provider);
      probes = [...probes.filter((item) => item.providerId !== id), { providerId: id, available: true, command }];
      return { provider };
    }
    throw new Error(`Unexpected API call ${url}`);
  });
});

afterEach(async () => {
  if (component) await unmount(component);
  document.body.replaceChildren();
  api.mockReset();
});

describe("AgentHub settings agent and provider controls", () => {
  it("starts collapsed, expands an agent, persists add/edit/delete and keyboard/drag reorder", async () => {
    await render();
    expect(target.querySelectorAll(".settings-agent-card")).toHaveLength(2);
    expect(target.querySelectorAll(".settings-agent-card-body")).toHaveLength(0);
    expect(target.querySelectorAll(".settings-nav button")).toHaveLength(3);
    expect(target.querySelector(".settings-agent-card")?.textContent).toContain("Codex · model=old");

    const handle = target.querySelectorAll<HTMLButtonElement>(".settings-drag-handle")[1];
    expect(handle.getAttribute("aria-keyshortcuts")).toBe("ArrowUp ArrowDown");
    handle.focus();
    handle.dispatchEvent(new KeyboardEvent("keydown", { key: "ArrowUp", bubbles: true }));
    await tick();
    expect(target.querySelector(".settings-agent-card strong")?.textContent).toBe("Beta");
    expect(document.activeElement).toBe(handle);
    const firstHandle = target.querySelector<HTMLButtonElement>(".settings-drag-handle")!;
    firstHandle.dispatchEvent(new Event("dragstart", { bubbles: true }));
    target.querySelectorAll(".settings-agent-card")[1].dispatchEvent(new Event("dragover", { bubbles: true, cancelable: true }));
    target.querySelectorAll(".settings-agent-card")[1].dispatchEvent(new Event("drop", { bubbles: true, cancelable: true }));
    await tick();
    expect(target.querySelector(".settings-agent-card strong")?.textContent).toBe("Alpha");

    await click("Delete Beta");
    await click("Add agent");
    expect(target.querySelectorAll(".settings-agent-card-body")).toHaveLength(1);
    const name = target.querySelector<HTMLInputElement>(".settings-agent-card-body input")!;
    name.value = "Gamma";
    name.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    const provider = target.querySelector<HTMLSelectElement>(".settings-agent-card-body select")!;
    provider.value = "pi";
    provider.dispatchEvent(new Event("change", { bubbles: true }));
    await tick();
    await click("Save all");
    await vi.waitFor(() => expect(config.agents.map((agent: any) => agent.name)).toEqual(["Alpha", "Gamma"]));
    expect(config.agents[1].providerId).toBe("pi");
    expect(config.agents[0].environment).toEqual({ TOKEN: "" });
    await unmount(component);
    component = undefined as any;
    await render();
    expect(target.querySelectorAll(".settings-agent-card-body")).toHaveLength(0);
    expect([...target.querySelectorAll(".settings-agent-card strong")].map((node) => node.textContent)).toEqual(["Alpha", "Gamma"]);
  });

  it("merges a confirmed provider path with unsaved agent edits, and preserves drafts on errors", async () => {
    await render();
    await click("Delete Beta");
    await click("Change Codex executable path");
    const path = target.querySelector<HTMLInputElement>('input[aria-label="Codex executable path"]')!;
    path.value = "/new/codex";
    path.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    api.mockImplementationOnce(async () => { throw new Error("not executable"); });
    await click("Confirm Codex executable path");
    await vi.waitFor(() => expect(target.textContent).toContain("not executable"));
    expect(config.agentProviders[0].command).toBeUndefined();
    await click("Confirm Codex executable path");
    await vi.waitFor(() => expect(target.querySelector('button[aria-label="Change Codex executable path"]')).not.toBeNull());
    expect(config.agentProviders.find((item: any) => item.id === "codex").command).toBe("/new/codex");

    await click("Save all");
    expect(config.agentProviders.find((item: any) => item.id === "codex").command).toBe("/new/codex");
    await vi.waitFor(() => expect(config.agents.map((agent: any) => agent.name)).toEqual(["Alpha"]));
    await vi.waitFor(() => expect(target.textContent).toContain("Saved"));

    await click("Add agent");
    const name = target.querySelector<HTMLInputElement>(".settings-agent-card-body input")!;
    name.value = "alpha";
    name.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    expect(target.querySelector(".settings-agent-card-body [aria-invalid=true]")).not.toBeNull();
    await click("Save all");
    expect(config.agents).toHaveLength(1);
    expect(target.textContent).toContain("already used");
    name.value = "Delta";
    name.dispatchEvent(new Event("input", { bubbles: true }));
    await tick();
    api.mockImplementationOnce(async () => { throw new Error("Save rejected"); });
    await click("Save all");
    await vi.waitFor(() => expect(target.textContent).toContain("Save rejected"));
    expect(config.agents).toHaveLength(1);
    await click("Save all");
    await vi.waitFor(() => expect(config.agents).toHaveLength(2));
  });
});
