import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/spectest';
import {
  DOCUMENT_KIND,
  MACHINE_OUTPUT_BUDGETS,
  REPORT_MAX_JOB_DIAGNOSTICS,
  REPORT_MAX_JOBS,
  REPORT_MAX_MESSAGE_BYTES,
  RESULT_PROTOCOL_VERSION,
  RESULT_V2_SCHEMA_ID,
  TEST_CASE_MAX_BYTES_PER_BATCH,
  TEST_CASE_MAX_BYTES_PER_TASK,
  TEST_CASE_MAX_OUTPUT_BYTES,
  TEST_CASE_MAX_PER_TASK,
  TEST_CASE_MAX_TEXT_BYTES,
  TEST_CASE_STATUS,
  type TestCase,
  V2_FIELD_NAMES,
  VIOLATION_CODE,
  type Violation,
  validateDocument,
  validateSessionStream,
} from '../src/index';

// The v2 result contract's cross-language corpus.
//
// protocols/cli/conformance/manifest.json is ONE file read by both runtimes:
// this suite and the Go TestConformanceCorpus. Each case pins the EXACT
// violation set (codes and paths, sorted) its document produces, so a validator
// that drifts in either language — a different code, a different path, one
// violation too many or too few — fails that language's tests against a corpus
// the other still passes.
//
// A contract change lands as ONE commit touching three places: this corpus, the
// Go validator (protocols/cli/result_v2_rules.go), and the TypeScript validator
// (src/result-v2.ts). Never loosen a case to make one runtime pass.

const PROTOCOL_DIR = join(__dirname, '../../../../protocols/cli');
const CORPUS_PATH = join(PROTOCOL_DIR, 'conformance/manifest.json');
const PACK_PATH = join(PROTOCOL_DIR, 'conformance/pack.json');
const SCHEMA_PATH = join(PROTOCOL_DIR, 'schemas/result-v2.json');
// The total-budget corpus case deliberately parses and sanitizes a 1.1 MiB
// stream twice. Give that bounded stress case headroom when the full gate is
// also compiling and testing the rest of the workspace.
const LARGE_STREAM_CASE_TIMEOUT_MS = 15_000;

interface ConformanceCase {
  id: string;
  document: string;
  expect: 'accept' | 'reject';
  summary: string;
  value?: unknown;
  raw?: string;
  violations?: Violation[];
}

interface ConformanceManifest {
  protocol: string;
  suite: string;
  protocolVersion: number;
  documents: string[];
  cases: ConformanceCase[];
  streamCases: ConformanceStreamCase[];
}

interface ConformanceStreamCase {
  id: string;
  expect: 'accept' | 'reject';
  summary: string;
  live: string;
  artifact: string;
  replacements?: Array<{ token: string; text: string; count: number }>;
  violations?: Violation[];
}

function streamTexts(testCase: ConformanceStreamCase): { live: string; artifact: string } {
  let live = testCase.live;
  let artifact = testCase.artifact;
  for (const replacement of testCase.replacements ?? []) {
    const value = replacement.text.repeat(replacement.count);
    live = live.split(replacement.token).join(value);
    artifact = artifact.split(replacement.token).join(value);
  }
  return { live, artifact };
}

const corpus = JSON.parse(readFileSync(CORPUS_PATH, 'utf8')) as ConformanceManifest;

function documentText(testCase: ConformanceCase): string {
  if (testCase.raw !== undefined) {
    return testCase.raw;
  }
  return JSON.stringify(testCase.value);
}

const FEATURE = 'typescript/cli-machine-output';

// Most corpus fixtures prove the shared-corpus requirement; a few named
// fixtures are the executable evidence for a more specific clause and are
// bound there instead, so the gate can tell those clauses apart.
const STREAM_MODE_POLICY_CASES = new Set([
  'stream-sequence.normal-suppresses-debug-detail',
  'stream-sequence.verbose-admits-normal-overflow',
  'stream-sequence.split-elisions-are-exact',
  'stream-sequence.failure-reserve-is-not-reclaimed',
  'stream-sequence.normal-elides-every-test-case',
  'stream-sequence.verbose-admits-every-test-case',
]);

function corpusBinding(id: string): { feature: string; requirement: string; check: string } {
  if (id === 'envelope.run.aborted-keeps-failures-visible') {
    return { feature: FEATURE, requirement: 'strict-verdict', check: 'an-aborted-run-keeps-failures-visible' };
  }
  if (STREAM_MODE_POLICY_CASES.has(id)) {
    return { feature: FEATURE, requirement: 'bounded-stream-policy', check: 'the-stream-mode-policy-is-enforced' };
  }
  return { feature: FEATURE, requirement: 'shared-corpus', check: 'every-fixture-produces-its-pinned-violations' };
}

