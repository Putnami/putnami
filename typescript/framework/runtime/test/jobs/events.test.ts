import { afterAll, describe, expect, it } from 'bun:test';
import { readdir, readFile } from 'node:fs/promises';
import { join } from 'node:path';
import {
  compareReadyEndpoints,
  EVENT_TYPES_V2,
  hasEventErrors,
  isKnownProtocolVersion,
  isValidReadyEndpoint,
  JobEventEmitter,
  MAX_KNOWN_PROTOCOL_VERSION,
  negotiatedRuntimeEventVersion,
  parseRuntimeEvent,
  PROTOCOL_VERSION,
  PROTOCOL_VERSION_2,
  READY_ENDPOINT_SCHEMES,
  READY_LOG_KEY,
  READY_TARGETS,
  type ReadyEndpoint,
  readyEndpointUrl,
  readyMarker,
  readyMarkerFromLogRecord,
  type RuntimeEvent,
  RUNTIME_EVENTS_ENV,
  sortReadyEndpoints,
  validateRuntimeEvent,
  validateRuntimeEventStream,
  validateReadyData,
} from '../../src/jobs';

// These tests ARE run by a Putnami CLI that advertises v2, and a
// default-constructed JobEventEmitter reads that advertisement. Clearing it
// makes the suite hermetic: every emitter below states its version explicitly.
// Counterpart: TestMain in tooling/extension-sdk/jsonl/readiness_test.go.
const ADVERTISED_VERSION = process.env[RUNTIME_EVENTS_ENV];
delete process.env[RUNTIME_EVENTS_ENV];
afterAll(() => {
  if (ADVERTISED_VERSION !== undefined) process.env[RUNTIME_EVENTS_ENV] = ADVERTISED_VERSION;
});

// FIXTURES_DIR is the shared corpus the Go protocol package
// (protocols/runtime) asserts against too. Both languages
// validating the same files is the cross-language parity guarantee.
const FIXTURES_DIR = join(__dirname, '../../../../../protocols/runtime/fixtures');

function captureLines(emit: (e: JobEventEmitter) => void, version?: number): string[] {
  const lines: string[] = [];
  const emitter = new JobEventEmitter((line) => lines.push(line), version);
  emit(emitter);
  return lines;
}

function captureEvents(emit: (e: JobEventEmitter) => void, version?: number): RuntimeEvent[] {
  return captureLines(emit, version).map((line) => {
    const event = parseRuntimeEvent(line.trimEnd());
    expect(event).not.toBeNull();
    return event as RuntimeEvent;
  });
}

