<script lang="ts">
  import ServicesPanel from "../../src/components/ServicesPanel.svelte";

  let { initialWorkspaceId, onToast }: { initialWorkspaceId: string; onToast: (message: string) => void } = $props();
  // svelte-ignore state_referenced_locally
  let workspaceId = $state(initialWorkspaceId);

  export function switchWorkspace(nextWorkspaceId: string): void {
    workspaceId = nextWorkspaceId;
  }
</script>

<ServicesPanel {workspaceId} {onToast} />
