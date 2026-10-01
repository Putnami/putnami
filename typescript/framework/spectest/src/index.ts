/**
 * Binds ordinary `bun:test` tests to the executable-spec verification wire:
 * a test that protects a declared check is registered through
 * `specTest`, and the check's verdict is written as a process-local fragment
 * the Putnami test adapter merges into the project's
 * `putnami-feature-verification` report artifact.
 *
 * Threshold checks have a measured producer beside that acceptance one:
 * `observeMeasurement` registers a test whose body returns the aggregate it
 * measured, and publishes that aggregate with the invocation window the body
 * actually covered. A fragment carries a verdict or a measurement, never
 * both, so a producer can state a number but never its own verdict.
 *
 * The module is deliberately inert outside a Putnami-provided fragment
 * directory: `bun test` without the adapter registers every test exactly as
 * before, writes nothing, and can never fail over reporting. It is equally
 * deliberately dumb: it states what THIS process observed — feature,
 * requirement, check, verdict or measurement, declaration site — and nothing
 * else. The adapter owns validation, project attribution, and merging; core
 * recomputes every verdict against the authored criterion; and no call here
 * can make a gate green that the test's own outcome does not support, because
 * the fragment is written only after the test body settled: a body that
 * throws records `failed` (and publishes no measurement at all), and a body
 * that never settles (timeout, crash) records nothing, which core resolves as
 * missing evidence — the fail-closed direction.
 *
 * It imports `bun:test` and node builtins only, and ships as its own
 * package, `@putnami/spectest`, so nothing outside a bun test file ever loads
 * it and projects below `@putnami/runtime` can depend on it. The
 * `@putnami/runtime/spectest` subpath re-exports it. The wire stays owned by `go.putnami.dev/protocol/features`:
 * the fragment JSON matches the Go `spectest.Fragment` shape, read by the
 * shared adapter merge in `tooling/extension-sdk/specreport`.
 */

