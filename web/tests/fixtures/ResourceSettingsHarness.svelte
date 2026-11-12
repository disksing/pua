<script lang="ts">
  import { onMount } from "svelte";
  import ResourceSettingsPanel from "../../src/components/ResourceSettingsPanel.svelte";
  import type { DetailPanelModel } from "../../src/components/models";
  import type { ModelChannel } from "../../src/components/model-channel";
  let { channel }: { channel: ModelChannel<DetailPanelModel> } = $props();
  // svelte-ignore state_referenced_locally
  let model = $state(channel.current());
  onMount(() => channel.subscribe(next => model = next));
</script>
<ResourceSettingsPanel {model} />
