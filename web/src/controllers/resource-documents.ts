export const RESOURCE_DOCUMENTS = [
  { id: "spec", name: "spec.md", label: "Spec", icon: "file-text" },
  { id: "memory", name: "memory.md", label: "Memory", icon: "book-open" },
] as const;

export function resourceDocumentSavePath(workspaceId: string, resourceId: string, path: string): string {
  const segments = path.split("/");
  const spec = segments.at(-1) === "spec.md" && !segments.includes("artifacts") && !segments.includes("templates");
  return `/api/workspaces/${encodeURIComponent(workspaceId)}/resources/${encodeURIComponent(resourceId)}/${spec ? "spec" : "documents"}?path=${encodeURIComponent(path)}`;
}
