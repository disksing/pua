import { readFileSync } from "node:fs";

import { mount, tick, unmount } from "svelte";
import { afterEach, expect, it, vi } from "vitest";
import PersistentChat from "../../src/components/PersistentChat.svelte";
import { createPUAAppChannels } from "../../src/app-channels";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { while (cleanups.length) await cleanups.pop()?.(); vi.unstubAllGlobals(); delete window.marked; delete window.DOMPurify; document.body.replaceChildren(); });

function loadVendor<T>(relativePath: string, globalName: string): T {
 const source = readFileSync(new URL(relativePath, import.meta.url), "utf8");
 return new Function("window", "globalThis", `const module=undefined,exports=undefined,define=undefined;${source}\nreturn globalThis[${JSON.stringify(globalName)}];`)(window, window) as T;
}
function installMarkdownVendors(): void {
 window.marked = loadVendor<NonNullable<Window["marked"]>>("../../static/vendor/marked/marked.min.js", "marked");
 window.DOMPurify = loadVendor<NonNullable<Window["DOMPurify"]>>("../../static/vendor/dompurify/purify.min.js", "DOMPurify");
}
function response(value: unknown, status = 200) { return new Response(JSON.stringify(value), { status, headers: { "Content-Type": "application/json" } }); }
function pathname(input: unknown) { return String(input).split("?")[0]; }
function setup() {
 const channel = createPUAAppChannels().detail;
 channel.publish({ ...channel.current(), identity: "w:project1.task1", workspaceId: "w", resourceId: "project1.task1", currentUserName: "disksing", resourceTitle: "Task 1" });
 const target = document.body.appendChild(document.createElement("div"));
 const component = mount(PersistentChat, { target, props: { channel } });
 cleanups.push(() => unmount(component));
 return { channel, target };
}
function button(target: HTMLElement, text: string) { const result = [...target.querySelectorAll("button")].find(b => b.textContent?.trim() === text); expect(result).toBeTruthy(); return result!; }
it("shows saved agent names and keeps unknown historical senders generic across binding changes", async () => {
 vi.stubGlobal("fetch", vi.fn(async (input) => {
  if (pathname(input).endsWith("/status")) return response({ sessionState: "idle", resolvedAgent: "Current Agent" });
  return response({ id: "shared", open: true, epoch: 1, unreadCount: 0, messages: [
   { id: "a", role: "agent", sender: "Original Agent", text: "Original reply", createdAt: "2026-11-17T00:00:00Z" },
   { id: "b", role: "agent", sender: "reasoning", text: "Profile reply", createdAt: "2026-11-17T00:00:01Z" },
   { id: "c", role: "agent", text: "Legacy reply", createdAt: "2026-11-17T00:00:02Z" },
   { id: "d", role: "agent", sender: "  ", text: "Unnamed reply", createdAt: "2026-11-17T00:00:03Z" },
   { id: "e", role: "user", sender: "Other User", text: "User input", createdAt: "2026-11-17T00:00:04Z" },
   { id: "f", role: "user", text: "Old input", createdAt: "2026-11-17T00:00:05Z" },
  ] });
 }));
 const { channel, target } = setup();
 const headings = () => [...target.querySelectorAll(".message-meta strong")].map(element => element.textContent);
 await vi.waitFor(() => expect(headings()).toEqual(["Original Agent", "reasoning", "Agent", "Agent", "Other User", "User"]));
 channel.publish({ ...channel.current(), currentUserName: "other", agentBinding: { kind: "agent", name: "Next Agent" } });
 await tick();
 await vi.waitFor(() => expect(headings()).toEqual(["Original Agent", "reasoning", "Agent", "Agent", "Other User", "User"]));
});
it("shows waiting previews separately from shared Chat messages", async () => {
 vi.stubGlobal("fetch", vi.fn(async (input) => {
  if (pathname(input).endsWith("/status")) return response({ sessionState: "working", waitingMessages: [
   { messageId: "m", role: "agent", sender: { id: "project1" }, text: "background request" },
   { messageId: "n", type: "scheduler_tick", text: "scheduled request" },
  ] });
  if (pathname(input).endsWith("/history/turns")) return response({ segments: [], page: { hasMore: false } });
  return response({ id: "c", open: true, epoch: 1, messages: [] });
 }));
 const { target } = setup();
 await vi.waitFor(() => expect(target.querySelector(".queued-inputs")?.textContent).toContain("Waiting · 2"));
 expect([...target.querySelectorAll(".queued-inputs button")].map(button => button.getAttribute("aria-label"))).toEqual(["Waiting · Agent · project1", "Waiting · Scheduler"]);
 expect(target.querySelector(".queued-inputs [role=tooltip]")?.textContent).toContain("background request");
 expect(target.querySelector("article, .chat-input [role=tooltip]")).toBeNull();
});
it("keeps archived chat history visible without input or execution controls", async () => {
 const fetch = vi.fn(async (input) => {
  if (pathname(input).endsWith("/status")) return response({ sessionState: "working" });
  return response({ id: "c", open: true, epoch: 1, messages: [
   { id: "m", role: "agent", text: "Archived reply", createdAt: "2026-10-13T00:00:00Z" },
  ] });
 });
 vi.stubGlobal("fetch", fetch);
 const { channel, target } = setup();
 channel.publish({
  ...channel.current(),
  detail: { id: "project1.task1", type: "task", title: "Task 1", path: "project1/archive/task1", archived: true },
 });
 await vi.waitFor(() => expect(target.querySelector(".chat-closed")).not.toBeNull());
 expect(target.querySelector(".chat-closed")?.textContent).toContain("Archived · Read only");
 expect(target.querySelector("article")?.textContent).toContain("Archived reply");
 expect(target.querySelector("textarea, .public-chat-execution, [aria-label='Chat options']")).toBeNull();
 expect(fetch.mock.calls.some(([input]) => pathname(input).endsWith("/status"))).toBe(false);
});
it("keeps history on close and reopen and retains drafts across parent refreshes", async () => {
 let chat = { id: "c", open: true, epoch: 1, messages: [{ id: "m", role: "agent", text: "Public reply", createdAt: "2026-10-13T00:00:00Z" }] };
 vi.stubGlobal("fetch", vi.fn(async (input, init) => {
  const url = pathname(input);
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
 target.querySelector<HTMLButtonElement>('[aria-label="Chat options"]')!.click();
 await tick();
 button(target, "Close chat").click(); await vi.waitFor(() => expect(target.querySelector("textarea")).toBeNull());
 await vi.waitFor(() => expect(document.activeElement).toBe(button(target, "Reopen chat")));
 expect(target.textContent).toContain("Public reply");
 button(target, "Reopen chat").click(); await vi.waitFor(() => expect(target.querySelector("textarea")).not.toBeNull());
 await vi.waitFor(() => expect(document.activeElement).toBe(target.querySelector("textarea")));
 expect(target.textContent).toContain("Public reply");
});
it("retries an uncertain send with the same id and never uses the execution mailbox", async () => {
 const ids: string[] = [];
 const fetch = vi.fn(async (input, init) => {
  const path = pathname(input);
  if (path.endsWith("/status")) return response({ sessionState: "idle" });
  if (path.endsWith("/messages")) { const body = JSON.parse(String(init?.body)); ids.push(body.id); return response({ error: "temporarily unavailable" }, 503); }
  return response({ id: "c", open: true, epoch: 1, messages: [] });
 });
 vi.stubGlobal("fetch", fetch);
 const { target } = setup();
 await vi.waitFor(() => expect(target.querySelector("textarea")).not.toBeNull());
 const input = target.querySelector("textarea")!; input.value = "please investigate"; input.dispatchEvent(new Event("input", { bubbles: true })); await tick();
 target.querySelector<HTMLButtonElement>(".chat-send-button")!.click(); await vi.waitFor(() => expect(target.querySelector('[role="alert"]')).not.toBeNull());
 target.querySelector<HTMLButtonElement>(".chat-send-button")!.click(); await vi.waitFor(() => expect(ids).toHaveLength(2)); expect(ids[0]).toBe(ids[1]);
 expect(fetch.mock.calls.filter(([path]) => pathname(path).endsWith("/messages")).every(([path]) => pathname(path).includes("/chat/messages"))).toBe(true);
});

it("publishes chat counts and marks public messages read without touching either event cursor",async()=>{
 const reads:string[]=[];
 let unreadCount=2;
 vi.stubGlobal('fetch',vi.fn(async(input,init)=>{
  const path=pathname(input);
  if(path.endsWith('/status'))return response({sessionState:'idle'});
  if(init?.method==='POST'){reads.push(path);unreadCount=0;}
  return response({id:'c',open:true,epoch:1,unreadCount,messages:[{id:'last',role:'agent',text:'Public unread',createdAt:'2026-10-14T00:00:00Z'}]});
 }));
 const {channel}=setup();const onChatState=vi.fn();
 channel.publish({...channel.current(),onChatState});
 await vi.waitFor(()=>expect(onChatState).toHaveBeenLastCalledWith({open:true,unreadCount:0}));
 expect(onChatState).toHaveBeenCalledWith({open:true,unreadCount:2});
 expect(reads).toEqual(['/api/workspaces/w/resources/project1.task1/chat/read']);
});

it("ignores a pending old user's chat read after switching user",async()=>{
 let release: (value:Response)=>void=()=>{};
 const pendingRead=new Promise<Response>(resolve=>release=resolve);
 const fetch=vi.fn(async(input,init)=>{
  const path=pathname(input),user=new Headers(init?.headers).get('X-PUA-User');
  if(path.endsWith('/status'))return response({sessionState:'idle'});
  if(path.endsWith('/read'))return pendingRead;
  return response({id:user,open:user==='disksing',epoch:1,unreadCount:user==='disksing'?1:0,messages:user==='disksing'?[{id:'old',role:'agent',text:'old user history',createdAt:'2026-10-14T00:00:00Z'}]:[]});
 });
 vi.stubGlobal('fetch',fetch);
 const {channel,target}=setup();const onChatState=vi.fn();
 channel.publish({...channel.current(),onChatState});
 await vi.waitFor(()=>expect(fetch.mock.calls.some(([path])=>pathname(path).endsWith('/read'))).toBe(true));
 channel.publish({...channel.current(),currentUserName:'other'});
 await vi.waitFor(()=>expect(onChatState).toHaveBeenLastCalledWith({open:false,unreadCount:0}));
 const count=onChatState.mock.calls.length;
 release(response({id:'disksing',open:true,unreadCount:0,messages:[]}));await tick();
 expect(onChatState).toHaveBeenCalledTimes(count);
 expect(target.textContent).not.toContain('old user history');
});

it("shows the same chat and actual senders after switching user", async () => {
 const users: string[] = [];
 vi.stubGlobal("fetch", vi.fn(async (input, init) => {
  if (pathname(input).endsWith("/status")) return response({ sessionState: "idle" });
  users.push(new Headers(init?.headers).get("X-PUA-User") || "");
  return response({ id: "shared", open: true, epoch: 1, unreadCount: 0, messages: [
   { id: "a", role: "user", sender: "disksing", text: "First input", createdAt: "2026-10-14T00:00:00Z" },
   { id: "b", role: "user", sender: "other", text: "Second input", createdAt: "2026-10-14T00:00:01Z" },
  ] });
 }));
 const { channel, target } = setup();
 await vi.waitFor(() => expect(target.querySelectorAll("article")).toHaveLength(2));
 expect(target.querySelectorAll("article")[0].textContent).toContain("disksing");
 expect(target.querySelectorAll("article")[1].textContent).toContain("other");
 expect(target.querySelectorAll("article.own-message")).toHaveLength(1);
 expect(target.querySelector("article.own-message")?.textContent).toContain("First input");
 channel.publish({ ...channel.current(), currentUserName: "other" });
 await vi.waitFor(() => expect(users).toContain("other"));
 await vi.waitFor(() => expect(target.querySelectorAll("article")).toHaveLength(2));
 expect(target.querySelectorAll("article")[0].textContent).toContain("disksing");
 expect(target.querySelectorAll("article")[1].textContent).toContain("other");
 expect(target.textContent).toContain("First input");
 expect(target.textContent).toContain("Second input");
 expect(target.querySelectorAll("article.own-message")).toHaveLength(1);
 expect(target.querySelector("article.own-message")?.textContent).toContain("Second input");
});

it("finds older pending approvals and focuses the card from the dock", async () => {
 let approvalBody: unknown;
 const eventRequests: URL[] = [];
 vi.stubGlobal("fetch", vi.fn(async (input, init) => {
  const path = pathname(input);
  if (path.endsWith("/status")) return response({ sessionState: "attention_required", generation: { generationId: "gen" }, session: { id: "hub", state: "waiting_approval", currentTurnId: "turn", pendingApprovalIds: ["approval-1", "approval-1"] } });
  if (path.endsWith("/events")) {
    const url = new URL(String(input), "http://localhost");
    eventRequests.push(url);
    if (url.searchParams.has("latest")) return response({ frames: [], page: { hasMoreBefore: true, nextBefore: 100 } });
    const data = { approvalId: "approval-1", title: "Approval requested", question: "Continue?" };
    return response({ frames: [{
     schema: "agenthub.semantic-events.v1", cursor: 1, mode: "replace",
     events: [{ id: "sem_1_0", sourceEventId: 1, index: 0, type: "approval.requested", data }],
    }], page: { hasMoreBefore: false } });
  }
  if (path.endsWith("/approval")) { approvalBody = JSON.parse(String(init?.body)); return response({}); }
  return response({ id: "c", open: true, epoch: 1, unreadCount: 0, messages: [] });
 }));
 const { target } = setup();
 await vi.waitFor(() => expect(target.querySelector(".approval")).not.toBeNull());
 expect(eventRequests.map(url => url.searchParams.get("before"))).toEqual([null, "100"]);
 expect(target.querySelectorAll(".public-chat-approvals .approval")).toHaveLength(1);
 expect(target.querySelector(".public-chat-execution")?.textContent).toContain("Waiting for approval");
 expect(target.querySelector(".turn-working-indicator")).toBeNull();
 expect(button(target, "Stop execution").disabled).toBe(false);
 button(target, "View pending approval").click();
 expect(document.activeElement).toBe(target.querySelector(".public-chat-approvals"));
 button(target, "Allow once").click();
 await vi.waitFor(() => expect(approvalBody).toEqual({ requestId: "approval-1", decision: "accept" }));
 await vi.waitFor(() => expect(target.querySelector(".approval")).toBeNull());
});

it("stops a live Turn from closed Chat and preserves partial-success feedback", async () => {
 const stopRequests: URL[] = [];
 let stopped = false;
 vi.stubGlobal("fetch", vi.fn(async (input) => {
  const url = new URL(String(input), "http://localhost");
  if (url.pathname.endsWith("/status")) {
   return response(stopped ? { sessionState: "idle" } : {
    sessionState: "working", generation: { generationId: "gen" }, session: { id: "hub", state: "running", currentTurnId: "turn" },
   });
  }
  if (url.pathname.endsWith("/turn/end")) {
   stopRequests.push(url);
   stopped = true;
   return response({ status: "interrupted", taskStateError: "write unavailable" });
  }
  return response({ id: "c", open: false, epoch: 2, messages: [] });
 }));
 const { target } = setup();
 await vi.waitFor(() => expect(target.querySelector(".turn-working-indicator")?.textContent).toContain("Agent is working"));
 expect(target.querySelector("textarea")).toBeNull();
 const stop = button(target, "Stop execution");
 stop.click();
 stop.click();
 await vi.waitFor(() => expect(target.querySelector(".public-chat-execution")).toBeNull());
 expect(stopRequests.map(url => url.searchParams.get("generationId"))).toEqual(["gen"]);
 expect(target.querySelector('[role="alert"]')?.textContent).toContain("Execution stopped. Could not pause the task. write unavailable");
 expect(button(target, "Reopen chat")).toBeTruthy();
});

it("keeps a failed close visible after Chat refresh and restores focus to its options", async () => {
 vi.stubGlobal("fetch", vi.fn(async input => {
  const path = pathname(input);
  if (path.endsWith("/status")) return response({ sessionState: "idle" });
  if (path.endsWith("/close")) return response({ error: "unavailable" }, 503);
  return response({ id: "c", open: true, epoch: 1, messages: [] });
 }));
 const { target } = setup();
 const trigger = await vi.waitFor(() => {
  const value = target.querySelector<HTMLButtonElement>('[aria-label="Chat options"]');
  expect(value).not.toBeNull();
  return value!;
 });
 trigger.click();
 await vi.waitFor(() => expect(document.activeElement).toBe(button(target, "Close chat")));
 button(target, "Close chat").dispatchEvent(new KeyboardEvent("keydown", { key: "Escape", bubbles: true }));
 await vi.waitFor(() => expect(document.activeElement).toBe(trigger));
 expect(trigger.getAttribute("aria-expanded")).toBe("false");
 trigger.click();
 await tick();
 button(target, "Close chat").click();
 await vi.waitFor(() => expect(target.querySelector('[role="alert"]')?.textContent).toContain("Could not close chat. unavailable"));
 expect(target.querySelector(".public-chat-execution")).toBeNull();
 await vi.waitFor(() => expect(document.activeElement).toBe(trigger));
 expect(target.querySelector("textarea")).not.toBeNull();
});

it("opens a workspace file link from a chat message in a read-only preview", async () => {
 installMarkdownVendors();
 const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
  const path = String(input);
  if (path.includes("/files?path=")) return response({
   path: "project1-forge/task580/artifacts/report.md", name: "report.md", size: 18,
   binary: false, image: false, truncated: false, mimeType: "text/markdown", contentHash: "hash", content: "# Previewed report",
  });
  if (path.endsWith("/status")) return response({ sessionState: "idle" });
  return response({ id: "c", open: true, epoch: 1, unreadCount: 0, messages: [
   { id: "m", role: "agent", text: "[report](/project1-forge/task580/artifacts/report.md)", createdAt: "2026-11-12T00:00:00Z" },
  ] });
 });
 vi.stubGlobal("fetch", fetchMock);
 const { channel, target } = setup();

 const link = await vi.waitFor(() => {
  const value = target.querySelector<HTMLAnchorElement>("a[href='/project1-forge/task580/artifacts/report.md']");
  expect(value).not.toBeNull();
  return value!;
 });
 const routeBefore = window.location.pathname;
 link.click();

 await vi.waitFor(() => expect(target.querySelector("[role='dialog'][aria-label='File preview']")?.textContent).toContain("Previewed report"));
 expect(window.location.pathname).toBe(routeBefore);
 expect(fetchMock.mock.calls.map(([input]) => String(input))).toContain("/api/workspaces/w/files?path=%2Fproject1-forge%2Ftask580%2Fartifacts%2Freport.md");
 expect(target.querySelector("[role='dialog'] .markdown-editor-shell")).toBeNull();
});
