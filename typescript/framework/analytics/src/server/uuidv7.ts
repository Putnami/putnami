import { randomBytes } from 'node:crypto';

/**
 * Generates a UUID version 7 (body §D.12).
 *
 * Time-ordered by construction: the first 48 bits are the Unix millisecond,
 * so the primary key of `analytics_event` clusters by arrival instead of
 * scattering an index the way UUID v4 does. The browser generates the same
 * shape from `crypto.getRandomValues`, and the id is what makes a retried
 * beacon idempotent — it must never change across a retry.
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
  const random = randomBytes(10);
  bytes[6] = 0x70 | (random[0] & 0x0f);
  bytes[7] = random[1];
  bytes[8] = 0x80 | (random[2] & 0x3f);
  bytes.set(random.subarray(3), 9);

  const hex = Buffer.from(bytes).toString('hex');
  return [hex.slice(0, 8), hex.slice(8, 12), hex.slice(12, 16), hex.slice(16, 20), hex.slice(20)].join('-');
}