describe('runtime job event emitter', () => {
  it('emits one protocol-valid event per method', () => {
    const events = captureEvents((e) => {
      e.meta('@putnami/typescript', 'build');
      e.log('info', 'plain log');
      e.log('error', 'forwarded', { requestId: 'r-1' }, { message: 'boom', stack: 'trace' });
      e.info('info helper');
      e.warn('warn helper');
      e.error('error helper');
      e.debug('debug helper');
      e.phaseStart('compile');
      e.phaseEnd('compile', 'success');
      e.progress(3, 10, 'compiling');
      e.diagnostic('warning', 'unused variable', { file: 'main.ts', line: 12 });
      e.diagnostic('error', 'type mismatch', { file: 'main.ts', line: 4, column: 9 }, 'TS2322');
      e.metric('tests-total', 42, 'count');
      e.metric('coverage', 81.5, 'percent');
      e.artifact('bin', 'app', 'binary', 'dist/app');
      e.artifact('pkg', 'lib', 'package', 'dist/lib.tgz', { registry: 'npm', version: '1.2.3' });
      e.summary('all good');
      e.summary('42 tests', { tests: 42, failures: 0 });
      e.result('OK', { binaryPath: 'dist/app' });
    });

    expect(events).toHaveLength(19);
    for (const event of events) {
      expect(event.v).toBe(PROTOCOL_VERSION);
      expect(event.time).toBeString();
      expect(validateRuntimeEvent(event)).toEqual([]);
    }
  });

  it('matches the Go SDK wire shape for meta, result, and summary extras', () => {
    const [meta, result, summary] = captureEvents((e) => {
      e.meta('@putnami/go', 'build');
      e.result('FAILED', undefined, { message: 'build failed', code: 'BUILD_ERROR' });
      e.summary('2 tests', { tests: 2 });
    });

    // Counterpart: tooling/extension-sdk/jsonl emitter output, pinned by
    // tooling/extension-sdk/jsonl/drift_test.go on the Go side.
    expect(meta.level).toBe('info');
    expect(meta.message).toBe('Starting build');
    expect(meta.data).toEqual({ extension: '@putnami/go', job: 'build' });

    expect(result.level).toBe('info');
    expect(result.message).toBe('Job FAILED');
    expect(result.data).toEqual({
      status: 'FAILED',
      error: { message: 'build failed', code: 'BUILD_ERROR' },
    });

    expect(summary.tests).toBe(2);
    expect(summary.message).toBe('2 tests');
  });

  it('drops non-finite metric values instead of violating the schema', () => {
    const events = captureEvents((e) => {
      e.metric('bad', Number.NaN, 'count');
    });
    expect(events).toHaveLength(1);
    expect(events[0].type).toBe('log');
    expect(events[0].level).toBe('debug');
  });

  it('emits timestamps in the protocol format', () => {
    const [event] = captureEvents((e) => e.info('x'));
    expect(event.time).toMatch(/^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/);
  });
});

describe('cross-language conformance', () => {
  // Counterpart: protocols/runtime/conformance_test.go. The Go
  // parser+validator accepts every valid fixture and rejects every
  // invalid one; the TypeScript implementation must agree.
  async function readFixtureStream(
    dir: string,
    file: string,
  ): Promise<{ events: RuntimeEvent[]; parseFailures: number }> {
    const content = await readFile(join(dir, file), 'utf8');
    const events: RuntimeEvent[] = [];
    let parseFailures = 0;
    for (const line of content.split('\n')) {
      if (!line.trim()) continue;
      const event = parseRuntimeEvent(line);
      if (event === null) parseFailures++;
      else events.push(event);
    }
    return { events, parseFailures };
  }

  it('accepts every valid shared fixture', async () => {
    const dir = join(FIXTURES_DIR, 'valid');
    const files = (await readdir(dir)).filter((f) => f.endsWith('.jsonl'));
    expect(files.length).toBeGreaterThan(0);

    for (const file of files) {
      const { events, parseFailures } = await readFixtureStream(dir, file);
      expect(parseFailures).toBe(0);
      expect(hasEventErrors(validateRuntimeEventStream(events))).toBe(false);
    }
  });

  it('rejects every invalid shared fixture', async () => {
    const dir = join(FIXTURES_DIR, 'invalid');
    const files = (await readdir(dir)).filter((f) => f.endsWith('.jsonl'));
    expect(files.length).toBeGreaterThan(0);

    for (const file of files) {
      const { events, parseFailures } = await readFixtureStream(dir, file);
      const rejected = parseFailures > 0 || hasEventErrors(validateRuntimeEventStream(events));
      expect(rejected).toBe(true);
    }
  });

  // Counterpart: protocols/runtime/conformance_v2_test.go. "Conformance
  // fixtures across both runtimes" means exactly this: one corpus, two
  // validators, same verdicts — including the serve/restart readiness stream.
  it('accepts every valid v2 fixture', async () => {
    const dir = join(FIXTURES_DIR, 'v2/valid');
    const files = (await readdir(dir)).filter((f) => f.endsWith('.jsonl'));
    expect(files.length).toBeGreaterThan(0);

    for (const file of files) {
      const { events, parseFailures } = await readFixtureStream(dir, file);
      expect(parseFailures).toBe(0);
      expect(hasEventErrors(validateRuntimeEventStream(events))).toBe(false);
      for (const event of events) {
        expect(event.v).toBe(PROTOCOL_VERSION_2);
      }
    }
  });

  it('rejects every invalid v2 fixture', async () => {
    const dir = join(FIXTURES_DIR, 'v2/invalid');
    const files = (await readdir(dir)).filter((f) => f.endsWith('.jsonl'));
    expect(files.length).toBeGreaterThan(0);

    for (const file of files) {
      const { events, parseFailures } = await readFixtureStream(dir, file);
      const rejected = parseFailures > 0 || hasEventErrors(validateRuntimeEventStream(events));
      expect(rejected).toBe(true);
    }
  });

  it('re-announces readiness on every serve iteration', async () => {
    const { events } = await readFixtureStream(join(FIXTURES_DIR, 'v2/valid'), 'serve-restart.jsonl');
    expect(hasEventErrors(validateRuntimeEventStream(events))).toBe(false);
    // Readiness is per serve ITERATION, not per process: unlike `result`, more
    // than one `ready` in a stream is legal and is how a restart is observed.
    expect(events.filter((e) => e.type === 'ready')).toHaveLength(2);
  });
});

