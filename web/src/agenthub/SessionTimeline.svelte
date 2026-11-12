<script lang="ts">
  import { onMount, tick } from "svelte";

  import ActivityGroup from "../components/ActivityGroup.svelte";
  import ApprovalCard from "../components/ApprovalCard.svelte";
  import TimelineMessage from "../components/TimelineMessage.svelte";
  import TimelineNotice from "../components/TimelineNotice.svelte";
  import UnknownEvent from "../components/UnknownEvent.svelte";
  import type { TimelineItem } from "../components/models";
  import { agentHubPath, api } from "./core/api";
  import { catchUpFrames, flattenFrames, mergeIncomingFrames, projectLiveFrame } from "./core/events";
  import { buildTimeline } from "./core/timeline";
  import type { AgentHubSession, SemanticFrame } from "./types";

  let { session, onSessionChanged = () => {} }: { session: AgentHubSession; onSessionChanged?: () => void } = $props();

  let frames = $state<SemanticFrame[]>([]);
  let loading = $state(true);
  let error = $state("");
  let container = $state<HTMLElement>();
  let timeline = $derived(buildTimeline(flattenFrames(frames)) as TimelineItem[]);

  onMount(() => {
    let source: EventSource | undefined;
    let disposed = false;
    let cursor = 0;
    let recovering = false;

    const project = (incoming: SemanticFrame[]) => {
      frames = mergeIncomingFrames(frames, incoming);
      void scrollToBottom();
    };
    const connect = () => {
      if (disposed) return;
      source = new EventSource(agentHubPath(`/v1/sessions/${session.id}/events?stream=true&after=${cursor}`));
      source.onmessage = async (message) => {
        if (disposed || recovering) return;
        try {
          const frame = JSON.parse(message.data) as SemanticFrame;
          if (frame.cursor > cursor + 1) {
            recovering = true;
            source?.close();
          }
          cursor = await projectLiveFrame({ sessionId: session.id, cursor, frame, project });
          if (frame.events.some((event) => /^(session|turn|approval)\./.test(event.type))) onSessionChanged();
          if (recovering) {
            recovering = false;
            connect();
          }
        } catch (reason) {
          error = reason instanceof Error ? reason.message : String(reason);
        }
      };
      source.onerror = () => {};
    };

    catchUpFrames(session.id)
      .then((history: { frames: SemanticFrame[]; cursor: number }) => {
        if (disposed) return;
        frames = history.frames;
        cursor = history.cursor;
        loading = false;
        void scrollToBottom();
        connect();
      })
      .catch((reason: unknown) => {
        if (disposed) return;
        loading = false;
        error = reason instanceof Error ? reason.message : String(reason);
      });

    return () => { disposed = true; source?.close(); };
  });

  async function scrollToBottom(): Promise<void> {
    await tick();
    if (container) container.scrollTop = container.scrollHeight;
  }

  async function resolveApproval(_generationId: string, approvalId: string, reply: { decision?: string; optionId?: string; text?: string }): Promise<void> {
    await api(`/v1/sessions/${session.id}/approvals/${approvalId}`, { method: "POST", body: JSON.stringify(reply) });
  }

  function toast(message: string): void {
    error = message;
  }
</script>

<div class="session-timeline" bind:this={container}>
  {#if error}<TimelineNotice title="Timeline error" text={error} error alert onDismiss={() => error = ""} />{/if}
  {#if loading}
    <div class="timeline-loading"><span class="spinner"></span><span>Loading durable Session history…</span></div>
  {:else if !timeline.length}
    <div class="timeline-empty"><strong>No conversation yet</strong><span>Send a message to begin a Turn.</span></div>
  {:else}
    {#each timeline as item, index (`${String(item.key ?? index)}:${item.kind}`)}
      {#if item.kind === "message"}
        <TimelineMessage {item} agentName={session.agentName || "Agent"} />
      {:else if item.kind === "activity"}
        <ActivityGroup {item} />
      {:else if item.kind === "approval"}
        <ApprovalCard {item} generationId={session.id} contextIdentity={session.id} onApproval={resolveApproval} onToast={toast} />
      {:else if item.kind === "error"}
        <TimelineNotice title="Provider error" text={item.text || "The provider reported an error."} error />
      {:else if item.kind === "lifecycle"}
        <div class={`timeline-lifecycle ${item.tone || "muted"}`}><span></span><strong>{item.text || "Session event"}</strong></div>
      {:else}
        <UnknownEvent {item} />
      {/if}
    {/each}
  {/if}
</div>
