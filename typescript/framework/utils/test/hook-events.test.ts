import { afterEach, beforeEach, describe, expect, it } from 'bun:test';
import {
  createEvent,
  emitArtifact,
  emitError,
  emitEvent,
  emitLog,
  emitProgress,
  emitSummary,
  parseEvent,
  validateHookContext,
} from '../src/server/hooks';

const originalStdoutWrite = process.stdout.write.bind(process.stdout);
const writes: string[] = [];

beforeEach(() => {
  writes.length = 0;
  process.stdout.write = ((chunk: unknown) => {
    writes.push(String(chunk));
    return true;
  }) as typeof process.stdout.write;
});

afterEach(() => {
  process.stdout.write = originalStdoutWrite as typeof process.stdout.write;
});

function parseWrittenEvents(): Record<string, unknown>[] {
  return writes.map((line) => JSON.parse(line.trim()) as Record<string, unknown>);
}

describe('hook-events', () => {
  it('creates timestamped events', () => {
    const event = createEvent({
      type: 'log',
      level: 'info',
      message: 'hello',
      data: { step: 1 },
    });

    expect(event.v).toBe(1);
    expect(typeof event.time).toBe('string');
    expect(event.type).toBe('log');
    expect(event.message).toBe('hello');
  });

  it('emits events to stdout as JSON lines', () => {
    emitEvent(
      createEvent({
        type: 'log',
        level: 'debug',
        message: 'raw event',
      }),
    );

    const [event] = parseWrittenEvents();
    expect(event['type']).toBe('log');
    expect(event['level']).toBe('debug');
    expect(event['message']).toBe('raw event');
  });

  it('emits all helper event types', () => {
    emitLog('warn', 'log message', { value: 42 });
    emitProgress('progress message', 30, 'step-1');
    emitArtifact('write', 'dist/file.js');
    emitArtifact('copy', 'dist/copied.js', 'custom artifact message');
    emitSummary('summary message', { durationMs: 12, outputs: 2 }, 'debug');
    emitError('error message');
    emitError('error with stack', new Error('boom'), true);

    const events = parseWrittenEvents();
    expect(events.map((event) => event['type'])).toEqual([
      'log',
      'progress',
      'artifact',
      'artifact',
      'summary',
      'error',
      'error',
    ]);

    expect(events[0]['level']).toBe('warn');
    expect((events[0]['data'] as Record<string, unknown>)['value']).toBe(42);

    expect(events[1]['level']).toBe('info');
    expect(events[1]['percent']).toBe(30);
    expect(events[1]['step']).toBe('step-1');

    expect(events[2]['message']).toBe('write dist/file.js');
    expect(events[3]['message']).toBe('custom artifact message');

    expect(events[4]['level']).toBe('debug');
    expect((events[4]['data'] as Record<string, unknown>)['outputs']).toBe(2);

    expect(events[5]['recoverable']).toBe(false);
    expect(events[5]['data']).toBeUndefined();

    expect(events[6]['recoverable']).toBe(true);
    const errorData = events[6]['data'] as Record<string, unknown>;
    expect(errorData['code']).toBe('Error');
    expect(typeof errorData['stack']).toBe('string');
  });

  it('parses valid JSONL events and rejects invalid lines', () => {
    const validLine = JSON.stringify({
      v: 1,
      time: new Date().toISOString(),
      type: 'log',
      level: 'info',
      message: 'ok',
    });

    expect(parseEvent(validLine)?.message).toBe('ok');
    expect(parseEvent(JSON.stringify({ v: 1, type: 'log' }))).toBeNull();
    expect(parseEvent('not-json')).toBeNull();
  });

  it('rejects malformed and hostile JSONL lines at the boundary', () => {
    // Non-object JSON payloads must never be cast to a HookEvent.
    expect(parseEvent('null')).toBeNull();
    expect(parseEvent('42')).toBeNull();
    expect(parseEvent('"a string"')).toBeNull();
    expect(parseEvent('[{"v":1,"type":"log","level":"info","message":"x"}]')).toBeNull();

    // Wrong schema version / unknown type / unknown level / non-string message.
    expect(parseEvent(JSON.stringify({ v: 2, type: 'log', level: 'info', message: 'x' }))).toBeNull();
    expect(parseEvent(JSON.stringify({ v: 1, type: 'evil', level: 'info', message: 'x' }))).toBeNull();
    expect(parseEvent(JSON.stringify({ v: 1, type: 'log', level: 'fatal', message: 'x' }))).toBeNull();
    expect(parseEvent(JSON.stringify({ v: 1, type: 'log', level: 'info', message: 123 }))).toBeNull();

    // A prototype-pollution attempt is just an object that fails the envelope check.
    expect(parseEvent('{"__proto__":{"polluted":true},"type":"log"}')).toBeNull();
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
  });
});

describe('validateHookContext', () => {
  const validContext = {
    workspaceRoot: '/workspace',
    projectRoot: '/project',
    extensionRoot: '/extension',
    outputRoot: '/output',
    cacheRoot: '/cache',
    debug: false,
    hook: 'preBuild',
    extension: '@putnami/test',
  };

  it('accepts a well-formed context (with optional config / mode)', () => {
    expect(validateHookContext(validContext)).toEqual([]);
    expect(validateHookContext({ ...validContext, config: { feature: true }, mode: 'build' })).toEqual([]);
    expect(validateHookContext({ ...validContext, hook: 'configExtract', mode: 'config-extract' })).toEqual([]);
  });

  it('rejects non-object input', () => {
    expect(validateHookContext(null).length).toBeGreaterThan(0);
    expect(validateHookContext('a string').length).toBeGreaterThan(0);
    expect(validateHookContext([]).length).toBeGreaterThan(0);
  });

  it('reports missing required envelope fields', () => {
    const errors = validateHookContext({ debug: false });
    const fields = errors.map((e) => e.field);
    expect(fields).toContain('context.projectRoot');
    expect(fields).toContain('context.workspaceRoot');
    expect(fields).toContain('context.extension');
  });

  it('rejects wrong types for required fields', () => {
    const errors = validateHookContext({ ...validContext, projectRoot: 123, debug: 'nope' });
    const fields = errors.map((e) => e.field);
    expect(fields).toContain('context.projectRoot');
    expect(fields).toContain('context.debug');
  });

  it('rejects an out-of-range mode and a non-object config', () => {
    expect(validateHookContext({ ...validContext, mode: 'deploy' }).some((e) => e.field === 'context.mode')).toBe(true);
    expect(validateHookContext({ ...validContext, config: 'oops' }).some((e) => e.field === 'context.config')).toBe(
      true,
    );
  });

  it('treats projectName as an optional string (backward compatible)', () => {
    // A context from a newer Go runner carries the putnami project identity.
    expect(validateHookContext({ ...validContext, projectName: 'identity/workloads/auth-server' })).toEqual([]);
    // A context from an older runner omits it entirely and must still validate.
    expect(validateHookContext(validContext)).toEqual([]);
    // When present it must be a string.
    expect(
      validateHookContext({ ...validContext, projectName: 123 }).some((e) => e.field === 'context.projectName'),
    ).toBe(true);
  });
});
