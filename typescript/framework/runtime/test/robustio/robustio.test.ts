import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import { existsSync, mkdtempSync, readFileSync, renameSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { robustRemove, robustRemoveSync, robustRename, robustRenameSync } from '../../src/robustio';
import { isTransient, RETRY_BUDGET_MS, type RetryOptions, retry, retrySync } from '../../src/robustio/retry';

/** Returns what `op` throws, or undefined when it returns. */
function thrownBy(op: () => void): unknown {
  try {
    op();
  } catch (error) {
    return error;
  }
  return undefined;
}

function failure(code: string): NodeJS.ErrnoException {
  return Object.assign(new Error(`${code}: simulated`), { code });
}

/**
 * An operation that throws the given failures in order, then returns 'done'.
 * `calls` counts every attempt.
 */
function scripted(failures: NodeJS.ErrnoException[]) {
  const state = { calls: 0 };
  const step = (): string => {
    const next = failures[state.calls++];
    if (next) {
      throw next;
    }
    return 'done';
  };
  return { state, step };
}

/** Runs one scripted operation through the sync or the async retry. */
const runners: Record<string, (op: () => string, options: RetryOptions) => Promise<string>> = {
  sync: async (op, options) => retrySync(op, options),
  async: (op, options) => retry(async () => op(), options),
};

describe('robustio retry policy', () => {
  it('retries only the sharing failures of Windows', () => {
    for (const code of ['EPERM', 'EACCES', 'EBUSY']) {
      expect(isTransient(failure(code), 'win32')).toBe(true);
      expect(isTransient(failure(code), 'linux')).toBe(false);
      expect(isTransient(failure(code), 'darwin')).toBe(false);
    }
    for (const error of [failure('ENOENT'), failure('EEXIST'), new Error('no code'), 'EBUSY', undefined, null]) {
      expect(isTransient(error, 'win32')).toBe(false);
    }
  });

  it('waits the same two seconds as the Go robustio package', () => {
    expect(RETRY_BUDGET_MS).toBe(2000);
  });

  for (const [mode, run] of Object.entries(runners)) {
    describe(mode, () => {
      it('retries a Windows sharing failure until the call succeeds', async () => {
        const { state, step } = scripted([failure('EBUSY'), failure('EPERM'), failure('EACCES')]);
        expect(await run(step, { platform: 'win32', budgetMs: 60_000 })).toBe('done');
        expect(state.calls).toBe(4);
      });

      it('throws another failure on Windows at once', async () => {
        const missing = failure('ENOENT');
        const { state, step } = scripted([missing]);
        await expect(run(step, { platform: 'win32', budgetMs: 60_000 })).rejects.toBe(missing);
        expect(state.calls).toBe(1);
      });

      for (const platform of ['linux', 'darwin'] as const) {
        it(`calls once on ${platform}, even for a sharing failure`, async () => {
          const busy = failure('EBUSY');
          const { state, step } = scripted([busy]);
          await expect(run(step, { platform, budgetMs: 60_000 })).rejects.toBe(busy);
          expect(state.calls).toBe(1);
        });
      }

      it('gives up within its budget and throws the last failure', async () => {
        const budgetMs = 50;
        let calls = 0;
        let last: NodeJS.ErrnoException | undefined;
        const alwaysBusy = (): string => {
          calls++;
          last = failure('EBUSY');
          throw last;
        };
        const start = performance.now();
        const outcome = await run(alwaysBusy, { platform: 'win32', budgetMs }).catch((error: unknown) => error);
        expect(outcome).toBe(last);
        expect(performance.now() - start).toBeLessThan(budgetMs + 1000);
        expect(calls).toBeGreaterThanOrEqual(2);
      });
    });
  }
});

describe('robustio file operations', () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'putnami-robustio-'));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  function files(): { from: string; to: string } {
    const from = join(dir, 'from');
    const to = join(dir, 'to');
    writeFileSync(from, 'new');
    writeFileSync(to, 'old');
    return { from, to };
  }

  it('renames over an existing file, and a missing source fails with ENOENT', async () => {
    const { from, to } = files();
    await robustRename(from, to);
    expect(readFileSync(to, 'utf8')).toBe('new');
    await expect(robustRename(from, to)).rejects.toMatchObject({ code: 'ENOENT' });
  });

  it('renames synchronously the same way', () => {
    const { from, to } = files();
    robustRenameSync(from, to);
    expect(readFileSync(to, 'utf8')).toBe('new');
    expect(thrownBy(() => robustRenameSync(from, to))).toMatchObject({ code: 'ENOENT' });
  });

  it('removes a file, and a missing file is not an error', async () => {
    const { to } = files();
    await robustRemove(to);
    expect(existsSync(to)).toBe(false);
    await robustRemove(to);
  });

  it('removes synchronously the same way', () => {
    const { to } = files();
    robustRemoveSync(to);
    expect(existsSync(to)).toBe(false);
    robustRemoveSync(to);
  });
});

