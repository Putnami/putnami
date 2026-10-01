import type { HtmlHTMLAttributes } from 'react';
import { useDocumentMeta } from './document-context';
import { documentHelper } from './document.helper';

type TitleAttr = HtmlHTMLAttributes<HTMLTitleElement>;

export const Title = (attr: TitleAttr) => {
  // Try React context first (works across Suspense/lazy boundaries)
  const contextMeta = useDocumentMeta();

  const title = typeof attr.children === 'string' ? attr.children : attr.title;

  if (title) {
    if (contextMeta) {
      // Use React context (preferred for SSR with lazy components)
      contextMeta.title = title;
    } else {
      // Fallback to documentHelper (browser or non-context SSR)
      documentHelper().title = title;
    }
  }

  return null;
};
