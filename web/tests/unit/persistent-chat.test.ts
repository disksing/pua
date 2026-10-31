import { mount, tick, unmount } from "svelte";
import { afterEach, expect, it, vi } from "vitest";
import PersistentChat from "../../src/components/PersistentChat.svelte";
import { createPUAAppChannels } from "../../src/app-channels";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { while (cleanups.length) await cleanups.pop()?.(); vi.unstubAllGlobals(); document.body.replaceChildren(); });
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
 button(target, "Close chat").click(); await vi.waitFor(() => expect(target.querySelector("textarea")).toBeNull());
 expect(target.textContent).toContain("Public reply");
 button(target, "Reopen chat").click(); await vi.waitFor(() => expect(target.querySelector("textarea")).not.toBeNull());
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
 channel.publish({ ...channel.current(), currentUserName: "other" });
 await vi.waitFor(() => expect(users).toContain("other"));
 await vi.waitFor(() => expect(target.querySelectorAll("article")).toHaveLength(2));
 expect(target.querySelectorAll("article")[0].textContent).toContain("disksing");
 expect(target.querySelectorAll("article")[1].textContent).toContain("other");
 expect(target.textContent).toContain("First input");
 expect(target.textContent).toContain("Second input");
});

it("keeps approval content outside the fixed heading and permits a response", async () => {
 let approvalBody: unknown;
 vi.stubGlobal("fetch", vi.fn(async (input, init) => {
  const path = pathname(input);
  if (path.endsWith("/status")) return response({ sessionState: "attention_required", generation: { generationId: "gen" }, session: { pendingApprovalIds: ["approval-1"] } });
  if (path.endsWith("/events")) return response({ events: [{ id: 1, type: "approval.requested", data: { approvalId: "approval-1", title: "Approval requested", question: "Continue?" } }] });
  if (path.endsWith("/approval")) { approvalBody = JSON.parse(String(init?.body)); return response({}); }
  return response({ id: "c", open: true, epoch: 1, unreadCount: 0, messages: [] });
 }));
 const { target } = setup();
 await vi.waitFor(() => expect(target.querySelector(".approval")).not.toBeNull());
 expect(target.querySelector(".chat-heading .approval")).toBeNull();
 expect(target.querySelector(".chat-heading .session-controls")?.textContent).toContain("attention_required");
 button(target, "Allow once").click();
 await vi.waitFor(() => expect(approvalBody).toEqual({ requestId: "approval-1", decision: "accept" }));
});
