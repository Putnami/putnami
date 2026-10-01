import type { LinkHTMLAttributes, MetaHTMLAttributes, ScriptHTMLAttributes, StyleHTMLAttributes } from 'react';
import type { DocumentHelper } from './document.types';
import { escapeCssAttrValue } from './tag.utils';

export class BrowserDocumentHelper implements DocumentHelper {
  get title() {
    return document.title;
  }
  set title(title: string) {
    document.title = title || '';
  }
  get lang() {
    return document.documentElement.lang;
  }
  set lang(lang: string) {
    document.documentElement.lang = lang || 'en';
  }
  /**
   * Adds a link element, deduplicating by href.
   * Skips if an identical element already exists.
   */
  addLink(link: LinkHTMLAttributes<HTMLLinkElement>) {
    if (link.href) {
      const existing = document.head.querySelector(`link[href="${escapeCssAttrValue(link.href)}"]`);
      if (existing) {
        if (this.matchesAttributes(existing, link)) return;
        existing.remove();
      }
    }
    this.applyTag('link', link);
  }

  /**
   * Adds a meta element, deduplicating by name, property, or httpEquiv.
   * Skips if an identical element already exists.
   */
  addMeta(meta: MetaHTMLAttributes<HTMLMetaElement>) {
    const key = meta.name || meta.property || meta.httpEquiv;
    if (key) {
      const escaped = escapeCssAttrValue(key);
      const selector = meta.name
        ? `meta[name="${escaped}"]`
        : meta.property
          ? `meta[property="${escaped}"]`
          : `meta[http-equiv="${escaped}"]`;
      const existing = document.head.querySelector(selector);
      if (existing) {
        if (this.matchesAttributes(existing, meta)) return;
        existing.remove();
      }
    }
    this.applyTag('meta', meta);
  }

  /**
   * Adds a script element, deduplicating by src or by type for inline scripts.
   * Skips if an identical element already exists.
   */
  addScript(script: ScriptHTMLAttributes<HTMLScriptElement>) {
    const selector = script.src
      ? `script[src="${escapeCssAttrValue(script.src)}"]`
      : script.type
        ? `script[type="${escapeCssAttrValue(script.type)}"]`
        : undefined;
    if (selector) {
      const existing = document.head.querySelector(selector);
      if (existing) {
        if (this.matchesAttributes(existing, script)) return;
        existing.remove();
      }
    }
    this.applyTag('script', script);
  }

  /**
   * Adds a style element, deduplicating by text content.
   * Skips if an identical style element already exists.
   */
  addStyle(style: StyleHTMLAttributes<HTMLStyleElement>) {
    if (typeof style.children === 'string') {
      const existing = Array.from(document.head.querySelectorAll('style')).find(
        (el) => el.textContent === style.children,
      );
      if (existing) return;
    }
    this.applyTag('style', style);
  }

  private matchesAttributes(element: Element, attributes: object): boolean {
    for (const [key, value] of Object.entries(attributes)) {
      if (key === 'children') {
        if (typeof value === 'string' && element.textContent !== value) return false;
        continue;
      }
      if (value !== undefined && value !== null) {
        if (element.getAttribute(key.toLowerCase()) !== String(value)) return false;
      }
    }
    return true;
  }

  private applyTag<T extends HTMLElement>(tag: string, attributes: object) {
    const el = document.createElement(tag) as T & { children?: string };
    for (const [key, value] of Object.entries(attributes)) {
      if (key === 'children') {
        if (typeof value === 'string') {
          el.textContent = value;
        }
        continue;
      }
      if (value !== undefined && value !== null) {
        el.setAttribute(key.toLowerCase(), String(value));
      }
    }
    document.head.appendChild(el);
  }
}
