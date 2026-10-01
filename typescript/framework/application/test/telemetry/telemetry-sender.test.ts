import { afterEach, describe, expect, it, mock } from 'bun:test';
import type { MetricsBucket } from '../../src/telemetry/telemetry.collector';
import {
  buildMetricsBody,
  buildTracesBody,
  joinUrl,
  postOtlp,
  sendMetrics,
  sendTraces,
} from '../../src/telemetry/telemetry.sender';

// Minimal shapes for poking at the rendered OTLP body in assertions.
interface OtlpAny {
  stringValue?: string;
  intValue?: string;
  doubleValue?: number;
}
interface OtlpKV {
  key: string;
  value: OtlpAny;
}
interface OtlpMetric {
  name: string;
  sum?: {
    dataPoints: Array<{ asInt?: string; asDouble?: number }>;
    isMonotonic?: boolean;
    aggregationTemporality?: number;
  };
  gauge?: { dataPoints: Array<{ asDouble?: number }> };
  histogram?: {
    dataPoints: Array<{ count: string; sum?: number; min?: number; max?: number; bucketCounts?: string[] }>;
  };
}
interface OtlpMetricsRequest {
  resourceMetrics: Array<{
    resource: { attributes?: OtlpKV[] };
    scopeMetrics: Array<{ scope: { name?: string }; metrics: OtlpMetric[] }>;
  }>;
}

function parse(body: string): OtlpMetricsRequest {
  return JSON.parse(body) as OtlpMetricsRequest;
}

function attrValue(attrs: OtlpKV[] | undefined, key: string): string | undefined {
  return attrs?.find((a) => a.key === key)?.value.stringValue;
}

const sampleBuckets: MetricsBucket[] = [
  {
    ts: 1_700_000_000,
    counters: { 'http.GET./api.200': 5 },
    gauges: { 'http.active': 2 },
    histograms: {
      'http.GET./api.duration': { count: 5, sum: 250, min: 10, max: 120 },
    },
  },
];

describe('buildMetricsBody', () => {
  it('renders buckets into a valid OTLP metrics envelope', () => {
    const req = parse(buildMetricsBody(sampleBuckets, { app: 'my-app' }));

    const rm = req.resourceMetrics[0];
    expect(attrValue(rm.resource.attributes, 'service.name')).toBe('my-app');
    expect(attrValue(rm.resource.attributes, 'putnami.framework')).toBe('typescript');

    const sm = rm.scopeMetrics[0];
    expect(sm.scope.name).toBe('putnami');
    const names = sm.metrics.map((m) => m.name);
    expect(names).toContain('http.GET./api.200');
    expect(names).toContain('http.active');
    expect(names).toContain('http.GET./api.duration');
  });

  it('encodes a counter as a monotonic delta sum with an int data point', () => {
    const req = parse(buildMetricsBody(sampleBuckets, { app: 'svc' }));
    const counter = req.resourceMetrics[0].scopeMetrics[0].metrics.find((m) => m.name === 'http.GET./api.200');
    expect(counter?.sum?.isMonotonic).toBe(true);
    expect(counter?.sum?.aggregationTemporality).toBe(1); // delta
    expect(counter?.sum?.dataPoints[0].asInt).toBe('5');
  });

  it('encodes a gauge as a double data point', () => {
    const req = parse(buildMetricsBody(sampleBuckets, { app: 'svc' }));
    const g = req.resourceMetrics[0].scopeMetrics[0].metrics.find((m) => m.name === 'http.active');
    expect(g?.gauge?.dataPoints[0].asDouble).toBe(2);
  });

  it('encodes a histogram with count/sum/min/max', () => {
    const req = parse(buildMetricsBody(sampleBuckets, { app: 'svc' }));
    const h = req.resourceMetrics[0].scopeMetrics[0].metrics.find((m) => m.name === 'http.GET./api.duration');
    const dp = h?.histogram?.dataPoints[0];
    expect(dp?.count).toBe('5');
    expect(dp?.sum).toBe(250);
    expect(dp?.min).toBe(10);
    expect(dp?.max).toBe(120);
    expect(dp?.bucketCounts).toEqual(['5']);
  });

  it('produces one timestamped data point per bucket for a repeated metric', () => {
    const buckets: MetricsBucket[] = [
      { ts: 1_700_000_000, counters: { a: 1 }, gauges: {}, histograms: {} },
      { ts: 1_700_000_001, counters: { a: 3 }, gauges: {}, histograms: {} },
    ];
    const req = parse(buildMetricsBody(buckets, { app: 'svc' }));
    const a = req.resourceMetrics[0].scopeMetrics[0].metrics.find((m) => m.name === 'a');
    expect(a?.sum?.dataPoints).toHaveLength(2);
  });

  it('renders an empty request when there are no buckets', () => {
    expect(buildMetricsBody([], { app: 'svc' })).toBe('{}');
  });
});

