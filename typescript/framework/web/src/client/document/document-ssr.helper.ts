import type { LinkHTMLAttributes, MetaHTMLAttributes, ScriptHTMLAttributes, StyleHTMLAttributes } from 'react';
import { tryContext } from '@putnami/runtime';
import type { DocumentHelper, DocumentMeta } from './document.types';
import { ensureDocumentMeta } from '../../shared/context-slots';
import { asHtml, escapeCssAttrValue, escapeHtml } from './tag.utils';
import { safeJsonForScript } from '../../ssr/safe-json';

export class SsrDocumentHelper implements DocumentHelper {
  private readonly documentMeta: DocumentMeta;

  constructor(
    meta?: DocumentMeta,
    /**
     * Per-request CSP nonce stamped on every head-injected <script> so a
     * strict `script-src 'nonce-...'` policy permits framework-emitted
     * inline scripts (color mode, PageMeta) like it does the hydration
     * scripts.
     */
    private readonly nonce?: string,
  ) {
    if (meta) {
      this.documentMeta = meta;
      return;
    }

    // Resolve the per-request meta from AsyncLocalStorage (concurrent-safe).
    // This module belongs to the server build graph: publication builds
    // browser-condition entrypoints separately, so it can never be linked into
    // a chunk the published browser entry imports. The window guard is still
    // the runtime contract for source/dev consumers that bundle `src/*.ts`
    // themselves, and @putnami/runtime's browser condition keeps `tryContext`
    // resolvable there.
    if (typeof window === 'undefined') {
      const localContext = tryContext<Record<string, unknown>>();
      if (localContext) {
        this.documentMeta = ensureDocumentMeta(localContext);
        return;
      }
    }

    // No request context available: use a fresh request-local object.
    // Never share mutable SSR state across requests (no globalThis slot),
    // so concurrent renders cannot bleed or clear each other's meta.
    this.documentMeta = {};
  }

  set title(title: string) {
    this.documentMeta.title = title;
  }
  get title() {
    return this.documentMeta.title || '';
  }

  set lang(lang: string) {
    this.documentMeta.lang = lang;
  }
  get lang() {
    return this.documentMeta.lang || 'en';
  }
  /**
   * Adds a link element, deduplicating by href.
   */
  addLink(link: LinkHTMLAttributes<HTMLLinkElement>) {
    this.documentMeta.links ||= [];
    if (link.href) {
      const existingIndex = this.documentMeta.links.findIndex((l) => l.href === link.href);
      if (existingIndex >= 0) {
        this.documentMeta.links[existingIndex] = link; // Replace existing
        return;
      }
    }
    this.documentMeta.links.push(link);
  }

  /**
   * Adds a meta element, deduplicating by name, property, or httpEquiv.
   */
  addMeta(meta: MetaHTMLAttributes<HTMLMetaElement>) {
    this.documentMeta.metas ||= [];
    const key = meta.name || meta.property || meta.httpEquiv;
    if (key) {
      const existingIndex = this.documentMeta.metas.findIndex((m) => (m.name || m.property || m.httpEquiv) === key);
      if (existingIndex >= 0) {
        this.documentMeta.metas[existingIndex] = meta; // Replace existing
        return;
      }
    }
    this.documentMeta.metas.push(meta);
  }

  /**
   * Adds a script element, deduplicating by src or by type for inline scripts.
   */
  addScript(script: ScriptHTMLAttributes<HTMLScriptElement>) {
    this.documentMeta.scripts ||= [];
    const dedupeKey = script.src ? 'src' : script.type ? 'type' : undefined;
    if (dedupeKey) {
      const existingIndex = this.documentMeta.scripts.findIndex((s) => s[dedupeKey] === script[dedupeKey]);
      if (existingIndex >= 0) {
        this.documentMeta.scripts[existingIndex] = script; // Replace existing
        return;
      }
    }
    this.documentMeta.scripts.push(script);
  }

