import { afterEach, describe, expect } from 'bun:test';
import { type Context, useContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { api, endpoint } from '../../src/api';
import { application } from '../../src/application';
import { grpc } from '../../src/grpc/grpc.plugin';
import { createGrpcTestClient, type GrpcTestClient } from '../../src/grpc/grpc-testing';
import { http, type HttpPlugin } from '../../src/http/http.plugin';
import { proto } from '../../src/proto';
import { TelemetryCollector } from '../../src/telemetry/telemetry.collector';
import { TelemetryMiddleware } from '../../src/telemetry/telemetry.middleware';
import { trace } from '../../src/http/trace.plugin';

describe('gRPC observability', () => {
  let app: Application;
  let httpPlugin: HttpPlugin;
  let client: GrpcTestClient;

  afterEach(async () => {
    await client?.close();
    await app?.stop();
  });

  async function setup(opts?: { collector?: TelemetryCollector }) {
    httpPlugin = http({ port: 0 });
    if (opts?.collector) {
      httpPlugin.use(TelemetryMiddleware(opts.collector));
    }

    const apiPlugin = api({ autoScan: false });
    const protoPlugin = proto({ packageName: 'test.v1', output: false });
    const grpcPlugin = grpc();

    // Register a simple endpoint that returns the trace ID from the inner context.
    // The gRPC plugin maps GET /items → ListItems RPC.
    apiPlugin.register(
      '/items',
      {
        GET: endpoint((_ctx) => {
          const context = useContext<Context>();
          return { traceId: context.traceId ?? null };
        }),
      },
      'GET',
    );

    app = application().use(httpPlugin).use(apiPlugin).use(protoPlugin).use(grpcPlugin).use(trace());

    await app.start();

    client = createGrpcTestClient({
      baseUrl: `http://localhost:${httpPlugin.getServer()?.port}`,
      packageName: 'test.v1',
    });

    return { httpPlugin, client };
  }

  specTest(
    'should propagate traceId from X-Correlation-ID header to handler context',
    {
      feature: 'typescript/grpc-services',
      requirement: 'endpoint-pipeline',
      check: 'the-caller-trace-id-reaches-the-handler',
    },
    async () => {
      await setup();

      const res = await client.unary<{ traceId: string }>(
        'ItemsService/ListItems',
        {},
        {
          headers: { 'X-Correlation-ID': 'test-trace-123' },
        },
      );

      expect(res.status).toBe(200);
      expect(res.data.traceId).toBe('test-trace-123');
    },
  );

  specTest(
    'should propagate traceId from GCP X-Cloud-Trace-Context header',
    {
      feature: 'typescript/grpc-services',
      requirement: 'endpoint-pipeline',
      check: 'the-caller-trace-id-reaches-the-handler',
    },
    async () => {
      await setup();

      const gcpTraceId = 'abc123def456';
      const res = await client.unary<{ traceId: string }>(
        'ItemsService/ListItems',
        {},
        {
          headers: { 'X-Cloud-Trace-Context': `${gcpTraceId}/1;o=1` },
        },
      );

      expect(res.status).toBe(200);
      expect(res.data.traceId).toBe(gcpTraceId);
    },
  );

  specTest(
    'should auto-generate traceId when no trace header is provided',
    {
      feature: 'typescript/grpc-services',
      requirement: 'endpoint-pipeline',
      check: 'a-call-without-a-trace-header-gets-a-generated-trace-id',
    },
    async () => {
      await setup();

      const res = await client.unary<{ traceId: string }>('ItemsService/ListItems', {});

      expect(res.status).toBe(200);
      // Trace middleware generates a UUID when no header is present
      expect(res.data.traceId).toBeDefined();
      expect(res.data.traceId).not.toBeNull();
      expect(typeof res.data.traceId).toBe('string');
    },
  );

  specTest(
    'should record telemetry metrics for gRPC requests',
    {
      feature: 'typescript/grpc-services',
      requirement: 'endpoint-pipeline',
      check: 'a-call-is-counted-under-its-rpc-path',
    },
    async () => {
      const collector = new TelemetryCollector();
      await setup({ collector });

      const res = await client.unary('ItemsService/ListItems', {});
      expect(res.status).toBe(200);

      const buckets = collector.drainAll();
      const allCounters = Object.assign({}, ...buckets.map((b) => b.counters));

      // The gRPC POST route is tracked under the gRPC path pattern
      const grpcRouteKey = 'http.POST./test.v1.ItemsService/ListItems.200';
      expect(allCounters[grpcRouteKey]).toBe(1);
    },
  );
});
