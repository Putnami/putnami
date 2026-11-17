import { afterEach, beforeEach, describe, expect, mock } from 'bun:test';
import { type LogEntry, registerTelemetryTokenSource, resetDefaultLogger, setRootLogger } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { MemoryLogger, resetTelemetryTokenSourceForTest } from '@putnami/runtime/testing';
import { OtlpLogSink } from '../../src/telemetry/otlp-log.sink';

interface OtlpAny {
  stringValue?: string;
  intValue?: string;
}
interface OtlpLogRecord {
  timeUnixNano?: string;
  severityNumber?: number;
  severityText?: string;
  body?: OtlpAny;
  attributes?: Array<{ key: string; value: OtlpAny }>;
  traceId?: string;
}
interface OtlpLogsRequest {
  resourceLogs: Array<{
    resource: { attributes?: Array<{ key: string; value: OtlpAny }> };
    scopeLogs: Array<{ scope: { name?: string }; logRecords: OtlpLogRecord[] }>;
  }>;
}

function entry(over: Partial<LogEntry>): LogEntry {
  return {
    level: 'info',
    message: 'msg',
    timestamp: new Date('2023-11-14T22:13:20.000Z').toISOString(),
    ...over,
  };
}

const FEATURE = 'typescript/service-telemetry';
const OTEL_KEYS = ['OTEL_SERVICE_NAME', 'OTEL_RESOURCE_ATTRIBUTES'] as const;

/** The string attributes of the resource of a logs request body. */
function resourceAttributes(body: string | undefined): Record<string, string> {
  const req = JSON.parse(body as string) as OtlpLogsRequest;
  const attributes = req.resourceLogs[0].resource.attributes ?? [];
  return Object.fromEntries(attributes.map((a) => [a.key, a.value.stringValue ?? '']));
}

