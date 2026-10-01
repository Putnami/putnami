import { describe, expect, it } from 'bun:test';
import type { LogEntry } from '../../src/logger/logger.type';
import {
  type ConformanceCase,
  compareRecord,
  findCase,
  findRecord,
  loadCases,
  TOKEN_NUMBER,
  TOKEN_STRING,
  TOKEN_TIMESTAMP,
} from '../../src/testing/log-conformance';

/**
 * These tests pin the harness's OWN semantics — tokens, `$open`, closed objects,
 * the sorted-key 2-space canonical form — because every boundary test of
 * `@putnami/application`, `@putnami/events` and `@putnami/database` trusts them.
 * They are the TypeScript twin of `go/framework/logger/logtest/logtest_test.go`;
 * the two implementations must accept and reject exactly the same records.
 */

/** A case whose record is `record` (id/boundary only matter for messages). */
function caseOf(record: Record<string, unknown>): ConformanceCase {
  return { id: 'harness.case', level: 'required', boundary: 'http', record };
}

/**
 * An entry that renders to the given top-level record fields. `data` carries a
 * lone object, which is exactly how the sink receives a boundary group, so the
 * fixtures below travel the real `buildJsonRecord` flattening path.
 */
function entryOf(fields: Record<string, unknown>, overrides: Partial<LogEntry> = {}): LogEntry {
  return {
    level: 'info',
    message: 'ok',
    timestamp: '2026-07-25T10:11:12.345Z',
    data: [fields],
    ...overrides,
  };
}

