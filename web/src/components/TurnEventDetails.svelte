<script lang="ts">
  import "./EventTimeline.css";
  import ActivityGroup from "./ActivityGroup.svelte";
  import ApprovalCard from "./ApprovalCard.svelte";
  import LifecycleNotice from "./LifecycleNotice.svelte";
  import TimelineMessage from "./TimelineMessage.svelte";
  import TimelineNotice from "./TimelineNotice.svelte";
  import UnknownEvent from "./UnknownEvent.svelte";
  import { formatClock, groupTimelineActivities, markTurnAgentRuns, markTurnFinalAssistant } from "./timeline-events";
  import type { ConversationBlock, EventTimelineModel, TimelineItem } from "./models";
  import type { ResourceTitleResolver } from "./markdown";

  let { block, project, presentItem, agentName = "Agent", workspaceId, resolveResourceTitle, onNavigate, onOpenFile, onExpand, readOnly, contextIdentity, onApproval, onToast }: {
    block: ConversationBlock;
    project: EventTimelineModel["project"];
    presentItem?: (item: TimelineItem) => TimelineItem | undefined;
    agentName?: string;
    workspaceId: string;
    resolveResourceTitle: ResourceTitleResolver;
    onNavigate: (resourceId: string) => void;
    onOpenFile: (path: string) => void;
    onExpand: (item: TimelineItem) => Promise<void> | undefined;
    readOnly: boolean;
    contextIdentity: string;
    onApproval: EventTimelineModel["onApproval"];
    onToast: EventTimelineModel["onToast"];
  } = $props();

  const name = $derived(block.generation.agentName || block.generation.resolvedProfile || block.generation.binding?.name || agentName);
  const items = $derived.by(() => {
    const raw = block.events
      ? project(block.events).map(item => ({ ...item, generationId: block.generation.generationId }))
      : block.items || [];
    const marked = markTurnFinalAssistant(raw);
    return markTurnAgentRuns(groupTimelineActivities(presentItem ? marked.flatMap(item => presentItem(item) || []) : marked));
  });

  function timelineKey(item: TimelineItem): string {
    const key = item.kind === "activity" && item.rangeStartEventId
      ? String(item.rangeStartEventId)
      : String(item.key ?? item.approvalId ?? item.time ?? item.type ?? "event");
    return `${item.generationId || block.generation.generationId}:${item.kind}:${key}`;
  }
</script>

{#each items as item (timelineKey(item))}
  <div data-timeline-key={timelineKey(item)}>
    {#if item.agentStart}
      <div data-component-owner="event-timeline" class="agent-run-header"><strong>{name}</strong>{#if formatClock(item.time)}<span>{formatClock(item.time)}</span>{/if}</div>
    {/if}
    {#if item.kind === "message"}
      <TimelineMessage {item} agentName={name} {workspaceId} {resolveResourceTitle} {onNavigate} {onOpenFile} />
    {:else if item.kind === "activity"}
      <ActivityGroup {item} onExpand={() => onExpand(item)} />
    {:else if item.kind === "approval"}
      <ApprovalCard {item} {readOnly} generationId={block.generation.generationId} {contextIdentity} {onApproval} {onToast} />
    {:else if item.kind === "lifecycle"}
      <LifecycleNotice {item} />
    {:else if item.kind === "error"}
      <TimelineNotice title="Provider error" text={item.text || ""} error />
    {:else}
      <UnknownEvent {item} />
    {/if}
  </div>
{/each}
