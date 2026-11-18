<script lang="ts">
  import "./MarkdownDocument.css";

  import { isMarkdownFile } from "./detail";
  import { markdownHTML, markdownResourceNavigation, type ResourceTitleResolver } from "./markdown";
  import type { ResourceFileModel } from "./models";
  import Icon from "./Icon.svelte";

  let { file, workspaceId, editable = false, resolveResourceTitle, onNavigate, onOpenFile, onEdit, onAnnotate }: { file: ResourceFileModel; workspaceId: string; editable?: boolean; resolveResourceTitle: ResourceTitleResolver; onNavigate: (resourceId: string) => void; onOpenFile?: (path: string) => void; onEdit?: (path: string) => void; onAnnotate?: (path: string) => void } = $props();
  const markdown = $derived(isMarkdownFile(file.name));
  const navigation = $derived({ resolveResourceTitle, onNavigate, onOpenFile, documentPath: file.path || file.name });
</script>

<div class="content-section" data-component-owner="markdown-document" data-doc-file={file.name} data-document-identity={`${workspaceId}:${file.path || file.name}:preview:${file.contentHash || "unversioned"}`}>
  {#if markdown}<div class="markdown-preview"><div class="markdown-document-actions">{#if editable && onEdit}<button type="button" class="secondary-button" onclick={() => onEdit(file.path || file.name)}><Icon name="pencil" /><span>Edit</span></button>{/if}{#if editable && onAnnotate}<button type="button" class="secondary-button" onclick={() => onAnnotate(file.path || file.name)}><Icon name="message-square-plus" /><span>Annotate</span></button>{/if}</div><div class="markdown-view markdown-rendered" use:markdownResourceNavigation={navigation}>{@html markdownHTML(file.content || "", { workspaceId, resolveResourceTitle })}</div></div>
  {:else}<pre class="markdown-view">{file.content || ""}</pre>{/if}
</div>
