import { puaRoutePath } from "../controllers/route-controller";

export type ResourceTitleResolver = (resourceId: string) => string | null;

export interface MarkdownRenderContext {
  workspaceId: string;
  resolveResourceTitle: ResourceTitleResolver;
}

interface PUAResourceToken {
  type: "puaResource";
  raw: string;
  resourceId: string;
}

interface ProtectedLinkToken {
  type: "puaProtectedLink";
  raw: string;
  tokens: unknown[];
}

interface MarkedLexerContext {
  lexer: {
    state: { inLink?: boolean; inRawBlock?: boolean };
    inlineTokens(source: string): unknown[];
  };
}

interface MarkedRendererContext {
  parser: {
    options: { puaMarkdownContext?: MarkdownRenderContext };
    parseInline(tokens: unknown[]): string;
  };
}

interface MarkedParser {
  use(extension: Record<string, unknown>): void;
  parse(source: string, options?: Record<string, unknown>): string;
}

const RESOURCE_ID_SOURCE = "[A-Za-z0-9][A-Za-z0-9._-]{0,159}";
const RESOURCE_REFERENCE = new RegExp(`^\\[\\[(${RESOURCE_ID_SOURCE})\\]\\]`);
let parserSource: NonNullable<Window["marked"]> | null = null;
let parserInstance: MarkedParser | null = null;

export function markdownHTML(content: string, context?: MarkdownRenderContext): string {
  if (!window.marked || !window.DOMPurify) return `<pre>${escapeHTML(content)}</pre>`;
  const parser = configuredParser();
  if (!parser) {
    window.marked.setOptions({ breaks: true, gfm: true });
    return window.DOMPurify.sanitize(window.marked.parse(String(content ?? "")));
  }
  return window.DOMPurify.sanitize(parser.parse(String(content ?? ""), {
    breaks: true,
    gfm: true,
    puaMarkdownContext: context,
  }));
}

interface MarkdownNavigationContext {
  resolveResourceTitle: ResourceTitleResolver;
  onNavigate: (resourceId: string) => void;
  onOpenFile?: (path: string) => void;
  documentPath?: string;
}

export function handleMarkdownResourceClick(event: MouseEvent, context: MarkdownNavigationContext): void {
  if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  if (!(event.target instanceof Element)) return;
  const root = event.currentTarget;
  if (!(root instanceof Node)) return;

  const resourceAnchor = event.target.closest<HTMLAnchorElement>("a[data-pua-resource-id]");
  if (resourceAnchor && root.contains(resourceAnchor) && (!resourceAnchor.target || resourceAnchor.target === "_self")) {
    const resourceId = resourceAnchor.dataset.puaResourceId || "";
    if (isSafeResourceId(resourceId) && context.resolveResourceTitle(resourceId)) {
      event.preventDefault();
      context.onNavigate(resourceId);
      return;
    }
  }

  if (!context.onOpenFile) return;
  const fileAnchor = event.target.closest<HTMLAnchorElement>("a[href]");
  if (!fileAnchor || !root.contains(fileAnchor) || (fileAnchor.target && fileAnchor.target !== "_self")) return;
  const href = fileAnchor.getAttribute("href") || "";
  const documentPath = context.documentPath;
  const path = href === "memory.md" && documentPath?.split("/").pop() === "spec.md"
    ? documentPath.replace(/spec\.md$/, "memory.md")
    : workspaceFileLinkPath(href);
  if (path == null) return;
  event.preventDefault();
  context.onOpenFile(path);
}

function workspaceFileLinkPath(href: string): string | null {
  if (!href.startsWith("/") || href.startsWith("//")) return null;
  if (href.startsWith("/w/") || href.startsWith("/api/")) return null;
  let path = href;
  try {
    path = decodeURIComponent(path);
  } catch (_) {
    // Keep the original href. The server remains responsible for validating
    // and resolving every local file candidate.
  }
  const withoutRoot = path.slice(1);
  if (!withoutRoot || withoutRoot === "." || withoutRoot === "..") return null;
  return path;
}

export function markdownResourceNavigation(node: HTMLElement, initialContext: MarkdownNavigationContext) {
  let context = initialContext;
  const handleClick = (event: MouseEvent) => handleMarkdownResourceClick(event, context);
  node.addEventListener("click", handleClick);
  return {
    update(nextContext: MarkdownNavigationContext) { context = nextContext; },
    destroy() { node.removeEventListener("click", handleClick); },
  };
}