  /**
   * Adds a style element, deduplicating by text content.
   * Skips if an identical style element already exists.
   */
  addStyle(style: StyleHTMLAttributes<HTMLStyleElement>) {
    this.documentMeta.styles ||= [];
    if (typeof style.children === 'string') {
      const exists = this.documentMeta.styles.some((s) => s.children === style.children);
      if (exists) return;
    }
    this.documentMeta.styles.push(style);
  }

  get headHtml(): string {
    this.pullExtractedStyles();
    const sent: SentHead = {
      title: this.documentMeta.title,
      lang: this.documentMeta.lang,
      metas: new Map(),
      links: new Map(),
      scripts: new Set(),
      elements: new Set(),
    };

    const elems: string[] = [];
    elems.push('<head>');
    if (this.documentMeta.title) {
      elems.push(`<title>${escapeHtml(this.documentMeta.title)}</title>`);
    }
    for (const meta of this.documentMeta.metas ?? []) {
      const tag = asHtml('meta', meta);
      sent.metas.set(metaKey(meta), tag);
      elems.push(tag);
    }
    for (const link of this.documentMeta.links ?? []) {
      const tag = asHtml('link', link);
      sent.links.set(linkKey(link), tag);
      elems.push(tag);
    }
    for (const script of this.documentMeta.scripts ?? []) {
      const tag = this.scriptHtml(script);
      sent.elements.add(script);
      sent.scripts.add(tag);
      elems.push(tag);
    }
    for (const style of this.documentMeta.styles ?? []) {
      sent.elements.add(style);
      elems.push(asHtml('style', style));
    }
    elems.push('</head>');
    this.sent = sent;
    return elems.join('');
  }

  /**
   * The styles a render produced since the head, or since the last call, as
   * HTML. The streaming renderer writes them before the React chunk that needs
   * them. Only extracted Emotion styles (`data-emotion`) are returned: the
   * Emotion client moves them into `<head>` before hydration, so they never
   * reach the hydrated root. Any other style waits for {@link lateHeadHtml}.
   */
  lateStylesHtml(): string {
    const sent = this.sent;
    if (!sent || !this.documentMeta.styleExtractors?.length) return '';
    const html: string[] = [];
    for (const style of this.pullExtractedStyles()) {
      if (!isEmotionStyle(style) || sent.elements.has(style)) continue;
      sent.elements.add(style);
      html.push(asHtml('style', style));
    }
    return html.join('');
  }

  /**
   * What the render added to the document head after the head was sent, as
   * HTML written after the root: the styles and scripts not sent yet, and one
   * nonced script that sets the title and lang and puts each new or changed
   * meta and link tag into `<head>`. Empty when nothing changed.
   */
  lateHeadHtml(): string {
    const sent = this.sent;
    if (!sent) return '';
    this.pullExtractedStyles();
    const elems: string[] = [];
    for (const style of this.documentMeta.styles ?? []) {
      if (sent.elements.has(style)) continue;
      sent.elements.add(style);
      elems.push(asHtml('style', style));
    }
    for (const script of this.documentMeta.scripts ?? []) {
      if (sent.elements.has(script)) continue;
      sent.elements.add(script);
      // `addScript` replaces an entry by a new object: the same script added
      // again late is the same HTML, and runs once.
      const tag = this.scriptHtml(script);
      if (sent.scripts.has(tag)) continue;
      sent.scripts.add(tag);
      elems.push(tag);
    }

    const tags: Array<{ s?: string; h: string }> = [];
    for (const meta of this.documentMeta.metas ?? []) {
      const key = metaKey(meta);
      const tag = asHtml('meta', meta);
      if (sent.metas.get(key) === tag) continue;
      sent.metas.set(key, tag);
      tags.push({ s: metaSelector(meta), h: tag });
    }
    for (const link of this.documentMeta.links ?? []) {
      const key = linkKey(link);
      const tag = asHtml('link', link);
      if (sent.links.get(key) === tag) continue;
      sent.links.set(key, tag);
      tags.push({ s: link.href ? `link[href="${escapeCssAttrValue(String(link.href))}"]` : undefined, h: tag });
    }
    const patch: { t?: string; l?: string; e: typeof tags } = { e: tags };
    if (this.documentMeta.title !== sent.title && this.documentMeta.title !== undefined) {
      patch.t = this.documentMeta.title;
      sent.title = this.documentMeta.title;
    }
    if (this.documentMeta.lang !== sent.lang && this.documentMeta.lang !== undefined) {
      patch.l = this.documentMeta.lang;
      sent.lang = this.documentMeta.lang;
    }
    if (patch.t !== undefined || patch.l !== undefined || tags.length > 0) {
      const nonceAttr = this.nonce ? ` nonce="${escapeHtml(this.nonce)}"` : '';
      elems.push(`<script${nonceAttr}>${LATE_HEAD_SCRIPT}(${safeJsonForScript(patch)})</script>`);
    }
    return elems.join('');
  }

