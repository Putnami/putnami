import { escapeHtml } from '@putnami/utils';
import type { HtmlHTMLAttributes } from 'react';

// Re-exported for existing call sites and tests that import the escaper from
// this module; backed by the shared `Bun.escapeHTML`-accelerated helper.
export { escapeHtml };

/** Tags whose content is raw text per the HTML spec (not HTML-escaped). */
const RAW_TEXT_TAGS = new Set(['script', 'style']);

/** Escape a value for use inside a CSS `[attr="value"]` selector. */
export const escapeCssAttrValue = (value: string) => value.replace(/\\/g, '\\\\').replace(/"/g, '\\"');

/**
 * Neutralize raw-text-element breakout sequences for `<script>`/`<style>` content.
 *
 * Inside a raw-text element the HTML parser does not decode entities; the only
 * sequence that can terminate the element early is a case-insensitive `</script`
 * or `</style` (and `<!--`, which can flip the tokenizer into a comment-like state).
 * We escape just those by splitting the `<`, so hostile `</script>...` inside
 * `<Script>{userControlledString}</Script>` cannot break out — while leaving
 * ordinary content such as `if (a < b)` or `el.querySelector('script')` untouched.
 *
 * `dangerouslySetInnerHTML` on non-raw-text tags is trusted-only and not affected.
 */
export const escapeRawText = (value: string) =>
  value.replace(/<\/(script|style)/gi, '<\\/$1').replace(/<!--/g, '<\\!--');

export const asHtml = <A>(tag: string, attr: HtmlHTMLAttributes<A>) => {
  const buf: string[] = [];
  buf.push(`<${tag} `);
  for (const [k, v] of Object.entries(attr)) {
    if (k === 'children' || k === 'dangerouslySetInnerHTML') {
      continue;
    }
    if (v !== undefined && v !== null) {
      buf.push(`${k.toLowerCase()}="${escapeHtml(String(v))}"`);
    }
  }

  // React's dangerouslySetInnerHTML is not in the base attribute type; access via Record
  const rawHtml = (attr as Record<string, { __html?: string } | undefined>)['dangerouslySetInnerHTML']?.__html;
  const content = rawHtml ?? attr.children;

  if (typeof content === 'string') {
    buf.push('>');
    buf.push(emitContent(tag, content, rawHtml !== undefined));
    buf.push(`</${tag}>`);
  } else {
    buf.push('/>');
  }
  return buf.join('');
};

/**
 * Decide how to emit element content based on the tag's parsing mode.
 *
 * - Raw-text tags (`script`/`style`): entities are not decoded, so we only
 *   neutralize end-tag breakout sequences via {@link escapeRawText}. This keeps
 *   inline JS/CSS intact while preventing `</script>` injection.
 * - Other tags with text children: HTML-escape so untrusted text can't inject markup.
 * - Other tags with `dangerouslySetInnerHTML`: trusted-only raw HTML, emitted verbatim.
 */
const emitContent = (tag: string, content: string, isRawHtml: boolean): string => {
  if (RAW_TEXT_TAGS.has(tag)) {
    return escapeRawText(content);
  }
  return isRawHtml ? content : escapeHtml(content);
};
