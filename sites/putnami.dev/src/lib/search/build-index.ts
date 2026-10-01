/**
 * Search index builder.
 *
 * Core indexing logic extracted as a reusable module.
 * Used by both the build-search-index.ts CLI script and the search plugin.
 */
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs';
import { basename, extname, join, relative } from 'node:path';
import { tokenize } from './tokenizer';
import type { HitEntry, SearchDoc, SearchIndex } from './types';
import { FIELD_BODY, FIELD_EXCERPT, FIELD_HEADINGS, FIELD_TITLE } from './types';

// ---------------------------------------------------------------------------
// Markdown parsing helpers (zero-dependency)
// ---------------------------------------------------------------------------

/** Strip fenced code blocks from markdown. */
function stripCodeBlocks(md: string): string {
  return md.replace(/```[\s\S]*?```/g, '').replace(/`[^`\n]+`/g, '');
}

/** Strip markdown formatting: links, emphasis, images, HTML tags. */
function stripMarkdown(md: string): string {
  let text = md;
  text = text.replace(/!\[[^\]]*\]\([^)]*\)/g, '');
  text = text.replace(/\[([^\]]*)\]\([^)]*\)/g, '$1');
  text = text.replace(/(\*{1,3}|_{1,3})(.*?)\1/g, '$2');
  text = text.replace(/~~(.*?)~~/g, '$1');
  text = text.replace(/<[^>]*>/g, '');
  text = text.replace(/^#{1,6}\s+/gm, '');
  text = text.replace(/^>\s*/gm, '');
  text = text.replace(/^[-*_]{3,}\s*$/gm, '');
  text = text.replace(/^\s*[-*+]\s+/gm, '');
  text = text.replace(/^\s*\d+\.\s+/gm, '');
  text = text.replace(/\n{2,}/g, '\n').trim();
  return text;
}

/** Extract frontmatter from markdown content. Returns [frontmatter, body]. */
function extractFrontmatter(content: string): [Record<string, string>, string] {
  const match = content.match(/^---\n([\s\S]*?)\n---\n([\s\S]*)$/);
  if (!match) return [{}, content];

  const fm: Record<string, string> = {};
  for (const line of match[1].split('\n')) {
    const colonIdx = line.indexOf(':');
    if (colonIdx > 0) {
      const key = line.slice(0, colonIdx).trim();
      const value = line
        .slice(colonIdx + 1)
        .trim()
        .replace(/^["']|["']$/g, '');
      fm[key] = value;
    }
  }
  return [fm, match[2]];
}

/** Extract title from markdown: frontmatter.title > first # heading > filename. */
function extractTitle(fm: Record<string, string>, body: string, filePath: string): string {
  if (fm['title']) return fm['title'];
  const h1Match = body.match(/^#\s+(.+)$/m);
  if (h1Match) return stripMarkdown(h1Match[1]).trim();
  const name = basename(filePath, extname(filePath));
  return normalizeName(name);
}

/** Extract h2/h3 heading texts from markdown body. */
function extractHeadings(body: string): string[] {
  const headings: string[] = [];
  const regex = /^#{2,3}\s+(.+)$/gm;
  let match = regex.exec(body);
  while (match !== null) {
    headings.push(stripMarkdown(match[1]).trim());
    match = regex.exec(body);
  }
  return headings;
}

/** Extract first non-empty paragraph from body (stripped markdown, max ~200 chars). */
function extractExcerpt(body: string): string {
  const cleaned = stripCodeBlocks(body);
  const lines = cleaned.split('\n');
  let paragraph = '';

  for (const line of lines) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith('#') || trimmed.startsWith('---') || trimmed.startsWith('```')) continue;
    paragraph = stripMarkdown(trimmed);
    if (paragraph.length > 10) break;
  }

  if (paragraph.length > 200) {
    paragraph = `${paragraph.slice(0, 197)}...`;
  }
  return paragraph;
}

/** Normalize a filename: strip order prefix, replace dashes/underscores with spaces, capitalize. */
function normalizeName(name: string): string {
  return name
    .replace(/^\d+-/, '')
    .replace(/[-_]/g, ' ')
    .replace(/\b\w/g, (c) => c.toUpperCase());
}

// ---------------------------------------------------------------------------
// Path / URL mapping
// ---------------------------------------------------------------------------

