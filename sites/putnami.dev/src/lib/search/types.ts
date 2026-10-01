/**
 * Search index types shared between indexer and runtime.
 */

/** A single document in the search index. */
export interface SearchDoc {
  /** Numeric document ID. */
  id: number;
  /** URL path (e.g. "/docs/getting-started"). */
  url: string;
  /** Document title. */
  title: string;
  /** Heading texts (h2, h3). */
  headings: string[];
  /** Short excerpt (first paragraph, max ~200 chars). */
  excerpt: string;
  /** Breadcrumb segments derived from the URL path. */
  breadcrumbs: string[];
}

/**
 * Packed hit entry: [docId, fieldMask]
 *
 * Field mask bits:
 *   0x1 = title
 *   0x2 = headings
 *   0x4 = excerpt (early body)
 *   0x8 = body
 */
export type HitEntry = [number, number];

/** Field mask constants. */
export const FIELD_TITLE = 0x1;
export const FIELD_HEADINGS = 0x2;
export const FIELD_EXCERPT = 0x4;
export const FIELD_BODY = 0x8;

/** The serialized search index format. */
export interface SearchIndex {
  /** Index format version for cache-busting. Content hash of all docs. */
  version: string;
  /** All indexed documents. */
  docs: SearchDoc[];
  /** Inverted index: token -> array of [docId, fieldMask]. */
  lex: Record<string, HitEntry[]>;
}

/** A single search result returned to the UI. */
export interface SearchResult {
  /** Document title. */
  title: string;
  /** URL path. */
  url: string;
  /** Breadcrumb segments. */
  breadcrumbs: string[];
  /** Excerpt snippet with context. */
  snippet: string;
  /** Relevance score (higher is better). */
  score: number;
}
