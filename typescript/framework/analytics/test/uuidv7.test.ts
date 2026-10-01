import { describe, expect, it } from 'bun:test';
import { uuidv7 as clientUuidv7 } from '../src/client/uuidv7';
import { UUID_V7_RE } from '../src/server/sanitize/vocabulary';
import { uuidv7 } from '../src/server/uuidv7';

/** The two implementations are a deliberate copy; every rule applies to both. */
const IMPLEMENTATIONS: [string, (now?: number) => string][] = [
  ['server', uuidv7],
  ['browser', clientUuidv7],
];

describe.each(IMPLEMENTATIONS)('uuidv7 (%s)', (_name, generate) => {
  it('emits an id the protocol regex accepts', () => {
    for (let i = 0; i < 50; i++) {
      expect(generate()).toMatch(UUID_V7_RE);
    }
  });

  it('encodes the millisecond in the first 48 bits', () => {
    const ms = Date.UTC(2026, 8, 2, 10, 0, 0);
    const id = generate(ms);
    const encoded = Number.parseInt(id.slice(0, 8) + id.slice(9, 13), 16);

    expect(encoded).toBe(ms);
  });

  it('sorts lexicographically by time', () => {
    const early = generate(Date.UTC(2026, 0, 1));
    const late = generate(Date.UTC(2026, 8, 2));

    expect(early < late).toBe(true);
  });

  it('never repeats within the same millisecond', () => {
    const ms = Date.UTC(2026, 8, 2);
    const ids = new Set(Array.from({ length: 1000 }, () => generate(ms)));

    expect(ids.size).toBe(1000);
  });
});

describe('uuidv7 copies', () => {
  // The tracker cannot import node:crypto, so the twenty lines exist twice.
  // This is the test that keeps the two shapes from drifting.
  it('agree on the encoded timestamp and the version and variant nibbles', () => {
    const ms = Date.UTC(2026, 8, 2, 8, 30, 0);
    const server = uuidv7(ms);
    const browser = clientUuidv7(ms);

    expect(browser.slice(0, 13)).toBe(server.slice(0, 13));
    expect(browser[14]).toBe('7');
    expect(server[14]).toBe('7');
    expect('89ab').toContain(browser[19] as string);
  });
});
