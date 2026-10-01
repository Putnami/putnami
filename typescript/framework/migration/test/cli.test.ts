import { describe, expect, test } from 'bun:test';
import { type AppLike, runMigrate } from '../src/cli';
import {
  type ApplyOpts,
  type DriftReport,
  KindSQL,
  type MigrationRecord,
  type MigrationRunner,
  MigrationRegistry,
  type RollbackOpts,
} from '../src';

// --- Test doubles ---------------------------------------------------------

interface CapturedStreams {
  stdout: string;
  stderr: string;
}

function capture(): { streams: CapturedStreams; out: { stdout: (s: string) => void; stderr: (s: string) => void } } {
  const streams: CapturedStreams = { stdout: '', stderr: '' };
  return {
    streams,
    out: {
      stdout: (s) => {
        streams.stdout += s;
      },
      stderr: (s) => {
        streams.stderr += s;
      },
    },
  };
}

function fakeRunner(
  opts: {
    kind?: string;
    apply?: MigrationRecord[];
    status?: MigrationRecord[];
    rollback?: MigrationRecord[];
    report?: DriftReport;
    applyError?: Error;
    rollbackError?: Error;
  } = {},
): MigrationRunner {
  const kind = opts.kind ?? KindSQL;
  return {
    kind,
    async apply(_opts?: ApplyOpts) {
      if (opts.applyError) throw opts.applyError;
      return opts.apply ?? [];
    },
    async status() {
      return opts.status ?? [];
    },
    async rollback(_opts?: RollbackOpts) {
      if (opts.rollbackError) throw opts.rollbackError;
      return opts.rollback ?? [];
    },
    async verify() {
      return opts.report ?? { kind };
    },
  };
}

function makeApp(setup: (reg: MigrationRegistry) => void): AppLike {
  const reg = new MigrationRegistry();
  setup(reg);
  return {
    prepare: async () => {},
    stop: async () => {},
    getMigrationRegistry: () => reg,
  };
}

// --- Tests ----------------------------------------------------------------

