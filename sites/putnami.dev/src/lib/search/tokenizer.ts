/**
 * Shared tokenizer for search indexing and querying.
 * Used by both the build-time indexer and the client-side search runtime.
 */

const STOPWORDS = new Set(['the', 'a', 'an', 'and', 'or', 'is', 'in', 'to', 'of', 'for', 'it', 'on', 'at', 'by']);

/**
 * Split a camelCase or PascalCase identifier into parts.
 * e.g. "CloudRun" -> ["cloud", "run", "cloudrun"]
 */
function splitCamelCase(word: string): string[] {
  const parts = word
    .replace(/([a-z])([A-Z])/g, '$1 $2')
    .toLowerCase()
    .split(/\s+/);
  if (parts.length > 1) {
    return [...parts, parts.join('')];
  }
  return parts;
}

/**
 * Tokenize a string for search indexing or querying.
 *
 * Rules:
 * - Lowercase
 * - Split on non-alphanumeric characters
 * - Also split on '-' and '_' but index joined token too
 *   (e.g. "cloud-run" -> ["cloud", "run", "cloudrun"])
 * - Split camelCase/PascalCase identifiers
 * - Ignore tokens with length < 2
 * - Remove minimal stopwords
 */
export function tokenize(text: string): string[] {
  if (!text) return [];

  const result: string[] = [];
  // Split on non-alphanumeric (keeps internal sequences of alphanumeric chars)
  const rawTokens = text.toLowerCase().split(/[^a-z0-9]+/);

  for (const raw of rawTokens) {
    if (!raw || raw.length < 2) continue;
    if (STOPWORDS.has(raw)) continue;
    result.push(raw);
  }

  // Also process the original text for camelCase/hyphenated compound tokens
  const compoundTokens = text.split(/[^a-zA-Z0-9_-]+/);
  for (const compound of compoundTokens) {
    if (!compound) continue;

    // Handle hyphen/underscore joined tokens -> index joined form
    if (compound.includes('-') || compound.includes('_')) {
      const parts = compound.split(/[-_]+/).filter((p) => p.length >= 2);
      if (parts.length > 1) {
        const joined = parts.join('').toLowerCase();
        if (joined.length >= 2 && !STOPWORDS.has(joined)) {
          result.push(joined);
        }
      }
    }

    // Handle camelCase/PascalCase
    if (/[a-z][A-Z]/.test(compound)) {
      for (const part of splitCamelCase(compound)) {
        if (part.length >= 2 && !STOPWORDS.has(part)) {
          result.push(part);
        }
      }
    }
  }

  return [...new Set(result)];
}
