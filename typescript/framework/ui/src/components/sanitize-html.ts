/**
 * Hand-rolled, dependency-free HTML sanitizer for rendered markdown.
 *
 * `MarkdownContent`/`MarkdownRenderer` inject an HTML string into the DOM via
 * `dangerouslySetInnerHTML`. As public exports, any caller may pass user-controlled
 * content, so the HTML must be sanitized before it reaches the DOM. This module
 * applies a strict allowlist that keeps the safe, structural/semantic markup a
 * markdown processor emits (headings, lists, tables, code blocks, mermaid/code-group
 * containers, links, images) while removing script execution vectors:
 *
 * - disallowed elements are dropped (and `<script>`/`<style>` content is discarded);
 * - all event-handler attributes (`on*`) are removed;
 * - URL attributes (`href`, `src`, …) are restricted to safe schemes, neutralizing
 *   `javascript:`, `vbscript:`, and non-image `data:` payloads;
 * - only allowlisted attributes survive, plus `data-*` and `aria-*` on allowed tags;
 * - inline styles are dropped except two pattern-checked carve-outs: Shiki color
 *   variables on `pre`/`span`, and a percentage `width` on `col`.
 *
 * It works without a DOM (so it runs identically during SSR and in the browser) by
 * tokenizing the markup rather than parsing it into nodes.
 */

import { escapeHtml } from '@putnami/utils';

/** Tags that are safe to keep in rendered markdown output. */
const ALLOWED_TAGS = new Set<string>([
  // sections / text
  'a',
  'abbr',
  'b',
  'blockquote',
  'br',
  'button',
  'caption',
  'code',
  'col',
  'colgroup',
  'dd',
  'del',
  'details',
  'div',
  'dl',
  'dt',
  'em',
  'figcaption',
  'figure',
  'h1',
  'h2',
  'h3',
  'h4',
  'h5',
  'h6',
  'hr',
  'i',
  'img',
  'ins',
  'kbd',
  'li',
  'mark',
  'ol',
  'p',
  'pre',
  'q',
  's',
  'samp',
  'section',
  'small',
  'span',
  'strong',
  'sub',
  'summary',
  'sup',
  'table',
  'tbody',
  'td',
  'tfoot',
  'th',
  'thead',
  'tr',
  'u',
  'ul',
  'wbr',
]);

/**
 * Elements whose entire contents must be discarded when the element is stripped.
 * For these, the text between the open and close tag is executable or style data,
 * not display content, so it is removed along with the tags.
 */
const VOID_DANGEROUS_CONTENT = new Set<string>(['script', 'style', 'noscript', 'template', 'xmp']);

/** Attributes allowed on any element. */
const GLOBAL_ATTRS = new Set<string>(['class', 'id', 'title', 'dir', 'lang', 'role']);

/** Per-tag allowed attributes (in addition to the global set). */
const TAG_ATTRS: Record<string, Set<string>> = {
  a: new Set(['href', 'name', 'target', 'rel']),
  button: new Set(['type', 'disabled']),
  img: new Set(['src', 'alt', 'width', 'height', 'loading', 'decoding']),
  td: new Set(['colspan', 'rowspan', 'headers', 'scope']),
  th: new Set(['colspan', 'rowspan', 'headers', 'scope', 'abbr']),
  col: new Set(['span']),
  colgroup: new Set(['span']),
  ol: new Set(['start', 'type', 'reversed']),
  details: new Set(['open']),
};

/** Attributes whose values are URLs and must be scheme-checked. */
const URL_ATTRS = new Set<string>(['href', 'src']);

/** Schemes permitted in URL attributes. */
const SAFE_URL_SCHEMES = new Set<string>(['http:', 'https:', 'mailto:', 'tel:', 'ftp:']);

/** Shiki emits these CSS custom properties for dual light/dark code themes. */
const SHIKI_STYLE_PROPS = new Set<string>(['--shiki-light', '--shiki-dark', '--shiki-light-bg', '--shiki-dark-bg']);

const HEX_COLOR_RE = /^#[0-9a-fA-F]{3,8}$/;

