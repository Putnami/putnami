/**
 * Generates a UUID version 7 in the browser (body §D.12).
 *
 * The same twenty lines as `src/server/uuidv7.ts`, with
 * `crypto.getRandomValues` in place of `node:crypto` — the tracker may not
 * import a `node:` module, so the copy is the boundary and
 * `test/uuidv7.test.ts` pins the two against the protocol regex.
 *
 * The id is what makes a retried beacon idempotent: it is minted once, stored
 * in the queue, and never regenerated, so a batch that arrives twice folds
 * into one row through `ON CONFLICT (event_id)`.
 *
 * @param now - The millisecond timestamp to encode; defaults to the clock.
 * @returns A lowercase, dashed UUID v7.
 */
export function uuidv7(now: number = Date.now()): string {
  const bytes = new Uint8Array(16);
  let ms = Math.floor(now);
  for (let index = 5; index >= 0; index--) {
    bytes[index] = ms % 256;
    ms = Math.floor(ms / 256);
  }
  const random = new Uint8Array(10);
  crypto.getRandomValues(random);
  bytes[6] = 0x70 | (random[0] & 0x0f);
  bytes[7] = random[1];
  bytes[8] = 0x80 | (random[2] & 0x3f);
  bytes.set(random.subarray(3), 9);

  let hex = '';
  for (const byte of bytes) {
    hex += byte.toString(16).padStart(2, '0');
  }
  return `${hex.slice(0, 8)}-${hex.slice(8, 12)}-${hex.slice(12, 16)}-${hex.slice(16, 20)}-${hex.slice(20)}`;
}
