import { BrowserDocumentHelper } from './document-browser.helper';
import type { DocumentHelper } from './document.types';

export type { DocumentHelper, DocumentMeta, DocumentMetaContext } from './document.types';

// Browser-only factory selected by the package.json `browser` field, which
// applies to source/dev consumers and to the published browser build graph
// (bun's browser target honours the field). It never imports
// SsrDocumentHelper; the package-wide containment boundary is the separate
// browser build graph, not this per-module redirect.
export const documentHelper = (): DocumentHelper => new BrowserDocumentHelper();
