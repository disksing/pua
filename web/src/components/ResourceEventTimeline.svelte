<script lang="ts">
  import { onMount } from "svelte";
  import { ApiClient } from "../api/client";
  import EventTimeline from "./EventTimeline.svelte";
  import { createModelChannel } from "./model-channel";
  import type { DetailPanelModel, EventTimelineModel, ResourceMessageStatus } from "./models";
  import { projectConversationEvents } from "./timeline-events";

  // The owner keys this component by workspace, resource, user and channel.
  // Each mount owns its controller, HTTP requests and native SSE connection.
  let { model, dialogue }: { model: DetailPanelModel; dialogue: boolean } = $props();
  // svelte-ignore state_referenced_locally
  const selected = model;
  // svelte-ignore state_referenced_locally
  const isDialogue = dialogue;
  const base = `/api/workspaces/${encodeURIComponent(selected.workspaceId)}/resources/${encodeURIComponent(selected.resourceId)}`;
  const runtime = isDialogue ? `${base}/chat/runtime` : base;
  const route = (path: string) => path.startsWith(`${base}/`) ? runtime + path.slice(base.length) : path;
  const api = new ApiClient((input, init) => {
    const headers = new Headers(init?.headers);
    headers.set("X-PUA-User", selected.currentUserName);
    return fetch(route(String(input)), { ...init, headers });
  });
  let disposed = false;
  let readThrough = 0;
  let desiredRead = 0;
  let reading = false;
  async function viewed(snapshot: import("./models").ChatContextSnapshot) {
    desiredRead = Math.max(desiredRead, snapshot.completedTurnNumber || 0);
    if (disposed || reading || desiredRead <= readThrough) return;
    reading = true;
    try {
      while (!disposed && desiredRead > readThrough) {
        const through = desiredRead;
        await api.latest(`${base}/read`, { scope: "read", method: "PUT", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ throughTurnNumber: through }) });
        if (disposed) return;
        readThrough = through;
        selected.onSessionRead?.();
      }
    } catch { /* A later history snapshot retries without clearing unread state. */ }
    finally { reading = false; }
  }
  const controllerOptions = {
    api,
    collapseNonUserTurns: false,
    trackReadState: true,
    eventSourceFactory: (path: string) => {
      const url = new URL(route(path), window.location.href);
      // EventSource has no custom headers. Only the dialogue GET stream
      // accepts this selected-user parameter, with the usual user validation.
      if (isDialogue) url.searchParams.set("user", selected.currentUserName);
      return new EventSource(url.pathname + url.search);
    },
  };
  const channel = createModelChannel<EventTimelineModel>({
    identity: `${selected.workspaceId}:${selected.resourceId}:${selected.currentUserName}:${isDialogue}`,
    workspaceId: selected.workspaceId, resourceId: selected.resourceId,
    status: null, submitting: false, agentName: "Agent",
    resolveResourceTitle: selected.resolveResourceTitle, onNavigate: selected.onNavigate,
    project: projectConversationEvents,
    // Events remain local to this pane; dialogue events must never update
    // the application's execution-channel status or public chat messages.
    onEvent: () => {}, onNotice: () => {}, onApproval: async () => {}, onToast: () => {},
  });
  onMount(() => {
    void api.latest<ResourceMessageStatus>(`${base}/status`, { scope: "initial-status" })
      .then(status => channel.publish({ ...channel.current(), status }))
      .catch(() => { /* The controller's status sync retries transient failures. */ });
    return () => { disposed = true; api.dispose(); };
  });
</script>

<EventTimeline {channel} {controllerOptions} onViewed={viewed} readOnly />
