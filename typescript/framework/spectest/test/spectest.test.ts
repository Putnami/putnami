import { afterAll, describe, expect, test } from 'bun:test';
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  readdirSync,
  realpathSync,
  rmSync,
  statSync,
  writeFileSync,
} from 'node:fs';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

import { FRAGMENT_DIR_ENV, observeMeasurement, specTest } from '../src/index';

/**
 * The helper is exercised end to end through a REAL child `bun test` run: a
 * generated suite in a scratch directory registers passing, failing,
 * skipped, and done-style tests through `specTest`, and the parent asserts
 * the fragments the child process left behind. That is the honest shape —
 * the verdict in a fragment must be what the runner observed, so the runner
 * has to actually run.
 *
 * The suite is generated OUTSIDE this project on purpose: a committed
 * fixture matching bun's test-file pattern would be discovered by this
 * project's own `bun test` and its deliberately failing case would fail the
 * suite that tests it.
 */

const helperPath = resolve(import.meta.dir, '../src/index.ts');
const scratchRoots: string[] = [];

// Exercise the public registration path in this process as well as in the
// isolated child below. The private fragment directory keeps these synthetic
// bindings out of Putnami's project evidence while allowing Bun's LCOV reporter
// to observe the implementation that the child process executes end to end.
const directRoot = mkdtempSync(join(tmpdir(), 'putnami-spectest-direct-'));
const directFragments = join(directRoot, 'fragments');
mkdirSync(directFragments);
scratchRoots.push(directRoot);
const inheritedFragmentDir = process.env[FRAGMENT_DIR_ENV];
process.env[FRAGMENT_DIR_ENV] = directFragments;
const directBinding = (check: string) => ({ feature: 'ts/spectest-coverage', requirement: 'registration', check });
specTest('direct passing case', directBinding('passing'), () => {});
specTest('direct done case', directBinding('done'), (done) => done());
// putnami:allow-skip this case proves that specTest.skip registers a skipped case
specTest.skip('direct skipped case', directBinding('skipped'), () => {});
observeMeasurement('direct measured case', directBinding('measured'), () => ({
  name: 'spectest.direct.duration',
  aggregation: 'p95',
  value: 1,
  unit: 'ms',
}));
// putnami:allow-skip this case proves that observeMeasurement.skip registers a skipped measurement
observeMeasurement.skip('direct skipped measurement', directBinding('measured-skipped'), () => ({
  name: 'spectest.direct.duration',
  aggregation: 'p95',
  value: 1,
  unit: 'ms',
}));

// A Windows stack frame names its file with a drive or a UNC share and
// backslashes. A registration resolves its declaration site at once, so each
// fragment below holds the site parsed from one injected frame. The cases run
// before the test that reads their fragments, in registration order.
const windowsFrames = {
  drive: ['    at <anonymous> (C:\\w\\app\\test\\feature.test.ts:12:5)', 'C:\\w\\app\\test\\feature.test.ts'],
  module: ['    at C:\\w\\app\\test\\feature.test.ts:3', 'C:\\w\\app\\test\\feature.test.ts'],
  forward: ['    at <anonymous> (C:/w/app/test/feature.test.ts:12:5)', 'C:/w/app/test/feature.test.ts'],
  unc: ['    at <anonymous> (\\\\host\\share\\app\\feature.test.ts:12:5)', '\\\\host\\share\\app\\feature.test.ts'],
  url: ['    at load (http://localhost:3000/app.js:1:2)', '/app.js'],
} as const;
const windowsFragments = join(directRoot, 'windows');
const stackError = Error as ErrorConstructor & {
  prepareStackTrace?: (_error: Error, _stack: unknown[]) => string;
};
const previousPrepareStackTrace = stackError.prepareStackTrace;
try {
  for (const [kind, [frame]] of Object.entries(windowsFrames)) {
    const directory = join(windowsFragments, kind);
    mkdirSync(directory, { recursive: true });
    process.env[FRAGMENT_DIR_ENV] = directory;
    stackError.prepareStackTrace = () => `Error\n${frame}`;
    specTest(`windows ${kind} frame`, directBinding(`windows-${kind}`), () => {});
  }
} finally {
  stackError.prepareStackTrace = previousPrepareStackTrace;
}
if (inheritedFragmentDir === undefined) {
  delete process.env[FRAGMENT_DIR_ENV];
} else {
  process.env[FRAGMENT_DIR_ENV] = inheritedFragmentDir;
}

