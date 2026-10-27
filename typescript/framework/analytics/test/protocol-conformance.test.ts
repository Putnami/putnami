import { describe, expect } from 'bun:test';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { sanitizeBatch } from '../src/server/sanitize/sanitizer';
import { DROP_REASONS } from '../src/server/sanitize/vocabulary';

// Each binding proves a requirement of the analytics protocol's feature
// under a check of this package's own.
const VALID_CORPUS = {
  feature: 'analytics/collection-wire-contract',
  requirement: 'the-fixture-corpus-is-the-contract',
  check: 'the-typescript-sanitizer-accepts-every-valid-batch',
};
const INVALID_CORPUS = {
  feature: 'analytics/collection-wire-contract',
  requirement: 'the-fixture-corpus-is-the-contract',
  check: 'the-typescript-sanitizer-drops-each-invalid-batch-with-its-code',
};
const GOLDEN = {
  feature: 'analytics/collection-wire-contract',
  requirement: 'the-fixture-corpus-is-the-contract',
  check: 'the-typescript-sanitizer-accepts-the-golden-batch',
};

// The shared fixture corpus of protocols/analytics is the cross-language test
// surface: the Go validator runs these same files in
// protocols/analytics/conformance_test.go. Running them through the TypeScript
// sanitizer here is what proves the two runtimes agree — a reject branch no
// fixture exercises is one this side could silently omit.
const FIXTURES_DIR = join(__dirname, '../../../../protocols/analytics/fixtures');

// The corpus is authored against an application that declared these three
// action names: `signup_click` in action.json and the golden batch, `cta_click`
// in mixed-max.json. `search` is the second name of the epic's example
// declaration. The declared-name rule is TypeScript-only, so the Go twin cannot
// carry this set and the corpus does not name it.
const DECLARED = new Set(['signup_click', 'search', 'cta_click']);

type Fixture = { name: string; data: unknown; events: number };

function loadFixtures(kind: 'valid' | 'invalid'): Fixture[] {
  const dir = join(FIXTURES_DIR, 'batch', kind);
  return readdirSync(dir)
    .filter((name) => name.endsWith('.json'))
    .sort()
    .map((name) => {
      const data = JSON.parse(readFileSync(join(dir, name), 'utf8')) as { events?: unknown[] };
      return { name, data, events: Array.isArray(data.events) ? data.events.length : 0 };
    });
}

const VALID = loadFixtures('valid');
const INVALID = loadFixtures('invalid');

describe('protocol conformance: valid corpus', () => {
  specTest('reads the four valid batches', VALID_CORPUS, () => {
    expect(VALID.map((fixture) => fixture.name)).toEqual([
      'action.json',
      'full-page-view.json',
      'minimal-page-view.json',
      'mixed-max.json',
    ]);
  });

  for (const fixture of VALID) {
    specTest(`accepts every event of ${fixture.name}`, VALID_CORPUS, () => {
      const result = sanitizeBatch(fixture.data, DECLARED);

      expect(result.dropped).toEqual([]);
      expect(result.events).toHaveLength(fixture.events);
      expect(result.sentAt).toBeInstanceOf(Date);
    });
  }
});

describe('protocol conformance: invalid corpus', () => {
  specTest('reads one fixture per Go error code, minus parse_error', INVALID_CORPUS, () => {
    expect(INVALID).toHaveLength(17);
  });

  for (const fixture of INVALID) {
    const stem = fixture.name.replace(/\.json$/, '');
    specTest(`drops ${fixture.name} with reason ${stem}`, INVALID_CORPUS, () => {
      const result = sanitizeBatch(fixture.data, DECLARED);
      const reasons = result.dropped.map((drop) => drop.reason);

      // Four fixtures legitimately report two reasons: putting an action or a
      // property on a page view is both misplaced and still checked for shape,
      // exactly as the Go validator reports it. The stem must be among them.
      expect(reasons).toContain(stem);
      expect(result.events).toHaveLength(0);
    });
  }

  specTest('never invents a reason outside the closed vocabulary', INVALID_CORPUS, () => {
    for (const fixture of INVALID) {
      for (const drop of sanitizeBatch(fixture.data, DECLARED).dropped) {
        expect(DROP_REASONS).toContain(drop.reason);
      }
    }
  });
});

describe('protocol conformance: the golden batch', () => {
  specTest('round-trips the one importable reference payload', GOLDEN, () => {
    const golden = JSON.parse(readFileSync(join(FIXTURES_DIR, 'equivalence', 'batch.golden.json'), 'utf8')) as {
      events: unknown[];
    };

    const result = sanitizeBatch(golden, DECLARED);

    expect(result.dropped).toEqual([]);
    expect(result.events).toHaveLength(golden.events.length);
    expect(result.events.map((event) => event.kind)).toEqual(['page_view', 'action']);
  });
});
