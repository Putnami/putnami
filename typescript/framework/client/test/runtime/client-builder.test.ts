import { afterAll, beforeAll, describe, expect, spyOn, test } from 'bun:test';
import { BaseClient, type ClientConfig } from '../../src/runtime/base-client';
import { ClientBuilder } from '../../src/runtime/client-builder';
import { clearGeneratedClientUsages, getGeneratedClientUsages } from '../../src/runtime/generated-client-design';

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;

// Test client class
class TestClient extends BaseClient {
  readonly serviceName = 'test';

  async ping(): Promise<{ ok: boolean }> {
    return this.request('GET', '/ping');
  }
}

// Proto client with specHash (simulates generated connect client)
class ProtoTestClient extends BaseClient {
  readonly serviceName = 'proto-test';
  static readonly specHash = 'abc123';
  static readonly packageName = 'proto-test.v1';

  async call(): Promise<{ ok: boolean }> {
    return this.request('POST', '/proto-test.v1.ProtoTestService/Call');
  }
}

// Stands in for a generated client whose operations span two producer features
// plus one that belongs to none — the shape per-operation attribution exists for.
class DesignedTestClient extends BaseClient {
  readonly serviceName = 'designed-test';
  static readonly design = {
    language: 'ts',
    client: 'DesignedTestClient',
    service: 'TestService',
    specHash: 'spec-123',
    operations: [
      {
        operationId: 'ping',
        method: 'GET',
        path: '/ping',
        producerProject: '@test/provider',
        producerFeature: 'test/ping',
      },
      {
        operationId: 'getV1_Operator_Cli-usage',
        method: 'GET',
        path: '/v1/operator/cli-usage',
        producerProject: '@test/provider',
        producerFeature: 'test/cli-usage',
      },
      { operationId: 'health', method: 'GET', path: '/health' },
    ],
  } as const;

  constructor(config: ClientConfig) {
    super({ ...config, design: DesignedTestClient.design });
  }

  async ping(): Promise<{ ok: boolean }> {
    return this.request('GET', '/ping', { operationId: 'ping' });
  }

  // The TypeScript symbol normalizes the canonical id's punctuation away; the
  // call still carries the canonical id.
  async getV1_Operator_Cli_usage(): Promise<{ ok: boolean }> {
    return this.request('GET', '/v1/operator/cli-usage', { operationId: 'getV1_Operator_Cli-usage' });
  }

