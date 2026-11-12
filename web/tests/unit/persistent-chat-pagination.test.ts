import { mount, tick, unmount } from "svelte";
import { afterEach, expect, it, vi } from "vitest";
import PersistentChat from "../../src/components/PersistentChat.svelte";
import { createPUAAppChannels } from "../../src/app-channels";

type Message = { id: string; role: string; text: string; createdAt: string };
const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  vi.unstubAllGlobals();
  document.body.replaceChildren();
});
function message(index: number): Message {
  return { id: `m${String(index).padStart(3, "0")}`, role: "agent", text: `Message ${index}`, createdAt: "2026-10-31T00:00:00Z" };
}
function response(body: unknown) {
  return new Response(JSON.stringify(body), { headers: { "Content-Type": "application/json" } });
}
function page(messages: Message[], query: URLSearchParams) {
  const limit = Number(query.get("limit") || messages.length);
  const before = query.get("before"), after = query.get("after");
  let start = 0, end = messages.length;
  if (before) end = messages.findIndex(item => item.id === before);
  if (after) start = messages.findIndex(item => item.id === after) + 1;
  if (after) end = Math.min(end, start + limit);
  else start = Math.max(start, end - limit);
  return { id: "c", open: true, epoch: 1, unreadCount: 0, messages: messages.slice(start, end), hasOlder: start > 0, hasNewer: end < messages.length };
}
function setup() {
  const channel = createPUAAppChannels().detail;
  channel.publish({ ...channel.current(), identity: "w:project1.task1", workspaceId: "w", resourceId: "project1.task1", currentUserName: "disksing", resourceTitle: "Task 1" });
  const scroller = document.body.appendChild(document.createElement("div"));
  scroller.id = "chatTimeline";
  const target = scroller.appendChild(document.createElement("div"));
  Object.defineProperties(scroller, {
    clientHeight: { configurable: true, value: 400 },
    scrollHeight: { configurable: true, get: () => target.querySelectorAll("article").length * 100 + 500 },
  });
  const component = mount(PersistentChat, { target, props: { channel } });
  cleanups.push(() => unmount(component));
  return { channel, scroller, target };
}

it("loads older messages without moving the reader and catches up more than one page of new messages", async () => {
  const messages = Array.from({ length: 100 }, (_, index) => message(index));
  const requests: URL[] = [];
  vi.stubGlobal("fetch", vi.fn(async (input, init) => {
    const url = new URL(String(input), "http://localhost");
    if (url.pathname.endsWith("/status")) return response({ sessionState: "idle" });
    requests.push(url);
    if (url.pathname.endsWith("/messages")) {
      const body = JSON.parse(String(init?.body));
      for (let index = 100; index < 145; index++) messages.push(message(index));
      messages.push({ id: body.id, role: "user", text: body.text, createdAt: "2026-10-31T00:00:00Z" });
    }
    return response(page(messages, url.searchParams));
  }));
  const { target, scroller } = setup();
  await vi.waitFor(() => expect(target.querySelectorAll("article")).toHaveLength(40));
  expect(target.textContent).toContain("Message 60");
  expect(target.textContent).not.toContain("Message 59");
  expect(requests[0].searchParams.get("limit")).toBe("40");
  await tick();
  scroller.scrollTop = 50;
  scroller.dispatchEvent(new Event("scroll"));
  await vi.waitFor(() => expect(target.querySelectorAll("article")).toHaveLength(80));
  await vi.waitFor(() => expect(scroller.scrollTop).toBe(4050));
  expect(target.textContent).toContain("Message 20");
  expect(target.textContent).not.toContain("Message 19");
  expect(requests.some(url => url.searchParams.get("before") === "m060")).toBe(true);

  const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
  input.value = "A new request";
  input.dispatchEvent(new Event("input", { bubbles: true }));
  await tick();
  target.querySelector<HTMLButtonElement>(".chat-send-button")!.click();
  await vi.waitFor(() => expect(target.querySelectorAll("article")).toHaveLength(126));
  expect(target.textContent).toContain("Message 100");
  expect(target.textContent).toContain("Message 144");
  expect(target.textContent).toContain("A new request");
  expect(requests.some(url => url.searchParams.get("after") === "m099")).toBe(true);
  expect(requests.some(url => url.searchParams.get("after") === "m139")).toBe(true);
  const rendered = [...target.querySelectorAll("article")].map(article => article.textContent);
  expect(rendered.filter(text => text?.includes("Message 100"))).toHaveLength(1);
}, 15000);

it("discards an older page when the selected user changes", async () => {
  const messages = Array.from({ length: 50 }, (_, index) => message(index));
  let releaseOlder: (value: Response) => void = () => {};
  let olderRequested = false;
  vi.stubGlobal("fetch", vi.fn(async (input, init) => {
    const url = new URL(String(input), "http://localhost");
    if (url.pathname.endsWith("/status")) return response({ sessionState: "idle" });
    const user = new Headers(init?.headers).get("X-PUA-User");
    if (user === "other") return response({ ...page([message(999)], url.searchParams), id: "other" });
    if (url.searchParams.has("before")) {
      olderRequested = true;
      return await new Promise<Response>(resolve => { releaseOlder = resolve; });
    }
    return response(page(messages, url.searchParams));
  }));
  const { target, scroller, channel } = setup();
  await vi.waitFor(() => expect(target.querySelectorAll("article")).toHaveLength(40));
  scroller.scrollTop = 0;
  scroller.dispatchEvent(new Event("scroll"));
  await vi.waitFor(() => expect(olderRequested).toBe(true));
  channel.publish({ ...channel.current(), currentUserName: "other" });
  await vi.waitFor(() => expect(target.textContent).toContain("Message 999"));
  releaseOlder(response(page(messages, new URLSearchParams("limit=40&before=m010"))));
  await tick();
  expect(target.textContent).not.toContain("Message 0");
  expect(target.querySelectorAll("article")).toHaveLength(1);
});
