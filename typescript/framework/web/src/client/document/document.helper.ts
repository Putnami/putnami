import { BrowserDocumentHelper } from './document-browser.helper';
import { SsrDocumentHelper } from './document-ssr.helper';
import type { DocumentHelper } from './document.types';

export type { DocumentHelper, DocumentMeta, DocumentMetaContext } from './document.types';

// Default (server-capable) factory. Browser builds swap this module for
// `document.helper.browser.ts` via the package.json `browser` field, so
// SsrDocumentHelper stays out of client bundles. Publication does not rely on
// that redirect: the browser entrypoint is transpiled in its own build graph
// (see AI.md "Browser / server boundary"), which is what keeps this module
// off every browser-reachable chunk.
export const documentHelper = (): DocumentHelper => {
  if (typeof window === 'undefined') {
    return new SsrDocumentHelper();
  }
  return new BrowserDocumentHelper();
};