  async health(): Promise<{ ok: boolean }> {
    return this.request('GET', '/health', { operationId: 'health' });
  }
}

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    async fetch(req) {
      const url = new URL(req.url);

      if (url.pathname === '/ping' || url.pathname === '/health' || url.pathname === '/v1/operator/cli-usage') {
        return Response.json({ ok: true });
      }

      if (url.pathname === '/_/api.proto') {
        return new Response('syntax = "proto3";', { status: 200 });
      }

      if (url.pathname === '/proto-test.v1.ProtoTestService/Call') {
        return Response.json({ ok: true });
      }

      return new Response('Not found', { status: 404 });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

describe('ClientBuilder', () => {
  test('ordinary clients do not pay the generated-design provenance path', () => {
    // Without PUTNAMI_PROJECT_ROOT and PWD, the project root is the working
    // directory, so a provenance lookup would read it.
    const projectRoot = process.env.PUTNAMI_PROJECT_ROOT;
    const pwd = process.env.PWD;
    const cwd = spyOn(process, 'cwd');
    try {
      delete process.env.PUTNAMI_PROJECT_ROOT;
      delete process.env.PWD;
      expect(() => ClientBuilder.for(TestClient)).not.toThrow();
      expect(cwd).not.toHaveBeenCalled();
      ClientBuilder.for(DesignedTestClient);
      expect(cwd).toHaveBeenCalled();
    } finally {
      cwd.mockRestore();
      if (projectRoot === undefined) delete process.env.PUTNAMI_PROJECT_ROOT;
      else process.env.PUTNAMI_PROJECT_ROOT = projectRoot;
      if (pwd === undefined) delete process.env.PWD;
      else process.env.PWD = pwd;
    }
  });

  test('buildSync creates a working HTTP client', () => {
    const client = ClientBuilder.for(TestClient).baseUrl(baseUrl).buildSync();

    expect(client).toBeInstanceOf(TestClient);
    expect(client.serviceName).toBe('test');
  });

  test('build negotiates transport', async () => {
    const client = await ClientBuilder.for(TestClient).baseUrl(baseUrl).build();

    expect(client).toBeInstanceOf(TestClient);
  });

  test('build detects Connect for proto clients', async () => {
    const client = await ClientBuilder.for(ProtoTestClient).baseUrl(baseUrl).build();

    expect(client).toBeInstanceOf(ProtoTestClient);
  });

  test('throws without baseUrl', () => {
    expect(() => ClientBuilder.for(TestClient).buildSync()).toThrow('baseUrl is required');
  });

  test('chains configuration methods', () => {
    const client = ClientBuilder.for(TestClient)
      .baseUrl(baseUrl)
      .clientId('test-service')
      .timeout(5000)
      .retry({ maxRetries: 1 })
      .buildSync();

    expect(client).toBeInstanceOf(TestClient);
  });

  test('passes custom interceptors into the built client', async () => {
    let seen = false;

    const client = ClientBuilder.for(TestClient)
      .baseUrl(baseUrl)
      .interceptors([
        async (request, next) => {
          seen = true;
          request.headers.set('X-Test-Interceptor', 'true');
          return next(request);
        },
      ])
      .buildSync();

    const result = await client.ping();

    expect(result.ok).toBe(true);
    expect(seen).toBe(true);
  });

  test('carries exact feature trace and records generated-client usage', async () => {
    clearGeneratedClientUsages();
    const traces: unknown[] = [];
    const client = ClientBuilder.for(DesignedTestClient)
      .baseUrl(baseUrl)
      .interceptors([
        async (request, next) => {
          traces.push(request.featureTrace);
          return next(request);
        },
      ])
      .buildSync();

    await client.ping();

    // Method, path, spec hash, canonical operation, project, and feature all
    // come from the invoked operation.
    expect(traces[0]).toEqual({
      generatedClient: 'DesignedTestClient',
      operationId: 'ping',
      method: 'GET',
      path: '/ping',
      specHash: 'spec-123',
      producerProject: '@test/provider',
      producerFeature: 'test/ping',
    });

    // A second operation in the SAME client resolves to its own feature.
    await client.getV1_Operator_Cli_usage();
    expect(traces[1]).toEqual({
      generatedClient: 'DesignedTestClient',
      operationId: 'getV1_Operator_Cli-usage',
      method: 'GET',
      path: '/v1/operator/cli-usage',
      specHash: 'spec-123',
      producerProject: '@test/provider',
      producerFeature: 'test/cli-usage',
    });

    // An unattributed operation still resolves, but carries no false lineage.
    await client.health();
    expect(traces[2]).toEqual({
      generatedClient: 'DesignedTestClient',
      operationId: 'health',
      method: 'GET',
      path: '/health',
      specHash: 'spec-123',
    });

    expect(getGeneratedClientUsages()).toEqual([
      expect.objectContaining({ design: expect.objectContaining({ client: 'DesignedTestClient' }) }),
    ]);
  });

  test('checkDrift does not block build', async () => {
    const client = await ClientBuilder.for(ProtoTestClient).baseUrl(baseUrl).checkDrift().build();

    expect(client).toBeInstanceOf(ProtoTestClient);
  });

  test('buildSync with checkDrift runs drift check in background', () => {
    const client = ClientBuilder.for(ProtoTestClient).baseUrl(baseUrl).checkDrift().buildSync();

    expect(client).toBeInstanceOf(ProtoTestClient);
  });

  test('timeout() rejects non-positive values', () => {
    expect(() => ClientBuilder.for(TestClient).timeout(0)).toThrow(/positive/);
    expect(() => ClientBuilder.for(TestClient).timeout(-100)).toThrow(/positive/);
  });

  test('timeout() rejects non-finite values', () => {
    expect(() => ClientBuilder.for(TestClient).timeout(Number.POSITIVE_INFINITY)).toThrow(/positive/);
    expect(() => ClientBuilder.for(TestClient).timeout(Number.NaN)).toThrow(/positive/);
  });

  test('retry() rejects absurdly large maxRetries', () => {
    expect(() => ClientBuilder.for(TestClient).retry({ maxRetries: 1_000_000 })).toThrow(/maxRetries/);
  });

  test('retry() rejects negative maxRetries', () => {
    expect(() => ClientBuilder.for(TestClient).retry({ maxRetries: -1 })).toThrow(/maxRetries/);
  });

  test('retry() accepts in-range maxRetries', () => {
    expect(() => ClientBuilder.for(TestClient).baseUrl(baseUrl).retry({ maxRetries: 5 }).buildSync()).not.toThrow();
  });

  test('BaseClient rejects non-positive timeoutMs at construction', () => {
    expect(() => new TestClient({ baseUrl, transport: 'http', timeoutMs: 0 })).toThrow(/positive/);
  });
});
