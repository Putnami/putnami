import { createContext, useContext, type ReactNode } from 'react';
import type { DocumentMeta } from './document.types';

/**
 * React context for document metadata during SSR.
 * This ensures document meta is shared across all components including lazy-loaded ones.
 */
export const DocumentMetaContext = createContext<DocumentMeta | null>(null);

/**
 * Provider for document metadata context.
 * Wrap your component tree with this to enable proper meta tag handling in SSR.
 */
export function DocumentMetaProvider({ children, meta }: { children: ReactNode; meta: DocumentMeta }) {
  return <DocumentMetaContext.Provider value={meta}>{children}</DocumentMetaContext.Provider>;
}

/**
 * Hook to access the document meta context.
 * Returns the shared DocumentMeta object or null if not in SSR context.
 */
export function useDocumentMeta(): DocumentMeta | null {
  return useContext(DocumentMetaContext);
}
