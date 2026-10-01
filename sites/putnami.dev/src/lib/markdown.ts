import type { TocItem } from '@putnami/ui';
import type { Tokens } from 'marked';
import { marked } from 'marked';
import { createSlugger, MARKED_OPTIONS, parseInlineSync, toPlainTextFromHtml } from './markdown-utils';

export type Highlighter = (code: string, lang: string) => Promise<string> | string;

interface CodeGroupBlock {
  label: string;
  lang: string;
  code: string;
}

function escapeHtml(str: string): string {
  return str.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}

function escapeAttr(str: string): string {
  return escapeHtml(str).replace(/"/g, '&quot;');
}

const plainHighlighter: Highlighter = (code, lang) =>
  `<pre class="code-block"><code class="language-${lang}">${escapeHtml(code)}</code></pre>`;

/**
 * Extract :::code-group blocks from markdown, replacing them with placeholders.
 * Returns the cleaned markdown and the extracted code groups.
 */
function extractCodeGroups(markdown: string): { markdown: string; groups: Map<string, CodeGroupBlock[]> } {
  const groups = new Map<string, CodeGroupBlock[]>();
  let groupId = 0;

  const processed = markdown.replace(/^:::code-group\s*\n([\s\S]*?)^:::\s*$/gm, (_, content: string) => {
    const blocks: CodeGroupBlock[] = [];
    const blockPattern = /```(\w+)\s*\[([^\]]+)\]\s*\n([\s\S]*?)```/g;

    for (const match of content.matchAll(blockPattern)) {
      blocks.push({
        lang: match[1],
        label: match[2],
        code: match[3].trimEnd(),
      });
    }

    if (blocks.length === 0) return content;

    const id = `<!--CODE_GROUP_${groupId++}-->`;
    groups.set(id, blocks);
    return id;
  });

  return { markdown: processed, groups };
}

async function renderCodeGroup(blocks: CodeGroupBlock[], highlight: Highlighter): Promise<string> {
  const tabs = blocks
    .map(
      (block, i) =>
        `<button class="code-group-tab${i === 0 ? ' active' : ''}" data-index="${i}" data-label="${escapeAttr(block.label)}" type="button">${escapeAttr(block.label)}</button>`,
    )
    .join('');

  const panels = (
    await Promise.all(
      blocks.map(async (block, i) => {
        const highlighted = await highlight(block.code, block.lang);
        return `<div class="code-group-panel${i === 0 ? ' active' : ''}" data-index="${i}" data-label="${escapeAttr(block.label)}">${highlighted}</div>`;
      }),
    )
  ).join('');

  return `<div class="code-group"><div class="code-group-tabs">${tabs}</div><div class="code-group-panels">${panels}</div></div>`;
}

/**
 * Render markdown to HTML.
 *
 * Code blocks are highlighted via the supplied `highlight` function. The default
 * `plainHighlighter` emits unstyled `<pre><code>` blocks — the production build
 * pre-renders docs with a shiki-backed highlighter (see `markdown-build.ts`),
 * leaving this runtime path shiki-free.
 */
export async function renderMarkdown(markdown: string, highlight: Highlighter = plainHighlighter): Promise<string> {
  const slugger = createSlugger();
  const renderer = new marked.Renderer();

  const { markdown: cleanedMarkdown, groups: codeGroups } = extractCodeGroups(markdown);

  // `<!-- cols: 24 15 27 34 -->` on the line before a table declares relative
  // column weights (normalized to percentages, one per column). GFM tables
  // cannot express widths, and browser auto-layout sizes columns from their
  // content — a column of code chips crowds out a column of prose. The
  // directive is an HTML comment on purpose: GitHub, Copy as Markdown, and
  // llms.txt all show the same table untouched; only this renderer acts on it,
  // by emitting a <colgroup>.
  //
  // A directive that does not match the next table (wrong count, non-positive
  // weight) is dropped and the table falls back to auto layout — a cosmetic
  // hint must never break a build.
  let pendingCols: number[] | null = null;

  renderer.html = (token: Tokens.HTML | Tokens.Tag) => {
    const match = /^<!--\s*cols:\s*([\d\s.]+?)\s*-->$/.exec(token.text.trim());
    if (match) {
      pendingCols = match[1].split(/\s+/).map(Number);
      return '';
    }
    return token.text;
  };

  const renderTableBase = marked.Renderer.prototype.table;
  renderer.table = function (token: Tokens.Table) {
    const html = renderTableBase.call(this, token);
    const weights = pendingCols;
    pendingCols = null;
    if (!weights || weights.length !== token.header.length || weights.some((w) => !(w > 0))) return html;
    const total = weights.reduce((sum, w) => sum + w, 0);
    const cols = weights.map((w) => `<col style="width:${((w / total) * 100).toFixed(1)}%"/>`).join('');
    return html.replace('<table>', `<table><colgroup>${cols}</colgroup>`);
  };

  renderer.heading = (token: Tokens.Heading) => {
    const rendered = parseInlineSync(token.text);
    const id = slugger.slug(token.text, rendered);
    return `<h${token.depth} id="${id}">${rendered}</h${token.depth}>`;
  };

  const codeBlocks: { id: string; code: string; lang: string }[] = [];
  let codeBlockId = 0;

  renderer.code = (token: Tokens.Code | string, language?: string) => {
    const code = typeof token === 'string' ? token : token.text;
    const lang = sanitizeLanguage(typeof token === 'string' ? language : token.lang);

    if (lang === 'mermaid') {
      return `<div class="mermaid-diagram">${escapeHtml(code)}</div>`;
    }

    const id = `__CODE_BLOCK_${codeBlockId++}__`;
    codeBlocks.push({ id, code, lang });
    return id;
  };

  let html = marked.parse(cleanedMarkdown, { ...MARKED_OPTIONS, renderer }) as string;

  const renderedGroups = await Promise.all(
    [...codeGroups].map(async ([id, blocks]) => [id, await renderCodeGroup(blocks, highlight)] as const),
  );
  for (const [id, rendered] of renderedGroups) {
    html = html.replace(id, rendered);
  }

  const renderedBlocks = await Promise.all(
    codeBlocks.map(async (block) => [block.id, await highlight(block.code, block.lang)] as const),
  );
  for (const [id, rendered] of renderedBlocks) {
    html = html.replace(id, rendered);
  }

  return html;
}

export function extractHeadings(markdown: string): TocItem[] {
  const headings: TocItem[] = [];
  const slugger = createSlugger();
  const tokens = marked.lexer(markdown, MARKED_OPTIONS);

  for (const token of tokens) {
    if (token.type !== 'heading') {
      continue;
    }

    const heading = token as Tokens.Heading;
    const rendered = parseInlineSync(heading.text);
    headings.push({
      level: heading.depth,
      id: slugger.slug(heading.text, rendered),
      text: toPlainTextFromHtml(rendered),
    });
  }

  return headings;
}

function sanitizeLanguage(language?: string): string {
  const match = language?.toLowerCase().match(/[a-z0-9_-]+/);
  return match?.[0] || 'text';
}