  /** What the head sent, so the late output writes only what is new. */
  private sent?: SentHead;

  /**
   * Run the style extractors (e.g. Emotion CSS) and add what they return to
   * the document styles. An extractor returns what was inserted since its
   * previous call, so it is called again for each chunk the render streams.
   */
  private pullExtractedStyles(): StyleHTMLAttributes<HTMLStyleElement>[] {
    const pulled: StyleHTMLAttributes<HTMLStyleElement>[] = [];
    for (const extractor of this.documentMeta.styleExtractors ?? []) {
      for (const style of extractor()) {
        pulled.push(style);
        this.addStyle(style);
      }
    }
    return pulled;
  }

  private scriptHtml(script: ScriptHTMLAttributes<HTMLScriptElement>): string {
    const withNonce = this.nonce && !script.nonce ? { ...script, nonce: this.nonce } : script;
    return asHtml('script', withNonce);
  }
}

interface SentHead {
  title?: string;
  lang?: string;
  /** Each meta and link tag the head holds, by the key that dedupes it. */
  metas: Map<string, string>;
  links: Map<string, string>;
  /** The HTML of each script already written. */
  scripts: Set<string>;
  /** The script and style entries already written. */
  elements: Set<object>;
}

const metaKey = (meta: MetaHTMLAttributes<HTMLMetaElement>) =>
  meta.name
    ? `name:${meta.name}`
    : meta.property
      ? `property:${meta.property}`
      : meta.httpEquiv
        ? `http-equiv:${meta.httpEquiv}`
        : `html:${asHtml('meta', meta)}`;

const linkKey = (link: LinkHTMLAttributes<HTMLLinkElement>) =>
  link.href ? `href:${link.href}` : `html:${asHtml('link', link)}`;

/** The selector of the head meta a late one replaces, as `asHtml` writes its attribute. */
const metaSelector = (meta: MetaHTMLAttributes<HTMLMetaElement>): string | undefined => {
  if (meta.name) return `meta[name="${escapeCssAttrValue(String(meta.name))}"]`;
  if (meta.property) return `meta[property="${escapeCssAttrValue(String(meta.property))}"]`;
  if (meta.httpEquiv) return `meta[httpequiv="${escapeCssAttrValue(String(meta.httpEquiv))}"]`;
  return undefined;
};

const isEmotionStyle = (style: StyleHTMLAttributes<HTMLStyleElement>) =>
  typeof (style as Record<string, unknown>)['data-emotion'] === 'string';

/**
 * Applies a late head patch: `t` the title, `l` the lang, `e` the tags, each
 * replacing the head element its selector `s` names. The tags are HTML the
 * server built with `asHtml`, so their attributes are escaped.
 */
const LATE_HEAD_SCRIPT =
  '(function(p){var d=document,h=d.head;if(p.t!==undefined)d.title=p.t;if(p.l!==undefined)d.documentElement.lang=p.l;' +
  'p.e.forEach(function(x){try{if(x.s){var o=h.querySelector(x.s);if(o)o.remove()}}catch(_){}h.insertAdjacentHTML("beforeend",x.h)})})';
