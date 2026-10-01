import { island } from '@putnami/web';
import { MarkdownToc, ThemeProvider, type TocItem } from '@putnami/ui';
import { putnamiTheme } from '../../../theme';

/**
 * Island wrapper for the on-this-page table of contents. Hydrates on idle to
 * run the IntersectionObserver scroll-spy that highlights the current heading.
 * Without hydration the TOC is still a usable static list of in-page anchors —
 * the island only adds the active-section highlighting.
 *
 * The `items` array is passed through as island props.
 */
function DocToc({ items }: { items: TocItem[] }) {
  return (
    <ThemeProvider theme={putnamiTheme}>
      <MarkdownToc items={items} />
    </ThemeProvider>
  );
}

export default island().load('idle').render(DocToc);