describe('runMigrate', () => {
  test('no args prints usage and exits 1', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(() => makeApp(() => {}), [], out);
    expect(code).toBe(1);
    expect(streams.stderr).toContain('Usage:');
  });

  test('--help exits 0', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(() => makeApp(() => {}), ['--help'], out);
    expect(code).toBe(0);
    expect(streams.stdout).toContain('Usage:');
  });

  test('unknown subcommand exits 1', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(() => makeApp(() => {}), ['sideways'], out);
    expect(code).toBe(1);
    expect(streams.stderr).toContain('unknown subcommand');
  });

  test('up applies all runners and prints records', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () =>
        makeApp((reg) => {
          reg.registerRunner(
            fakeRunner({
              apply: [{ kind: KindSQL, namespace: 'iam', name: 'iam/001', status: 'applied', target: 'default' }],
            }),
          );
        }),
      ['up'],
      out,
    );
    expect(code).toBe(0);
    expect(streams.stdout).toContain('iam/001');
  });

  test('up with no pending prints clean message', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(() => makeApp((reg) => reg.registerRunner(fakeRunner())), ['up'], out);
    expect(code).toBe(0);
    expect(streams.stdout).toContain('no pending migrations');
  });

  test('up to <name> accepts target', async () => {
    const { out } = capture();
    const code = await runMigrate(
      () => makeApp((reg) => reg.registerRunner(fakeRunner())),
      ['up', 'to', 'iam/001'],
      out,
    );
    expect(code).toBe(0);
  });

  test('up rejects bare target without "to" keyword', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(() => makeApp((reg) => reg.registerRunner(fakeRunner())), ['up', 'iam/001'], out);
    expect(code).toBe(1);
    expect(streams.stderr).toContain('unexpected arguments');
  });

  test('verify exits 3 on drift', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () =>
        makeApp((reg) =>
          reg.registerRunner(
            fakeRunner({
              report: {
                kind: KindSQL,
                hashDrifts: [{ name: 'iam/001', storedHash: 'aaaa', currentHash: 'bbbb' }],
              },
            }),
          ),
        ),
      ['verify'],
      out,
    );
    expect(code).toBe(3);
    expect(streams.stdout).toContain('DRIFT DETECTED');
  });

  test('verify exits 0 on clean', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(() => makeApp((reg) => reg.registerRunner(fakeRunner())), ['verify'], out);
    expect(code).toBe(0);
    expect(streams.stdout).toContain('no drift');
  });

  test('inspect emits valid JSON', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () =>
        makeApp((reg) => {
          reg.registerRunner(fakeRunner());
          reg.addSource({ kind: KindSQL, namespace: 'iam' });
          reg.addSource({ kind: KindSQL, namespace: 'secrets' });
        }),
      ['inspect'],
      out,
    );
    expect(code).toBe(0);
    const view = JSON.parse(streams.stdout);
    expect(view.kinds).toHaveLength(1);
    expect(view.kinds[0].kind).toBe(KindSQL);
    expect(view.kinds[0].runnerRegistered).toBe(true);
    expect(view.kinds[0].sources).toHaveLength(2);
  });

  test('status propagates runner output', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () =>
        makeApp((reg) =>
          reg.registerRunner(
            fakeRunner({
              status: [
                { kind: KindSQL, namespace: 'iam', name: 'iam/001', status: 'applied', target: 'default' },
                { kind: KindSQL, namespace: 'iam', name: 'iam/002', status: 'pending', target: 'default' },
              ],
            }),
          ),
        ),
      ['status'],
      out,
    );
    expect(code).toBe(0);
    expect(streams.stdout).toContain('iam/001');
    expect(streams.stdout).toContain('iam/002');
    expect(streams.stdout).toContain('applied');
    expect(streams.stdout).toContain('pending');
  });

  test('up surfaces runner errors with exit 2', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () => makeApp((reg) => reg.registerRunner(fakeRunner({ applyError: new Error('boom') }))),
      ['up'],
      out,
    );
    expect(code).toBe(2);
    expect(streams.stderr).toContain('boom');
  });

  test('down surfaces a single runner rollback error with exit 2', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () => makeApp((reg) => reg.registerRunner(fakeRunner({ rollbackError: new Error('rollback boom') }))),
      ['down'],
      out,
    );
    expect(code).toBe(2);
    expect(streams.stderr).toContain('rollback boom');
    expect(streams.stderr).toContain(`[${KindSQL}]`);
    // A failed rollback must never be reported as "nothing to roll back".
    expect(streams.stdout).not.toContain('nothing to roll back');
  });

  test('down does not let one kind failure mask other kinds (records + every error surfaced)', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () =>
        makeApp((reg) => {
          // kinds() is lexicographically sorted, so 'gcs' runs before 'sql'.
          // The first kind throws; the second must still be attempted, its
          // records printed, and the first kind's error must not be swallowed.
          reg.registerRunner(fakeRunner({ kind: 'gcs', rollbackError: new Error('gcs down failed') }));
          reg.registerRunner(
            fakeRunner({
              kind: KindSQL,
              rollback: [
                { kind: KindSQL, namespace: 'iam', name: 'iam/001', status: 'rolled-back', target: 'default' },
              ],
            }),
          );
        }),
      ['down'],
      out,
    );
    expect(code).toBe(2);
    // The succeeding kind's record is still reported.
    expect(streams.stdout).toContain('iam/001');
    // The failing kind's error is surfaced with its kind label.
    expect(streams.stderr).toContain('[gcs]');
    expect(streams.stderr).toContain('gcs down failed');
  });

  test('down aggregates errors when multiple kinds fail to roll back', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () =>
        makeApp((reg) => {
          reg.registerRunner(fakeRunner({ kind: 'gcs', rollbackError: new Error('gcs down failed') }));
          reg.registerRunner(fakeRunner({ kind: KindSQL, rollbackError: new Error('sql down failed') }));
        }),
      ['down'],
      out,
    );
    expect(code).toBe(2);
    // Both kinds' errors are surfaced — neither is swallowed.
    expect(streams.stderr).toContain('[gcs]');
    expect(streams.stderr).toContain('gcs down failed');
    expect(streams.stderr).toContain(`[${KindSQL}]`);
    expect(streams.stderr).toContain('sql down failed');
    expect(streams.stderr).toContain('rollback failed for 2 kinds');
  });

  test('status --output=jsonl emits one parseable JSON record per line', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () =>
        makeApp((reg) =>
          reg.registerRunner(
            fakeRunner({
              status: [
                { kind: KindSQL, namespace: 'iam', name: 'iam/001', status: 'applied', target: 'default' },
                { kind: KindSQL, namespace: 'iam', name: 'iam/002', status: 'pending', target: 'default' },
              ],
            }),
          ),
        ),
      ['status', '--output=jsonl'],
      out,
    );
    expect(code).toBe(0);
    const lines = streams.stdout.trim().split('\n');
    expect(lines).toHaveLength(2);
    const parsed = lines.map((l) => JSON.parse(l));
    expect(parsed[0].name).toBe('iam/001');
    expect(parsed[1].status).toBe('pending');
    expect(streams.stdout).not.toContain('KIND'); // no table header in jsonl mode
  });

  test('inspect --output=jsonl emits one JSON object per kind', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () => makeApp((reg) => reg.registerRunner(fakeRunner())),
      ['inspect', '--output=jsonl'],
      out,
    );
    expect(code).toBe(0);
    const lines = streams.stdout.trim().split('\n').filter(Boolean);
    expect(lines.length).toBeGreaterThanOrEqual(1);
    for (const l of lines) {
      expect(() => JSON.parse(l)).not.toThrow();
    }
    expect(JSON.parse(lines[0] ?? '{}').kind).toBe(KindSQL);
  });

  test('rejects an invalid --output value with exit 1', async () => {
    const { streams, out } = capture();
    const code = await runMigrate(
      () => makeApp((reg) => reg.registerRunner(fakeRunner())),
      ['status', '--output=yaml'],
      out,
    );
    expect(code).toBe(1);
    expect(streams.stderr).toContain("--output must be 'table' or 'jsonl'");
  });
});
