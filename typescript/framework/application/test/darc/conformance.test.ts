import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { ArchitectureImport, Consistency } from '../../src/architecture/contract.types';
import { Command } from '../../src/darc/command';
import { ContractError, type DarcRecord, DarcError, type Freshness } from '../../src/darc/contract';
import { Projection, type ProjectionWriter, type Update } from '../../src/darc/projection';
import { Reference } from '../../src/darc/reference';
import { Snapshot } from '../../src/darc/snapshot';

/**
 * The cross-language DARC conformance corpus, TypeScript side.
 *
 * `protocols/architecture/fixtures/conformance/*-behavior.json` states what a
 * contract DOES, and the Go runner is `go/framework/app/darc/conformance_test.go`.
 * Both execute the same cases: a behavior that differs between the two languages
 * fails here or there rather than shipping as two runtimes that describe the
 * same manifest differently.
 *
 * Every case runs twice, through one runner: once as its own `it` so a failure
 * names the case, and once inside the `specTest` that attests the whole corpus
 * was answered. The second pass costs microseconds and is what binds parity to a
 * declared requirement rather than to a test file somebody could quietly delete.
 *
 * Refusals are pinned by a stable token, never by a message. `refused` is the
 * token for an operation the corpus requires to fail without pinning the clause.
 */

const CORPUS_DIR = join(__dirname, '../../../../../protocols/architecture/fixtures/conformance');

interface CorpusUpdate {
  readonly id: string;
  readonly value: string;
  readonly sourceVersion: string;
  readonly idempotencyKey?: string;
  readonly observedAt?: string;
}

interface CorpusExpect {
  readonly changed?: boolean;
  readonly found?: boolean;
  readonly value?: string;
  readonly freshness?: string;
  readonly provenance?: string;
  readonly error?: string;
  readonly since?: string;
  readonly ids?: string[];
  readonly versions?: string[];
  readonly facts?: string[];
  readonly attempted?: number;
  readonly failed?: number;
  readonly observed?: number;
}

interface CorpusStep extends CorpusUpdate {
  readonly op: string;
  readonly name?: string;
  readonly version?: string;
  readonly payload?: string;
  readonly seconds?: number;
  readonly fail?: boolean;
  readonly expect: CorpusExpect;
}

interface CorpusCase {
  readonly name: string;
  readonly base: string;
  readonly set?: Record<string, unknown>;
  readonly remove?: string[];
  readonly consistency?: string;
  readonly now: string;
  readonly sources?: Record<string, CorpusUpdate[]>;
  readonly carrier?: boolean;
  readonly expectError?: string;
  readonly steps?: CorpusStep[];
}

interface Corpus {
  readonly bases: Record<string, Record<string, unknown>>;
  readonly consistencyPresets?: Record<string, Consistency>;
  readonly cases: CorpusCase[];
}

function loadCorpus(name: string): Corpus {
  const corpus = JSON.parse(readFileSync(join(CORPUS_DIR, name), 'utf8')) as Corpus;
  if (corpus.cases.length === 0) throw new Error(`${name} is empty; a corpus that asserts nothing is worse than none`);
  return corpus;
}

/** Apply the case's shallow merge and consistency preset to its named base. */
function buildContract(corpus: Corpus, testCase: CorpusCase): ArchitectureImport {
  const base = corpus.bases[testCase.base];
  if (!base) throw new Error(`case "${testCase.name}" names base "${testCase.base}", which the corpus does not define`);
  const document: Record<string, unknown> = { ...base, ...(testCase.set ?? {}) };
  for (const member of testCase.remove ?? []) delete document[member];
  if (testCase.consistency) {
    const preset = corpus.consistencyPresets?.[testCase.consistency];
    if (!preset) throw new Error(`case "${testCase.name}" names an undefined consistency preset`);
    document['consistency'] = preset;
  }
  return document as unknown as ArchitectureImport;
}

/** Reduce a thrown value to the stable token the corpus pins. */
function errorToken(error: unknown): string {
  if (error === undefined) return '';
  if (error instanceof DarcError) return error.code;
  if (error instanceof ContractError) return 'contract';
  return 'refused';
}

interface Captured<T> {
  readonly value?: T;
  readonly token: string;
  readonly error?: unknown;
}

function capture<T>(run: () => T): Captured<T> {
  try {
    return { value: run(), token: '' };
  } catch (error) {
    return { token: errorToken(error), error };
  }
}

async function captureAsync<T>(run: () => Promise<T>): Promise<Captured<T>> {
  try {
    return { value: await run(), token: '' };
  } catch (error) {
    return { token: errorToken(error), error };
  }
}