/**
 * Holds `path` open from another process the way a Go reader does, sharing
 * read access but not delete access, until `holdMs` after the handle opened.
 * Resolves once the handle is open.
 */
async function holdFromAnotherProcess(path: string, holdMs: number): Promise<{ released: Promise<number> }> {
  const script = [
    `$f = [System.IO.File]::Open($env:PUTNAMI_ROBUSTIO_HOLD, 'Open', 'Read', 'Read')`,
    `[Console]::Out.WriteLine('held')`,
    `[Console]::Out.Flush()`,
    `Start-Sleep -Milliseconds ${holdMs}`,
    '$f.Close()',
  ].join('; ');
  const child = Bun.spawn(['powershell.exe', '-NoProfile', '-NonInteractive', '-Command', script], {
    env: { ...process.env, PUTNAMI_ROBUSTIO_HOLD: path },
    stdout: 'pipe',
    stderr: 'inherit',
  });
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  const untilHeld = async (seen: string): Promise<void> => {
    if (seen.includes('held')) {
      return;
    }
    const { done, value } = await reader.read();
    if (done) {
      throw new Error(`the holder exited before it opened ${path}: ${seen}`);
    }
    return untilHeld(seen + decoder.decode(value));
  };
  await untilHeld('');
  reader.releaseLock();
  return { released: child.exited };
}

/** The holder is a PowerShell process, which can take seconds to start on a cold host. */
const HOLDER_TIMEOUT_MS = 30_000;

describe.skipIf(process.platform !== 'win32')('robustio on a Windows host', () => {
  let dir: string;

  beforeEach(() => {
    dir = mkdtempSync(join(tmpdir(), 'putnami-robustio-win-'));
  });

  afterEach(() => {
    rmSync(dir, { recursive: true, force: true });
  });

  it(
    'waits for another process that holds the destination of a rename',
    async () => {
      const from = join(dir, 'from');
      const to = join(dir, 'to');
      writeFileSync(from, 'new');
      writeFileSync(to, 'old');
      const { released } = await holdFromAnotherProcess(to, 300);
      // Precondition: a bare rename fails with a retried code, so the test proves
      // the retry and not a host that lets the rename through.
      expect(isTransient(thrownBy(() => renameSync(from, to)))).toBe(true);
      await robustRename(from, to);
      expect(await released).toBe(0);
      expect(readFileSync(to, 'utf8')).toBe('new');
    },
    HOLDER_TIMEOUT_MS,
  );

  it(
    'waits for another process that holds a file it removes',
    async () => {
      const path = join(dir, 'held');
      writeFileSync(path, 'body');
      const { released } = await holdFromAnotherProcess(path, 300);
      expect(isTransient(thrownBy(() => rmSync(path, { force: true })))).toBe(true);
      robustRemoveSync(path);
      expect(await released).toBe(0);
      expect(existsSync(path)).toBe(false);
    },
    HOLDER_TIMEOUT_MS,
  );
});
