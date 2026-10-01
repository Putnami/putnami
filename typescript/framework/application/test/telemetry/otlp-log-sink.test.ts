import { afterEach, describe, expect, it, mock } from 'bun:test';
import type { LogEntry } from '@putnami/runtime';
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

describe('OtlpLogSink', () => {
  const originalFetch = globalThis.fetch;
  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('batches records and POSTs them as OTLP/JSON to /v1/logs on close', async () => {
    let url: string | undefined;
    let body: string | undefined;
    globalThis.fetch = mock((u: string, init: RequestInit) => {
      url = u;
      body = init.body as string;
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;

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
  });

  it('flattens a nested context group into dotted attributes', async () => {
    let body: string | undefined;
    globalThis.fetch = mock((_u: string, init: RequestInit) => {
      body = init.body as string;
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;

    const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 's', flushIntervalMs: 1_000_000 });
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
  });

  it('flattens a single structured data object with the same precedence as JsonSink', async () => {
    let body: string | undefined;
    globalThis.fetch = mock((_u: string, init: RequestInit) => {
      body = init.body as string;
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;

    const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 's', flushIntervalMs: 1_000_000 });
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
  });

  it('serializes cyclic and nested BigInt context values without rejecting close', async () => {
    let body: string | undefined;
    globalThis.fetch = mock((_u: string, init: RequestInit) => {
      body = init.body as string;
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;
    const cyclic: { self?: unknown } = {};
    cyclic.self = cyclic;

    const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 's', flushIntervalMs: 1_000_000 });
    sink.write(entry({ context: { cyclic, nested: { value: { deep: { id: 42n } } } } }));
    await expect(sink.close()).resolves.toBeUndefined();

    const req = JSON.parse(body as string) as OtlpLogsRequest;
    const attrs = req.resourceLogs[0].scopeLogs[0].logRecords[0].attributes ?? [];
    const find = (k: string): OtlpAny | undefined => attrs.find((a) => a.key === k)?.value;
    expect(find('cyclic.self.self')?.stringValue).toBe('{"self":"[Circular]"}');
    expect(find('nested.value.deep')?.stringValue).toBe('{"id":"42"}');
  });

  it('does not POST when there is nothing buffered', async () => {
    const fetchMock = mock(() => Promise.resolve(new Response(null, { status: 200 })));
    globalThis.fetch = fetchMock as typeof fetch;
    const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 's', flushIntervalMs: 1_000_000 });
    await sink.close();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('drops the oldest records beyond the queue cap', async () => {
    let body: string | undefined;
    globalThis.fetch = mock((_u: string, init: RequestInit) => {
      body = init.body as string;
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;

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
  });

  it('swallows collector errors (best-effort)', async () => {
    globalThis.fetch = mock(() => Promise.reject(new Error('down'))) as typeof fetch;
    const sink = new OtlpLogSink({ endpoint: 'https://collector:4318', serviceName: 's', flushIntervalMs: 1_000_000 });
    sink.write(entry({ message: 'x' }));
    await expect(sink.close()).resolves.toBeUndefined();
  });
});
