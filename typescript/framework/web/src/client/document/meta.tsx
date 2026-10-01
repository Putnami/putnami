import type { MetaHTMLAttributes } from 'react';
import { useDocumentMeta } from './document-context';
import { documentHelper } from './document.helper';

type MetaAttr = MetaHTMLAttributes<HTMLMetaElement>;

export const Meta = (attr: MetaAttr) => {
  // Try React context first (works across Suspense/lazy boundaries)
  const contextMeta = useDocumentMeta();

  if (contextMeta) {
    // Use React context (preferred for SSR with lazy components)
    contextMeta.metas ||= [];
    const key = attr.name || attr.property || attr.httpEquiv;
    if (key) {
      const existingIndex = contextMeta.metas.findIndex((m) => (m.name || m.property || m.httpEquiv) === key);
      if (existingIndex >= 0) {
        contextMeta.metas[existingIndex] = attr;
      } else {
        contextMeta.metas.push(attr);
      }
    } else {
      contextMeta.metas.push(attr);
    }
  } else {
    // Fallback to documentHelper (browser or non-context SSR)
    documentHelper().addMeta(attr);
  }

  return null;
};
