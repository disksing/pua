import { mount, tick, unmount } from "svelte";
import { afterEach, describe, expect, it, vi } from "vitest";

import ActivityPanel from "../../src/components/ActivityPanel.svelte";
import type { ShellActivityLists, ShellInboxMessage } from "../../src/models/shell";

const cleanups: Array<() => Promise<void>> = [];
afterEach(async () => {
  while (cleanups.length) await cleanups.pop()?.();
  document.body.replaceChildren();
});

function message(overrides: Partial<ShellInboxMessage> = {}): ShellInboxMessage {
  return {
    id: "msg-1",
    resourceId: "project1.task2",
    resourceTitle: "Task 2",
    senderName: "agent",
    text: "hello from agent",
    timeLabel: "just now",
    unread: true,
    replied: false,
    ...overrides,
  };
}

function activity(): ShellActivityLists {
  return { running: [], unread: [], problems: [] };
}

function mountPanel(overrides: Record<string, unknown> = {}) {
  const props = {
    activity: activity(),
    inbox: [message()],
    onSelect: vi.fn(async () => undefined),
    onOpenInboxMessage: vi.fn(async () => undefined),
    onReplyInboxMessage: vi.fn(async () => undefined),
    onDeleteInboxMessage: vi.fn(async () => undefined),
    onToast: vi.fn(),
    ...overrides,
  };
  const target = document.body.appendChild(document.createElement("div"));
  const component = mount(ActivityPanel, { target, props });
  cleanups.push(() => unmount(component));
  return { target, props };
}

async function openInboxTab(target: HTMLElement): Promise<void> {
  const tab = target.querySelector<HTMLButtonElement>('button[role="tab"][aria-controls="activity-panel-inbox"]')!;
  tab.click();
  await tick();
}

async function openReplyForm(target: HTMLElement): Promise<HTMLTextAreaElement> {
  const reply = target.querySelector<HTMLButtonElement>(".inbox-actions button:nth-child(2)")!;
  reply.click();
  await tick();
  return target.querySelector<HTMLTextAreaElement>(".inbox-reply-input")!;
}

describe("ActivityPanel inbox reply keyboard handling", () => {
  it("lets Space and Enter reach the reply textarea instead of opening the message", async () => {
    const { target, props } = mountPanel();
    await tick();
    await openInboxTab(target);
    const textarea = await openReplyForm(target);

    for (const key of [" ", "Enter"]) {
      const event = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
      textarea.dispatchEvent(event);
      await tick();
      expect(event.defaultPrevented).toBe(false);
    }
    expect(props.onOpenInboxMessage).not.toHaveBeenCalled();
  });

  it("still opens the message when Space or Enter is pressed on the focused row", async () => {
    const { target, props } = mountPanel();
    await tick();
    await openInboxTab(target);

    const row = target.querySelector<HTMLElement>(".inbox-row")!;
    for (const key of [" ", "Enter"]) {
      const event = new KeyboardEvent("keydown", { key, bubbles: true, cancelable: true });
      row.dispatchEvent(event);
      await tick();
      expect(event.defaultPrevented).toBe(true);
    }
    expect(props.onOpenInboxMessage).toHaveBeenCalledTimes(2);
  });

  it("keeps Space working on the focused action buttons inside the row", async () => {
    const { target, props } = mountPanel();
    await tick();
    await openInboxTab(target);

    const openButton = target.querySelector<HTMLButtonElement>(".inbox-actions button:nth-child(1)")!;
    const event = new KeyboardEvent("keydown", { key: " ", bubbles: true, cancelable: true });
    openButton.dispatchEvent(event);
    await tick();
    expect(event.defaultPrevented).toBe(false);
    expect(props.onOpenInboxMessage).not.toHaveBeenCalled();
  });
});