test('records the declaration site of a Windows stack frame', () => {
  for (const [kind, [, file]] of Object.entries(windowsFrames)) {
    const directory = join(windowsFragments, kind);
    const [name] = readdirSync(directory).filter((entry) => entry.endsWith('.json'));
    const fragment = JSON.parse(readFileSync(join(directory, name ?? ''), 'utf8')) as { file: string };
    expect({ kind, file: fragment.file }).toEqual({ kind, file });
  }
});

afterAll(() => {
  for (const root of scratchRoots) {
    rmSync(root, { recursive: true, force: true });
  }
});

type CaseOutcome = 'passed' | 'failed' | 'skipped';

interface ChildRun {
  exitCode: number;
  fragments: Record<string, unknown>[];
  suitePath: string;
  /** The scratch directory holding everything the child could reach: suite, home, temporary directory. */
  root: string;
  /** The outcome bun reported for each case, by test name, read from its JUnit report. */
  outcomes: Record<string, CaseOutcome>;
}

/** Reads each case's outcome from the JUnit report bun wrote: a failure, a skip, or neither. */
function caseOutcomes(junit: string): Record<string, CaseOutcome> {
  const outcomes: Record<string, CaseOutcome> = {};
  for (const [, name, body] of junit.matchAll(/<testcase name="([^"]*)"[^>]*?(?:\/>|>([\s\S]*?)<\/testcase>)/g)) {
    outcomes[name ?? ''] = body?.includes('<failure') ? 'failed' : body?.includes('<skipped') ? 'skipped' : 'passed';
  }
  return outcomes;
}

/**
 * Lists every file under `root` that holds a JSON object with a `check`
 * member, the mark of a fragment, whatever its name or depth. A staging file
 * counts, because it holds the same bytes.
 */
function fragmentsUnder(root: string): string[] {
  const found: string[] = [];
  for (const entry of readdirSync(root, { recursive: true, encoding: 'utf8' })) {
    const path = join(root, entry);
    try {
      if (!statSync(path).isFile()) continue;
      const value: unknown = JSON.parse(readFileSync(path, 'utf8'));
      if (typeof value === 'object' && value !== null && 'check' in value) found.push(entry);
    } catch {
      // Not a readable JSON file: the suite, the JUnit report, a bun cache entry.
    }
  }
  return found.sort();
}

function runChildSuite(options: { fragmentsDir: boolean }): ChildRun {
  const root = mkdtempSync(join(tmpdir(), 'putnami-spectest-'));
  scratchRoots.push(root);
  const suiteDir = join(root, 'suite');
  const fragmentsDir = join(root, 'fragments');
  const homeDir = join(root, 'home');
  const tempDir = join(root, 'tmp');
  const junitPath = join(root, 'junit.xml');
  for (const directory of [suiteDir, fragmentsDir, homeDir, tempDir]) {
    mkdirSync(directory);
  }

  const suitePath = join(suiteDir, 'generated.test.ts');
  writeFileSync(
    suitePath,
    `import { observeMeasurement, specTest } from ${JSON.stringify(helperPath)};

const binding = (check: string) => ({ feature: 'ts/spectest-demo', requirement: 'lifecycle', check });

specTest('passing case', binding('passing-check'), () => {});
specTest('failing case', binding('failing-check'), () => {
  throw new Error('deliberate failure');
});
specTest.skip('skipped case', binding('skipped-check'), () => {});
specTest('done style case', binding('done-check'), (done) => {
  done();
});

const measurement = { name: 'demo.flush.duration', aggregation: 'p95', value: 12.5, unit: 'ms' };

observeMeasurement('measuring case', binding('measured-check'), () => measurement);
observeMeasurement('async measuring case', binding('async-measured-check'), async () => measurement);
observeMeasurement('failing measuring case', binding('failed-measured-check'), () => {
  throw new Error('deliberate measurement failure');
});
observeMeasurement.skip('skipped measuring case', binding('skipped-measured-check'), () => measurement);
`,
  );

  // The working directory, the home directory and every temporary directory
  // the child can name sit inside `root`, so a fragment written anywhere the
  // binding might default to lands where the parent can find it.
  const env: Record<string, string | undefined> = {
    ...process.env,
    HOME: homeDir,
    USERPROFILE: homeDir,
    TMPDIR: tempDir,
    TEMP: tempDir,
    TMP: tempDir,
  };
  if (options.fragmentsDir) {
    env[FRAGMENT_DIR_ENV] = fragmentsDir;
  } else {
    env[FRAGMENT_DIR_ENV] = undefined;
  }
  const result = Bun.spawnSync([process.execPath, 'test', '--reporter=junit', `--reporter-outfile=${junitPath}`], {
    cwd: suiteDir,
    env,
  });

  const fragments = readdirSync(fragmentsDir)
    .filter((name) => name.endsWith('.json'))
    .sort()
    .map((name) => JSON.parse(readFileSync(join(fragmentsDir, name), 'utf8')) as Record<string, unknown>);
  return {
    exitCode: result.exitCode,
    fragments,
    suitePath: realpathSync(suitePath),
    root,
    outcomes: caseOutcomes(readFileSync(junitPath, 'utf8')),
  };
}