/**
 * Markdown table renderers can emit `<col style="width:24.0%">` to declare
 * column weights — GFM has no other way to carry them. A percentage width on
 * `col` is pure layout with no execution or exfiltration surface, so it is the
 * one non-Shiki style declaration the sanitizer keeps.
 */
const COL_WIDTH_STYLE_RE = /^width:\d{1,3}(\.\d+)?%$/;

const decodeForSchemeCheck = (value: string): string =>
  stripSchemeObfuscationChars(
    value
      // Strip HTML entities that could hide a scheme (e.g. &#106;avascript:).
      .replace(/&#x([0-9a-f]+);?/gi, (_, hex) => String.fromCodePoint(Number.parseInt(hex, 16)))
      .replace(/&#(\d+);?/g, (_, dec) => String.fromCodePoint(Number.parseInt(dec, 10))),
  ).toLowerCase();

const stripSchemeObfuscationChars = (value: string): string =>
  Array.from(value)
    .filter((char) => {
      const codePoint = char.codePointAt(0) ?? 0;
      return codePoint > 0x20 && codePoint !== 0xa0 && codePoint !== 0x20_28 && codePoint !== 0x20_29;
    })
    .join('');

/** Returns true when a URL attribute value is safe to keep. */
const isSafeUrl = (value: string): boolean => {
  const normalized = decodeForSchemeCheck(value);

  // Relative URLs, anchors, and query-only URLs have no scheme and are safe.
  if (normalized === '' || normalized.startsWith('#') || normalized.startsWith('/') || normalized.startsWith('?')) {
    return true;
  }

  // data: URLs are only allowed for images.
  if (normalized.startsWith('data:')) {
    return /^data:image\/(png|gif|jpe?g|webp|avif|bmp|svg\+xml);/.test(normalized);
  }

  const schemeMatch = normalized.match(/^([a-z][a-z0-9+.-]*:)/);
  if (!schemeMatch) {
    // No recognizable scheme — treat as a relative path (e.g. "foo/bar").
    return true;
  }

  return SAFE_URL_SCHEMES.has(schemeMatch[1] as string);
};

const ATTR_RE = /([a-zA-Z_:][-a-zA-Z0-9_:.]*)(?:\s*=\s*("[^"]*"|'[^']*'|[^\s"'>]+))?/g;

interface ParsedAttr {
  name: string;
  value: string | null;
  quote: string;
}

const parseAttributes = (raw: string): ParsedAttr[] => {
  const attrs: ParsedAttr[] = [];
  for (const match of raw.matchAll(ATTR_RE)) {
    const name = (match[1] as string).toLowerCase();
    let value: string | null = match[2] ?? null;
    let quote = '"';
    if (value != null) {
      const first = value[0];
      if (first === '"' || first === "'") {
        quote = first;
        value = value.slice(1, -1);
      }
    }
    attrs.push({ name, value, quote });
  }
  return attrs;
};

/** Encodes an attribute value for safe re-serialization inside double quotes. */
const encodeAttrValue = (value: string): string => escapeHtml(value);

const isAllowedAttr = (tag: string, name: string): boolean => {
  // Drop every event handler and known scripting hooks outright.
  if (name.startsWith('on')) return false;
  if (name === 'style') return false; // avoid CSS-based injection; styling comes from the theme
  if (name === 'srcdoc' || name === 'formaction' || name === 'xlink:href') return false;
  if (name.startsWith('data-') || name.startsWith('aria-')) return true;
  if (GLOBAL_ATTRS.has(name)) return true;
  return TAG_ATTRS[tag]?.has(name) ?? false;
};

const sanitizeShikiStyle = (tag: string, value: string): string | null => {
  if (tag === 'col') {
    const declaration = value.trim().replace(/;$/, '');
    return COL_WIDTH_STYLE_RE.test(declaration) ? declaration : null;
  }

  if (tag !== 'pre' && tag !== 'span') return null;

  const kept: string[] = [];
  for (const declaration of value.split(';')) {
    const index = declaration.indexOf(':');
    if (index < 0) continue;

    const property = declaration.slice(0, index).trim().toLowerCase();
    const propertyValue = declaration.slice(index + 1).trim();
    if (!SHIKI_STYLE_PROPS.has(property) || !HEX_COLOR_RE.test(propertyValue)) continue;

    kept.push(`${property}:${propertyValue}`);
  }

  return kept.length ? kept.join(';') : null;
};

const sanitizeAttributes = (tag: string, raw: string): string => {
  const kept: string[] = [];
  for (const attr of parseAttributes(raw)) {
    if (attr.name === 'style') {
      if (attr.value == null) continue;
      const style = sanitizeShikiStyle(tag, attr.value);
      if (style) kept.push(`style="${encodeAttrValue(style)}"`);
      continue;
    }

    if (!isAllowedAttr(tag, attr.name)) continue;

    const value = attr.value;
    if (value != null && URL_ATTRS.has(attr.name) && !isSafeUrl(value)) {
      continue; // drop unsafe URL attribute entirely
    }

    if (value == null) {
      kept.push(attr.name);
    } else {
      kept.push(`${attr.name}="${encodeAttrValue(value)}"`);
    }
  }
  // Harden links that open new tabs against reverse-tabnabbing.
  if (tag === 'a') {
    const hasTargetBlank = kept.some((a) => /^target="?_blank/.test(a));
    const hasRel = kept.some((a) => a === 'rel' || a.startsWith('rel='));
    if (hasTargetBlank && !hasRel) {
      kept.push('rel="noopener noreferrer"');
    }
  }
  return kept.length ? ` ${kept.join(' ')}` : '';
};

const TOKEN_RE = /<!--[\s\S]*?-->|<\/?[a-zA-Z][^>]*>/g;

/**
 * Sanitizes an HTML string against the allowlist above and returns markup that is
 * safe to inject via `dangerouslySetInnerHTML`. Text content is preserved verbatim
 * (it is inert once disallowed tags are removed); disallowed tags are dropped, and
 * the contents of script/style-like elements are discarded.
 */
export function sanitizeHtml(html: string): string {
  if (!html) return '';

  let result = '';
  let lastIndex = 0;
  // When > 0 we are inside a stripped dangerous element and must drop all text.
  let suppressDepth = 0;
  let suppressTag: string | null = null;

  for (const token of html.matchAll(TOKEN_RE)) {
    const raw = token[0];
    const start = token.index;

    const text = html.slice(lastIndex, start);
    if (suppressDepth === 0) {
      result += text;
    }
    lastIndex = start + raw.length;

    // Comments are dropped.
    if (raw.startsWith('<!--')) continue;

    const isClosing = raw[1] === '/';
    const nameMatch = raw.match(/^<\/?\s*([a-zA-Z][a-zA-Z0-9-]*)/);
    if (!nameMatch) continue;
    const tag = (nameMatch[1] as string).toLowerCase();

    if (suppressDepth > 0) {
      // Only the matching closing tag of a dangerous element re-enables output.
      if (isClosing && tag === suppressTag) {
        suppressDepth = 0;
        suppressTag = null;
      }
      continue;
    }

    if (VOID_DANGEROUS_CONTENT.has(tag)) {
      if (!isClosing && !raw.endsWith('/>')) {
        suppressDepth = 1;
        suppressTag = tag;
      }
      continue; // never emit the tag itself
    }

    if (!ALLOWED_TAGS.has(tag)) {
      // Drop the tag but keep any text children (handled by the text slice above).
      continue;
    }

    if (isClosing) {
      result += `</${tag}>`;
      continue;
    }

    const selfClosing = raw.endsWith('/>');
    const attrStart = raw.indexOf(nameMatch[0]) + nameMatch[0].length;
    const attrEnd = selfClosing ? raw.length - 2 : raw.length - 1;
    const rawAttrs = raw.slice(attrStart, Math.max(attrStart, attrEnd));
    const attrs = sanitizeAttributes(tag, rawAttrs);
    result += selfClosing ? `<${tag}${attrs} />` : `<${tag}${attrs}>`;
  }

  if (suppressDepth === 0) {
    result += html.slice(lastIndex);
  }

  return result;
}
