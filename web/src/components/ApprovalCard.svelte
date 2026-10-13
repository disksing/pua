<script lang="ts">
  import "./ApprovalCard.css";

  import Icon from "./Icon.svelte";
  import type { EventTimelineModel, TimelineItem } from "./models";

  type ApprovalReply = Parameters<EventTimelineModel["onApproval"]>[2];
  let { item, generationId, contextIdentity, onApproval, onToast, readOnly = false }: {
    item: TimelineItem;
    generationId: string;
    contextIdentity: string;
    onApproval: EventTimelineModel["onApproval"];
    onToast: EventTimelineModel["onToast"];
    readOnly?: boolean;
  } = $props();
  let draft = $state("");
  let pending = $state(false);
  let localIdentity = $state(approvalIdentity());

  $effect(() => {
    const next = approvalIdentity();
    if (next === localIdentity) return;
    localIdentity = next;
    draft = "";
    pending = false;
  });

  function approvalIdentity(): string {
    return `${contextIdentity}:${String(item.approvalId || "")}`;
  }

  async function approve(reply: ApprovalReply): Promise<void> {
    const approvalId = String(item.approvalId || "");
    if (readOnly || !approvalId || pending) return;
    pending = true;
    try {
      await onApproval(generationId, approvalId, reply);
      draft = "";
    } catch (reason) {
      onToast(reason instanceof Error ? reason.message : String(reason));
    } finally {
      pending = false;
    }
  }

  function optionLabel(option: NonNullable<TimelineItem["options"]>[number]): string {
    return option.name || String(option.kind || "").replace(/[_-]+/g, " ").trim() || option.optionId;
  }
</script>

<div data-component-owner="event-timeline" class="agent-event approval">
  <div><Icon name="shield-question" /><strong>{item.title || "Approval requested"}</strong></div>
  {#if item.question}<p class="approval-question">{item.question}</p>{/if}
  {#if item.detail}<p>{item.detail}</p>{/if}
  {#if item.status === "pending"}
    {#if readOnly}
      <p>Pending{item.options?.length ? ` · ${item.options.map(optionLabel).join(" / ")}` : ""}</p>
    {:else if item.options?.length}
      <div class="approval-options">{#each item.options as option (option.optionId)}<button class="primary-button" class:secondary-button={String(option.kind || "").startsWith("reject")} disabled={pending} onclick={() => approve({ optionId: option.optionId })}>{optionLabel(option)}</button>{/each}</div>
    {:else}
      <div class="approval-actions"><button class="primary-button" disabled={pending} onclick={() => approve({ decision: "accept" })}><Icon name="check" /><span>Allow once</span></button><button class="secondary-button" disabled={pending} onclick={() => approve({ decision: "decline" })}><Icon name="x" /><span>Decline</span></button></div>
    {/if}
    {#if item.question && !readOnly}<form class="approval-reply" onsubmit={(event) => { event.preventDefault(); if (draft.trim()) void approve({ text: draft.trim() }); }}><input bind:value={draft} placeholder="Reply with a custom answer…" aria-label="Custom reply"><button type="submit" class="primary-button" disabled={!draft.trim() || pending}>Send</button></form>{/if}
  {:else}
    <p>{item.decision || (item.status === "accepted" ? "Allowed" : "Declined")}{item.reply ? `: ${item.reply}` : ""}</p>
  {/if}
</div>
