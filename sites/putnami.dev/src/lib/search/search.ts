/**
 * Client-side search runtime.
 * Loads the prebuilt index and performs ranked queries locally.
 */
import { loadSearchIndex } from './load-index';
import { tokenize } from './tokenizer';
import type { SearchIndex, SearchResult } from './types';
import { FIELD_BODY, FIELD_EXCERPT, FIELD_HEADINGS, FIELD_TITLE } from './types';

/** Score weights per field. */
const WEIGHT_TITLE = 10;
const WEIGHT_HEADINGS = 6;
const WEIGHT_EXCERPT = 3;
const WEIGHT_BODY = 1;

/** Compute score from a field bitmask. */
function scoreFromMask(mask: number): number {
  let score = 0;
  if (mask & FIELD_TITLE) score += WEIGHT_TITLE;
  if (mask & FIELD_HEADINGS) score += WEIGHT_HEADINGS;
  if (mask & FIELD_EXCERPT) score += WEIGHT_EXCERPT;
  if (mask & FIELD_BODY) score += WEIGHT_BODY;
  return score;
}

/**
 * Search the index for matching documents.
 *
 * Ranking:
 * - Primary key: number of unique query tokens matched (more = better)
 * - Secondary key: total weighted score across all matched tokens
 * - Tie-break: URL (lexicographic, ascending)
 *
 * @param query The user's search query string.
 * @param maxResults Maximum results to return (default 10).
 * @returns Ranked search results.
 */
export async function search(query: string, maxResults = 10): Promise<SearchResult[]> {
  const index = await loadSearchIndex();
  return searchSync(index, query, maxResults);
}

/**
 * Synchronous search against an already-loaded index.
 * Exported for testing and direct use when the index is already available.
 */
export function searchSync(index: SearchIndex, query: string, maxResults = 10): SearchResult[] {
  const queryTokens = tokenize(query.trim());
  if (queryTokens.length === 0) return [];

  // For each doc, track: total score and set of matched query tokens
  const docScores = new Map<number, { score: number; matchedTokens: Set<string> }>();

  for (const qt of queryTokens) {
    const hits = index.lex[qt];
    if (!hits) continue;

    for (const [docId, mask] of hits) {
      let entry = docScores.get(docId);
      if (!entry) {
        entry = { score: 0, matchedTokens: new Set() };
        docScores.set(docId, entry);
      }
      entry.score += scoreFromMask(mask);
      entry.matchedTokens.add(qt);
    }
  }

  if (docScores.size === 0) return [];

  // Build result array and sort
  const entries = [...docScores.entries()].map(([docId, { score, matchedTokens }]) => ({
    docId,
    score,
    tokenCount: matchedTokens.size,
  }));

  entries.sort((a, b) => {
    // Primary: more matched tokens first
    if (b.tokenCount !== a.tokenCount) return b.tokenCount - a.tokenCount;
    // Secondary: higher score first
    if (b.score !== a.score) return b.score - a.score;
    // Tie-break: URL lexicographic
    return index.docs[a.docId].url.localeCompare(index.docs[b.docId].url);
  });

  return entries.slice(0, maxResults).map(({ docId, score }) => {
    const doc = index.docs[docId];
    return {
      title: doc.title,
      url: doc.url,
      breadcrumbs: doc.breadcrumbs,
      snippet: doc.excerpt,
      score,
    };
  });
}
