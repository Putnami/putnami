import { afterEach, describe, expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import {
  apiKeyStrategy,
  application,
  api,
  authenticate,
  endpoint,
  getCollector,
  http,
  Int,
  openapi,
  Stream,
  telemetry,
} from '@putnami/application';
import { ConflictException, NotFoundException, OneOf, Optional, Pattern, resetConfigLoader } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { readOpenApiSource } from '../../src/generator/openapi-reader';
import { generateTypeScriptClient } from '../../src/generator/ts/ts-generator';
import { CredentialRegistryClosedError } from '../../src/runtime/credential';
import { ClientFrameworkError, ClientServiceConfigError } from '../../src/runtime/errors';
import type { StreamObserver } from '../../src/runtime/stream.type';
import { SuccessBody } from '../../src/runtime/success-body';

const CLIENT_ENTRY = resolve(import.meta.dir, '..', '..', 'src', 'index.ts');
const temporaryDirectories: string[] = [];
const originalConfig = process.env.CONFIG_DATA;

afterEach(() => {
  for (const directory of temporaryDirectories.splice(0)) rmSync(directory, { recursive: true, force: true });
  if (originalConfig === undefined) delete process.env.CONFIG_DATA;
  else process.env.CONFIG_DATA = originalConfig;
  resetConfigLoader();
});

describe('first-party provider to generated consumer', () => {
  test('derives, emits, binds through config/DI, and calls a real Putnami provider', async () => {
    let providerTraceparent: string | null = null;
    const providerHttp = http({ port: 0 });
    providerHttp.prepend(authenticate({ anyOf: [apiKeyStrategy({ keys: ['runtime-only-api-key'] })] }));
    const operationSecurity = { alternatives: [{ allOf: [{ profile: 'service-key' }] }] } as const;
    const providerApi = api({
      autoScan: false,
      client: {
        service: { id: 'catalog.items', audience: 'api://widgets' },
        credentials: { 'service-key': { kind: 'api-key', header: 'X-Api-Key' } },
      },
    });
    providerApi.register(
      '/widgets/[id]',
      endpoint()
        .params({ id: String })
        .returns({ id: String, name: String })
        .secure({ principalKind: 'apikey' })
        .client({ security: operationSecurity, idempotency: { kind: 'safe' } })
        .handle((context) => {
          providerTraceparent = context.req.headers.get('traceparent');
          return { id: context.params.id, name: 'framework-generated' };
        }),
      'GET',
    );
    providerApi.register(
      '/widgets/echo',
      endpoint()
        .query({ term: String, verbose: Boolean })
        .headers({ 'x-tenant': String })
        .returns({ term: String, tenant: String, note: Optional(String) })
        .secure({ principalKind: 'apikey' })
        .client({ security: operationSecurity, idempotency: { kind: 'safe' } })
        .handle((context) => {
          const { term, verbose } = context.queryParams();
          const tenant = context.headerParams()['x-tenant'];
          // An absent optional property is a different answer from a present
          // one, and the generated client must keep them apart.
          return verbose ? { term, tenant, note: `echo:${term}` } : { term, tenant };
        }),
      'GET',
    );
    providerApi.register(
      '/widgets/watch',
      endpoint()
        .returns(Stream({ id: String }))
        .secure({ principalKind: 'apikey' })
        .client({ security: operationSecurity, idempotency: { kind: 'safe' } })
        .handle(async (context) => {
          context.send({ id: 'stream-1' });
          context.send({ id: 'stream-2' });
        }),
      'GET',
    );
    providerApi.register(
      '/widgets/conflicts/[kind]',
      endpoint()
        .params({ kind: OneOf('conflict', 'existing') })
        .returns({ ok: Boolean })
        .mayThrow('Conflict', 'AlreadyExists')
        .secure({ principalKind: 'apikey' })
        .client({ security: operationSecurity, idempotency: { kind: 'safe' } })
        .handle((context) => {
          throw new ConflictException({
            code: context.params.kind === 'existing' ? 'AlreadyExists' : 'Conflict',
            remoteOnly: 'runtime-only-api-key',
          });
        }),
      'GET',
    );
    providerApi.register(
      '/widgets/missing/[id]',
      endpoint()
        .params({ id: String })
        .returns({ id: String })
        .mayThrow('NotFound')
        .throws(404, 'Missing widget', { code: OneOf('NotFound'), reason: Pattern(/^safe-[a-z]+$/) })
        .secure({ principalKind: 'apikey' })
        .client({ security: operationSecurity, idempotency: { kind: 'safe' } })
        .handle((context) => {
          throw new NotFoundException({ code: 'NotFound', reason: `safe-${context.params.id}` });
        }),
      'GET',
    );
    providerApi.register(
      '/widgets/failing-watch',
      endpoint()
        .returns(Stream({ id: String }))
        .mayThrow('NotFound')
        .throws(404, 'Missing widget stream', { code: String, reason: String })
        .secure({ principalKind: 'apikey' })
        .client({ security: operationSecurity, idempotency: { kind: 'safe' } })
        .handle(async () => {
          throw new NotFoundException({ code: 'NotFound', reason: 'safe missing reason' });
        }),
      'GET',
    );
    const providerOpenApi = openapi({ title: 'Widgets', version: '1.0.0' });
    const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
    const fleetHttp = http({ port: 0 });
    fleetHttp.prepend(authenticate({ anyOf: [apiKeyStrategy({ keys: ['runtime-only-api-key'] })] }));
    const fleetApi = api({ autoScan: false });
    fleetApi.register(
      '/widgets/[id]',
      endpoint()
        .params({ id: String })
        .returns({ id: String, name: String })
        .secure({ principalKind: 'apikey' })
        .handle((context) => ({ id: context.params.id, name: 'fleet-member' })),
      'GET',
    );
    fleetApi.register(
      '/widgets/watch',
      endpoint()
        .returns(Stream({ id: String }))
        .secure({ principalKind: 'apikey' })
        .handle(async (context) => {
          context.send({ id: 'fleet-stream' });
        }),
      'GET',
    );
    const fleet = application().use(fleetHttp).use(fleetApi);
    const consumerHttp = http({ port: 0 });
    const consumerApi = api({ autoScan: false });
    let proxyCall: ((id: string) => Promise<{ id: string; name: string }>) | undefined;
    consumerApi.register(
      '/proxy/[id]',
      endpoint()
        .params({ id: String })
        .returns({ id: String, name: String })
        .handle((context) => {
          if (!proxyCall) throw new Error('generated client binding is not initialized');
          return proxyCall(context.params.id);
        }),
      'GET',
    );
    const consumer = application()
      .use(telemetry({ enabled: true, endpoint: 'http://localhost:0', app: 'consumer-tests' }))
      .use(consumerHttp)
      .use(consumerApi);

    await provider.start();
    try {
      await fleet.start();
      const server = providerHttp.getServer();
      if (!server) throw new Error('provider HTTP server did not start');
      const contract = providerOpenApi.spec();
      if (!contract) throw new Error('provider OpenAPI contract was not emitted');
      const ir = readOpenApiSource(JSON.stringify(contract), { mode: 'firstParty' });
      const widgetMethod = ir.services
        .flatMap((service) => service.methods)
        .find((method) => method.path === '/widgets/{id}');
      const echoMethod = ir.services
        .flatMap((service) => service.methods)
        .find((method) => method.path === '/widgets/echo');
      const watchMethod = ir.services
        .flatMap((service) => service.methods)
        .find((method) => method.path === '/widgets/watch');
      const conflictMethod = ir.services
        .flatMap((service) => service.methods)
        .find((method) => method.path === '/widgets/conflicts/{kind}');
      const missingMethod = ir.services
        .flatMap((service) => service.methods)
        .find((method) => method.path === '/widgets/missing/{id}');
      const failingWatchMethod = ir.services
        .flatMap((service) => service.methods)
        .find((method) => method.path === '/widgets/failing-watch');
      if (!widgetMethod || !echoMethod || !watchMethod || !conflictMethod || !missingMethod || !failingWatchMethod)
        throw new Error('provider operations did not reach the generated IR');
      const files = generateTypeScriptClient(ir, { packageName: '@test/widgets-first-party-client' });
      expect(files.every((file) => !file.content.includes('runtime-only-api-key'))).toBe(true);
      const directory = mkdtempSync(join(tmpdir(), 'putnami-first-party-client-'));
      temporaryDirectories.push(directory);
      for (const file of files) {
        const destination = join(directory, file.path);
        mkdirSync(dirname(destination), { recursive: true });
        writeFileSync(destination, file.content.replaceAll("'@putnami/client'", `'${CLIENT_ENTRY}'`));
      }

      const generated = await import(join(directory, 'src', 'index.ts'));
      type GeneratedWidgetsClient = Record<string, (...args: never[]) => Promise<unknown>>;
      const WidgetsClient = generated.WidgetsClient as new (...args: never[]) => GeneratedWidgetsClient;
      const registerWidgetsClient = generated.registerWidgetsClient as (target: typeof consumer) => typeof consumer;
      process.env.CONFIG_DATA = JSON.stringify({
        clients: {
          clientId: 'consumer-tests',
          services: {
            'catalog.items': {
              url: `http://localhost:${server.port}`,
              allowInsecure: true,
              credentials: { 'service-key': { source: 'static', value: 'runtime-only-api-key' } },
            },
          },
        },
      });
      resetConfigLoader();

      expect(registerWidgetsClient(consumer)).toBe(consumer);
      await consumer.start();
      const first = consumer.context.get(WidgetsClient);
      const second = consumer.context.get(WidgetsClient);
      expect(first).toBe(second);
      const getWidget = first[widgetMethod.name] as (
        input: {
          path: { id: string };
        },
        options?: { endpoint?: string; signal?: AbortSignal; successBody?: SuccessBody },
      ) => Promise<{ id: string; name: string }>;
      proxyCall = (id) => getWidget.call(first, { path: { id } });
      const watchWidgets = first[watchMethod.name] as (options?: {
        signal?: AbortSignal;
        endpoint?: string;
      }) => StreamObserver<{ id: string }>;
      const failingWatch = first[failingWatchMethod.name] as (options?: {
        signal?: AbortSignal;
      }) => StreamObserver<{ id: string }>;
      const getConflict = first[conflictMethod.name] as (input: {
        path: { kind: 'conflict' | 'existing' };
      }) => Promise<{ ok: boolean }>;
      const getMissing = first[missingMethod.name] as (input: { path: { id: string } }) => Promise<{ id: string }>;
      expect(await getWidget.call(first, { path: { id: 'w-64' } })).toEqual({
        id: 'w-64',
        name: 'framework-generated',
      });
      const fleetEndpoint = `http://localhost:${fleetHttp.getServer()?.port}`;
      const fanout = await Promise.all([
        getWidget.call(first, { path: { id: 'same-input' } }),
        getWidget.call(first, { path: { id: 'same-input' } }, { endpoint: fleetEndpoint }),
      ]);
      expect(fanout.map((answer) => answer.name)).toEqual(['framework-generated', 'fleet-member']);
      // The generated call also hands over the provider's bytes, exactly as
      // written on the wire, and the option survives endpoint selection.
      const sink = new SuccessBody();
      expect(await getWidget.call(first, { path: { id: 'w-bytes' } }, { successBody: sink })).toEqual({
        id: 'w-bytes',
        name: 'framework-generated',
      });
      const wire = await fetch(`http://localhost:${server.port}/widgets/w-bytes`, {
        headers: { 'X-Api-Key': 'runtime-only-api-key' },
      }).then((response) => response.text());
      expect(new TextDecoder().decode(sink.bytes)).toBe(wire);
      const fleetSink = new SuccessBody();
      await getWidget.call(first, { path: { id: 'w-bytes' } }, { endpoint: fleetEndpoint, successBody: fleetSink });
      expect(JSON.parse(new TextDecoder().decode(fleetSink.bytes))).toEqual({ id: 'w-bytes', name: 'fleet-member' });
      await expect(getWidget.call(first, { path: { id: 'w-64' } }, { endpoint: '' })).rejects.toThrow();
      const invalidEndpointStream = watchWidgets.call(first, { endpoint: '' });
      await expect(streamError(invalidEndpointStream)).resolves.toBeInstanceOf(ClientServiceConfigError);
      expect(await collectStream(watchWidgets.call(first, { endpoint: fleetEndpoint }))).toEqual([
        { id: 'fleet-stream' },
      ]);
      // Declared query string and declared request header: the consumer passes
      // typed inputs and writes neither a URL nor a header.
      const echoWidget = first[echoMethod.name] as (input: {
        query: { term: string; verbose: boolean };
        headers: { 'x-tenant': string };
      }) => Promise<{ term: string; tenant: string; note?: string }>;
      const terse = await echoWidget.call(first, {
        query: { term: 'alpha', verbose: false },
        headers: { 'x-tenant': 'tenant-a' },
      });
      expect(terse).toEqual({ term: 'alpha', tenant: 'tenant-a' });
      expect('note' in terse).toBe(false);
      const verbose = await echoWidget.call(first, {
        query: { term: 'beta', verbose: true },
        headers: { 'x-tenant': 'tenant-b' },
      });
      expect(verbose).toEqual({ term: 'beta', tenant: 'tenant-b', note: 'echo:beta' });
      expect(await collectStream(watchWidgets.call(first))).toEqual([{ id: 'stream-1' }, { id: 'stream-2' }]);
      // The SSE terminal-error envelope carries the declared stable code, so it
      // narrows to the generated type exactly like a unary declared error. The
      // provider used to emit the PascalCase `ErrorResponseCode` (`NotFound`)
      // while the contract declared `not_found`; `projectStreamError` now emits
      // the stable code, which is what makes the guard below true.
      const streamFailure = await collectStream(failingWatch.call(first)).catch((error) => error);
      const streamNotFoundGuard = generated[
        Object.keys(generated).find((name) => name.endsWith('NotFoundError') && name.includes('FailingWatch')) ?? ''
      ] as (error: unknown) => boolean;
      expect(streamNotFoundGuard(streamFailure)).toBe(true);
      expect(streamFailure).toMatchObject({
        service: 'catalog.items',
        method: failingWatchMethod.operationId,
        status: 404,
        code: 'not_found',
        details: { code: 'NotFound', reason: 'safe missing reason' },
      });
      const alreadyExists = await getConflict.call(first, { path: { kind: 'existing' } }).catch((error) => error);
      const alreadyExistsGuard = generated[
        Object.keys(generated).find((name) => name.endsWith('AlreadyExistsError')) ?? ''
      ] as (error: unknown) => boolean;
      const conflictGuard = generated[Object.keys(generated).find((name) => name.endsWith('ConflictError')) ?? ''] as (
        error: unknown,
      ) => boolean;
      expect(alreadyExistsGuard(alreadyExists)).toBe(true);
      expect(conflictGuard(alreadyExists)).toBe(false);
      expect(alreadyExists).toMatchObject({
        service: 'catalog.items',
        method: conflictMethod.operationId,
        status: 409,
        code: 'already_exists',
      });
      expect(alreadyExists.details).toBeUndefined();
      expect('responseBody' in alreadyExists).toBe(false);
      expect(JSON.stringify(alreadyExists)).not.toContain('runtime-only-api-key');

      const missing = await getMissing.call(first, { path: { id: 'widget' } }).catch((error) => error);
      const missingGuard = generated[
        Object.keys(generated).find((name) => name.endsWith('NotFoundError') && name.includes('Missing')) ?? ''
      ] as (error: unknown) => boolean;
      expect(missingGuard(missing)).toBe(true);
      expect(missing).toMatchObject({
        service: 'catalog.items',
        method: missingMethod.operationId,
        status: 404,
        code: 'not_found',
        details: { code: 'NotFound', reason: 'safe-widget' },
      });
      expect(
        missingGuard(
          new ClientFrameworkError({
            service: 'catalog.items',
            method: missingMethod.operationId,
            status: 404,
            code: 'not_found',
            details: { code: 'NotFound', reason: 42 },
          }),
        ),
      ).toBe(false);

      // Isolate one real inbound consumer request so the complete W3C parent
      // chain and metric result can be asserted without relying on mocks.
      const collector = getCollector();
      collector?.drainAll();
      collector?.drainSpans();
      const incomingTraceId = '11111111111111111111111111111111';
      const incomingSpanId = '2222222222222222';
      const consumerPort = consumerHttp.getServer()?.port;
      const proxyResponse = await fetch(`http://localhost:${consumerPort}/proxy/w-otel`, {
        headers: { traceparent: `00-${incomingTraceId}-${incomingSpanId}-01` },
      });
      expect(await proxyResponse.json()).toEqual({ id: 'w-otel', name: 'framework-generated' });

      const spans = collector?.drainSpans() ?? [];
      const serverSpan = spans.find((span) => span.kind === 2);
      const callSpan = spans.find((span) => span.name === widgetMethod.operationId);
      const attemptSpan = spans.find((span) => span.name === `${widgetMethod.operationId} attempt`);
      expect(serverSpan).toMatchObject({ traceId: incomingTraceId, parentSpanId: incomingSpanId });
      expect(callSpan).toMatchObject({ traceId: incomingTraceId, parentSpanId: serverSpan?.spanId, statusCode: 1 });
      expect(attemptSpan).toMatchObject({ traceId: incomingTraceId, parentSpanId: callSpan?.spanId, statusCode: 1 });
      expect(providerTraceparent).toBe(`00-${incomingTraceId}-${attemptSpan?.spanId}-01`);

      const buckets = collector?.drainAll() ?? [];
      const counterSeries = buckets.flatMap((bucket) => bucket.counterSeries ?? []);
      const histogramSeries = buckets.flatMap((bucket) => bucket.histogramSeries ?? []);
      expect(counterSeries).toContainEqual({
        name: 'rpc.client.calls',
        value: 1,
        attributes: {
          'network.protocol.name': 'rest-json',
          'rpc.method': widgetMethod.operationId,
          'rpc.service': 'catalog.items',
          'rpc.system': 'putnami',
          'http.response.status_code': 200,
        },
      });
      expect(counterSeries.some((series) => series.name === 'rpc.client.attempts' && series.value === 1)).toBe(true);
      expect(histogramSeries.map((series) => series.name)).toContainAllValues([
        'rpc.client.duration',
        'rpc.client.attempt.duration',
        'rpc.client.auth.duration',
      ]);
      await consumer.stop();
      const closedEndpointStream = watchWidgets.call(first, { endpoint: fleetEndpoint });
      await expect(streamError(closedEndpointStream)).resolves.toBeInstanceOf(CredentialRegistryClosedError);
    } finally {
      await consumer.stop();
      await fleet.stop();
      await provider.stop();
    }
  });

  specTest(
    'refuses to emit an operation whose integer field declares no width',
    {
      feature: 'typescript/service-clients',
      requirement: 'strict-generation',
      check: 'an-integer-without-a-declared-width-is-refused',
    },
    async () => {
      // ADR 0004: an integer's width is declared, never inferred. The TypeScript
      // provider now declares one — `Int` projects `format: "int64"` — so a
      // first-party TypeScript provider that exposes an integer field generates,
      // and its 64-bit field arrives as `bigint`. The refusal is still what
      // protects that: the same contract with the format removed is rejected
      // rather than emitted as `number`, which would narrow 64 bits to 53.
      const providerHttp = http({ port: 0 });
      const providerApi = api({
        autoScan: false,
        client: { service: { id: 'catalog.numbers', audience: 'api://numbers' }, credentials: {} },
      });
      providerApi.register(
        '/widgets/numbers',
        endpoint()
          .body({ value: Int })
          .returns({ value: Int })
          .client({ security: { alternatives: [{ allOf: [] }] }, idempotency: { kind: 'idempotent' } })
          .handle(async (context) => ({ value: (await context.body()).value })),
        'POST',
      );
      const providerOpenApi = openapi({ title: 'Numbers', version: '1.0.0' });
      const provider = application().use(providerHttp).use(providerApi).use(providerOpenApi);
      await provider.start();
      try {
        const contract = providerOpenApi.spec();
        if (!contract) throw new Error('provider OpenAPI contract was not emitted');
        // The contract itself reads: the defect is unrepresentable emission, not an
        // invalid contract.
        const ir = readOpenApiSource(JSON.stringify(contract), { mode: 'firstParty' });
        expect(ir.services.flatMap((service) => service.methods)).toHaveLength(1);
        const files = generateTypeScriptClient(ir, { packageName: '@test/numbers-client' });
        const types = files.find((file) => file.path === 'src/types.ts')?.content ?? '';
        expect(types).toContain('value: bigint');

        // The same contract with the declared width removed is refused.
        const untyped = JSON.parse(JSON.stringify(contract)) as {
          components: { schemas: Record<string, { properties: Record<string, { format?: string }> }> };
        };
        for (const schema of Object.values(untyped.components.schemas)) {
          for (const property of Object.values(schema.properties ?? {})) property.format = undefined;
        }
        const widthless = readOpenApiSource(JSON.stringify(untyped), { mode: 'firstParty' });
        expect(() => generateTypeScriptClient(widthless, { packageName: '@test/numbers-client' })).toThrow(
          /clientgen_unsupported_semantic: .*declares type "integer" without an int32\|int64\|uint32\|uint64 format/,
        );
      } finally {
        await provider.stop();
      }
    },
  );
});

function collectStream<T>(stream: StreamObserver<T>): Promise<T[]> {
  return new Promise((resolve, reject) => {
    const values: T[] = [];
    stream.onMessage((value) => values.push(value));
    stream.onError(reject);
    stream.onComplete(() => resolve(values));
  });
}

function streamError<T>(stream: StreamObserver<T>): Promise<Error> {
  return new Promise((resolve) => stream.onError(resolve));
}
