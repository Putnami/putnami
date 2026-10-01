import { declareEvents } from '@putnami/analytics';

/**
 * The action vocabulary this site records, and the whole of it.
 *
 * An undeclared name throws at the call site and is dropped on arrival, so
 * this list is what bounds the payload — and what a reader has to check to
 * know everything the site measures beyond page views.
 *
 * What is deliberately absent: the search query. It is text a visitor typed,
 * and this package's boundary is that no field of a form is ever stored. The
 * number of results is enough to tell a search that found nothing from one
 * that did.
 */
export const siteEvents = declareEvents({
  /**
   * A CLI artifact was resolved to its registry download URL through `/dl`.
   * Not an install count: `install.sh` fetches from the registry directly, and
   * the redirect is cacheable for 60 seconds.
   */
  cli_download: { artifact: String, platform: String, target: String, channel: String },
  /** A documentation page was opened from the search palette. */
  docs_search_open: { hits: Number },
  /** A page was copied as Markdown, from the doc page or from the palette. */
  doc_copy_markdown: { surface: String },
});