/**
 * Convert a file path relative to docs root to a URL path.
 * e.g. "01-getting-started/02-introduction.md" -> "/docs/getting-started/introduction"
 *      "01-getting-started/index.md" -> "/docs/getting-started"
 */
export function filePathToUrl(relPath: string): string {
  const segments = relPath.split('/');
  const urlSegments: string[] = [];

  for (const seg of segments) {
    let name = seg;
    if (name.endsWith('.md')) {
      name = name.slice(0, -3);
    }
    name = name.replace(/^\d+-/, '');
    urlSegments.push(name);
  }

  if (urlSegments[urlSegments.length - 1] === 'index') {
    urlSegments.pop();
  }

  const path = urlSegments.join('/');
  return path ? `/docs/${path}` : '/docs';
}

/** Derive breadcrumbs from URL path segments. */
function urlToBreadcrumbs(url: string): string[] {
  const parts = url
    .replace(/^\/docs\/?/, '')
    .split('/')
    .filter(Boolean);
  return parts.map((p) => normalizeName(p));
}

// ---------------------------------------------------------------------------
// File walker
// ---------------------------------------------------------------------------

/** Recursively find all .md files under a directory. */
function walkMarkdownFiles(dir: string): string[] {
  const results: string[] = [];
  if (!existsSync(dir)) return results;

  const entries = readdirSync(dir);
  for (const entry of entries) {
    if (entry.startsWith('.')) continue;
    const fullPath = join(dir, entry);
    const stat = statSync(fullPath);
    if (stat.isDirectory()) {
      results.push(...walkMarkdownFiles(fullPath));
    } else if (entry.endsWith('.md')) {
      results.push(fullPath);
    }
  }

  results.sort();
  return results;
}

// ---------------------------------------------------------------------------
// Index builder
// ---------------------------------------------------------------------------

/**
 * Build a search index from markdown files in the given directory.
 *
 * @param docsRoot Absolute path to the directory containing markdown files.
 * @returns The complete search index ready for serialization.
 */
export function buildSearchIndex(docsRoot: string): SearchIndex {
  const files = walkMarkdownFiles(docsRoot);
  const docs: SearchDoc[] = [];
  const lex: Record<string, HitEntry[]> = Object.create(null);
  const contentParts: string[] = [];

  for (const filePath of files) {
    const relPath = relative(docsRoot, filePath);
    const content = readFileSync(filePath, 'utf-8');

    contentParts.push(content);

    const [fm, body] = extractFrontmatter(content);
    const title = extractTitle(fm, body, filePath);
    const headings = extractHeadings(body);
    const excerpt = extractExcerpt(body);
    const url = filePathToUrl(relPath);
    const breadcrumbs = urlToBreadcrumbs(url);

    const id = docs.length;
    docs.push({ id, url, title, headings, excerpt, breadcrumbs });

    const titleTokens = new Set(tokenize(title));
    const headingTokens = new Set(tokenize(headings.join(' ')));
    const excerptTokens = new Set(tokenize(excerpt));

    const bodyText = stripMarkdown(stripCodeBlocks(body));
    const bodyTokens = new Set(tokenize(bodyText));

    const allTokens = new Set([...titleTokens, ...headingTokens, ...excerptTokens, ...bodyTokens]);

    for (const token of allTokens) {
      let mask = 0;
      if (titleTokens.has(token)) mask |= FIELD_TITLE;
      if (headingTokens.has(token)) mask |= FIELD_HEADINGS;
      if (excerptTokens.has(token)) mask |= FIELD_EXCERPT;
      if (bodyTokens.has(token)) mask |= FIELD_BODY;

      if (!lex[token]) {
        lex[token] = [];
      }
      lex[token].push([id, mask]);
    }
  }

  // Sort lex entries by docId for determinism
  for (const token in lex) {
    lex[token].sort((a, b) => a[0] - b[0]);
  }

  // Sort lex keys for deterministic output
  const sortedLex: Record<string, HitEntry[]> = Object.create(null);
  for (const key of Object.keys(lex).sort()) {
    sortedLex[key] = lex[key];
  }

  // Compute content hash for versioning
  const hasher = new Bun.CryptoHasher('sha256');
  for (const part of contentParts) {
    hasher.update(part);
  }
  const version = hasher.digest('hex').slice(0, 12);

  return { version, docs, lex: sortedLex };
}
