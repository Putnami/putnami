import { marked } from 'marked';

export const MARKED_OPTIONS = {
  gfm: true,
  breaks: false,
  mangle: false,
  headerIds: false,
  async: false,
};

const HTML_ENTITY_MAP: Record<string, string> = {
  '&amp;': '&',
  '&lt;': '<',
  '&gt;': '>',
  '&quot;': '"',
  '&#39;': "'",
};

function stripHtml(html: string): string {
  return html.replace(/<[^>]*>/g, '');
}

function decodeHtmlEntities(text: string): string {
  return text.replace(/&(amp|lt|gt|quot|#39);/g, (match) => HTML_ENTITY_MAP[match] ?? match);
}

export function toPlainTextFromHtml(html: string): string {
  return decodeHtmlEntities(stripHtml(html)).trim();
}

export function toPlainText(markdown: string): string {
  return toPlainTextFromHtml(parseInlineSync(markdown));
}

function slugifyPlain(plain: string): string {
  const slug = plain
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '');

  return slug || 'section';
}

export function createSlugger() {
  const seen = new Map<string, number>();

  return {
    /**
     * Generate a unique slug. If `renderedHtml` is provided, it is used to derive the
     * plain text (avoiding a redundant `parseInlineSync` call).
     */
    slug(value: string, renderedHtml?: string): string {
      const plain = renderedHtml ? toPlainTextFromHtml(renderedHtml) : toPlainText(value);
      const base = slugifyPlain(plain);
      const count = seen.get(base) ?? 0;
      seen.set(base, count + 1);
      return count === 0 ? base : `${base}-${count}`;
    },
  };
}

export function parseInlineSync(markdown: string): string {
  const html = marked.parseInline(markdown, MARKED_OPTIONS);
  if (typeof html === 'string') {
    return html;
  }

  throw new Error('Expected sync markdown parsing but got a Promise.');
}
