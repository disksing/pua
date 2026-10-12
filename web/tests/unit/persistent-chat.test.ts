import { mount, tick, unmount } from "svelte";
import { afterEach, expect, it, vi } from "vitest";
import PersistentChat from "../../src/components/PersistentChat.svelte";
import { createPUAAppChannels } from "../../src/app-channels";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { while (cleanups.length) await cleanups.pop()?.(); vi.unstubAllGlobals(); document.body.replaceChildren(); });
function response(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } }); }
function setup() {
 const channel = createPUAAppChannels().detail;
 channel.publish({ ...channel.current(), identity: "w:project1.task1", workspaceId: "w", resourceId: "project1.task1", currentUserName: "disksing", resourceTitle: "Task 1" });
 const target = document.body.appendChild(document.createElement("div"));
 const component = mount(PersistentChat, { target, props: { channel } });
 cleanups.push(() => unmount(component));
 return { channel, target };
}
function button(target: HTMLElement, text: string) { const result = [...target.querySelectorAll("button")].find(b => b.textContent?.trim() === text); expect(result).toBeTruthy(); return result!; }
it("keeps history on close and reopen and retains drafts across parent refreshes", async () => {
 let chat = { id: "c", open: true, epoch: 1, messages: [{ id: "m", role: "agent", text: "Public reply", createdAt: "2026-10-13T00:00:00Z" }] };
 vi.stubGlobal("fetch", vi.fn(async (input, init) => {
  const url = String(input);
  if (url.endsWith("/close")) chat = { ...chat, open: false };
  if (url.endsWith("/open")) chat = { ...chat, open: true, epoch: 2 };
  if (url.endsWith("/status")) return response({ sessionState: "idle" });
  return response(chat);
 }));
 const { channel, target } = setup();
 await vi.waitFor(() => expect(target.textContent).toContain("Public reply"));
 const input = target.querySelector("textarea")!; input.value = "unfinished thought"; input.dispatchEvent(new Event("input", { bubbles: true })); await tick();
 channel.publish({ ...channel.current(), resourceTitle: "Updated title" }); await tick();
 expect(target.querySelector("textarea")?.value).toBe("unfinished thought");
 button(target, "Close chat").click(); await vi.waitFor(() => expect(target.querySelector("textarea")).toBeNull());
 expect(target.textContent).toContain("Public reply");
 button(target, "Reopen chat").click(); await vi.waitFor(() => expect(target.querySelector("textarea")).not.toBeNull());
 expect(target.textContent).toContain("Public reply");
});
it("retries an uncertain send with the same id and never uses the execution mailbox", async () => {
 const ids: string[] = [];
 const fetch = vi.fn(async (input, init) => {
  const path = String(input);
  if (path.endsWith("/status")) return response({ sessionState: "idle" });
  if (path.endsWith("/messages")) { const body = JSON.parse(String(init?.body)); ids.push(body.id); return response({ error: "temporarily unavailable" }, 503); }
  return response({ id: "c", open: true, epoch: 1, messages: [] });
 });
 vi.stubGlobal("fetch", fetch);
 const { target } = setup();
 await vi.waitFor(() => expect(target.querySelector("textarea")).not.toBeNull());
 const input = target.querySelector("textarea")!; input.value = "please investigate"; input.dispatchEvent(new Event("input", { bubbles: true })); await tick();
 button(target, "Send").click(); await vi.waitFor(() => expect(target.querySelector('[role="alert"]')).not.toBeNull());
 button(target, "Send").click(); await vi.waitFor(() => expect(ids).toHaveLength(2)); expect(ids[0]).toBe(ids[1]);
 expect(fetch.mock.calls.filter(([path]) => String(path).endsWith("/messages")).every(([path]) => String(path).includes("/chat/messages"))).toBe(true);
});
