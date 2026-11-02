/**
 * Serialize a value to JSON safe for embedding inside an HTML `<script>` block.
 *
 * Escapes sequences that would otherwise allow content to break out of the
 * script context (`</script>`, `<!--`) or trigger charset-based XSS
 * (`\u2028`, `\u2029`).
 */
export function safeJsonForScript(value: unknown): string {
  return JSON.stringify(value)
    .replace(/</g, '\\u003c')
    .replace(/>/g, '\\u003e')
    .replace(/\u2028/g, '\\u2028')
    .replace(/\u2029/g, '\\u2029');
}
