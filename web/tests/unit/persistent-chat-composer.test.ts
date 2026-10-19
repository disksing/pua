import { mount, tick, unmount } from "svelte";
import { afterEach, expect, it, vi } from "vitest";
import PersistentChat from "../../src/components/PersistentChat.svelte";
import { createPUAAppChannels } from "../../src/app-channels";

class UploadXHR {
  static instances: UploadXHR[] = [];
  headers: Record<string, string> = {};
  events = new Map<string, (event: any) => void>();
  progress: (event: any) => void = () => {};
  upload = { addEventListener: (_: string, fn: (event: any) => void) => { this.progress = fn; } };
  response = {}; responseType = ""; status = 201; url = ""; aborted = false;
  constructor() { UploadXHR.instances.push(this); }
  open(_: string, url: string) { this.url = url; }
  setRequestHeader(key: string, value: string) { this.headers[key] = value; }
  addEventListener(key: string, fn: (event: any) => void) { this.events.set(key, fn); }
  send() {}
  abort() { this.aborted = true; }
  finish(path: string) { this.response = { path, name: path.split("/").at(-1) }; this.events.get("load")?.({}); }
}
const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => { while (cleanups.length) await cleanups.pop()?.(); vi.unstubAllGlobals(); document.body.replaceChildren(); UploadXHR.instances = []; });
function response(value: unknown) { return new Response(JSON.stringify(value), { headers: { "Content-Type": "application/json" } }); }
async function setup() {
  vi.stubGlobal("XMLHttpRequest", UploadXHR);
  vi.stubGlobal("fetch", vi.fn(async () => response({ id: "c", open: true, epoch: 1, unreadCount: 0, messages: [], sessionState: "idle" })));
  const channel = createPUAAppChannels().detail;
  const onSaveAgentBinding = vi.fn(async () => {});
  channel.publish({ ...channel.current(), identity: "w:project1.task1", workspaceId: "w", resourceId: "project1.task1", currentUserName: "disksing", resourceTitle: "Task 1",
    detail: { id: "project1.task1", type: "task", title: "Task 1", path: "project1/task1" },
    agentProfiles: [{ key: "default" }, { key: "review" }], onSaveAgentBinding });
  const target = document.body.appendChild(document.createElement("div"));
  const component = mount(PersistentChat, { target, props: { channel } }); cleanups.push(() => unmount(component));
  await vi.waitFor(() => expect(target.querySelector("#chatInput")).not.toBeNull());
  return { channel, target, onSaveAgentBinding };
}
async function openUpload(target: HTMLElement) {
  target.querySelector<HTMLButtonElement>('[aria-label="Upload files"]')!.click(); await tick();
  const input = target.querySelector<HTMLInputElement>("#agentUploadInput")!;
  Object.defineProperty(input, "files", { configurable: true, value: [new File(["content"], "notes (1).txt")] });
  input.dispatchEvent(new Event("change", { bubbles: true })); await tick();
  return UploadXHR.instances.at(-1)!;
}
it("restores upload progress and appends a scoped link without losing the draft", async () => {
  const { target } = await setup();
  const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
  input.value = "Review this"; input.dispatchEvent(new Event("input", { bubbles: true })); await tick();
  const request = await openUpload(target);
  expect(request.url).toBe("/api/workspaces/w/resources/project1.task1/uploads");
  expect(request.headers["X-PUA-User"]).toBe("disksing");
  request.progress({ lengthComputable: true, loaded: 5, total: 10 }); await tick();
  expect(target.querySelector('[role="progressbar"]')?.getAttribute("aria-valuenow")).toBe("50");
  request.finish("artifacts/upload/notes (1).txt"); await tick();
  [...target.querySelectorAll<HTMLButtonElement>("button")].find(b => b.textContent === "Done")!.click(); await tick();
  expect(input.value).toBe("Review this\n[notes (1).txt](/project1/task1/artifacts/upload/notes%20%281%29.txt)");
  expect(target.querySelector('[role="dialog"]')).toBeNull();
});
it.each(["user", "resource"])("aborts uploads and isolates the new %s draft", async (change) => {
  const { target, channel } = await setup();
  const request = await openUpload(target);
  channel.publish({ ...channel.current(), ...(change === "user" ? { currentUserName: "other" } : { identity: "w:project1.task2", resourceId: "project1.task2" }) });
  await tick();
  expect(request.aborted).toBe(true);
  request.finish("artifacts/upload/old.txt"); await tick();
  await vi.waitFor(() => expect(target.querySelector<HTMLTextAreaElement>("#chatInput")?.value).toBe(""));
  expect(target.querySelector('[role="dialog"]')).toBeNull();
});
it("saves the execution binding from the composer and surfaces failures", async () => {
  const { target, onSaveAgentBinding } = await setup();
  onSaveAgentBinding.mockRejectedValueOnce(new Error("binding unavailable"));
  target.querySelector<HTMLButtonElement>('[aria-label="Binding target"]')!.click(); await tick();
  [...target.querySelectorAll<HTMLButtonElement>('[role="option"]')].find(b => b.textContent?.includes("review"))!.click();
  await vi.waitFor(() => expect(onSaveAgentBinding).toHaveBeenCalledWith({ kind: "profile", name: "review" }));
  await vi.waitFor(() => expect(target.querySelector('[role="alert"]')?.textContent).toContain("binding unavailable"));
});
it("keeps newly attached files when an earlier send finishes", async () => {
  const { target } = await setup();
  let accept!: (value: Response) => void;
  vi.mocked(globalThis.fetch).mockImplementation(async (path) => String(path).endsWith("/chat/messages")
    ? await new Promise<Response>(resolve => { accept = resolve; })
    : response({ id: "c", open: true, epoch: 1, messages: [], sessionState: "idle" }));
  const input = target.querySelector<HTMLTextAreaElement>("#chatInput")!;
  input.value = "First request"; input.dispatchEvent(new Event("input", { bubbles: true })); await tick();
  target.querySelector<HTMLButtonElement>(".chat-send-button")!.click();
  await vi.waitFor(() => expect(accept).toBeTypeOf("function"));
  const upload = await openUpload(target); upload.finish("artifacts/upload/later.txt"); await tick();
  [...target.querySelectorAll<HTMLButtonElement>("button")].find(b => b.textContent === "Done")!.click(); await tick();
  accept(response({ id: "c", open: true, epoch: 1, messages: [] }));
  await vi.waitFor(() => expect(input.disabled).toBe(false));
  expect(input.value).toContain("[later.txt](/project1/task1/artifacts/upload/later.txt)");
});