import { test } from 'bun:test';
import { createHash, randomBytes } from 'node:crypto';
import { renameSync, rmSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

/** The environment variable naming the adapter-provided fragment directory. */
export const FRAGMENT_DIR_ENV = 'PUTNAMI_SPEC_FRAGMENTS';

/** The declared check one test protects. */
export interface SpecBinding {
  readonly feature: string;
  readonly requirement: string;
  readonly check: string;
}

/**
 * The closed measurement-reduction vocabulary of the verification wire,
 * mirroring `features.VerificationAggregation`. It is a union rather than a
 * string so a typo is a compile error here instead of an adapter warning
 * after the run.
 */
export type MeasurementAggregation =
  | 'value'
  | 'count'
  | 'sum'
  | 'avg'
  | 'min'
  | 'max'
  | 'p50'
  | 'p95'
  | 'p99'
  | 'ratio';

/**
 * One observed metric aggregate: the bounded semantic metric name, the
 * reduction that produced `value`, and its unit. The authored target lives in
 * `putnami.features.json` only; nothing here can state a verdict.
 */
export interface Measurement {
  readonly name: string;
  readonly aggregation: MeasurementAggregation;
  readonly value: number;
  readonly unit: string;
}

/** The acceptance verdict vocabulary a fragment can carry. */
type FragmentStatus = 'passed' | 'failed' | 'skipped';

/** The closed RFC 3339 period a measurement covers. */
interface ObservedWindow {
  start: string;
  end: string;
}

interface Fragment extends SpecBinding {
  status?: FragmentStatus;
  measurement?: Measurement;
  window?: ObservedWindow;
  file: string;
  symbol: string;
}

type TestFunction = Parameters<typeof test>[1];
type TestOptions = Parameters<typeof test>[2];

/**
 * Registers `test(name, fn)` as the protecting check of one feature
 * requirement. Outside a Putnami test run the registration is exactly
 * `test(name, fn)`; inside one, the verdict this process observed for the
 * body is recorded after it settles, with the test name as the provenance
 * symbol a reader can find in the declaring file.
 */
export function specTest(name: string, binding: SpecBinding, fn: TestFunction, options?: TestOptions): void {
  const directory = process.env[FRAGMENT_DIR_ENV] ?? '';
  if (!directory) {
    register(test, name, fn, options);
    return;
  }
  const file = declarationFile();
  const record = (status: FragmentStatus) => writeFragment(directory, { ...binding, status, file, symbol: name });
  register(test, name, wrapVerdict(fn, record), options);
}

/**
 * Registers `test.skip(name, fn)` and records the skip, mirroring Go's
 * `t.Skip` verdict: a skipped check never supports its requirement.
 */
specTest.skip = (name: string, binding: SpecBinding, fn: TestFunction, options?: TestOptions): void => {
  const directory = process.env[FRAGMENT_DIR_ENV] ?? '';
  if (directory) {
    writeFragment(directory, { ...binding, status: 'skipped', file: declarationFile(), symbol: name });
  }
  register(test.skip, name, fn, options);
};

/**
 * The body of a measuring test: it does the work and returns the aggregate it
 * measured. The return type is what makes forgetting to measure a compile
 * error rather than a silently absent observation.
 */
export type MeasuringFunction = () => Measurement | Promise<Measurement>;

/**
 * Registers a test that measures one declared threshold check and publishes
 * the aggregate its body returned, mirroring Go's
 * `spectest.ObserveMeasurement`. The observed window is the honest invocation
 * span — body entry to body settle — which is what an invocation-window
 * criterion evaluates.
 *
 * A body that throws or rejects publishes NOTHING: its measurement was taken
 * under conditions the test itself rejected, and an absent observation
 * resolves as missing, the fail-closed direction. The standard test verdict is
 * never altered, and the fragment carries no verdict of its own — core
 * recomputes the threshold verdict from the authored target.
 *
 * Done-style bodies are deliberately unsupported: the measurement travels back
 * as the body's return value, so an `async` body is the one shape that can
 * both await work and state what it measured.
 */
export function observeMeasurement(
  name: string,
  binding: SpecBinding,
  fn: MeasuringFunction,
  options?: TestOptions,
): void {
  const directory = process.env[FRAGMENT_DIR_ENV] ?? '';
  if (!directory) {
    register(test, name, fn as TestFunction, options);
    return;
  }
  const file = declarationFile();
  const record = (measurement: Measurement, window: ObservedWindow) =>
    writeFragment(directory, { ...binding, measurement, window, file, symbol: name });
  register(test, name, wrapMeasurement(fn, record), options);
}

/**
 * Registers `test.skip(name, fn)` and records nothing. A measurement fragment
 * has no verdict to carry, so the only honest report of a skipped measuring
 * test is its absence — which core resolves as missing evidence, exactly as
 * Go's helper does for a skipped test.
 */
observeMeasurement.skip = (name: string, _binding: SpecBinding, fn: MeasuringFunction, options?: TestOptions): void => {
  register(test.skip, name, fn as TestFunction, options);
};

/**
 * Wraps the measuring body so the aggregate is published only after the body
 * settled successfully, with the window it actually covered.
 */
function wrapMeasurement(
  fn: MeasuringFunction,
  record: (measurement: Measurement, window: ObservedWindow) => void,
): TestFunction {
  return (async () => {
    const start = Date.now();
    const measurement = await fn();
    record(measurement, { start: instant(start), end: instant(Date.now()) });
  }) as TestFunction;
}

/**
 * Renders an epoch instant as the same second-precision RFC 3339 timestamp Go
 * writes, so a window means the same thing whichever runtime observed it.
 */
function instant(epochMilliseconds: number): string {
  return `${new Date(epochMilliseconds).toISOString().slice(0, 19)}Z`;
}

function register(into: typeof test | typeof test.skip, name: string, fn: TestFunction, options?: TestOptions): void {
  if (options === undefined) {
    into(name, fn);
  } else {
    into(name, fn, options);
  }
}

/**
 * Wraps the test body so the recorded verdict is what the runner observed: a
 * settled completion is `passed`, a thrown or rejected body — or a
 * done-callback invoked with an error — is `failed`. The wrapper preserves
 * the body's callback arity, which is how bun distinguishes done-style
 * tests.
 */
function wrapVerdict(fn: TestFunction, record: (status: FragmentStatus) => void): TestFunction {
  const body = fn as (...args: unknown[]) => unknown;
  if (typeof fn === 'function' && fn.length > 0) {
    return ((done: (error?: unknown) => void) => {
      let recorded = false;
      const once = (status: FragmentStatus) => {
        if (!recorded) {
          recorded = true;
          record(status);
        }
      };
      try {
        body((error?: unknown) => {
          once(error ? 'failed' : 'passed');
          done(error);
        });
      } catch (error) {
        once('failed');
        throw error;
      }
    }) as TestFunction;
  }
  return (async () => {
    try {
      const value = await body();
      record('passed');
      return value;
    } catch (error) {
      record('failed');
      throw error;
    }
  }) as TestFunction;
}

const helperFile = fileURLToPath(import.meta.url);

/**
 * Resolves the absolute declaration site from the call stack: the first
 * frame outside this module. The adapter resolves it to a project-relative
 * provenance path and refuses one outside the reporting project.
 */
function declarationFile(): string {
  const stack = new Error().stack ?? '';
  for (const line of stack.split('\n')) {
    // A frame is `at fn (/abs/path:line:col)` — or, for module-evaluation
    // frames, `at /abs/path:line` with no column and no parentheses. On
    // Windows the absolute path starts with a drive (`C:\` or `C:/`) or is a
    // UNC path (`\\host\share`). A drive letter follows no other letter,
    // digit, `+`, `.` or `-`, which tells it from the end of a URL scheme.
    const match = /\(?((?<![A-Za-z0-9+.-])[A-Za-z]:[\\/][^):]*|\\\\[^):]+|\/[^):]+):\d+(?::\d+)?\)?\s*$/.exec(
      line.trim(),
    );
    if (match && match[1] !== helperFile) {
      return match[1];
    }
  }
  return '';
}

/**
 * Publishes one fragment atomically under a content-derived name, so
 * identical observations collapse and concurrent writers never interleave.
 * Failures are deliberately silent: reporting is the adapter's concern, and
 * a full disk must not turn a green test red from inside its own verdict.
 */
function writeFragment(directory: string, fragment: Fragment): void {
  try {
    const encoded = JSON.stringify(fragment);
    const digest = createHash('sha256').update(encoded).digest('hex').slice(0, 32);
    const final = join(directory, `${digest}.json`);
    const staging = join(directory, `.fragment-${process.pid}-${randomBytes(6).toString('hex')}.tmp`);
    writeFileSync(staging, encoded);
    try {
      renameSync(staging, final);
    } catch {
      rmSync(staging, { force: true });
    }
  } catch {
    // Reporting never fails the test process.
  }
}
