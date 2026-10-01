import { afterEach, describe, expect, it } from 'bun:test';
import { mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { HookExitCodes, readHookContext, runHookCommand, standardHookModel } from '../src/server/hooks';

class ExitSignal extends Error {
  code: number;

  constructor(code: number) {
    super(`process.exit(${code})`);
    this.code = code;
  }
}

const originalArgv = process.argv.slice();
const originalExit = process.exit;
const originalStdoutWrite = process.stdout.write.bind(process.stdout);
const originalStderrWrite = process.stderr.write.bind(process.stderr);
const originalProjectRoot = process.env['PUTNAMI_PROJECT_ROOT'];
const originalWorkspaceRoot = process.env['PUTNAMI_WORKSPACE_ROOT'];
const originalPrebuildContext = process.env['PUTNAMI_PREBUILD_CONTEXT'];

const tempDirs: string[] = [];

function makeTempDir(): string {
  const dir = mkdtempSync(join(tmpdir(), 'putnami-utils-hook-command-'));
  tempDirs.push(dir);
  return dir;
}

function makeContextFile(): string {
  const dir = makeTempDir();
  const contextPath = join(dir, 'context.json');
  writeFileSync(
    contextPath,
    JSON.stringify({
      workspaceRoot: '/workspace',
      projectRoot: '/project',
      extensionRoot: '/extension',
      outputRoot: '/output',
      cacheRoot: '/cache',
      debug: false,
      hook: 'preBuild',
      extension: '@putnami/test',
      config: { feature: true },
      mode: 'test',
    }),
  );
  return contextPath;
}

function captureStreams(): { stdoutLines: string[]; stderrChunks: string[] } {
  const stdoutLines: string[] = [];
  const stderrChunks: string[] = [];

  process.stdout.write = ((chunk: unknown, encodingOrCallback?: unknown, callback?: unknown) => {
    const outStr = String(chunk);
    if (outStr) stdoutLines.push(outStr);
    const outCb = typeof encodingOrCallback === 'function' ? encodingOrCallback : callback;
    if (typeof outCb === 'function') outCb();
    return true;
  }) as typeof process.stdout.write;

  process.stderr.write = ((chunk: unknown, encodingOrCallback?: unknown, callback?: unknown) => {
    const errStr = String(chunk);
    if (errStr) stderrChunks.push(errStr);
    const errCb = typeof encodingOrCallback === 'function' ? encodingOrCallback : callback;
    if (typeof errCb === 'function') errCb();
    return true;
  }) as typeof process.stderr.write;

  return { stdoutLines, stderrChunks };
}

function mockExitWithRecord(exitCodes: number[]): void {
  process.exit = ((code?: number) => {
    exitCodes.push(code ?? 0);
    return undefined as never;
  }) as typeof process.exit;
}

function mockExitWithSignal(): void {
  process.exit = ((code?: number) => {
    throw new ExitSignal(code ?? 0);
  }) as typeof process.exit;
}

afterEach(() => {
  process.argv = [...originalArgv];
  process.exit = originalExit;
  process.stdout.write = originalStdoutWrite as typeof process.stdout.write;
  process.stderr.write = originalStderrWrite as typeof process.stderr.write;

  process.env['PUTNAMI_PROJECT_ROOT'] = originalProjectRoot;
  process.env['PUTNAMI_WORKSPACE_ROOT'] = originalWorkspaceRoot;
  process.env['PUTNAMI_PREBUILD_CONTEXT'] = originalPrebuildContext;

  while (tempDirs.length > 0) {
    const dir = tempDirs.pop();
    if (dir) {
      rmSync(dir, { recursive: true, force: true });
    }
  }
});

describe('hook-command utils', () => {
  it('reads hook context from disk and throws when file is missing', () => {
    const contextPath = makeContextFile();
    const context = readHookContext(contextPath);
    expect(context.projectRoot).toBe('/project');
    expect(context.workspaceRoot).toBe('/workspace');
    // Free-form passthrough fields survive validation.
    expect(context.config).toEqual({ feature: true });
    expect(context.mode).toBe('test');

    expect(() => readHookContext(join(makeTempDir(), 'missing-context.json'))).toThrow('Context file not found');
  });

  it('rejects a context file containing invalid JSON', () => {
    const dir = makeTempDir();
    const contextPath = join(dir, 'context.json');
    writeFileSync(contextPath, '{ not json');

    expect(() => readHookContext(contextPath)).toThrow('Invalid JSON in context file');
  });

  it('rejects a structurally invalid / hostile context file with an actionable message', () => {
    const dir = makeTempDir();

    // Missing required string fields.
    const incompletePath = join(dir, 'incomplete.json');
    writeFileSync(incompletePath, JSON.stringify({ debug: true }));
    expect(() => readHookContext(incompletePath)).toThrow('Invalid hook context file');
    expect(() => readHookContext(incompletePath)).toThrow('context.projectRoot');

    // Wrong type on a field that flows into process.env.
    const wrongTypePath = join(dir, 'wrong-type.json');
    writeFileSync(
      wrongTypePath,
      JSON.stringify({
        workspaceRoot: '/workspace',
        projectRoot: 42,
        extensionRoot: '/extension',
        outputRoot: '/output',
        cacheRoot: '/cache',
        debug: false,
        hook: 'preBuild',
        extension: '@putnami/test',
      }),
    );
    expect(() => readHookContext(wrongTypePath)).toThrow('context.projectRoot');

    // Non-object JSON payload.
    const arrayPath = join(dir, 'array.json');
    writeFileSync(arrayPath, JSON.stringify(['not', 'a', 'context']));
    expect(() => readHookContext(arrayPath)).toThrow('Invalid hook context file');
  });

  it('extends hook models while preserving inherited metadata', () => {
    const model = standardHookModel.extend({
      name: 'generate',
      description: 'Generate files',
      flags: {
        message: {
          flag: '--message',
          type: 'string',
        },
      },
    });

    const extended = model.extend({
      flags: {
        verbose: {
          flag: '--verbose',
        },
      },
    });

    expect(model.name).toBe('generate');
    expect(model.description).toBe('Generate files');
    expect(model.flags['message']?.flag).toBe('--message');
    expect(extended.name).toBe('generate');
    expect(extended.description).toBe('Generate files');
    expect(extended.flags['putnamiContext']?.flag).toBe('--putnami-context');
    expect(extended.flags['verbose']?.flag).toBe('--verbose');
  });

  it('runs hook commands successfully and emits meta + summary events', async () => {
    const contextPath = makeContextFile();
    const { stdoutLines } = captureStreams();
    const exitCodes: number[] = [];
    mockExitWithRecord(exitCodes);

    const model = standardHookModel.extend({
      name: 'demo-hook',
      description: 'Demo hook',
      flags: {
        message: { flag: '--message', type: 'string' },
        alias: { flag: '--alias', type: 'string' },
        verbose: { flag: '--verbose' },
        dryRun: { flag: '--dry-run', type: 'boolean' },
      },
    });

    const runCalls: Array<{ options: Record<string, unknown>; context: Record<string, unknown> }> = [];

    process.argv = [
      'bun',
      'hook',
      '--putnami-context',
      contextPath,
      '--message',
      'hello',
      '--alias=short',
      '--verbose',
      '--dry-run=false',
      '--ignored',
    ];

    await runHookCommand({
      model,
      extension: '@putnami/test',
      hook: 'preBuild',
      version: '1.2.3',
      run: (options, context) => {
        runCalls.push({
          options: options as Record<string, unknown>,
          context: context as unknown as Record<string, unknown>,
        });
        return {
          exports: { loader: '/tmp/loader.ts' },
          assets: { bundle: '/tmp/bundle.js' },
          data: { cacheHit: true },
        };
      },
    });

    expect(exitCodes).toEqual([HookExitCodes.SUCCESS]);
    expect(runCalls.length).toBe(1);
    expect(runCalls[0]?.options['putnamiContext']).toBe(contextPath);
    expect(runCalls[0]?.options['message']).toBe('hello');
    expect(runCalls[0]?.options['alias']).toBe('short');
    expect(runCalls[0]?.options['verbose']).toBe(true);
    expect(runCalls[0]?.options['dryRun']).toBe(false);
    expect(runCalls[0]?.context['projectRoot']).toBe('/project');

    expect(process.env['PUTNAMI_PROJECT_ROOT']).toBe('/project');
    expect(process.env['PUTNAMI_WORKSPACE_ROOT']).toBe('/workspace');
    expect(process.env['PUTNAMI_PREBUILD_CONTEXT']).toBe('true');

    const events = stdoutLines.map((line) => JSON.parse(line.trim()) as Record<string, unknown>);
    expect(events[0]?.['type']).toBe('meta');
    expect(events[0]?.['message']).toBe('Starting preBuild');
    expect(events[0]?.['data']).toEqual(expect.objectContaining({ version: '1.2.3' }));

    const summary = events.find((event) => event['type'] === 'summary');
    expect(summary).toBeDefined();
    expect(summary?.['data']).toEqual(expect.objectContaining({ outputs: 2, cacheHit: true }));
  });

  it('emits an error event and BUILD_ERROR exit code when run fails', async () => {
    const contextPath = makeContextFile();
    const { stdoutLines } = captureStreams();
    const exitCodes: number[] = [];
    mockExitWithRecord(exitCodes);

    process.argv = ['bun', 'hook', '--putnami-context', contextPath];

    await runHookCommand({
      model: standardHookModel,
      extension: '@putnami/test',
      hook: 'preBuild',
      run: () => {
        throw new Error('boom');
      },
    });

    expect(exitCodes).toEqual([HookExitCodes.BUILD_ERROR]);
    const events = stdoutLines.map((line) => JSON.parse(line.trim()) as Record<string, unknown>);
    expect(events.some((event) => event['type'] === 'meta')).toBe(true);
    const errorEvent = events.find((event) => event['type'] === 'error');
    expect(errorEvent).toBeDefined();
    expect(errorEvent?.['message']).toBe('boom');
    expect(errorEvent?.['data']).toEqual(expect.objectContaining({ code: 'Error' }));
  });

  it('prints help and exits with SUCCESS when --help is provided', async () => {
    const { stderrChunks } = captureStreams();
    mockExitWithSignal();

    process.argv = ['bun', 'hook', '--help'];

    try {
      await runHookCommand({
        model: standardHookModel,
        extension: '@putnami/test',
        hook: 'preBuild',
        run: () => ({}),
      });
      expect(true).toBe(false);
    } catch (error) {
      expect(error).toBeInstanceOf(ExitSignal);
      expect((error as ExitSignal).code).toBe(HookExitCodes.SUCCESS);
    }

    const stderr = stderrChunks.join('');
    expect(stderr).toContain('Usage:');
    expect(stderr).toContain('--putnami-context');
  });

  it('exits with INVALID_CONFIG when context path is missing', async () => {
    const { stderrChunks } = captureStreams();
    mockExitWithSignal();

    process.argv = ['bun', 'hook', '--verbose'];

    try {
      await runHookCommand({
        model: standardHookModel,
        extension: '@putnami/test',
        hook: 'preBuild',
        run: () => ({}),
      });
      expect(true).toBe(false);
    } catch (error) {
      expect(error).toBeInstanceOf(ExitSignal);
      expect((error as ExitSignal).code).toBe(HookExitCodes.INVALID_CONFIG);
    }

    expect(stderrChunks.join('')).toContain('--putnami-context is required');
  });

  it('handles argument parse failures as INVALID_CONFIG', async () => {
    const { stderrChunks } = captureStreams();
    mockExitWithSignal();

    const baseFlags = {
      putnamiContext: {
        flag: '--putnami-context',
        description: 'Path to the context JSON file',
        type: 'string' as const,
        required: true,
      },
      help: {
        flag: '--help',
        shorts: ['-h'],
        description: 'Show help',
        type: 'boolean' as const,
      },
    };

    let shouldThrow = true;
    const throwingFlags = new Proxy(baseFlags, {
      ownKeys(target) {
        if (shouldThrow) {
          shouldThrow = false;
          throw new Error('failed to inspect flags');
        }
        return Reflect.ownKeys(target);
      },
      getOwnPropertyDescriptor(target, property) {
        return Object.getOwnPropertyDescriptor(target, property);
      },
      get(target, property, receiver) {
        return Reflect.get(target, property, receiver);
      },
    });

    const brokenModel = {
      name: 'broken',
      description: 'Broken model',
      flags: throwingFlags,
      extend: standardHookModel.extend,
    };

    process.argv = ['bun', 'hook', '--putnami-context', '/tmp/context.json'];

    try {
      await runHookCommand({
        model: brokenModel as unknown as typeof standardHookModel,
        extension: '@putnami/test',
        hook: 'preBuild',
        run: () => ({}),
      });
      expect(true).toBe(false);
    } catch (error) {
      expect(error).toBeInstanceOf(ExitSignal);
      expect((error as ExitSignal).code).toBe(HookExitCodes.INVALID_CONFIG);
    }

    const stderr = stderrChunks.join('');
    expect(stderr).toContain('Error parsing arguments');
    expect(stderr).toContain('Usage:');
  });
});
