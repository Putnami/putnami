import { afterEach, describe, expect } from 'bun:test';
import { setCollector, TelemetryCollector } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient, type ClientConfig } from '../../src/runtime/base-client';
import {
  serviceAttemptTelemetryInterceptor,
  serviceCallTelemetryInterceptor,
} from '../../src/runtime/service-telemetry';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

type StubTransport = { execute(request: ClientRequest): Promise<ClientResponse> };

const OPERATIONS: ClientConfig['operationContracts'] = {
  getWidget: {
    stream: 'unary',
    transports: [{ protocol: 'rest-json', path: '/widgets', encoding: 'json' }],
    security: { alternatives: [{ allOf: [] }] },
    errors: [],
    idempotency: { kind: 'safe' },
    resilience: { retry: { maxAttempts: 1 } },
  },
};

class WidgetClient extends BaseClient {
  readonly serviceName = 'catalog.items';

  constructor(config: Partial<ClientConfig> = {}) {
    super({
      baseUrl: 'https://service.test',
      transport: 'http',
      serviceId: 'catalog.items',
      operationContracts: OPERATIONS,
      ...config,
    });
  }

  call(): Promise<unknown> {
    return this.request('GET', '/widgets', { operationId: 'getWidget' });
  }
}

/** Answers 200 and keeps the headers each attempt carried. */
function answering(client: BaseClient, seen: Headers[]): void {
  (client as unknown as { transport: StubTransport }).transport = {
    execute: async (request): Promise<ClientResponse> => {
      seen.push(new Headers(request.headers));
      return { data: {}, status: 200, headers: new Headers() };
    },
  };
}

describe('a generated client built without a service binding', () => {
  let collector: TelemetryCollector | undefined;

  afterEach(() => {
    setCollector(undefined);
    collector = undefined;
  });

  specTest(
    'records a call span and an attempt span that carries the trace',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'an-unbound-generated-client-records-call-and-attempt-spans',
    },
    async () => {
      collector = new TelemetryCollector();
      setCollector(collector);
      const client = new WidgetClient();
      const seen: Headers[] = [];
      answering(client, seen);

      await client.call();

      const spans = collector.drainSpans();
      const call = spans.find((span) => span.name === 'getWidget');
      const attempt = spans.find((span) => span.name === 'getWidget attempt');
      expect(call?.kind).toBe(3);
      expect(call?.attributes['rpc.service']).toBe('catalog.items');
      expect(attempt?.parentSpanId).toBe(call?.spanId as string);
      expect(attempt?.traceId).toBe(call?.traceId as string);
      expect(seen[0]?.get('traceparent')).toBe(`00-${attempt?.traceId}-${attempt?.spanId}-01`);
    },
  );

  specTest(
    'records nothing twice for a bound client, which installs its own spans',
    {
      feature: 'typescript/service-clients',
      requirement: 'resilience-telemetry',
      check: 'an-unbound-generated-client-records-call-and-attempt-spans',
    },
    async () => {
      collector = new TelemetryCollector();
      setCollector(collector);
      // The telemetry a service binding installs (service-binding.ts).
      const bound = new WidgetClient({
        endpointBinding: (url) => ({ url, create: () => new WidgetClient() }),
        interceptors: [serviceCallTelemetryInterceptor('catalog.items')],
        attemptInterceptors: [serviceAttemptTelemetryInterceptor('catalog.items')],
      });
      answering(bound, []);

      await bound.call();

      const spans = collector.drainSpans();
      expect(spans.map((span) => span.name).sort()).toEqual(['getWidget', 'getWidget attempt']);
    },
  );
});
