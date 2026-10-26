import { describe, expect } from 'bun:test';
import { readdirSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/spectest';
import {
  isQualifyPass,
  QUALIFY_CONTRACT_SCHEMA_ID,
  QUALIFY_PHASE_NAMES,
  QUALIFY_PROTOCOL_VERSION,
  QUALIFY_SHAPES,
  QUALIFY_STATES,
  QUALIFY_VERDICT_SCHEMA_ID,
  QUALIFY_VIOLATION_CODE,
  type QualifyVerdict,
  qualifyContractDigest,
  validateQualifyContract,
  validateQualifyVerdict,
  type Violation,
} from '../src/index';

// The qualify corpus is ONE set of files read by both runtimes: this suite and
// the Go TestConformance_Corpus in protocols/qualify. expectations.json pins the
// exact distinct codes each invalid fixture produces, so a validator that drifts
// in either language fails against a corpus the other still passes. A contract
// change lands in the corpus, protocols/qualify/strict.go and src/qualify.ts
// together; never loosen a fixture to make one runtime pass.

const PROTOCOL_DIR = join(__dirname, '../../../../protocols/qualify');
const FIXTURES = join(PROTOCOL_DIR, 'fixtures');
// Each test proves a requirement of the qualify protocol's feature under a
// check of this package's own.
const SHAPES = {
  feature: 'qualify/workload-qualification-contract',
  requirement: 'one-member-set',
  check: 'the-typescript-shapes-and-vocabulary-match-the-schemas',
};
const CORPUS_LIST = {
  feature: 'qualify/workload-qualification-contract',
  requirement: 'one-fixture-corpus',
  check: 'the-typescript-corpus-is-what-expectations-lists',
};
const FIXTURE_CODES = {
  feature: 'qualify/workload-qualification-contract',
  requirement: 'one-fixture-corpus',
  check: 'the-typescript-reader-gives-every-fixture-its-listed-codes',
};
const DIGEST = {
  feature: 'qualify/workload-qualification-contract',
  requirement: 'the-contract-digest-is-canonical',
  check: 'the-typescript-digest-matches-the-pins',
};
const NON_DOCUMENTS = {
  feature: 'qualify/workload-qualification-contract',
  requirement: 'parsing-is-strict',
  check: 'the-typescript-reader-refuses-non-documents',
};
const CONTRACT_DEFECTS = {
  feature: 'qualify/workload-qualification-contract',
  requirement: 'contract-defects-get-their-codes',
  check: 'the-typescript-reader-refuses-malformed-contracts',
};
const VERDICT_DEFECTS = {
  feature: 'qualify/workload-qualification-contract',
  requirement: 'verdict-defects-get-their-codes',
  check: 'the-typescript-reader-gives-each-verdict-defect-its-code',
};
const UNPROVEN_PASS = {
  feature: 'qualify/workload-qualification-contract',
  requirement: 'a-pass-carries-its-proof',
  check: 'the-typescript-reader-refuses-an-unproven-pass',
};

interface Expectations {
  valid: string[];
  invalid: Record<string, string[]>;
}

const expectations = JSON.parse(readFileSync(join(FIXTURES, 'expectations.json'), 'utf8')) as Expectations;

function readJSON(path: string): unknown {
  return JSON.parse(readFileSync(path, 'utf8'));
}

// A contract- prefix is a contract document; every other fixture is a verdict.
// The Go conformance test applies the same rule.
async function validateFixture(name: string, value: unknown): Promise<Violation[]> {
  return name.startsWith('contract-') ? validateQualifyContract(value) : validateQualifyVerdict(value);
}

function distinctCodes(violations: Violation[]): string[] {
  return [...new Set(violations.map((violation) => violation.code))].sort();
}

function fixtureVerdict(name: string): QualifyVerdict {
  return readJSON(join(FIXTURES, 'valid', name)) as QualifyVerdict;
}

describe('qualify conformance corpus', () => {
  specTest('expectations list every fixture on disk and nothing else', CORPUS_LIST, () => {
    expect(readdirSync(join(FIXTURES, 'valid')).sort()).toEqual([...expectations.valid].sort());
    expect(readdirSync(join(FIXTURES, 'invalid')).sort()).toEqual(Object.keys(expectations.invalid).sort());
    expect(expectations.valid.length).toBeGreaterThan(0);
    expect(Object.keys(expectations.invalid).length).toBeGreaterThan(0);
  });

  for (const name of expectations.valid) {
    specTest(`valid/${name} validates cleanly`, FIXTURE_CODES, async () => {
      expect(await validateFixture(name, readJSON(join(FIXTURES, 'valid', name)))).toEqual([]);
    });
  }

  for (const [name, codes] of Object.entries(expectations.invalid)) {
    specTest(`invalid/${name} produces ${codes.join(', ')}`, FIXTURE_CODES, async () => {
      const violations = await validateFixture(name, readJSON(join(FIXTURES, 'invalid', name)));
      expect(distinctCodes(violations)).toEqual(codes);
      for (const code of distinctCodes(violations)) {
        expect(Object.values(QUALIFY_VIOLATION_CODE) as string[]).toContain(code);
      }
    });
  }

  specTest('the contract digest bytes match the Go pin', DIGEST, async () => {
    expect(await qualifyContractDigest([])).toBe(
      'sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945',
    );
    const contract = readJSON(join(FIXTURES, 'valid', 'contract-service-to-service.json')) as {
      requests: Parameters<typeof qualifyContractDigest>[0];
    };
    expect(await qualifyContractDigest(contract.requests)).toBe(
      'sha256:19782e4140d2d58581b8821654e85ea9c96deaf1b2683dd55d2912856c4d8adc',
    );
    const escaped = await qualifyContractDigest([
      { id: 'GET /a&b', method: 'GET', path: '/a&b', maxStatus: 499, provenance: 'manual' },
    ]);
    const bytes = new TextEncoder().encode(
      '[{"id":"GET /a&b","maxStatus":499,"method":"GET","path":"/a&b","provenance":"manual"}]',
    );
    const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', bytes));
    expect(escaped).toBe(`sha256:${Array.from(hash, (b) => b.toString(16).padStart(2, '0')).join('')}`);
  });
});

describe('qualify vocabulary and schema drift', () => {
  const verdictSchema = readJSON(join(PROTOCOL_DIR, 'schemas/verdict.json')) as SchemaNode;
  const contractSchema = readJSON(join(PROTOCOL_DIR, 'schemas/contract.json')) as SchemaNode;

  interface SchemaNode {
    $id?: string;
    $ref?: string;
    $defs?: Record<string, SchemaNode>;
    properties?: Record<string, SchemaNode>;
    items?: SchemaNode;
    enum?: string[];
    type?: string;
  }

  type Shape = (typeof QUALIFY_SHAPES)['verdict'];

  function resolve(root: SchemaNode, node: SchemaNode): SchemaNode {
    if (node.$ref?.startsWith('#/$defs/')) {
      const target = root.$defs?.[node.$ref.slice('#/$defs/'.length)];
      if (!target) throw new Error(`unresolved ${node.$ref}`);
      return target;
    }
    return node;
  }

  // Every object in a shape carries exactly the members its schema declares.
  function assertShapeMatchesSchema(root: SchemaNode, node: SchemaNode, shape: Shape | string, path: string): void {
    const resolved = resolve(root, node);
    if (typeof shape === 'string') return;
    if ('array' in shape) {
      expect(resolved.items, `${path} items`).toBeDefined();
      assertShapeMatchesSchema(root, resolved.items as SchemaNode, shape.array as Shape, `${path}[]`);
      return;
    }
    const members = Object.keys((shape as { object: Record<string, Shape> }).object).sort();
    expect(Object.keys(resolved.properties ?? {}).sort(), path).toEqual(members);
    for (const member of members) {
      const child = (shape as { object: Record<string, Shape> }).object[member] as Shape;
      assertShapeMatchesSchema(
        root,
        (resolved.properties as Record<string, SchemaNode>)[member],
        child,
        `${path}.${member}`,
      );
    }
  }

  specTest('the twin vocabulary and shapes match the published schemas', SHAPES, () => {
    expect(QUALIFY_PROTOCOL_VERSION).toBe(1);
    expect(verdictSchema.$id).toBe(QUALIFY_VERDICT_SCHEMA_ID);
    expect(contractSchema.$id).toBe(QUALIFY_CONTRACT_SCHEMA_ID);
    expect(verdictSchema.$defs?.['state']?.enum).toEqual([...QUALIFY_STATES]);
    expect(verdictSchema.properties?.['phases']?.items?.properties?.['name']?.enum).toEqual([...QUALIFY_PHASE_NAMES]);
    assertShapeMatchesSchema(verdictSchema, verdictSchema, QUALIFY_SHAPES.verdict, 'verdict');
    assertShapeMatchesSchema(contractSchema, contractSchema, QUALIFY_SHAPES.contract as Shape, 'contract');
    for (const state of QUALIFY_STATES) {
      expect(isQualifyPass(state)).toBe(state === 'passed');
    }
  });
});

describe('qualify validator rules (mirrors protocols/qualify conformance_test.go)', () => {
  function clone<T>(value: T): T {
    return JSON.parse(JSON.stringify(value)) as T;
  }

  specTest('a passed verdict refuses every non-pass phase and request state', UNPROVEN_PASS, () => {
    const base = fixtureVerdict('passed-url.json');
    expect(validateQualifyVerdict(base)).toEqual([]);
    for (const [index] of base.phases.entries()) {
      for (const state of QUALIFY_STATES.filter((s) => s !== 'passed')) {
        const verdict = clone(base);
        (verdict.phases[index] as { state: string }).state = state;
        expect(distinctCodes(validateQualifyVerdict(verdict))).toEqual([QUALIFY_VIOLATION_CODE.unprovenPass]);
      }
    }
    for (const [index] of base.requests.entries()) {
      for (const state of QUALIFY_STATES.filter((s) => s !== 'passed')) {
        const verdict = clone(base);
        (verdict.requests[index] as { state: string }).state = state;
        expect(distinctCodes(validateQualifyVerdict(verdict))).toEqual([QUALIFY_VIOLATION_CODE.unprovenPass]);
      }
    }
    const partial = clone(base);
    partial.cleanup = { state: 'partial', leftovers: ['database compose_x'] };
    expect(distinctCodes(validateQualifyVerdict(partial))).toEqual([QUALIFY_VIOLATION_CODE.unprovenPass]);
  });

  specTest('non-documents are parse errors', NON_DOCUMENTS, async () => {
    for (const input of [[], null, 'text', { protocolVersion: '1' }]) {
      expect(distinctCodes(validateQualifyVerdict(input))).toEqual([QUALIFY_VIOLATION_CODE.parseError]);
      expect(distinctCodes(await validateQualifyContract(input))).toEqual([QUALIFY_VIOLATION_CODE.parseError]);
    }
    const nested = await validateQualifyContract({ requests: [{ id: 'GET /', extra: true }] });
    expect(nested).toEqual([{ code: QUALIFY_VIOLATION_CODE.unknownField, path: 'requests[0].extra' }]);
  });

  specTest('malformed contract requests', CONTRACT_DEFECTS, async () => {
    const violations = await validateQualifyContract({
      protocolVersion: 2,
      derivedFrom: [{ kind: 'openapi' }],
      requests: [
        { id: 'GET /a', method: 'GET', path: '/a', maxStatus: 499, provenance: 'typed-api' },
        { id: 'GET /a', method: 'GET', path: '/a', maxStatus: 499, provenance: 'typed-api' },
        { id: 'wrong', method: 'HEAD', path: 'b', maxStatus: 99, provenance: ' ' },
      ],
      digest: 'sha256:not-hex',
    });
    expect(distinctCodes(violations)).toEqual([
      QUALIFY_VIOLATION_CODE.invalidDigest,
      QUALIFY_VIOLATION_CODE.invalidRequest,
      QUALIFY_VIOLATION_CODE.invalidSource,
      QUALIFY_VIOLATION_CODE.required,
      QUALIFY_VIOLATION_CODE.unsupportedProtocolVersion,
    ]);
  });

  const shapeRules: Record<string, [(v: QualifyVerdict) => void, string]> = {
    'url target without url': [(v) => (v.target.url = undefined), QUALIFY_VIOLATION_CODE.invalidTarget],
    'unknown target kind': [(v) => (v.target.kind = 'ssh'), QUALIFY_VIOLATION_CODE.invalidTarget],
    'unknown binding kind': [(v) => (v.binding.kind = 'image'), QUALIFY_VIOLATION_CODE.invalidBinding],
    'repeated phase': [
      (v) => ((v.phases[1] as { name: string }).name = 'resolve-target'),
      QUALIFY_VIOLATION_CODE.invalidPhase,
    ],
    'negative phase time': [
      (v) => ((v.phases[0] as { durationMs: number }).durationMs = -1),
      QUALIFY_VIOLATION_CODE.invalidPhase,
    ],
    'unknown phase state': [
      (v) => ((v.phases[0] as { state: string }).state = 'ok'),
      QUALIFY_VIOLATION_CODE.invalidState,
    ],
    'unknown request state': [
      (v) => ((v.requests[0] as { state: string }).state = 'skipped'),
      QUALIFY_VIOLATION_CODE.invalidState,
    ],
    'request without id': [(v) => ((v.requests[0] as { id: string }).id = ''), QUALIFY_VIOLATION_CODE.required],
    'impossible status': [
      (v) => ((v.requests[0] as { status: number }).status = 42),
      QUALIFY_VIOLATION_CODE.invalidRequest,
    ],
    'negative request time': [
      (v) => ((v.requests[0] as { durationMs: number }).durationMs = -3),
      QUALIFY_VIOLATION_CODE.invalidRequest,
    ],
    'negative request count': [(v) => (v.contract.requests = -1), QUALIFY_VIOLATION_CODE.invalidRequest],
    'malformed digest': [(v) => (v.contract.digest = 'abc'), QUALIFY_VIOLATION_CODE.invalidDigest],
    'unknown source kind': [
      (v) => ((v.contract.derivedFrom[0] as { kind: string }).kind = 'openapi'),
      QUALIFY_VIOLATION_CODE.invalidSource,
    ],
    'local time': [(v) => (v.startedAt = '2026-09-17 10:00:00'), QUALIFY_VIOLATION_CODE.invalidTimestamp],
    'missing project': [(v) => (v.project = ''), QUALIFY_VIOLATION_CODE.required],
    'clean with leftovers': [
      (v) => (v.cleanup = { state: 'clean', leftovers: ['pid 1'] }),
      QUALIFY_VIOLATION_CODE.invalidCleanup,
    ],
    'partial without leftovers': [
      (v) => (v.cleanup = { state: 'partial', leftovers: [] }),
      QUALIFY_VIOLATION_CODE.invalidCleanup,
    ],
  };
  for (const [name, [mutate, code]] of Object.entries(shapeRules)) {
    specTest(`shape rule: ${name}`, VERDICT_DEFECTS, () => {
      const verdict = clone(fixtureVerdict('digest-mismatch.json'));
      mutate(verdict);
      expect(distinctCodes(validateQualifyVerdict(verdict))).toEqual([code]);
    });
  }
});
