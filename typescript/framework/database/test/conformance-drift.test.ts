import { describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/spectest';

// The canonical transaction conformance corpus lives in
// protocols/transaction/conformance/manifest.json (embedded and exported by the
// Go protocol module). @putnami/database cannot reach into the Go module, so it
// ships a byte-identical committed copy under src/conformance/ that the exported
// runner loads. This guard fails loudly if the vendored copy drifts from the
// canonical source — the protocol remains the single source of truth.
const CANONICAL_PATH = join(__dirname, '../../../../protocols/transaction/conformance/manifest.json');
const VENDORED_PATH = join(__dirname, '../src/conformance/manifest.json');

describe('conformance corpus drift guard', () => {
  specTest(
    'vendored corpus is byte-identical to the canonical protocol corpus',
    {
      feature: 'typescript/relational-persistence',
      requirement: 'cross-language-conformance',
      check: 'the-vendored-corpus-is-byte-identical',
    },
    () => {
      const canonical = readFileSync(CANONICAL_PATH, 'utf8');
      const vendored = readFileSync(VENDORED_PATH, 'utf8');
      expect(vendored).toBe(canonical);
    },
  );
});