describe('CLI result v2 conformance corpus', () => {
  it('agrees with the corpus header', () => {
    expect(corpus.protocol).toBe('putnami.cli.result.v2');
    expect(corpus.protocolVersion).toBe(RESULT_PROTOCOL_VERSION);
    expect([...corpus.documents].sort()).toEqual(Object.values(DOCUMENT_KIND).sort());
    expect(corpus.cases.length).toBeGreaterThan(0);
  });

  for (const testCase of corpus.cases) {
    specTest(`${testCase.id} — ${testCase.summary}`, corpusBinding(testCase.id), () => {
      const expected = testCase.expect === 'accept' ? [] : (testCase.violations ?? []);
      if (testCase.expect === 'reject') {
        expect(expected.length).toBeGreaterThan(0);
        for (const violation of expected) {
          expect(Object.values(VIOLATION_CODE)).toContain(violation.code);
        }
      } else {
        expect(testCase.violations).toBeUndefined();
      }
      // A mismatch here means the change forgot the Go validator or the corpus:
      // update the fixture AND the other runtime.
      expect(validateDocument(testCase.document, documentText(testCase))).toEqual(expected);
    });
  }

  for (const testCase of corpus.streamCases) {
    specTest(
      `${testCase.id} — ${testCase.summary}`,
      corpusBinding(testCase.id),
      () => {
        const expected = testCase.expect === 'accept' ? [] : (testCase.violations ?? []);
        const { live, artifact } = streamTexts(testCase);
        expect(validateSessionStream(live, artifact)).toEqual(expected);
      },
      testCase.id === 'invalid.stream-sequence.total-budget-and-accounting' ? LARGE_STREAM_CASE_TIMEOUT_MS : undefined,
    );
  }

  specTest(
    'covers every document kind and every violation code',
    { feature: FEATURE, requirement: 'shared-corpus', check: 'the-corpus-covers-every-kind-and-code' },
    () => {
      const documents = new Set(corpus.cases.map((testCase) => testCase.document));
      const codes = new Set(
        [...corpus.cases, ...corpus.streamCases].flatMap((testCase) => (testCase.violations ?? []).map((v) => v.code)),
      );
      expect([...documents].sort()).toEqual(Object.values(DOCUMENT_KIND).sort());
      expect([...codes].sort()).toEqual(Object.values(VIOLATION_CODE).sort());
    },
  );
});

interface JsonSchemaDef {
  const?: unknown;
  properties?: Record<string, unknown>;
  required?: string[];
  allOf?: Array<{
    if?: { properties?: { mode?: { const?: string } } };
    then?: { properties?: { budget?: { properties?: Record<string, { const?: number }> } } };
  }>;
  oneOf?: unknown[];
}

const schema = JSON.parse(readFileSync(SCHEMA_PATH, 'utf8')) as {
  $id: string;
  $defs: Record<string, JsonSchemaDef>;
};

