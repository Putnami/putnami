import { afterEach, describe, expect, test } from 'bun:test';
import { runInContext, tryContext } from '@putnami/runtime';
import { startTelemetrySpan, type TelemetryTraceContext } from '../../src/telemetry/client-telemetry';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { clearCollector, setCollector } from '../../src/telemetry/telemetry.utils';

const TRACE_ID = '11111111111111111111111111111111';
const PARENT_SPAN_ID = '2222222222222222';

describe('framework telemetry spans', () => {
  let collector: TelemetryCollector | undefined;

  afterEach(() => {
    if (collector) clearCollector(collector);
    collector = undefined;
  });

  test('continues W3C context, creates a child span, and injects only bounded propagation headers', async () => {
    collector = new TelemetryCollector();
    setCollector(collector);
    const ambient = {
      __putnamiTrace: {
        traceId: TRACE_ID,
        spanId: PARENT_SPAN_ID,
        traceFlags: '01',
        tracestate: 'vendor=value',
        baggage: 'tenant=blue',
      } satisfies TelemetryTraceContext,
    };

    await runInContext(ambient, async () => {
      const span = startTelemetrySpan({
        name: 'listWidgets',
        kind: 'client',
        attributes: { 'rpc.system': 'putnami', 'rpc.service': 'catalog.items' },
      });
      const headers = new Headers({ traceparent: 'attacker-controlled' });
      span.inject(headers);

      expect(headers.get('traceparent')).toMatch(new RegExp(`^00-${TRACE_ID}-[0-9a-f]{16}-01$`));
      expect(headers.get('tracestate')).toBe('vendor=value');
      expect(headers.get('baggage')).toBe('tenant=blue');

      await span.run(async () => {
        const current = tryContext<{ __putnamiTrace: TelemetryTraceContext }>();
        expect(current?.__putnamiTrace.traceId).toBe(TRACE_ID);
        expect(current?.__putnamiTrace.spanId).not.toBe(PARENT_SPAN_ID);
      });
      span.finish({ attributes: { 'http.response.status_code': 200 } });
      span.finish({ code: 'must.not.record.twice' });
    });

    const spans = collector.drainSpans();
    expect(spans).toHaveLength(1);
    expect(spans[0]).toMatchObject({
      traceId: TRACE_ID,
      parentSpanId: PARENT_SPAN_ID,
      name: 'listWidgets',
      kind: 3,
      statusCode: 1,
      attributes: {
        'rpc.system': 'putnami',
        'rpc.service': 'catalog.items',
        'http.response.status_code': 200,
      },
    });
  });

  test('rejects malformed remote context and never propagates unbounded headers', () => {
    collector = new TelemetryCollector();
    setCollector(collector);
    const remoteHeaders = new Headers({
      traceparent: '00-00000000000000000000000000000000-0000000000000000-01',
      tracestate: `vendor=${'x'.repeat(600)}`,
      baggage: `tenant=${'y'.repeat(9000)}`,
    });
    const span = startTelemetrySpan({ name: 'GET /widgets', kind: 'server', remoteHeaders });
    expect(span.context.traceId).not.toBe('00000000000000000000000000000000');
    expect(span.context.tracestate).toBeUndefined();
    expect(span.context.baggage).toBeUndefined();
    span.finish({ code: 'client.remote' });
    expect(collector.drainSpans()[0]).toMatchObject({ kind: 2, statusCode: 2, statusMessage: 'client.remote' });
  });
});