// One child run serves every assertion about a recorded run: spawning bun is
// the expensive part, and both facts below describe the same run.
let recordedRun: ChildRun | undefined;
const recorded = (): ChildRun => {
  recordedRun ??= runChildSuite({ fragmentsDir: true });
  return recordedRun;
};

// The same suite run without a fragment directory, as the bare runner sees it.
let bareRun: ChildRun | undefined;
const bare = (): ChildRun => {
  bareRun ??= runChildSuite({ fragmentsDir: false });
  return bareRun;
};

// The verification wire's schema, read from the features protocol: a fragment
// is an observation without its provenance, so the schema decides which members
// a fragment may carry and what each may hold.
interface SchemaDefinition {
  pattern?: string;
  enum?: string[];
  required?: string[];
  properties?: Record<string, SchemaDefinition>;
  $ref?: string;
}
const wire = JSON.parse(
  readFileSync(
    resolve(import.meta.dir, '../../../../protocols/features/schemas/putnami-feature-verification.json'),
    'utf8',
  ),
) as { $defs: Record<string, SchemaDefinition> };

/** Resolves a `$ref` into the schema's `$defs`, then returns the definition. */
function definition(node: SchemaDefinition | undefined): SchemaDefinition {
  const name = node?.$ref?.replace('#/$defs/', '');
  return name ? (wire.$defs[name] ?? {}) : (node ?? {});
}

function expectMatches(value: unknown, node: SchemaDefinition | undefined): void {
  const { pattern } = definition(node);
  expect(typeof value).toBe('string');
  expect(value as string).toMatch(new RegExp(pattern ?? '', 'u'));
}

/** Asserts that a fragment fits the wire's observation, apart from its declaration site. */
function expectOnTheWire(fragment: Record<string, unknown>): void {
  const observation = wire.$defs['observation'] ?? {};
  const members = observation.properties ?? {};
  const rest = Object.fromEntries(
    Object.entries(fragment).filter(([member]) => member !== 'file' && member !== 'symbol'),
  );
  for (const member of Object.keys(rest)) {
    expect(Object.keys(members), `${member} is not an observation member`).toContain(member);
  }
  for (const member of (observation.required ?? []).filter((name) => name !== 'provenance')) {
    expect(rest, `${member} is required`).toHaveProperty(member);
  }
  expectMatches(rest['feature'], members['feature']);
  expectMatches(rest['requirement'], members['requirement']);
  expectMatches(rest['check'], members['check']);
  if (rest['status'] !== undefined) {
    expect(definition(members['status']).enum).toContain(rest['status'] as string);
  }
  if (rest['measurement'] !== undefined) {
    const measurement = definition(members['measurement']);
    const fields = measurement.properties ?? {};
    const given = rest['measurement'] as Record<string, unknown>;
    expect(Object.keys(given).sort()).toEqual([...(measurement.required ?? [])].sort());
    expectMatches(given['name'], fields['name']);
    expectMatches(given['unit'], fields['unit']);
    expect(definition(fields['aggregation']).enum).toContain(given['aggregation'] as string);
    expect(typeof given['value']).toBe('number');
  }
}

// This project's own spec bindings. They are registered AFTER the synthetic
// bindings above restored the inherited fragment directory, so these four —
// and only these four — land in Putnami's project evidence. Three prove the
// fragment contract of the features protocol; one proves what the wrapper adds.
const fragmentContract = (check: string) => ({
  feature: 'features/feature-contracts',
  requirement: 'proof-fragments',
  check,
});

