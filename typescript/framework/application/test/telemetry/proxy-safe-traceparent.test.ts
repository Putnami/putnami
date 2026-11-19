import { afterEach, describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import type { Application } from '../../src/application';
import { application } from '../../src/application';
import { http } from '../../src/http/http.plugin';
import { PUTNAMI_TRACEPARENT_HEADER, startTelemetrySpan } from '../../src/telemetry/client-telemetry';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { TelemetryMiddleware } from '../../src/telemetry/telemetry.middleware';
import { clearCollector, setCollector } from '../../src/telemetry/telemetry.utils';

const FEATURE = 'telemetry/otlp-push-contract';

interface Traceparent {
  traceId: string;
  spanId: string;
  traceFlags: string;
}

interface ProxySafeTraceparentFixture {
  header: string;
  cases: Array<{
    name: string;
    traceparent?: string;
    putnamiTraceparent?: string;
    source: 'none' | 'traceparent' | 'x-putnami-traceparent';
    parent?: Traceparent;
  }>;
}

const fixture = JSON.parse(
  readFileSync(
    join(import.meta.dir, '../../../../../protocols/telemetry/fixtures/conventions/proxy-safe-traceparent.json'),
    'utf8',
  ),
) as ProxySafeTraceparentFixture;

describe('proxy-safe trace context', () => {
  let app: Application | undefined;
  let collector: TelemetryCollector | undefined;

  afterEach(async () => {
    await app?.stop();
    app = undefined;
    if (collector) clearCollector(collector);
    collector = undefined;
  });

  /** Serve one route through the telemetry middleware, recording spans in a fresh collector. */
  async function serve(): Promise<string> {
    collector = new TelemetryCollector();
    setCollector(collector);
    const plugin = http({ port: 0 }).use(TelemetryMiddleware(collector));
    plugin.get('/items', () => ({ ok: true }));
    app = application().use(plugin);
    await app.start();
    return `http://localhost:${plugin.getServer()?.port}/items`;
  }

  specTest(
    'the server span takes its parent from the header the shared rule picks',
    {
      feature: FEATURE,
      requirement: 'the-client-span-survives-a-proxy',
      check: 'the-typescript-server-resolves-the-fixture-cases',
    },
    async () => {
      expect(fixture.header).toBe(PUTNAMI_TRACEPARENT_HEADER);
      expect(fixture.cases.length).toBeGreaterThan(0);
      const url = await serve();
      for (const testCase of fixture.cases) {
        const headers = new Headers();
        if (testCase.traceparent) headers.set('traceparent', testCase.traceparent);
        if (testCase.putnamiTraceparent) headers.set(fixture.header, testCase.putnamiTraceparent);
        expect((await fetch(url, { headers })).status).toBe(200);

        const spans = collector?.drainSpans() ?? [];
        // A case whose parent is not sampled records no server span at all.
        if (testCase.parent?.traceFlags === '00') {
          expect(spans, testCase.name).toHaveLength(0);
          continue;
        }
        expect(spans, testCase.name).toHaveLength(1);
        const span = spans[0];
        if (!testCase.parent) {
          expect(span?.parentSpanId, testCase.name).toBeUndefined();
          continue;
        }
        expect({ traceId: span?.traceId, parentSpanId: span?.parentSpanId }, testCase.name).toEqual({
          traceId: testCase.parent.traceId,
          parentSpanId: testCase.parent.spanId,
        });
      }
    },
  );

  specTest(
    'a client writes its traceparent twice, and the server finds the client span behind a rewriting proxy',
    {
      feature: FEATURE,
      requirement: 'a-client-sends-both-headers',
      check: 'the-typescript-client-sends-both-headers',
    },
    async () => {
      const url = await serve();
      const client = startTelemetrySpan({ name: 'listItems', kind: 'client' });
      const headers = new Headers();
      client.inject(headers);
      const sent = `00-${client.context.traceId}-${client.context.spanId}-${client.context.traceFlags}`;
      expect(headers.get('traceparent')).toBe(sent);
      expect(headers.get(PUTNAMI_TRACEPARENT_HEADER)).toBe(sent);

      // A proxy inserts a span it never exports and rewrites the parent ID of traceparent.
      headers.set('traceparent', `00-${client.context.traceId}-a1a1a1a1a1a1a1a1-${client.context.traceFlags}`);
      expect((await fetch(url, { headers })).status).toBe(200);
      client.finish();

      const spans = collector?.drainSpans() ?? [];
      const server = spans.find((span) => span.kind === 2);
      expect(server).toMatchObject({ traceId: client.context.traceId, parentSpanId: client.context.spanId });
    },
  );
});