describe('OtlpLogSink', () => {
  const originalFetch = globalThis.fetch;
  let saved: Record<string, string | undefined>;
  let logger: MemoryLogger;

  beforeEach(() => {
    saved = Object.fromEntries(OTEL_KEYS.map((key) => [key, process.env[key]]));
    for (const key of OTEL_KEYS) delete process.env[key];
    logger = new MemoryLogger();
    setRootLogger(logger);
  });

  afterEach(() => {
    globalThis.fetch = originalFetch;
    for (const key of OTEL_KEYS) {
      if (saved[key] === undefined) delete process.env[key];
      else process.env[key] = saved[key];
    }
    resetTelemetryTokenSourceForTest();
    resetDefaultLogger();
  });

  const warnings = () => logger.entries.filter((e) => e.level === 'warn').map((e) => e.message);

  /** A fetch double that answers `status` and records each call's authorization and body. */
  function recordingFetch(status = 200) {
    const calls: Array<{ authorization: string | undefined; body: string }> = [];
    globalThis.fetch = mock((_u: string, init: RequestInit) => {
      const headers = init.headers as Record<string, string>;
      calls.push({ authorization: headers['Authorization'], body: init.body as string });
      return Promise.resolve(new Response(null, { status }));
    }) as unknown as typeof fetch;
    return calls;
  }

  specTest(
    'asks the registered token source before each push, and a static bearer wins',
    { feature: FEATURE, requirement: 'per-push-credential', check: 'the-log-sink-authenticates-each-push' },
    async () => {
      const calls = recordingFetch();
      const asked: string[] = [];
      let issued = 0;
      registerTelemetryTokenSource(async (endpoint) => {
        asked.push(endpoint);
        issued += 1;
        return `token-${issued}`;
      });

      const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', flushIntervalMs: 1_000_000 });
      sink.write(entry({ message: 'first' }));
      await sink.flush();
      sink.write(entry({ message: 'second' }));
      await sink.close();

      const fixed = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        bearer: 'static',
        flushIntervalMs: 1_000_000,
      });
      fixed.write(entry({ message: 'third' }));
      await fixed.close();

      expect(asked).toEqual(['https://collector:4318', 'https://collector:4318']);
      expect(calls.map((call) => call.authorization)).toEqual(['Bearer token-1', 'Bearer token-2', 'Bearer static']);
    },
  );

  specTest(
    'a failing token source drops the batch and warns once',
    { feature: FEATURE, requirement: 'per-push-credential', check: 'a-log-sink-credential-failure-drops-the-batch' },
    async () => {
      const calls = recordingFetch();
      registerTelemetryTokenSource(async () => {
        throw new Error('metadata server unreachable: secret-detail');
      });

      const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', flushIntervalMs: 1_000_000 });
      sink.write(entry({ message: 'first' }));
      await sink.flush();
      sink.write(entry({ message: 'second' }));
      await sink.close();

      expect(calls).toHaveLength(0);
      const credentialWarnings = warnings().filter((message) => message.includes('could not obtain a credential'));
      expect(credentialWarnings).toHaveLength(1);
      expect(credentialWarnings[0]).not.toContain('secret-detail');
    },
  );

  specTest(
    'a collector refusal logs one warning however many pushes it refuses',
    { feature: FEATURE, requirement: 'per-push-credential', check: 'a-log-sink-refusal-warns-once' },
    async () => {
      const calls = recordingFetch(403);

      const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', flushIntervalMs: 1_000_000 });
      sink.write(entry({ message: 'first' }));
      await sink.flush();
      sink.write(entry({ message: 'second' }));
      await sink.close();

      expect(calls).toHaveLength(2);
      expect(warnings().filter((message) => message.includes('refused a push (HTTP 403)'))).toHaveLength(1);
    },
  );

  specTest(
    'the resource takes OTEL_SERVICE_NAME and OTEL_RESOURCE_ATTRIBUTES',
    {
      feature: FEATURE,
      requirement: 'service-identity-from-environment',
      check: 'the-log-sink-takes-the-environment-resource',
    },
    async () => {
      const calls = recordingFetch();
      process.env['OTEL_RESOURCE_ATTRIBUTES'] =
        'service.name=from-attributes,deployment.environment=prod,putnami.framework=go';

      const fromAttributes = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        serviceName: 'option',
        flushIntervalMs: 1_000_000,
      });
      fromAttributes.write(entry({}));
      await fromAttributes.close();

      process.env['OTEL_SERVICE_NAME'] = 'checkout';
      const fromName = new OtlpLogSink({ endpoint: 'https://collector:4318', flushIntervalMs: 1_000_000 });
      fromName.write(entry({}));
      await fromName.close();

      delete process.env['OTEL_SERVICE_NAME'];
      delete process.env['OTEL_RESOURCE_ATTRIBUTES'];
      const unnamed = new OtlpLogSink({ endpoint: 'https://collector:4318', flushIntervalMs: 1_000_000 });
      unnamed.write(entry({}));
      await unnamed.close();

      const [attributes, named, fallback] = calls.map((call) => resourceAttributes(call.body));
      expect(attributes['service.name']).toBe('from-attributes');
      expect(attributes['deployment.environment']).toBe('prod');
      expect(attributes['putnami.framework']).toBe('typescript');
      expect(named['service.name']).toBe('checkout');
      expect(named['deployment.environment']).toBe('prod');
      expect(fallback['service.name']).toBe('unknown');
    },
  );

  specTest(
    'batches records and POSTs them as OTLP/JSON to /v1/logs on close',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'log-record-mapping',
      check: 'a-log-record-maps-to-an-otlp-record',
    },
    async () => {
      let url: string | undefined;
      let body: string | undefined;
      globalThis.fetch = mock((u: string, init: RequestInit) => {
        url = u;
        body = init.body as string;
        return Promise.resolve(new Response(null, { status: 200 }));
      }) as unknown as typeof fetch;

      const sink = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        serviceName: 'checkout',
        flushIntervalMs: 1_000_000, // no background flush during the test
      });
      sink.write(
        entry({ level: 'info', message: 'request handled', logger: 'checkout.http', context: { route: '/checkout' } }),
      );
      sink.write(
        entry({
          level: 'error',
          message: 'payment failed',
          error: { name: 'PaymentError', message: 'declined' },
        }),
      );
      await sink.close();

      expect(url).toBe('https://collector:4318/v1/logs');
      const req = JSON.parse(body as string) as OtlpLogsRequest;
      const rl = req.resourceLogs[0];
      expect(rl.resource.attributes?.find((a) => a.key === 'service.name')?.value.stringValue).toBe('checkout');
      expect(rl.resource.attributes?.find((a) => a.key === 'putnami.framework')?.value.stringValue).toBe('typescript');

      const records = rl.scopeLogs[0].logRecords;
      expect(records).toHaveLength(2);
      expect(records[0].severityNumber).toBe(9); // info
      expect(records[0].severityText).toBe('info');
      expect(records[0].body?.stringValue).toBe('request handled');
      expect(records[0].attributes?.find((a) => a.key === 'route')?.value.stringValue).toBe('/checkout');
      expect(records[0].attributes?.find((a) => a.key === 'logger.name')?.value.stringValue).toBe('checkout.http');
      expect(records[1].severityNumber).toBe(17); // error
      expect(records[1].attributes?.find((a) => a.key === 'error.type')?.value.stringValue).toBe('PaymentError');
    },
  );

  specTest(
    'flattens a nested context group into dotted attributes',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'log-record-mapping',
      check: 'log-context-flattens-to-dotted-attributes',
    },
    async () => {
      let body: string | undefined;
      globalThis.fetch = mock((_u: string, init: RequestInit) => {
        body = init.body as string;
        return Promise.resolve(new Response(null, { status: 200 }));
      }) as unknown as typeof fetch;

      const sink = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        serviceName: 's',
        flushIntervalMs: 1_000_000,
      });
      sink.write(
        entry({
          context: {
            http: { method: 'GET', statusCode: 200 },
            event: { publishes: ['orders', 'carts'], publishCount: 2 },
          },
        }),
      );
      await sink.close();

      const req = JSON.parse(body as string) as OtlpLogsRequest;
      const attrs = req.resourceLogs[0].scopeLogs[0].logRecords[0].attributes ?? [];
      const find = (k: string): OtlpAny | undefined => attrs.find((a) => a.key === k)?.value;

      expect(find('http.method')?.stringValue).toBe('GET');
      expect(find('http.statusCode')?.intValue).toBe('200');
      expect(find('event.publishCount')?.intValue).toBe('2');
      // Arrays are JSON-stringified, not flattened into per-index keys.
      expect(find('event.publishes')?.stringValue).toBe('["orders","carts"]');
      // No "[object Object]" leaked as a value.
      for (const a of attrs) {
        expect(a.value.stringValue).not.toBe('[object Object]');
      }
    },
  );

  specTest(
    'flattens a single structured data object with the same precedence as JsonSink',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'log-record-mapping',
      check: 'log-context-flattens-to-dotted-attributes',
    },
    async () => {
      let body: string | undefined;
      globalThis.fetch = mock((_u: string, init: RequestInit) => {
        body = init.body as string;
        return Promise.resolve(new Response(null, { status: 200 }));
      }) as unknown as typeof fetch;

      const sink = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        serviceName: 's',
        flushIntervalMs: 1_000_000,
      });
      sink.write(
        entry({
          context: { database: { operation: 'stale' } },
          data: [{ database: { operation: 'query', datasource: 'primary', durationMs: 12, outcome: 'success' } }],
        }),
      );
      await sink.close();

      const req = JSON.parse(body as string) as OtlpLogsRequest;
      const attrs = req.resourceLogs[0].scopeLogs[0].logRecords[0].attributes ?? [];
      const find = (k: string): OtlpAny | undefined => attrs.find((a) => a.key === k)?.value;

      expect(find('database.operation')?.stringValue).toBe('query');
      expect(find('database.datasource')?.stringValue).toBe('primary');
      expect(find('database.durationMs')?.intValue).toBe('12');
      expect(find('database.outcome')?.stringValue).toBe('success');
    },
  );

  specTest(
    'serializes cyclic and nested BigInt context values without rejecting close',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'log-record-mapping',
      check: 'log-context-flattens-to-dotted-attributes',
    },
    async () => {
      let body: string | undefined;
      globalThis.fetch = mock((_u: string, init: RequestInit) => {
        body = init.body as string;
        return Promise.resolve(new Response(null, { status: 200 }));
      }) as unknown as typeof fetch;
      const cyclic: { self?: unknown } = {};
      cyclic.self = cyclic;

      const sink = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        serviceName: 's',
        flushIntervalMs: 1_000_000,
      });
      sink.write(entry({ context: { cyclic, nested: { value: { deep: { id: 42n } } } } }));
      await expect(sink.close()).resolves.toBeUndefined();

      const req = JSON.parse(body as string) as OtlpLogsRequest;
      const attrs = req.resourceLogs[0].scopeLogs[0].logRecords[0].attributes ?? [];
      const find = (k: string): OtlpAny | undefined => attrs.find((a) => a.key === k)?.value;
      expect(find('cyclic.self.self')?.stringValue).toBe('{"self":"[Circular]"}');
      expect(find('nested.value.deep')?.stringValue).toBe('{"id":"42"}');
    },
  );

  specTest(
    'does not POST when there is nothing buffered',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'log-sink-is-best-effort',
      check: 'an-empty-sink-posts-nothing',
    },
    async () => {
      const fetchMock = mock(() => Promise.resolve(new Response(null, { status: 200 })));
      globalThis.fetch = fetchMock as unknown as typeof fetch;
      const sink = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        serviceName: 's',
        flushIntervalMs: 1_000_000,
      });
      await sink.close();
      expect(fetchMock).not.toHaveBeenCalled();
    },
  );

  specTest(
    'drops the oldest records beyond the queue cap',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'log-sink-is-best-effort',
      check: 'the-queue-drops-the-oldest-records',
    },
    async () => {
      let body: string | undefined;
      globalThis.fetch = mock((_u: string, init: RequestInit) => {
        body = init.body as string;
        return Promise.resolve(new Response(null, { status: 200 }));
      }) as unknown as typeof fetch;

      const sink = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        serviceName: 's',
        flushIntervalMs: 1_000_000,
        maxQueue: 2,
      });
      sink.write(entry({ message: 'first' }));
      sink.write(entry({ message: 'second' }));
      sink.write(entry({ message: 'third' }));
      await sink.close();

      const req = JSON.parse(body as string) as OtlpLogsRequest;
      const messages = req.resourceLogs[0].scopeLogs[0].logRecords.map((r) => r.body?.stringValue);
      expect(messages).toEqual(['second', 'third']);
    },
  );

  specTest(
    'swallows collector errors (best-effort)',
    {
      feature: 'typescript/service-telemetry',
      requirement: 'log-sink-is-best-effort',
      check: 'a-collector-error-never-rejects-close',
    },
    async () => {
      globalThis.fetch = mock(() => Promise.reject(new Error('down'))) as unknown as typeof fetch;
      const sink = new OtlpLogSink({
        endpoint: 'https://collector:4318',
        serviceName: 's',
        flushIntervalMs: 1_000_000,
      });
      sink.write(entry({ message: 'x' }));
      await expect(sink.close()).resolves.toBeUndefined();
    },
  );
});
