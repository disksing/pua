import { mount, tick, unmount } from "svelte";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { EditorView } from "@codemirror/view";

import FilePreviewFullscreen from "../../src/components/FilePreviewFullscreen.svelte";
import { MemoryStorage } from "../fixtures/memory-storage";

const mounted: Array<ReturnType<typeof mount>> = [];

beforeEach(() => vi.stubGlobal("localStorage", new MemoryStorage()));

function setURL(params: Record<string, string>): void {
  const search = new URLSearchParams(params).toString();
  Object.defineProperty(window, "location", {
    configurable: true,
    value: {
      ...window.location,
      pathname: "/file",
      search: search ? `?${search}` : "",
      href: `/file?${search}`,
      assign: vi.fn(),
      replace: vi.fn(),
    },
  });
}

function fileFetchMock(requests: Array<{ method: string; url: string; body?: string }> = []) {
  return vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    requests.push({ method: init?.method || "GET", url, body: typeof init?.body === "string" ? init.body : undefined });
    if (init?.method === "PUT") {
      const content = init.body ? (JSON.parse(String(init.body)) as { content: string }).content : "";
      return new Response(JSON.stringify({ path: "project1/task1/task.md", name: "task.md", content, contentHash: "saved-hash" }), { headers: { "content-type": "application/json" } });
    }
    return new Response(JSON.stringify({ path: "project1/task1/task.md", name: "task.md", content: "# Doc\n\nOriginal.", contentHash: "doc-a" }), { headers: { "content-type": "application/json" } });
  });
}

function mountFullscreen(): HTMLElement {
  const target = document.createElement("div");
  document.body.append(target);
  mounted.push(mount(FilePreviewFullscreen, { target }));
  return target;
}

afterEach(async () => {
  while (mounted.length) await unmount(mounted.pop()!);
  document.body.replaceChildren();
  localStorage.clear();
  vi.restoreAllMocks();
  vi.unstubAllGlobals();
});

describe("FilePreviewFullscreen", () => {
  it("saves workspace chat.md through the scoped documents endpoint", async () => {
    setURL({ workspaceId: "ws", resourceId: "workspace", path: "chat.md", mode: "edit" });
    const requests: string[] = [];
    vi.stubGlobal("fetch", vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PUT") requests.push(String(input));
      return new Response(JSON.stringify({ path: "chat.md", name: "chat.md", content: "# Pending", contentHash: "chat-hash" }), { headers: { "content-type": "application/json" } });
    }));
    const target = mountFullscreen();
    await vi.waitFor(() => expect(target.querySelector(".cm-editor")).not.toBeNull());
    const view = EditorView.findFromDOM(target.querySelector<HTMLElement>(".cm-editor")!)!;
    view.dispatch({ changes: { from: view.state.doc.length, insert: "\nResult" } });
    await tick();
    Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Save")!.click();
    await vi.waitFor(() => expect(requests).toEqual(["/api/workspaces/ws/resources/workspace/documents?path=chat.md"]));
  });
  it("renders the file full screen from URL parameters without a modal backdrop", async () => {
    setURL({ workspaceId: "ws", resourceId: "project1.task1", section: "Files", path: "project1/task1/task.md", mode: "edit" });
    const fetch = fileFetchMock();
    vi.stubGlobal("fetch", fetch);
    const target = mountFullscreen();
    await vi.waitFor(() => expect(target.querySelector<HTMLElement>('[role="dialog"] .cm-editor')).not.toBeNull());
    const dialog = target.querySelector<HTMLElement>('[role="dialog"]')!;
    expect(dialog.querySelector(".file-modal-backdrop")).toBeNull();
    expect(dialog.parentElement?.classList.contains("fullscreen")).toBe(true);
    expect(dialog.textContent).not.toContain("Download");
    expect(dialog.textContent).not.toContain("Full screen");
    expect(Array.from(dialog.querySelectorAll("button")).some((button) => button.textContent?.trim() === "Preview")).toBe(true);
  });

  it("shows an error state when URL parameters are missing", async () => {
    setURL({});
    const fetch = vi.fn();
    vi.stubGlobal("fetch", fetch);
    const target = mountFullscreen();
    await vi.waitFor(() => expect(target.textContent).toContain("File preview unavailable"));
    expect(fetch).not.toHaveBeenCalled();
  });

  it("restores an unsaved draft and saves through the resource documents endpoint", async () => {
    localStorage.setItem("pua:file-preview-handoff", JSON.stringify({
      version: 1,
      workspaceId: "ws",
      resourceId: "project1.task1",
      section: "Files",
      path: "project1/task1/task.md",
      mode: "edit",
      baseline: "# Doc\n\nOriginal.",
      baselineHash: "doc-a",
      draft: "# Doc\n\nEdited draft.",
      annotations: [],
      savedAt: Date.now(),
    }));
    setURL({ workspaceId: "ws", resourceId: "project1.task1", section: "Files", path: "project1/task1/task.md", mode: "edit" });
    const requests: Array<{ method: string; url: string; body?: string }> = [];
    vi.stubGlobal("fetch", fileFetchMock(requests));
    const target = mountFullscreen();
    await vi.waitFor(() => expect(target.querySelector<HTMLElement>('[role="dialog"] .cm-editor')).not.toBeNull());
    expect(target.querySelector(".cm-content")?.textContent).toContain("Edited draft.");
    expect(localStorage.getItem("pua:file-preview-handoff")).toBeNull();

    const view = EditorView.findFromDOM(target.querySelector<HTMLElement>(".cm-editor")!)!;
    view.dispatch({ changes: { from: view.state.doc.length, insert: "\nAdded.\n" } });
    const save = Array.from(target.querySelectorAll<HTMLButtonElement>("button")).find((button) => button.textContent?.trim() === "Save")!;
    save.click();
    await vi.waitFor(() => expect(requests.some((request) => request.method === "PUT")).toBe(true));
    const put = requests.find((request) => request.method === "PUT")!;
    expect(put.url).toContain("/api/workspaces/ws/resources/project1.task1/documents?path=project1%2Ftask1%2Ftask.md");
    expect(JSON.parse(put.body || "{}").content).toContain("Added.");
  });

  it("restores annotations handed over from the dialog window", async () => {
    localStorage.setItem("pua:file-preview-handoff", JSON.stringify({
      version: 1,
      workspaceId: "ws",
      resourceId: "project1.task1",
      section: "Files",
      path: "project1/task1/task.md",
      mode: "annotate",
      baseline: "# Doc\n\nOriginal.",
      baselineHash: "doc-a",
      draft: "# Doc\n\nOriginal.",
      annotations: [{ id: "a1", from: 3, to: 6, quote: "Doc", comment: "Review note", stale: false }],
      savedAt: Date.now(),
    }));
    setURL({ workspaceId: "ws", resourceId: "project1.task1", section: "Files", path: "project1/task1/task.md", mode: "annotate" });
    vi.stubGlobal("fetch", fileFetchMock());
    const target = mountFullscreen();
    await vi.waitFor(() => expect(target.querySelector<HTMLElement>('[role="dialog"] .cm-editor')).not.toBeNull());
    const comment = target.querySelector<HTMLTextAreaElement>('textarea[data-comment-for="a1"]');
    expect(comment?.value).toBe("Review note");
    expect(Array.from(target.querySelectorAll("button")).some((button) => button.textContent?.trim() === "Add annotation")).toBe(true);
  });
});