describe('CLI result v2 schema binding', () => {
  it('agrees with the schema $id constant', () => {
    expect(schema.$id).toBe(RESULT_V2_SCHEMA_ID);
  });

  specTest(
    'pins protocolVersion against the schema const',
    { feature: FEATURE, requirement: 'version-token', check: 'version-2-token-matches-the-schema-const' },
    () => {
      expect((schema.$defs['protocolVersion'] as unknown as { const: number }).const).toBe(RESULT_PROTOCOL_VERSION);
    },
  );

  // The TypeScript member lists are what validateDocument enforces, so pinning
  // them against the schema is the same lock the Go drift test applies. A member
  // added to the schema without a validator fails here.
  for (const [name, fields] of Object.entries(V2_FIELD_NAMES)) {
    specTest(
      `${name} members match the schema $def`,
      { feature: FEATURE, requirement: 'schema-pinned-members', check: 'every-document-member-set-is-pinned' },
      () => {
        const def = schema.$defs[name];
        expect(def).toBeDefined();
        expect(Object.keys(def?.properties ?? {}).sort()).toEqual(fields.members);
        expect([...(def?.required ?? [])].sort()).toEqual(fields.required);
      },
    );
  }

  // The report's caps are a CONTRACT clause, so three parties must agree on
  // them: these constants, the Go constants a producer sizes its lists from, and
  // the schema a non-Go consumer binds to. The Go drift test applies the same
  // pin against the same file, which is what transitively ties the two runtimes'
  // numbers together.
  specTest(
    'pins the report bounds against the schema',
    { feature: FEATURE, requirement: 'schema-pinned-bounds', check: 'report-bounds-come-from-the-schema' },
    () => {
      const jobs = schema.$defs['reportFile']?.properties?.['jobs'] as { maxItems?: number };
      const diagnostics = schema.$defs['reportJob']?.properties?.['diagnostics'] as {
        maxItems?: number;
        items?: { properties?: { message?: { maxLength?: number } } };
      };
      expect(jobs.maxItems).toBe(REPORT_MAX_JOBS);
      expect(diagnostics.maxItems).toBe(REPORT_MAX_JOB_DIAGNOSTICS);
      expect(diagnostics.items?.properties?.message?.maxLength).toBe(REPORT_MAX_MESSAGE_BYTES);
    },
  );

  // The test-case bounds are pinned the same way: TEST_CASE_MAX_PER_TASK and
  // the byte budgets span records, so the schema states them as value-only
  // $defs, and the text and output bounds are the maxLength of the members
  // they bound.
  specTest(
    'pins the test-case bounds, statuses and members against the schema',
    { feature: FEATURE, requirement: 'schema-pinned-bounds', check: 'test-case-bounds-come-from-the-schema' },
    () => {
      const members = (schema.$defs['testCase']?.properties ?? {}) as Record<
        string,
        { maxLength?: number; enum?: string[] }
      >;
      expect(schema.$defs['testCaseMaxPerTask']?.const).toBe(TEST_CASE_MAX_PER_TASK);
      expect(schema.$defs['testCaseMaxBytesPerTask']?.const).toBe(TEST_CASE_MAX_BYTES_PER_TASK);
      expect(schema.$defs['testCaseMaxBytesPerBatch']?.const).toBe(TEST_CASE_MAX_BYTES_PER_BATCH);
      expect(members['name']?.maxLength).toBe(TEST_CASE_MAX_TEXT_BYTES);
      expect(members['suite']?.maxLength).toBe(TEST_CASE_MAX_TEXT_BYTES);
      expect(members['file']?.maxLength).toBe(TEST_CASE_MAX_TEXT_BYTES);
      expect(members['output']?.maxLength).toBe(TEST_CASE_MAX_OUTPUT_BYTES);
      expect([...(members['status']?.enum ?? [])].sort()).toEqual(Object.values(TEST_CASE_STATUS).sort());
      // Required<TestCase> fails to compile when the interface loses a member,
      // and the key comparison fails when it gains one the schema lacks.
      const everyMember: Required<TestCase> = {
        name: 'TestA',
        suite: 'example.com/a',
        status: TEST_CASE_STATUS.failed,
        durationMs: 1,
        output: 'boom',
        outputTruncated: true,
        file: 'a_test.go',
        line: 1,
      };
      expect(Object.keys(everyMember).sort()).toEqual(Object.keys(members).sort());
    },
  );

  specTest(
    'pins both machine-output budgets and the elision zero-pair rule against the schema',
    { feature: FEATURE, requirement: 'schema-pinned-bounds', check: 'machine-output-budgets-come-from-the-schema' },
    () => {
      const rules = schema.$defs['machineOutputSummary']?.allOf ?? [];
      const schemaBudgets = Object.fromEntries(
        rules.map((rule) => [rule.if?.properties?.mode?.const, rule.then?.properties?.budget?.properties]),
      );
      for (const [mode, budget] of Object.entries(MACHINE_OUTPUT_BUDGETS)) {
        const members = schemaBudgets[mode];
        expect(members).toBeDefined();
        for (const [name, value] of Object.entries(budget)) {
          expect(members?.[name]?.const).toBe(value);
        }
      }
      expect(schema.$defs['machineOutputElision']?.oneOf).toHaveLength(2);
    },
  );

  specTest(
    'validates every object $def the schema declares',
    { feature: FEATURE, requirement: 'schema-pinned-members', check: 'every-schema-def-has-a-pinned-member-list' },
    () => {
      const objectDefs = Object.entries(schema.$defs)
        .filter(([, def]) => Object.keys(def.properties ?? {}).length > 0)
        .map(([name]) => name)
        .sort();
      expect(objectDefs).toEqual(Object.keys(V2_FIELD_NAMES).sort());
    },
  );
});

describe('CLI result v2 conformance pack', () => {
  it('pins the committed pack-manifest convention', () => {
    const pack = JSON.parse(readFileSync(PACK_PATH, 'utf8')) as {
      id: string;
      corpus: string;
      capabilityKinds: string[];
      languages: string[];
    };
    expect(pack.id).toBe('putnami.cli.result.conformance');
    expect(pack.corpus).toBe('manifest.json');
    expect(pack.capabilityKinds).toEqual([]);
    expect(pack.languages).toEqual(['go', 'typescript']);
  });
});
