// biome-ignore-all lint/suspicious/noConsole: Testing JsonSink which writes to console.log
import { afterEach, beforeEach, describe, expect, it, mock } from 'bun:test';
import { buildJsonRecord, JsonSink } from '../../src/logger/json.sink';
import type { LogEntry } from '../../src/logger/logger.type';

describe('JsonSink', () => {
  let sink: JsonSink;
  let lines: string[];
  const originalConsoleLog = console.log;

  beforeEach(() => {
    sink = new JsonSink();
    lines = [];
    console.log = mock((line?: unknown) => {
      lines.push(String(line));
    });
  });

  afterEach(() => {
    console.log = originalConsoleLog;
  });

  const emit = (entry: Partial<LogEntry>): Record<string, unknown> => {
    sink.write({
      level: 'info',
      message: 'real message',
      timestamp: '2026-05-28T00:00:00.000Z',
      ...entry,
    });
    expect(lines).toHaveLength(1);
    return JSON.parse(lines[0]) as Record<string, unknown>;
  };

  it('serializes reserved fields', () => {
    const record = emit({ level: 'warn', logger: 'svc' });
    expect(record['severity']).toBe('WARNING');
    expect(record['message']).toBe('real message');
    expect(record['timestamp']).toBe('2026-05-28T00:00:00.000Z');
    expect(record['logger']).toBe('svc');
  });

  it('serializes normal context fields as top-level entries', () => {
    const record = emit({ context: { userId: '42', region: 'eu' } });
    expect(record['userId']).toBe('42');
    expect(record['region']).toBe('eu');
    // reserved fields untouched
    expect(record['message']).toBe('real message');
  });

  it('does not let a context field clobber a reserved key', () => {
    const record = emit({
      context: { message: 'user said hi', severity: 'BOGUS', timestamp: 'nope', custom: 'kept' },
    });
    // reserved fields keep the log's own values
    expect(record['message']).toBe('real message');
    expect(record['severity']).toBe('INFO');
    expect(record['timestamp']).toBe('2026-05-28T00:00:00.000Z');
    // non-reserved context still serializes
    expect(record['custom']).toBe('kept');
  });

  it('serializes a single data object as top-level entries', () => {
    const record = emit({ data: [{ orderId: 'o-1', amount: 10 }] });
    expect(record['orderId']).toBe('o-1');
    expect(record['amount']).toBe(10);
  });

  it('does not let a data object field clobber a reserved key', () => {
    const record = emit({
      level: 'error',
      data: [{ message: 'hijack', logger: 'evil', keep: 'yes' }],
    });
    expect(record['message']).toBe('real message');
    expect(record['severity']).toBe('ERROR');
    // logger was not set on the entry, so it must not appear from the data object
    expect(record['logger']).toBeUndefined();
    // non-reserved data field still serializes
    expect(record['keep']).toBe('yes');
  });

  it('does not let context override the error field', () => {
    const record = emit({
      error: { name: 'Boom', message: 'kaboom' },
      context: { error: 'fake-error' },
    });
    expect(record['error']).toEqual({ name: 'Boom', message: 'kaboom' });
  });

  it('keeps multi-element data under the reserved data key', () => {
    const record = emit({ data: ['a', 'b'] });
    expect(record['data']).toEqual(['a', 'b']);
  });

  it('skips an appended reserved-key context field', () => {
    // append('message', …) would land a "message" array in context; the sink
    // must keep the log's own message and only surface non-reserved fields.
    const record = emit({ context: { message: ['forged'], keep: 'yes' } });
    expect(record['message']).toBe('real message');
    expect(record['keep']).toBe('yes');
  });

  it('buildJsonRecord returns the same record the sink prints', () => {
    const entry: LogEntry = {
      level: 'warn',
      message: 'real message',
      timestamp: '2026-05-28T00:00:00.000Z',
      logger: 'svc',
      context: { event: { publishes: ['a', 'b'], publishCount: 2 } },
    };
    const record = buildJsonRecord(entry);
    // Drive the real sink and parse its console line; both must match exactly.
    const printed = emit(entry);
    expect(printed).toEqual(record);
  });
});
