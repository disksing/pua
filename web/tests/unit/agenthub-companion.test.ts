import { mount, tick, unmount } from "svelte";
import { afterEach, expect, it, vi } from "vitest";

import Companion from "../../src/agenthub/Companion.svelte";

const { api } = vi.hoisted(() => ({ api: vi.fn() }));
vi.mock("../../src/agenthub/core/api", () => ({ api, agentHubPath: (path: string) => path }));

afterEach(() => {
  vi.unstubAllGlobals();
  api.mockReset();
});

it.each([false, true])("renders the quota time cursor and reset text (standalone=%s)", async (standalone) => {
  vi.stubGlobal("ResizeObserver", class {
    observe() {}
    disconnect() {}
  });
  vi.stubGlobal("EventSource", class {
    close() {}
  });
  vi.stubGlobal("matchMedia", () => ({
    matches: true,
    addEventListener() {},
    removeEventListener() {},
  }));
  vi.spyOn(HTMLCanvasElement.prototype, "getContext").mockReturnValue(null);
  api.mockImplementation(async (path: string) => path === "/v1/quota" ? {
    quota: {
      configured: true,
      connected: true,
      providers: [{
        id: "claude",
        label: "Claude",
        quotas: [
          { key: "short", label: "5 hours", remainingPercent: 80, windowPositionPercent: 50, resetInSeconds: 9000 },
          { key: "long", label: "7 days", remainingPercent: 60, windowPositionPercent: 75, resetInSeconds: 453600 },
          { key: "unknown", label: "25 hours", remainingPercent: 70, resetInSeconds: 7200 },
          { key: "expired", label: "Expired", remainingPercent: 10, expired: true },
          { key: "balance", label: "Balance", display: "$0.00" },
          { key: "unlimited", label: "Included", unlimited: true },
        ],
      }],
    },
  } : { config: {} });

  const target = document.body.appendChild(document.createElement("div"));
  const component = mount(Companion, { target, props: { standalone, onOpenSettings: vi.fn() } });
  try {
    await tick();
    if (!standalone) target.querySelector<HTMLButtonElement>(".companion-pill")!.click();
    await vi.waitFor(() => expect(target.querySelectorAll(".companion-quota-row")).toHaveLength(6));
    const [short, long, unknown, expired, balance, unlimited] = target.querySelectorAll(".companion-quota-row");
    expect(short.querySelector<HTMLElement>(".companion-quota-track i")?.style.left).toBe("50%");
    expect(short.querySelector<HTMLElement>(".companion-quota-track span")?.style.width).toBe("80%");
    expect(long.querySelector<HTMLElement>(".companion-quota-track i")?.style.left).toBe("75%");
    expect(short.textContent).toContain("Resets in 2h 30m");
    expect(long.textContent).toContain("Resets in 5d 6h");
    expect(unknown.querySelector(".companion-quota-track i")).toBeNull();
    expect(unknown.textContent).toContain("Resets in 2h 0m");
    expect(expired.querySelector(".companion-quota-track i")).toBeNull();
    expect(expired.textContent).toContain("Reset due · awaiting new reading");
    expect(balance.querySelector(".companion-quota-track")).toBeNull();
    expect(balance.textContent).toContain("$0.00");
    expect(unlimited.querySelector(".companion-quota-track")).toBeNull();
    expect(unlimited.textContent).toContain("Unlimited");
  } finally {
    await unmount(component);
    target.remove();
  }
});