describe('runtime event protocol v2', () => {
  it('scopes the event vocabulary to the protocol version', () => {
    expect(EVENT_TYPES_V2).toContain('ready');
    expect(isKnownProtocolVersion(PROTOCOL_VERSION)).toBe(true);
    expect(isKnownProtocolVersion(PROTOCOL_VERSION_2)).toBe(true);
    expect(isKnownProtocolVersion(0)).toBe(false);
    expect(isKnownProtocolVersion(3)).toBe(false);

    // A ready event at v1 is an unknown type — that is what keeps v2 additive
    // instead of a retroactive widening of v1.
    const atV1 = validateRuntimeEvent({ v: 1, type: 'ready', data: { target: 'workload' } } as RuntimeEvent);
    expect(atV1.some((d) => d.code === 'invalid-event-type')).toBe(true);
  });

  it('rejects a stream that mixes protocol versions', () => {
    const diags = validateRuntimeEventStream([
      { v: 1, type: 'log', level: 'info', message: 'hello' },
      { v: 2, type: 'result', data: { status: 'OK' } },
    ] as RuntimeEvent[]);
    expect(diags.some((d) => d.code === 'mixed-protocol-version')).toBe(true);
  });

  it('orders endpoints by byte order, not locale order', () => {
    // Counterpart: protocols/runtime CompareReadyEndpoints. A locale-aware
    // comparison would order these differently on some ICU builds, which is
    // exactly the non-determinism the canonical order exists to prevent.
    const endpoints: ReadyEndpoint[] = [
      { scheme: 'https', host: 'localhost', port: 8443 },
      { scheme: 'http', host: 'localhost', port: 3000, path: '/api' },
      { scheme: 'grpc', host: '0.0.0.0', port: 50_051 },
      { scheme: 'http', host: 'localhost', port: 3000 },
    ];
    const sorted = sortReadyEndpoints(endpoints);
    expect(sorted.map(readyEndpointUrl)).toEqual([
      'grpc://0.0.0.0:50051',
      'http://localhost:3000',
      'http://localhost:3000/api',
      'https://localhost:8443',
    ]);
    // The caller's slice is never reordered under it.
    expect(endpoints[0].scheme).toBe('https');
    expect(compareReadyEndpoints(sorted[0], sorted[0])).toBe(0);
    expect(READY_TARGETS).toEqual(['server', 'workload']);
    expect(READY_ENDPOINT_SCHEMES).toEqual(['grpc', 'http', 'https', 'tcp']);
  });

  // Counterpart: TestDrift_ReadyWireShape in tooling/extension-sdk/jsonl. The
  // expected line below is byte-for-byte the one the Go emitter writes for the
  // same facts — member order included — so a divergence fails on both sides.
  it('matches the Go SDK readiness wire shape byte for byte', () => {
    const [line] = captureLines(
      (e) =>
        e.ready({
          target: 'server',
          name: 'api',
          endpoints: [
            { scheme: 'https', host: 'localhost', port: 8443 },
            { scheme: 'http', host: 'localhost', port: 3000, path: '/api' },
          ],
          durationMs: 420,
        }),
      PROTOCOL_VERSION_2,
    );
    const parsed = JSON.parse(line) as RuntimeEvent;
    const normalized = line.trimEnd().replace(parsed.time as string, 'T');
    expect(normalized).toBe(
      '{"v":2,"type":"ready","time":"T","data":{"target":"server","name":"api",' +
        '"endpoints":[{"scheme":"http","host":"localhost","port":3000,"path":"/api"},' +
        '{"scheme":"https","host":"localhost","port":8443}],"durationMs":420}}',
    );
    expect(line).not.toContain('"url"');
  });

  it('stamps one version on every line of a stream', () => {
    const events = captureEvents((e) => {
      e.meta('@putnami/typescript', 'serve');
      e.info('starting');
      e.ready({ target: 'server', endpoints: [{ scheme: 'http', host: 'localhost', port: 3000 }] });
      e.summary('done', { tests: 1 });
      e.result('OK');
    }, PROTOCOL_VERSION_2);

    expect(events).toHaveLength(5);
    for (const event of events) {
      expect(event.v).toBe(PROTOCOL_VERSION_2);
    }
    expect(hasEventErrors(validateRuntimeEventStream(events))).toBe(false);
  });

  it('writes nothing and reports false when readiness is not negotiated', () => {
    const emitter = new JobEventEmitter(() => {
      throw new Error('a v1 stream must not write a readiness line');
    }, PROTOCOL_VERSION);
    expect(emitter.supportsReady).toBe(false);
    expect(emitter.ready({ target: 'workload' })).toBe(false);

    // The default stream stays v1 with no advertisement, so an older consumer
    // receives the byte-identical stream it always parsed.
    const events = captureEvents((e) => e.info('hello'));
    expect(events[0].v).toBe(PROTOCOL_VERSION);
  });

  it('resolves the CLI advertisement exactly as the Go helper does', () => {
    // Counterpart: TestNegotiatedVersion_Total in protocols/runtime.
    const table: [string | undefined, number][] = [
      [undefined, PROTOCOL_VERSION],
      ['', PROTOCOL_VERSION],
      ['   ', PROTOCOL_VERSION],
      ['nonsense', PROTOCOL_VERSION],
      ['2.0', PROTOCOL_VERSION],
      ['0', PROTOCOL_VERSION],
      ['-1', PROTOCOL_VERSION],
      ['1', PROTOCOL_VERSION],
      ['2', PROTOCOL_VERSION_2],
      [' 2 ', PROTOCOL_VERSION_2],
      ['3', MAX_KNOWN_PROTOCOL_VERSION],
      ['99', MAX_KNOWN_PROTOCOL_VERSION],
      // In-int64 huge values clamp exactly as Go's Atoi+clamp does...
      ['9223372036854775807', MAX_KNOWN_PROTOCOL_VERSION],
      // ...and OUTSIDE int64, Go's Atoi errors -> fail closed to v1. parseInt
      // would have silently kept precision-lossy garbage and clamped it UP.
      ['9223372036854775808', PROTOCOL_VERSION],
      ['99999999999999999999999999', PROTOCOL_VERSION],
      ['-9223372036854775809', PROTOCOL_VERSION],
    ];
    for (const [raw, want] of table) {
      expect(negotiatedRuntimeEventVersion(raw)).toBe(want);
      expect(isKnownProtocolVersion(negotiatedRuntimeEventVersion(raw))).toBe(true);
    }
    expect(RUNTIME_EVENTS_ENV).toBe('PUTNAMI_RUNTIME_EVENTS');
  });

  it('returns a diagnostic instead of throwing on a non-array endpoints member', () => {
    // Untrusted wire input: .forEach on a non-array crashed the validator
    // before it could return findings - the one failure a validator must not have.
    const malformed = { target: 'server', endpoints: 'http://localhost:3000' } as unknown as Parameters<
      typeof validateReadyData
    >[0];
    const diags = validateReadyData(malformed);
    expect(diags.some((d) => d.code === 'invalid-type' && d.field === 'data.endpoints')).toBe(true);
  });

  it('accepts only an endpoint the Go validator accepts', () => {
    const endpoint = (port: number): ReadyEndpoint => ({ scheme: 'http', host: 'localhost', port });
    expect(isValidReadyEndpoint(endpoint(8080))).toBe(true);
    expect(isValidReadyEndpoint({ ...endpoint(443), path: '/api' })).toBe(true);
    // Go decodes the port into an int: a fractional or NaN port is a payload
    // Go rejects, so the TypeScript twin must reject it too.
    for (const port of [0, 70_000, 8080.5, Number.NaN]) {
      expect(isValidReadyEndpoint(endpoint(port))).toBe(false);
    }
    expect(isValidReadyEndpoint({ ...endpoint(80), path: 'api' })).toBe(false);
    expect(isValidReadyEndpoint({ scheme: 'amqp', host: 'h', port: 5672 } as unknown as ReadyEndpoint)).toBe(false);
  });

  describe('reserved readiness log marker', () => {
    it('recognizes and canonicalizes a well-formed marker', () => {
      const record = {
        severity: 'INFO',
        message: '⚡️ listening http://localhost:3000',
        [READY_LOG_KEY]: {
          target: 'server',
          endpoints: [
            { scheme: 'https', host: 'localhost', port: 8443 },
            { scheme: 'http', host: 'localhost', port: 3000 },
          ],
          durationMs: 12,
        },
      };
      const data = readyMarkerFromLogRecord(record);
      expect(data).not.toBeNull();
      expect(data?.endpoints?.map(readyEndpointUrl)).toEqual(['http://localhost:3000', 'https://localhost:8443']);
      expect(READY_LOG_KEY).toBe('putnami.ready');
    });

    it('fails closed on a marker that would produce an invalid event', () => {
      const cases: unknown[] = [
        { endpoints: [{ scheme: 'http', host: 'h', port: 1 }] }, // no target
        { target: 'database' },
        { target: 'server' }, // a server claim with no address
        { target: 'server', endpoints: [{ scheme: 'amqp', host: 'h', port: 5672 }] },
        { target: 'server', endpoints: [{ scheme: 'http', host: 'h', port: 70_000 }] },
        { target: 'server', endpoints: [{ scheme: 'http', host: 'h', port: 80, path: 'api' }] },
        {
          target: 'server',
          endpoints: [
            { scheme: 'http', host: 'h', port: 80 },
            { scheme: 'http', host: 'h', port: 80 },
          ],
        },
        { target: 'workload', durationMs: -1 },
        { target: 'server', endpoints: 'http://localhost' },
        // A null or scalar entry must fail closed, not throw out of the
        // canonicalizing map: this payload is untrusted workload output.
        { target: 'server', endpoints: [null] },
        { target: 'server', endpoints: ['http://localhost:3000'] },
        'ready',
        {},
      ];
      for (const marker of cases) {
        expect(readyMarkerFromLogRecord({ [READY_LOG_KEY]: marker })).toBeNull();
      }
      expect(readyMarkerFromLogRecord(null)).toBeNull();
      expect(readyMarkerFromLogRecord({ severity: 'INFO' })).toBeNull();
      expect(readyMarkerFromLogRecord({ [READY_LOG_KEY]: null })).toBeNull();
    });

    it('omits empty optional members, matching the Go omitempty rules', () => {
      expect(readyMarker({ target: 'workload' })).toEqual({ target: 'workload' });
      expect(readyMarker({ target: 'workload', name: '', durationMs: 0, endpoints: [] })).toEqual({
        target: 'workload',
      });
    });
  });
});
