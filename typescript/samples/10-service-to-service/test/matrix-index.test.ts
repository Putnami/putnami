import { describe, expect } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

// The machine index (test-scenarios.json) is a claim about this repository, and
// a claim nothing checks rots. This is what makes it evidence: every scenario
// names a test that exists, every cell of the matrix appears for every consumer
// and every family, and every combination that is not proven says why — a
// protocol rule for `n/a`, a reason for `missing`.

const PROJECT = join(import.meta.dir, '..');

interface MatrixIndex {
  readonly protocolVersion: number;
  readonly matrix: string;
  readonly cells: ReadonlyArray<{ readonly id: string }>;
  readonly families: ReadonlyArray<{ readonly id: string }>;
  readonly consumers: ReadonlyArray<{ readonly target: string }>;
  readonly scenarios: ReadonlyArray<{
    readonly id: string;
    readonly test: string;
    readonly result: string;
    readonly auth: string;
    readonly policy: string;
    readonly encoding: string;
  }>;
  readonly coverage: ReadonlyArray<{
    readonly target: string;
    readonly cell: string;
    readonly family: string;
    readonly status: string;
    readonly scenarios?: readonly string[];
    readonly rule?: string;
    readonly note?: string;
  }>;
}

const index = JSON.parse(readFileSync(join(PROJECT, 'test-scenarios.json'), 'utf8')) as MatrixIndex;

/** The test symbols a file declares: `func TestX` for Go, `it(…)` for bun. */
function declaredTests(path: string): Set<string> {
  const content = readFileSync(join(PROJECT, path), 'utf8');
  const declared = new Set<string>();
  if (path.endsWith('.go')) {
    for (const match of content.matchAll(/^func (Test\w+)/gm)) declared.add(match[1] as string);
    return declared;
  }
  // A bun test names itself with either quote style, through `it` or through
  // `specTest` when it also publishes a declared check. The index quotes the
  // name exactly as the file does.
  for (const match of content.matchAll(/^\s*(?:it|specTest)\(\s*(?:'([^']+)'|"([^"]+)")/gm)) {
    declared.add((match[1] ?? match[2]) as string);
  }
  return declared;
}

/** The feature this sample owns in putnami.features.json. */
const FEATURE = 'samples/ts-first-party-client-matrix';

describe('matrix index', () => {
  specTest(
    'names tests that exist',
    { feature: FEATURE, requirement: 'the-matrix-index-is-checkable', check: 'the-index-names-tests-that-exist' },
    () => {
      expect(index.protocolVersion).toBe(2);
      expect(index.matrix).toBe('client-matrix');

      const byFile = new Map<string, Set<string>>();
      for (const scenario of index.scenarios) {
        const separator = scenario.test.indexOf(':');
        expect(separator).toBeGreaterThan(0);
        const file = scenario.test.slice(0, separator);
        const symbol = scenario.test.slice(separator + 1);
        if (!byFile.has(file)) byFile.set(file, declaredTests(file));
        // Named with the scenario id: a failure says which row rotted.
        expect({ scenario: scenario.id, declares: byFile.get(file)?.has(symbol) }).toEqual({
          scenario: scenario.id,
          declares: true,
        });
        expect(scenario.result).toBe('pass');
        expect(scenario.auth).not.toBe('');
        expect(scenario.policy).not.toBe('');
        expect(scenario.encoding).not.toBe('');
      }
    },
  );

  specTest(
    'states something about every cell and every family, for every consumer',
    {
      feature: FEATURE,
      requirement: 'the-matrix-index-is-checkable',
      check: 'the-index-states-every-cell-and-family-for-every-consumer',
    },
    () => {
      const ids = new Set(index.scenarios.map((scenario) => scenario.id));
      expect(ids.size).toBe(index.scenarios.length);

      const seen = new Set<string>();
      for (const entry of index.coverage) {
        const key = `${entry.target}/${entry.cell}/${entry.family}`;
        expect(seen.has(key)).toBe(false);
        seen.add(key);
        if (entry.status === 'pass') {
          expect(entry.scenarios?.length ?? 0).toBeGreaterThan(0);
          for (const id of entry.scenarios ?? []) {
            expect({ key, known: ids.has(id) }).toEqual({ key, known: true });
          }
        } else if (entry.status === 'n/a') {
          // A combination is inapplicable because a protocol says so, never
          // because nothing implements it.
          expect({ key, rule: (entry.rule ?? '').length > 0 }).toEqual({ key, rule: true });
        } else if (entry.status === 'missing') {
          expect({ key, note: (entry.note ?? '').length > 0 }).toEqual({ key, note: true });
        } else {
          throw new Error(`${key} has unknown status ${entry.status}`);
        }
      }

      for (const consumer of index.consumers) {
        for (const cell of index.cells) {
          for (const family of index.families) {
            const key = `${consumer.target}/${cell.id}/${family.id}`;
            expect({ key, stated: seen.has(key) }).toEqual({ key, stated: true });
          }
        }
      }
    },
  );
});