describe('specTest', () => {
  specTest(
    'records the verdict the runner observed, one atomic fragment per check',
    fragmentContract('a-typescript-binding-records-the-verdict-the-runner-recorded'),
    () => {
      const run = recorded();
      const byCheck = new Map(run.fragments.map((fragment) => [fragment.check, fragment]));
      // The failing and skipped measuring cases publish nothing: a measurement
      // fragment has no verdict to carry, so absence is the only honest report.
      expect([...byCheck.keys()].sort()).toEqual([
        'async-measured-check',
        'done-check',
        'failing-check',
        'measured-check',
        'passing-check',
        'skipped-check',
      ]);
      expect(byCheck.get('passing-check')?.status).toBe('passed');
      expect(byCheck.get('failing-check')?.status).toBe('failed');
      expect(byCheck.get('skipped-check')?.status).toBe('skipped');
      expect(byCheck.get('done-check')?.status).toBe('passed');

      for (const fragment of run.fragments) {
        expectOnTheWire(fragment);
        expect(fragment.feature).toBe('ts/spectest-demo');
        expect(fragment.requirement).toBe('lifecycle');
        // The declaration site is the absolute path of the registering file,
        // and the symbol is the test name a reader can find in it.
        expect(fragment.file).toBe(run.suitePath);
        expect(typeof fragment.symbol).toBe('string');
        expect((fragment.symbol as string).length).toBeGreaterThan(0);
      }
      expect(run.fragments.map((fragment) => fragment.symbol).sort()).toEqual([
        'async measuring case',
        'done style case',
        'failing case',
        'measuring case',
        'passing case',
        'skipped case',
      ]);
    },
  );

  specTest(
    'publishes an acceptance verdict or a measurement, never both',
    fragmentContract('a-typescript-measurement-carries-its-window-and-no-verdict'),
    () => {
      const run = recorded();
      const byCheck = new Map(run.fragments.map((fragment) => [fragment.check, fragment]));

      for (const check of ['passing-check', 'failing-check', 'skipped-check', 'done-check']) {
        const fragment = byCheck.get(check);
        expect(fragment?.measurement).toBeUndefined();
        expect(fragment?.window).toBeUndefined();
      }

      for (const check of ['measured-check', 'async-measured-check']) {
        const fragment = byCheck.get(check);
        expectOnTheWire(fragment as Record<string, unknown>);
        // The wire refuses an observation carrying both, so the measured
        // producer must state no verdict of its own.
        expect(fragment?.status).toBeUndefined();
        expect(fragment?.measurement).toEqual({
          name: 'demo.flush.duration',
          aggregation: 'p95',
          value: 12.5,
          unit: 'ms',
        });
        // The window is the invocation span the body covered, in the same
        // second-precision RFC 3339 shape Go writes.
        const window = fragment?.window as { start: string; end: string };
        expect(window.start).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
        expect(window.end).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$/);
        expect(Date.parse(window.end)).toBeGreaterThanOrEqual(Date.parse(window.start));
      }
    },
  );

  specTest(
    'writes nothing without the fragment directory',
    fragmentContract('a-typescript-binding-writes-nothing-without-the-adapter-directory'),
    () => {
      // The bare run keeps its working directory, home directory and temporary
      // directories inside its scratch root, so a fragment written to any
      // default location is found there. The recorded run is the control: the
      // same scan finds its fragments, so an empty result is not blindness.
      expect(fragmentsUnder(recorded().root).length).toBe(recorded().fragments.length);
      expect(recorded().fragments.length).toBeGreaterThan(0);
      expect(fragmentsUnder(bare().root)).toEqual([]);
    },
  );

  specTest(
    'never changes the verdict the runner gives, with or without the fragment directory',
    {
      feature: 'typescript/spec-test-binding',
      requirement: 'honest-verdict',
      check: 'the-runner-verdict-is-unchanged',
    },
    () => {
      // Bare bun runs `test(name, fn)` unwrapped, so its outcomes are what the
      // runner gives: a body that returns passes, a body that throws fails, a
      // skipped case is skipped. The recorded run must give the same outcome
      // for every case, and so the same exit code.
      const expected: Record<string, CaseOutcome> = {
        'passing case': 'passed',
        'failing case': 'failed',
        'skipped case': 'skipped',
        'done style case': 'passed',
        'measuring case': 'passed',
        'async measuring case': 'passed',
        'failing measuring case': 'failed',
        'skipped measuring case': 'skipped',
      };
      expect(bare().outcomes).toEqual(expected);
      expect(recorded().outcomes).toEqual(expected);
      expect(recorded().exitCode).toBe(bare().exitCode);
      expect(bare().exitCode).not.toBe(0);
    },
  );
});
