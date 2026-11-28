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
  /**
   * Style extractors (e.g. Emotion CSS extraction). The SSR document helper
   * calls each one for the head, then before each chunk the render streams
   * after the shell: a call returns what was inserted since the previous one.
   */
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
