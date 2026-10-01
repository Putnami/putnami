import { describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import type { ArchitectureImport } from '../../src/architecture/contract.types';
import { parseDuration, validateArchitectureImport } from '../../src/architecture/contract';

/**
 * The cross-language conformance corpus, TypeScript side.
 *
 * `protocols/architecture/fixtures/conformance/contract-validation.json` states
 * which import contracts the protocol refuses and where. The Go runner is
 * `protocols/architecture/cross_language_conformance_test.go`, and both read the
 * same file: a rule that exists in one language only fails on that side rather
 * than shipping as two different answers to "is this contract valid".
 *
 * A divergence is never fixed by editing the corpus. Decide which runtime is
 * right first.
 */

const CORPUS_PATH = join(
  __dirname,
  '../../../../../protocols/architecture/fixtures/conformance/contract-validation.json',
);

interface CorpusDiagnostic {
  readonly code: string;
  readonly field: string;
}

interface CorpusCase {
  readonly name: string;
  readonly base: string;
  readonly set?: Record<string, unknown>;
  readonly remove?: string[];
  readonly diagnostics: CorpusDiagnostic[];
}

interface Corpus {
  readonly bases: Record<string, Record<string, unknown>>;
  readonly cases: CorpusCase[];
}

const corpus = JSON.parse(readFileSync(CORPUS_PATH, 'utf8')) as Corpus;

/** Apply the case's shallow top-level merge to its named base. */
function buildContract(testCase: CorpusCase): ArchitectureImport {
  const base = corpus.bases[testCase.base];
  if (!base) throw new Error(`case "${testCase.name}" names base "${testCase.base}", which the corpus does not define`);
  const document: Record<string, unknown> = { ...base, ...(testCase.set ?? {}) };
  for (const member of testCase.remove ?? []) delete document[member];
  return document as unknown as ArchitectureImport;
}

function sortDiagnostics(diagnostics: CorpusDiagnostic[]): CorpusDiagnostic[] {
  return [...diagnostics].sort((left, right) =>
    left.field === right.field ? left.code.localeCompare(right.code) : left.field.localeCompare(right.field),
  );
}

function runCase(testCase: CorpusCase): void {
  const got = validateArchitectureImport(buildContract(testCase))
    .filter((diagnostic) => diagnostic.severity === 'error')
    .map(({ code, field }) => ({ code, field }));
  expect(sortDiagnostics(got)).toEqual(sortDiagnostics(testCase.diagnostics));
}

describe('ARC contract validation conformance', () => {
  specTest(
    'answers every case of the shared contract-validation corpus',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'darc-runtime-enforcement',
      check: 'the-shared-contract-validation-corpus-is-answered-case-for-case',
    },
    () => {
      for (const testCase of corpus.cases) runCase(testCase);
      // A corpus that shrank silently would make this check pass by asserting
      // nothing, so the count is part of the claim.
      expect(corpus.cases.length).toBeGreaterThanOrEqual(61);
    },
  );

  for (const testCase of corpus.cases) {
    it(testCase.name, () => {
      runCase(testCase);
    });
  }
});

describe('Go duration parsing', () => {
  it('truncates fractional nanoseconds before returning milliseconds', () => {
    expect(parseDuration('0.1ns')).toBe(0);
    expect(parseDuration('1.1ns')).toBe(0.000_001);
    expect(parseDuration('0.9223372036854775809h')).toBe(parseDuration('0.922337203685477580h'));
  });

  it('refuses durations outside the signed-int64 nanosecond range', () => {
    expect(parseDuration('3000000h')).toBeUndefined();
    expect(parseDuration('2562047h47m16.854775807s')).toBe(9_223_372_036_854.775);
    expect(parseDuration('2562047h47m16.854775808s')).toBeUndefined();
    expect(parseDuration('-2562047h47m16.854775808s')).toBe(-9_223_372_036_854.775);
  });
});
