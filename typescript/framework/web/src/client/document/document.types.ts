import type { LinkHTMLAttributes, MetaHTMLAttributes, ScriptHTMLAttributes, StyleHTMLAttributes } from 'react';

export interface DocumentMetaContext {
  documentMeta?: DocumentMeta;
}

export interface DocumentMeta {
  lang?: string;
  title?: string;
  scripts?: ScriptHTMLAttributes<HTMLScriptElement>[];
  links?: LinkHTMLAttributes<HTMLLinkElement>[];
  metas?: MetaHTMLAttributes<HTMLMetaElement>[];
  styles?: StyleHTMLAttributes<HTMLStyleElement>[];
  /** Lazy style extractors called after SSR rendering completes (e.g. Emotion CSS extraction). */
  styleExtractors?: Array<() => StyleHTMLAttributes<HTMLStyleElement>[]>;
}

export interface DocumentHelper {
  title: string;
  lang: string;
  addScript: (scriptAttributes: ScriptHTMLAttributes<HTMLScriptElement>) => void;
  addLink: (linkAttributes: LinkHTMLAttributes<HTMLLinkElement>) => void;
  addMeta: (metaAttributes: MetaHTMLAttributes<HTMLMetaElement>) => void;
  addStyle: (styleAttributes: StyleHTMLAttributes<HTMLStyleElement>) => void;
  readonly headHtml?: string;
}
