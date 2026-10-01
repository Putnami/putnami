/**
 * Shared HTML escaping utilities.
 *
 * A single, security-sensitive escaper used across the framework's SSR render
 * path and markdown rendering. Several call sites previously hand-rolled the
 * same `.replace(/&/g, '&amp;')…` chain; consolidating them here keeps the
 * entity set consistent and lets the hot path use Bun's SIMD-accelerated
 * `Bun.escapeHTML` when available.
 *
 * Entity set (identical in both the Bun and fallback paths):
 *   `&` → `&amp;`   `<` → `&lt;`   `>` → `&gt;`   `"` → `&quot;`   `'` → `&#39;`
 *
 * `Bun.escapeHTML` emits `&#x27;` (hex) for single quotes; we normalize it to
 * the decimal `&#39;` so output is byte-identical to the historical escaper
 * (and existing XSS/escaping tests). Both forms decode to the same apostrophe.
 */

// Captured once at module load. Resolves to `Bun.escapeHTML` when running under
// Bun (SSR), and `undefined` in browser bundles where the JS fallback is used.
// Read through `globalThis` so the reference type-checks without the Bun global.
const bunEscapeHtml: ((input: string) => string) | undefined = (
  globalThis as { Bun?: { escapeHTML?: (input: string) => string } }
).Bun?.escapeHTML;

/**
 * Escape a string for safe interpolation into HTML text or double-quoted
 * attribute values. Escapes `&`, `<`, `>`, `"`, and `'`.
 */
export function escapeHtml(value: string): string {
  if (bunEscapeHtml !== undefined) {
    const escaped = bunEscapeHtml(value);
    // Only single quotes differ between Bun (`&#x27;`) and our canonical
    // `&#39;`; skip the extra pass when the input contains none.
    return value.includes("'") ? escaped.replace(/&#x27;/g, '&#39;') : escaped;
  }
  return value
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#39;');
}
