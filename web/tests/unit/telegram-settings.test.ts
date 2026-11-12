import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";
import TelegramSettings from "../../src/components/TelegramSettings.svelte";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren(); vi.unstubAllGlobals();
});
const state = { configured: true, enabled: true, botName: "example_bot", accounts: [], topicCount: 0, lastError: "", lastPoll: "", deliveryProblems: [] };
function response(value: unknown) { return new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } }); }
async function render(fetchMock: typeof fetch, currentUserName = "") {
  vi.stubGlobal("fetch", fetchMock);
  const target = document.body.appendChild(document.createElement("div"));
  const component = mount(TelegramSettings, { target, props: { workspaceId: "ws-a", workspaceName: "Example", currentUserName } });
  cleanups.push(() => unmount(component)); await tick();
  return target;
}
async function expand(target: HTMLElement) {
  const details = target.querySelector("details")!;
  details.open = true; details.dispatchEvent(new Event("toggle")); await tick();
  await vi.waitFor(() => expect(target.querySelector('input[type="password"]')).not.toBeNull());
}
function button(target: HTMLElement, text: string) { return Array.from(target.querySelectorAll("button")).find((value) => value.textContent?.trim() === text)!; }

describe("Telegram Workspace settings", () => {
  it("loads lazily, keeps tokens blank, and saves only the chosen Workspace", async () => {
    const requests: Array<{ path: string; init?: RequestInit }> = [];
    const mock = vi.fn(async (path: RequestInfo | URL, init?: RequestInit) => {
      requests.push({ path: String(path), init });
      return response(String(path).endsWith("/users") ? { users: [{ name: "Alice" }] } : state);
    });
    const target = await render(mock);
    expect(requests).toHaveLength(0); await expand(target);
    const token = target.querySelector<HTMLInputElement>('input[type="password"]')!;
    expect(token.value).toBe(""); expect(token.placeholder).toContain("leave empty");
    token.value = "123:replacement-token"; token.dispatchEvent(new Event("input", { bubbles: true }));
    target.querySelector("form")!.dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(requests.some((r) => r.init?.method === "PUT")).toBe(true));
    const saved = requests.find((r) => r.init?.method === "PUT")!;
    expect(saved.path).toBe("/api/workspaces/ws-a/telegram");
    expect(JSON.parse(String(saved.init?.body))).toEqual({ token: "123:replacement-token", enabled: true });
    await vi.waitFor(() => expect(token.value).toBe(""));
  });
  it("pairs the selected user and requires a deliberate unlink action", async () => {
    const requests: Array<{ path: string; init?: RequestInit }> = [];
    const mock = vi.fn(async (path: RequestInfo | URL, init?: RequestInit) => {
      requests.push({ path: String(path), init });
      if (String(path).endsWith("/users")) return response({ users: [{ name: "Alice" }, { name: "Bob" }] });
      if (String(path).endsWith("/pair")) return response({ url: "https://t.me/example_bot?start=once", expiresAt: "2030-01-01T00:10:00Z" });
      return response({ ...state, accounts: [{ user: "Bob", telegramId: 777 }] });
    });
    const target = await render(mock); await expand(target);
    const select = target.querySelector("select")!; select.value = "Bob"; select.dispatchEvent(new Event("change", { bubbles: true })); await tick();
    target.querySelectorAll("form")[1].dispatchEvent(new Event("submit", { bubbles: true, cancelable: true }));
    await vi.waitFor(() => expect(target.querySelector('a[href="https://t.me/example_bot?start=once"]')).not.toBeNull());
    expect(JSON.parse(String(requests.find((r) => r.path.endsWith("/pair"))?.init?.body))).toEqual({ user: "Bob" });
    select.value = "Alice"; select.dispatchEvent(new Event("change", { bubbles: true })); await tick();
    expect(target.querySelector('a[href="https://t.me/example_bot?start=once"]')).toBeNull();
    button(target, "Unlink account").click(); await tick();
    expect(requests.some((r) => r.path.endsWith("/unbind"))).toBe(false);
    button(target, "Unlink").click();
    await vi.waitFor(() => expect(requests.some((r) => r.path.endsWith("/unbind"))).toBe(true));
    expect(JSON.parse(String(requests.find((r) => r.path.endsWith("/unbind"))?.init?.body))).toEqual({ user: "Bob" });
  });
  it("shows connection errors and fences retry with the exact pending delivery", async () => {
    const requests: Array<{ path: string; init?: RequestInit }> = [];
    const mock = vi.fn(async (path: RequestInfo | URL, init?: RequestInit) => {
      requests.push({ path: String(path), init });
      return response(String(path).endsWith("/users") ? { users: [{ name: "Alice" }] } : { ...state, lastError: "Connection failed", deliveryProblems: [{ route: "route-a", delivery: "delivery-b", resource: "project1.task1", user: "Alice", status: "unknown", error: "Check Telegram before retrying" }] });
    });
    const target = await render(mock); await expand(target);
    expect(target.textContent).toContain("Connection failed");
    button(target, "Review retry").click(); await tick();
    expect(requests.some((r) => r.path.endsWith("/delivery"))).toBe(false);
    button(target, "Retry delivery").click();
    await vi.waitFor(() => expect(requests.some((r) => r.path.endsWith("/delivery"))).toBe(true));
    expect(JSON.parse(String(requests.find((r) => r.path.endsWith("/delivery"))?.init?.body))).toEqual({ route: "route-a", delivery: "delivery-b", action: "retry" });
  });
  it("shows pause guidance and requires review before clearing older delivery state", async () => {
    const requests: Array<{ path: string; init?: RequestInit }> = [];
    const mock = vi.fn(async (path: RequestInfo | URL, init?: RequestInit) => {
      requests.push({ path: String(path), init });
      return response(String(path).endsWith("/users") ? { users: [{ name: "Alice" }] } : {
        ...state,
        topicProblems: [{ resource: "project1.task1", user: "Alice", reason: "chat_closed", hint: "Reopen the chat in PUA.", since: "2026-10-29T12:00:00Z" }],
        deliveryProblems: [
          { route: "route-a", delivery: "held-b", resource: "project1.task1", user: "Alice", status: "chat_reopened", held: true, count: 2, pendingStatus: "unknown" },
          { route: "route-a", delivery: "active-c", resource: "project1.task1", user: "Alice", status: "unknown", error: "Check Telegram" },
        ],
      });
    });
    const target = await render(mock); await expand(target);
    expect(target.textContent).toContain("Reopen the chat in PUA.");
    expect(target.textContent).toContain("2 earlier messages may not have been forwarded");
    expect(target.textContent).toContain("may already be in Telegram");
    button(target, "Review notice").click(); await tick();
    expect(requests.some((r) => r.path.endsWith("/delivery"))).toBe(false);
    expect(target.textContent).toContain("saved pending delivery will be removed");
    button(target, "Clear review notice").click();
    await vi.waitFor(() => expect(requests.some((r) => r.path.endsWith("/delivery"))).toBe(true));
    expect(JSON.parse(String(requests.find((r) => r.path.endsWith("/delivery"))?.init?.body))).toEqual({ route: "route-a", delivery: "held-b", action: "acknowledge" });
  });
});

it("requires an explicit choice when multiple users exist without a web selection", async () => {
  const target = await render(vi.fn(async (path: RequestInfo | URL) => response(String(path).endsWith("/users") ? { users: [{ name: "User" }, { name: "disksing" }] } : state)));
  await expand(target);
  expect(target.querySelector("select")!.value).toBe("");
  expect(button(target, "Create pairing link").disabled).toBe(true);
});
