import { island } from '@putnami/web';
import { enhanceMarkdown } from '@putnami/ui';
import { useEffect } from 'react';

/**
 * Enhancer island for the rendered doc body. The doc HTML is emitted once as
 * static markup (a plain <MarkdownRenderer>, not an island) so it is never
 * re-serialized as island props — for large docs that html is ~60% of the page.
 * This island renders nothing; on idle it attaches the interactive behaviours
 * (copy buttons, code-group tabs, mermaid) to the static doc DOM in place.
 */
function DocEnhancer() {
  useEffect(() => {
    const root = document.querySelector('[data-markdown-root]');
    if (root instanceof HTMLElement) {
      return enhanceMarkdown(root);
    }
    return undefined;
  }, []);

  return null;
}

export default island().load('idle').render(DocEnhancer);