/** A hand-wound clock, so a freshness bound is exercised by moving time rather than by sleeping. */
class TestClock {
  constructor(private current: Date) {}
  now = (): Date => this.current;
  advance(seconds: number): void {
    this.current = new Date(this.current.getTime() + seconds * 1000);
  }
}

function toUpdates(declared: CorpusUpdate[] | undefined): Update<string>[] {
  return (declared ?? []).map((update) => ({
    id: update.id,
    value: update.value,
    sourceVersion: update.sourceVersion,
    ...(update.idempotencyKey === undefined ? {} : { idempotencyKey: update.idempotencyKey }),
    ...(update.observedAt === undefined ? {} : { observedAt: new Date(update.observedAt) }),
  }));
}

/**
 * Check one record read against the case's expectation. A fail-closed refusal
 * still carries the record it refused, so the assertion reads it from the error
 * exactly as a caller wanting to log the copy it would not serve would.
 */
function checkRecord(expected: CorpusExpect, captured: Captured<DarcRecord<string> | undefined>): void {
  const fromError = captured.error instanceof DarcError ? (captured.error.record as DarcRecord<string>) : undefined;
  const record = captured.value ?? fromError;
  if (expected.found !== undefined) expect(record !== undefined).toBe(expected.found);
  if (expected.value !== undefined) expect(record?.value).toBe(expected.value);
  // The corpus is untyped JSON; the cast is the boundary, not a widening.
  if (expected.freshness !== undefined) expect(record?.freshness).toBe(expected.freshness as Freshness);
  if (expected.provenance !== undefined) expect(record?.provenance).toBe(expected.provenance);
}

async function runProjectionCase(corpus: Corpus, testCase: CorpusCase): Promise<void> {
  const clock = new TestClock(new Date(testCase.now));
  let replaySince = '';
  const sources = testCase.sources ?? {};
  const built = capture(
    () =>
      new Projection<string>(buildContract(corpus, testCase), {
        clock: clock.now,
        ...('bootstrap' in sources ? { bootstrap: () => toUpdates(sources['bootstrap']) } : {}),
        ...('replay' in sources
          ? {
              replay: (since: string) => {
                replaySince = since;
                return toUpdates(sources['replay']);
              },
            }
          : {}),
      }),
  );
  if (testCase.expectError) {
    expect(built.token).toBe(testCase.expectError);
    return;
  }
  expect(built.token).toBe('');
  const projection = built.value as Projection<string>;

  // Claimed lazily so the cases proving the single-writer refusal are not
  // defeated by the harness taking the handle first.
  let writer: ProjectionWriter<string> | undefined;
  const claim = (): ProjectionWriter<string> => {
    writer ??= projection.writer(projection.contract().localModel?.writer ?? '');
    return writer;
  };

  for (const step of testCase.steps ?? []) {
    switch (step.op) {
      case 'apply': {
        const applied = capture(() => claim().apply(toUpdates([step])[0] as Update<string>));
        expect(applied.token).toBe(step.expect.error ?? '');
        if (step.expect.changed !== undefined) expect(applied.value).toBe(step.expect.changed);
        break;
      }
      case 'delete':
        expect(capture(() => claim().delete(step.id, step.sourceVersion)).token).toBe(step.expect.error ?? '');
        break;
      case 'get': {
        const read = capture(() => projection.get(step.id));
        expect(read.token).toBe(step.expect.error ?? '');
        checkRecord(step.expect, read);
        break;
      }
      case 'all':
        expect(projection.all().map((record) => record.id)).toEqual(step.expect.ids ?? []);
        break;
      case 'rebuild': {
        const rebuilt = await captureAsync(() => projection.rebuild());
        expect(rebuilt.token).toBe(step.expect.error ?? '');
        if (step.expect.since !== undefined) expect(replaySince).toBe(step.expect.since);
        break;
      }
      case 'writer':
        expect(capture(() => projection.writer(step.name ?? '')).token).toBe(step.expect.error ?? '');
        break;
      case 'advance':
        clock.advance(step.seconds ?? 0);
        break;
      default:
        throw new Error(`the corpus names an operation this runner does not implement: ${step.op}`);
    }
  }
}