describe('log conformance harness', () => {
  it('loads the canonical corpus for every boundary', () => {
    expect(loadCases('http')).toHaveLength(3);
    for (const boundary of ['http', 'event', 'database', 'migration']) {
      expect(loadCases(boundary).length).toBeGreaterThan(0);
    }
    const success = findCase(loadCases('http'), 'http.terminal.success');
    expect(success.record['logger']).toBe('http');
    expect(success.record['message']).toBe('[GET] /conformance/orders');
    // A renamed/deleted case must break the test, never silently disable it.
    expect(() => findCase(loadCases('http'), 'http.terminal.gone')).toThrow(/not found/);
    expect(() => loadCases('nope')).toThrow(/no case for boundary/);
  });

  it('accepts token leaves by type', () => {
    const want = caseOf({
      severity: 'INFO',
      message: 'ok',
      timestamp: TOKEN_TIMESTAMP,
      traceId: TOKEN_STRING,
      http: { durationMs: TOKEN_NUMBER },
    });
    const entry = entryOf({ http: { durationMs: 7 } }, { traceId: 'req-1' });

    const { ok, detail } = compareRecord(entry, want);
    expect(detail).toBe('');
    expect(ok).toBe(true);
  });

  it('rejects a token type mismatch', () => {
    // A duration emitted as a STRING is exactly the drift the token guards against.
    const want = caseOf({
      severity: 'INFO',
      message: 'ok',
      timestamp: TOKEN_TIMESTAMP,
      http: { durationMs: TOKEN_NUMBER },
    });
    const { ok, detail } = compareRecord(entryOf({ http: { durationMs: '7' } }), want);
    expect(ok).toBe(false);
    expect(detail).toContain(TOKEN_NUMBER);

    // Non-UTC / non-millisecond timestamps fail the pattern.
    const tsWant = caseOf({ severity: 'INFO', message: 'ok', timestamp: TOKEN_TIMESTAMP });
    for (const bad of ['2026-07-25T10:11:12Z', '2026-07-25T10:11:12.345678Z', '2026-07-25T10:11:12.345+02:00']) {
      expect(compareRecord(entryOf({}, { timestamp: bad, data: undefined }), tsWant).ok).toBe(false);
    }
    // An empty string is not a <string>.
    const traceWant = caseOf({ severity: 'INFO', message: 'ok', timestamp: TOKEN_TIMESTAMP, traceId: TOKEN_STRING });
    expect(compareRecord(entryOf({}, { traceId: '', data: undefined }), traceWant).ok).toBe(false);
  });

  it('rejects unexpected keys, at the top level and inside a closed group', () => {
    const want = caseOf({ severity: 'INFO', message: 'ok', timestamp: TOKEN_TIMESTAMP, http: { status: 200 } });

    const extraTopLevel = compareRecord(entryOf({ http: { status: 200 }, requestId: 'leaked' }), want);
    expect(extraTopLevel.ok).toBe(false);
    expect(extraTopLevel.detail).toContain('requestId');

    // Drift by addition inside a closed group (e.g. an extra `duration`).
    expect(compareRecord(entryOf({ http: { status: 200, duration: 7 } }), want).ok).toBe(false);
    // …and a missing expected group fails too.
    expect(compareRecord(entryOf({}, { data: undefined }), want).ok).toBe(false);
  });

  it('strips extras only under $open', () => {
    const want = caseOf({
      severity: 'ERROR',
      message: 'ok',
      timestamp: TOKEN_TIMESTAMP,
      error: { $open: true, name: TOKEN_STRING, message: 'boom' },
    });
    // TypeScript's serialized error legitimately adds stack/cause/custom fields;
    // Go's ErrorInfo adds code/category/retryable.
    const entry = entryOf(
      {},
      {
        level: 'error',
        data: undefined,
        error: { name: 'Error', message: 'boom', stack: '…', cause: { name: 'Error', message: 'inner' } },
      },
    );
    const { ok, detail } = compareRecord(entry, { ...want, record: { ...want.record, severity: 'ERROR' } });
    expect(detail).toBe('');
    expect(ok).toBe(true);

    // $open still pins the keys it declares…
    const other = entryOf({}, { level: 'error', data: undefined, error: { name: 'Error', message: 'other' } });
    expect(compareRecord(other, want).ok).toBe(false);
    // …and the marker never leaks into the canonical text.
    expect(compareRecord(other, want).detail).not.toContain('$open');
  });

  it('compares appended arrays element-wise', () => {
    const want = caseOf({
      severity: 'INFO',
      message: 'ok',
      timestamp: TOKEN_TIMESTAMP,
      event: {
        publishCount: 2,
        publishes: [
          { topic: 't', messageId: TOKEN_STRING },
          { topic: 't', messageId: TOKEN_STRING },
        ],
      },
    });
    const full = entryOf({
      event: {
        publishCount: 2,
        publishes: [
          { topic: 't', messageId: 'm-1' },
          { topic: 't', messageId: 'm-2' },
        ],
      },
    });
    expect(compareRecord(full, want).ok).toBe(true);

    // A dropped publish (accumulation regression) must fail.
    const short = entryOf({ event: { publishCount: 2, publishes: [{ topic: 't', messageId: 'm-1' }] } });
    expect(compareRecord(short, want).ok).toBe(false);
  });

  it('names the corpus and both runtimes in the failure detail', () => {
    const want: ConformanceCase = {
      id: 'event.terminal.dlq',
      level: 'required',
      boundary: 'event',
      record: { message: 'message dead-lettered' },
    };
    const { detail } = compareRecord(entryOf({}, { message: 'message handled', data: undefined }), want);
    expect(detail).toContain('protocols/logging/conformance/manifest.json');
    expect(detail).toContain('typescript/framework/events/test/logging-cross-language.test.ts');
    expect(detail).toContain('go/framework/events/logging_cross_language_test.go');
  });

  it('requires exactly one record per pinned (logger, message) pair', () => {
    const want = findCase(loadCases('event'), 'event.terminal.success');
    const handled: LogEntry = {
      level: 'info',
      message: 'message handled',
      timestamp: '2026-07-25T10:11:12.345Z',
      logger: 'events.handler',
    };
    expect(findRecord([handled], want)).toBe(handled);
    // Two terminal records for one delivery is a contract violation.
    expect(() => findRecord([handled, { ...handled }], want)).toThrow(/expected exactly 1/);
    expect(() => findRecord([], want)).toThrow(/expected exactly 1/);
  });
});
