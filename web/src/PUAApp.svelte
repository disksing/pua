<script lang="ts">
  import AppShell from "./components/AppShell.svelte";
  import ConfirmDialog from "./components/ConfirmDialog.svelte";
  import CreateDialog from "./components/CreateDialog.svelte";
  import DetailPanel from "./components/DetailPanel.svelte";
  import ChatList from "./components/ChatList.svelte";
  import PersistentChat from "./components/PersistentChat.svelte";
  import SettingsModal from "./components/SettingsModal.svelte";
  import Toast from "./components/Toast.svelte";
  import UploadDialog from "./components/UploadDialog.svelte";
  import { confirmDialogChannel } from "./controllers/confirm-dialog-controller";
  import type { PUAAppChannels } from "./app-channels";

  let { channels }: { channels: PUAAppChannels } = $props();
</script>

<AppShell channel={channels.appShell}>
  {#snippet chats()}<ChatList channel={channels.detail} />{/snippet}
  {#snippet details()}<DetailPanel channel={channels.detail} />{/snippet}
  {#snippet timeline()}<PersistentChat channel={channels.detail} />{/snippet}
  {#snippet composer()}{/snippet}
  {#snippet agentHeader()}{/snippet}
</AppShell>
<div data-component-owner="toast" style="display: contents"><Toast channel={channels.toast} /></div>
<div data-component-owner="upload-dialog" style="display: contents"><UploadDialog channel={channels.upload} /></div>
<div data-component-owner="create-dialog" style="display: contents"><CreateDialog channel={channels.create} /></div>
<div data-component-owner="settings" style="display: contents"><SettingsModal channel={channels.settings} /></div>
<div data-component-owner="confirm-dialog" style="display: contents"><ConfirmDialog channel={confirmDialogChannel} /></div>