function runSnapshotCase(corpus: Corpus, testCase: CorpusCase): void {
  const clock = new TestClock(new Date(testCase.now));
  const built = capture(() => new Snapshot<string>(buildContract(corpus, testCase), { clock: clock.now }));
  if (testCase.expectError) {
    expect(built.token).toBe(testCase.expectError);
    return;
  }
  expect(built.token).toBe('');
  const snapshot = built.value as Snapshot<string>;

  for (const step of testCase.steps ?? []) {
    switch (step.op) {
      case 'attach':
        expect(
          capture(() =>
            snapshot.attach(
              step.version ?? '',
              step.value,
              step.observedAt === undefined ? undefined : new Date(step.observedAt),
            ),
          ).token,
        ).toBe(step.expect.error ?? '');
        break;
      case 'at':
        checkRecord(
          step.expect,
          capture(() => snapshot.at(step.version ?? '')),
        );
        break;
      case 'latest': {
        const read = capture(() => snapshot.latest());
        expect(read.token).toBe(step.expect.error ?? '');
        checkRecord(step.expect, read);
        break;
      }
      case 'versions':
        expect(snapshot.versions()).toEqual(step.expect.versions ?? []);
        break;
      case 'advance':
        clock.advance(step.seconds ?? 0);
        break;
      default:
        throw new Error(`the corpus names an operation this runner does not implement: ${step.op}`);
    }
  }
}

async function runCommandOrReferenceCase(corpus: Corpus, testCase: CorpusCase): Promise<void> {
  const contract = buildContract(corpus, testCase);
  if (testCase.base === 'reference') {
    const built = capture(() => new Reference(contract));
    if (testCase.expectError) {
      expect(built.token).toBe(testCase.expectError);
      return;
    }
    expect(built.token).toBe('');
    const reference = built.value as Reference;
    for (const step of testCase.steps ?? []) {
      if (step.op === 'fact') {
        const resolved = capture(() => reference.fact(step.name ?? ''));
        expect(resolved.token).toBe(step.expect.error ?? '');
        if (step.expect.provenance !== undefined) expect(resolved.value).toBe(step.expect.provenance);
      } else if (step.op === 'facts') {
        expect(reference.facts()).toEqual(step.expect.facts ?? []);
      } else {
        throw new Error(`the corpus names an operation this runner does not implement: ${step.op}`);
      }
    }
    return;
  }

  let failNext = false;
  let observed = 0;
  const carrier =
    testCase.carrier === false
      ? undefined
      : () => {
          if (failNext) throw new Error('the carrier refused the payload');
        };
  const built = capture(
    () =>
      new Command<string>(contract, carrier, {
        onFailure: () => {
          observed += 1;
        },
      }),
  );
  if (testCase.expectError) {
    expect(built.token).toBe(testCase.expectError);
    return;
  }
  expect(built.token).toBe('');
  const command = built.value as Command<string>;

  for (const step of testCase.steps ?? []) {
    failNext = step.fail === true;
    switch (step.op) {
      case 'send':
        expect((await captureAsync(() => command.send(step.payload ?? ''))).token).toBe(step.expect.error ?? '');
        break;
      case 'emit':
        observed = 0;
        await command.emit(step.payload ?? '');
        if (step.expect.observed !== undefined) expect(observed).toBe(step.expect.observed);
        break;
      case 'stats': {
        const stats = command.stats();
        if (step.expect.attempted !== undefined) expect(stats.attempted).toBe(step.expect.attempted);
        if (step.expect.failed !== undefined) expect(stats.failed).toBe(step.expect.failed);
        break;
      }
      default:
        throw new Error(`the corpus names an operation this runner does not implement: ${step.op}`);
    }
  }
}

const projectionCorpus = loadCorpus('projection-behavior.json');
const snapshotCorpus = loadCorpus('snapshot-behavior.json');
const commandCorpus = loadCorpus('command-reference-behavior.json');

describe('DARC projection behavior conformance', () => {
  for (const testCase of projectionCorpus.cases) {
    it(testCase.name, () => runProjectionCase(projectionCorpus, testCase));
  }
});

describe('DARC snapshot behavior conformance', () => {
  for (const testCase of snapshotCorpus.cases) {
    it(testCase.name, () => {
      runSnapshotCase(snapshotCorpus, testCase);
    });
  }
});

describe('DARC command and reference behavior conformance', () => {
  for (const testCase of commandCorpus.cases) {
    it(testCase.name, () => runCommandOrReferenceCase(commandCorpus, testCase));
  }
});

describe('DARC cross-language parity', () => {
  specTest(
    'answers every case of the shared conformance corpus',
    {
      feature: 'typescript/application-lifecycle',
      requirement: 'darc-runtime-enforcement',
      check: 'the-shared-behavior-corpus-is-answered-case-for-case',
    },
    async () => {
      for (const testCase of projectionCorpus.cases) await runProjectionCase(projectionCorpus, testCase);
      for (const testCase of snapshotCorpus.cases) runSnapshotCase(snapshotCorpus, testCase);
      for (const testCase of commandCorpus.cases) await runCommandOrReferenceCase(commandCorpus, testCase);
      // A corpus that shrank silently would make this check pass by asserting
      // nothing, so the count is part of the claim.
      expect(
        projectionCorpus.cases.length + snapshotCorpus.cases.length + commandCorpus.cases.length,
      ).toBeGreaterThanOrEqual(47);
    },
  );
});