function configuredParser(): MarkedParser | null {
  const marked = window.marked;
  if (!marked?.Marked) return null;
  if (parserInstance && parserSource === marked) return parserInstance;
  const parser = new marked.Marked() as MarkedParser;
  parser.use({
    extensions: [
      {
        name: "puaProtectedLink",
        level: "inline",
        tokenizer(this: MarkedLexerContext, source: string): ProtectedLinkToken | undefined {
          if (this.lexer.state.inLink || this.lexer.state.inRawBlock) return undefined;
          const protectedLink = protectLeadingInlineLink(source);
          if (!protectedLink) return undefined;
          return {
            type: "puaProtectedLink",
            raw: protectedLink.raw,
            tokens: this.lexer.inlineTokens(protectedLink.markdown),
          };
        },
        renderer(this: MarkedRendererContext, token: ProtectedLinkToken): string {
          return this.parser.parseInline(token.tokens);
        },
        childTokens: ["tokens"],
      },
      {
        name: "puaResource",
        level: "inline",
        start(source: string): number {
          return source.indexOf("[[");
        },
        tokenizer(this: MarkedLexerContext, source: string): PUAResourceToken | undefined {
          if (this.lexer.state.inLink || this.lexer.state.inRawBlock) return undefined;
          const match = RESOURCE_REFERENCE.exec(source);
          if (!match) return undefined;
          return { type: "puaResource", raw: match[0], resourceId: match[1] };
        },
        renderer(this: MarkedRendererContext, token: PUAResourceToken): string {
          const context = this.parser.options.puaMarkdownContext;
          const title = context?.resolveResourceTitle(token.resourceId);
          if (!context || !title) return escapeHTML(token.raw);
          const href = puaRoutePath(context.workspaceId, token.resourceId);
          if (!href) return escapeHTML(token.raw);
          return `<a class="pua-resource-reference" href="${escapeHTML(href)}" data-pua-resource-id="${escapeHTML(token.resourceId)}">${escapeHTML(title)}</a>`;
        },
      },
    ],
  });
  parserSource = marked;
  parserInstance = parser;
  return parser;
}

function protectLeadingInlineLink(source: string): { raw: string; markdown: string } | null {
  const labelStart = source.startsWith("![") ? 1 : source.startsWith("[") ? 0 : -1;
  if (labelStart < 0) return null;
  const labelEnd = matchingDelimiter(source, labelStart, "[", "]");
  if (labelEnd < 0 || source[labelEnd + 1] !== "(") return null;
  const linkEnd = matchingDelimiter(source, labelEnd + 1, "(", ")");
  if (linkEnd < 0) return null;
  const label = source.slice(labelStart + 1, labelEnd);
  const protectedLabel = protectReferencesInLabel(label);
  if (protectedLabel === label) return null;
  const raw = source.slice(0, linkEnd + 1);
  return {
    raw,
    markdown: `${source.slice(0, labelStart + 1)}${protectedLabel}${source.slice(labelEnd, linkEnd + 1)}`,
  };
}

function matchingDelimiter(source: string, start: number, open: string, close: string): number {
  let depth = 0;
  let quote = "";
  for (let index = start; index < source.length; index++) {
    const character = source[index];
    if (character === "\n") return -1;
    if (character === "\\") { index++; continue; }
    if (quote) {
      if (character === quote) quote = "";
      continue;
    }
    if (open === "(" && depth === 1 && (character === '"' || character === "'")) {
      quote = character;
      continue;
    }
    if (character === "`") {
      const run = delimiterRun(source, index, "`");
      const end = source.indexOf("`".repeat(run), index + run);
      if (end >= 0) { index = end + run - 1; continue; }
    }
    if (character === open) depth++;
    else if (character === close && --depth === 0) return index;
  }
  return -1;
}

function protectReferencesInLabel(label: string): string {
  let result = "";
  let index = 0;
  while (index < label.length) {
    if (label[index] === "\\") {
      result += label.slice(index, index + 2);
      index += 2;
      continue;
    }
    if (label[index] === "`") {
      const run = delimiterRun(label, index, "`");
      const end = label.indexOf("`".repeat(run), index + run);
      if (end >= 0) {
        result += label.slice(index, end + run);
        index = end + run;
        continue;
      }
    }
    const match = RESOURCE_REFERENCE.exec(label.slice(index));
    if (match) {
      result += `\\[\\[${match[1]}\\]\\]`;
      index += match[0].length;
      continue;
    }
    result += label[index++];
  }
  return result;
}

function delimiterRun(source: string, start: number, delimiter: string): number {
  let length = 0;
  while (source[start + length] === delimiter) length++;
  return length;
}

function isSafeResourceId(value: string): boolean {
  return new RegExp(`^${RESOURCE_ID_SOURCE}$`).test(value);
}

function escapeHTML(value: string): string {
  return String(value ?? "").replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;").replaceAll('"', "&quot;").replaceAll("'", "&#39;");
}
