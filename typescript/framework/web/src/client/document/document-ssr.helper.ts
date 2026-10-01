import type { LinkHTMLAttributes, MetaHTMLAttributes, ScriptHTMLAttributes, StyleHTMLAttributes } from 'react';
import { tryContext } from '@putnami/runtime';
import type { DocumentHelper, DocumentMeta } from './document.types';
import { ensureDocumentMeta } from '../../shared/context-slots';
import { asHtml, escapeHtml } from './tag.utils';

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
    // Run style extractors (e.g. Emotion CSS) before building head HTML.
    // Extractors are registered during rendering and called here after allReady.
    if (this.documentMeta.styleExtractors) {
      for (const extractor of this.documentMeta.styleExtractors) {
        for (const style of extractor()) {
          this.addStyle(style);
        }
      }
      this.documentMeta.styleExtractors = undefined;
    }

    const elems: string[] = [];
    elems.push('<head>');
    if (this.documentMeta.title) {
      elems.push(`<title>${escapeHtml(this.documentMeta.title)}</title>`);
    }
    if (this.documentMeta.metas) {
      for (const meta of this.documentMeta.metas) {
        elems.push(asHtml('meta', meta));
      }
    }
    if (this.documentMeta.links) {
      for (const link of this.documentMeta.links) {
        elems.push(asHtml('link', link));
      }
    }
    if (this.documentMeta.scripts) {
      for (const script of this.documentMeta.scripts) {
        const withNonce = this.nonce && !script.nonce ? { ...script, nonce: this.nonce } : script;
        elems.push(asHtml('script', withNonce));
      }
    }
    if (this.documentMeta.styles) {
      for (const style of this.documentMeta.styles) {
        elems.push(asHtml('style', style));
      }
    }
    elems.push('</head>');
    return elems.join('');
  }
}