describe('joinUrl', () => {
  it('appends the signal path to the collector base URL', () => {
    expect(joinUrl('https://collector:4318', '/v1/metrics')).toBe('https://collector:4318/v1/metrics');
    expect(joinUrl('https://collector:4318/', '/v1/metrics')).toBe('https://collector:4318/v1/metrics');
    expect(joinUrl('https://collector:4318///', '/v1/logs')).toBe('https://collector:4318/v1/logs');
  });
});

describe('postOtlp', () => {
  const originalFetch = globalThis.fetch;
  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('returns ok for a 2xx response', async () => {
    globalThis.fetch = mock(() => Promise.resolve(new Response(null, { status: 200 }))) as typeof fetch;
    const result = await postOtlp('https://c:4318', '/v1/metrics', '{}');
    expect(result.ok).toBe(true);
  });

  it('returns ok: false for non-2xx and network errors', async () => {
    globalThis.fetch = mock(() => Promise.resolve(new Response('err', { status: 500 }))) as typeof fetch;
    expect((await postOtlp('https://c:4318', '/v1/metrics', '{}')).ok).toBe(false);

    globalThis.fetch = mock(() => Promise.reject(new Error('boom'))) as typeof fetch;
    expect((await postOtlp('https://c:4318', '/v1/metrics', '{}')).ok).toBe(false);
  });

  it('sets the content type and bearer token, and posts the body verbatim', async () => {
    let url: string | undefined;
    let headers: Headers | undefined;
    let body: string | undefined;
    globalThis.fetch = mock((u: string, init: RequestInit) => {
      url = u;
      headers = new Headers(init.headers);
      body = init.body as string;
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;

    await postOtlp('https://c:4318', '/v1/metrics', '{"x":1}', { bearer: 'tok', headers: { 'X-Tenant': 'acme' } });
    expect(url).toBe('https://c:4318/v1/metrics');
    expect(headers?.get('Content-Type')).toBe('application/json');
    expect(headers?.get('Authorization')).toBe('Bearer tok');
    expect(headers?.get('X-Tenant')).toBe('acme');
    expect(body).toBe('{"x":1}');
  });

  it('omits Authorization when no bearer is provided', async () => {
    let headers: Headers | undefined;
    globalThis.fetch = mock((_u: string, init: RequestInit) => {
      headers = new Headers(init.headers);
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;
    await postOtlp('https://c:4318', '/v1/metrics', '{}');
    expect(headers?.get('Authorization')).toBeNull();
  });
});

describe('sendMetrics', () => {
  const originalFetch = globalThis.fetch;
  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  it('posts the rendered metrics to <endpoint>/v1/metrics', async () => {
    let url: string | undefined;
    let body: string | undefined;
    globalThis.fetch = mock((u: string, init: RequestInit) => {
      url = u;
      body = init.body as string;
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;

    const result = await sendMetrics('https://collector:4318', sampleBuckets, { app: 'my-app', bearer: 'tok' });
    expect(result.ok).toBe(true);
    expect(url).toBe('https://collector:4318/v1/metrics');
    const req = parse(body as string);
    expect(req.resourceMetrics[0].scopeMetrics[0].metrics.length).toBeGreaterThan(0);
  });
});

describe('sendTraces', () => {
  const originalFetch = globalThis.fetch;
  afterEach(() => {
    globalThis.fetch = originalFetch;
  });

  const spans = [
    {
      traceId: '11111111111111111111111111111111',
      spanId: '2222222222222222',
      parentSpanId: '3333333333333333',
      name: 'listWidgets',
      kind: 3,
      startTimeUnixNano: '1700000000000000000',
      endTimeUnixNano: '1700000000001000000',
      attributes: { 'rpc.system': 'putnami', 'http.response.status_code': 200 },
      statusCode: 1,
    },
  ] as const;

  it('renders canonical span attributes and posts to /v1/traces', async () => {
    let url: string | undefined;
    let body: string | undefined;
    globalThis.fetch = mock((u: string, init: RequestInit) => {
      url = u;
      body = init.body as string;
      return Promise.resolve(new Response(null, { status: 200 }));
    }) as typeof fetch;

    const built = JSON.parse(buildTracesBody(spans, { app: 'consumer' })) as Record<string, unknown>;
    expect(JSON.stringify(built)).toContain('http.response.status_code');
    expect(JSON.stringify(built)).toContain('"intValue":"200"');

    expect((await sendTraces('https://collector:4318', spans, { app: 'consumer' })).ok).toBe(true);
    expect(url).toBe('https://collector:4318/v1/traces');
    expect(body).toBe(buildTracesBody(spans, { app: 'consumer' }));
  });
});
