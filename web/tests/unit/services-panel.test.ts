import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import type { ServiceStatus } from "../../src/api/types";
import ServicesPanelHarness from "../fixtures/ServicesPanelHarness.svelte";

const mounted: Array<ReturnType<typeof mount>> = [];

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => { resolve = resolvePromise; });
  return { promise, resolve };
}

function service(id: string): ServiceStatus {
  return {
    id,
    enabled: true,
    state: "ready",
    dependencies: [],
    readiness: { configured: true, ready: true },
    cleanup: { configured: false, succeeded: false },
    exports: {},
  };
}

function serviceResponse(...services: ServiceStatus[]): Response {
  return new Response(JSON.stringify({ services }), {
    headers: { "content-type": "application/json" },
  });
}

function serviceButton(target: HTMLElement, serviceId: string, label: string): HTMLButtonElement {
  const card = Array.from(target.querySelectorAll<HTMLElement>(".service-card"))
    .find((candidate) => candidate.querySelector("h3")?.textContent === serviceId);
  const button = Array.from(card?.querySelectorAll<HTMLButtonElement>("button") || [])
    .find((candidate) => candidate.textContent?.trim() === label);
  if (!button) throw new Error(`Missing ${label} button for ${serviceId}`);
  return button;
}

async function flushResponses(): Promise<void> {
  await new Promise<void>((resolve) => window.setTimeout(resolve, 0));
  await tick();
}

afterEach(async () => {
  while (mounted.length) await unmount(mounted.pop()!);
  document.body.replaceChildren();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("ServicesPanel Workspace identity", () => {
  it("rejects a late list and old row action after switching Workspaces", async () => {
    const firstA = deferred<Response>();
    const lateA = deferred<Response>();
    const firstB = deferred<Response>();
    let aLists = 0;
    let bLists = 0;
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = String(input);
      if (init?.method === "POST") return Promise.resolve(new Response(null, { status: 204 }));
      if (url === "/api/workspaces/workspace-a/services") return ++aLists === 1 ? firstA.promise : lateA.promise;
      if (url === "/api/workspaces/workspace-b/services") {
        bLists += 1;
        return bLists === 1 ? firstB.promise : Promise.resolve(serviceResponse(service("b-service")));
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);
    const onToast = vi.fn();
    const target = document.createElement("section");
    document.body.append(target);
    const component = mount(ServicesPanelHarness, {
      target,
      props: { initialWorkspaceId: "workspace-a", onToast },
    });
    mounted.push(component);

    await vi.waitFor(() => expect(aLists).toBe(1));
    firstA.resolve(serviceResponse(service("a-service")));
    await vi.waitFor(() => expect(target.textContent).toContain("a-service"));
    const oldAStart = serviceButton(target, "a-service", "Start");
    target.querySelector<HTMLButtonElement>(".services-heading button")!.click();
    await vi.waitFor(() => expect(aLists).toBe(2));

    component.switchWorkspace("workspace-b");
    await tick();
    expect(target.textContent).not.toContain("a-service");
    await vi.waitFor(() => expect(bLists).toBe(1));

    oldAStart.click();
    await tick();
    expect(fetchMock.mock.calls.map(([input]) => String(input)))
      .not.toContain("/api/workspaces/workspace-b/services/a-service/start");

    lateA.resolve(serviceResponse(service("stale-a-service")));
    await flushResponses();
    expect(target.textContent).not.toContain("stale-a-service");

    firstB.resolve(serviceResponse(service("b-service")));
    await vi.waitFor(() => expect(target.textContent).toContain("b-service"));
    serviceButton(target, "b-service", "Start").click();
    await vi.waitFor(() => expect(fetchMock.mock.calls.some(([input, init]) =>
      String(input) === "/api/workspaces/workspace-b/services/b-service/start" && init?.method === "POST",
    )).toBe(true));
    expect(onToast).not.toHaveBeenCalled();
  });

  it("ignores an A mutation completion after switching to B", async () => {
    const mutationA = deferred<Response>();
    let bLists = 0;
    const fetchMock = vi.fn((input: RequestInfo | URL, init?: RequestInit): Promise<Response> => {
      const url = String(input);
      if (url === "/api/workspaces/workspace-a/services" && !init?.method) {
        return Promise.resolve(serviceResponse(service("a-service")));
      }
      if (url === "/api/workspaces/workspace-a/services/a-service/start" && init?.method === "POST") {
        return mutationA.promise;
      }
      if (url === "/api/workspaces/workspace-b/services" && !init?.method) {
        bLists += 1;
        return Promise.resolve(serviceResponse(service("b-service")));
      }
      throw new Error(`Unexpected request: ${url}`);
    });
    vi.stubGlobal("fetch", fetchMock);
    const onToast = vi.fn();
    const target = document.createElement("section");
    document.body.append(target);
    const component = mount(ServicesPanelHarness, {
      target,
      props: { initialWorkspaceId: "workspace-a", onToast },
    });
    mounted.push(component);

    await vi.waitFor(() => expect(target.textContent).toContain("a-service"));
    serviceButton(target, "a-service", "Start").click();
    await vi.waitFor(() => expect(fetchMock).toHaveBeenCalledWith(
      "/api/workspaces/workspace-a/services/a-service/start",
      expect.objectContaining({ method: "POST" }),
    ));

    component.switchWorkspace("workspace-b");
    await vi.waitFor(() => expect(target.textContent).toContain("b-service"));
    mutationA.resolve(new Response(null, { status: 204 }));
    await flushResponses();

    expect(bLists).toBe(1);
    expect(target.textContent).toContain("b-service");
    expect(target.textContent).not.toContain("a-service");
    expect(onToast).not.toHaveBeenCalled();
  });
});
