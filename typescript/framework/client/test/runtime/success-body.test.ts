import { afterEach, describe, expect, test } from 'bun:test';
import { application, type ClientContractDocument, type ClientContractOperation } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import { ClientResponseContractError, ClientServiceConfigError } from '../../src/runtime/errors';
import { ServiceResponseCache } from '../../src/runtime/response-cache';
import { createServiceClientRegistration, type GeneratedServiceDescriptor } from '../../src/runtime/service-binding';
import type { DuplexStream, StreamObserver } from '../../src/runtime/stream.type';
import { SuccessBody } from '../../src/runtime/success-body';
import type { ClientRequest } from '../../src/runtime/transport.type';

const FEATURE = 'typescript/service-clients';
const REQUIREMENT = 'success-body';

/**
 * A body no serializer writes back: fields in neither declaration nor sorted
 * order, whitespace, a trailing newline, and an escape a serializer writes as
 * UTF-8.
 */
const FIDELITY_BODY = '{\n  "zeta" : "caf\\u00e9",\n  "alpha":"a"\n}\n';
const encoder = new TextEncoder();
const decoder = new TextDecoder();

const contract: ClientContractDocument = {
  protocolVersion: 1,
  service: { id: 'fidelity', audience: 'urn:fidelity' },
  credentials: {},
};

const FIDELITY_SUCCESS: NonNullable<ClientRequest['successes']> = [
  {
    status: 200,
    description: 'Fidelity',
    content: [
      {
        mediaType: 'application/json',
        schema: {
          type: 'object',
          properties: { zeta: { type: 'string' }, alpha: { type: 'string' } },
          required: ['zeta', 'alpha'],
          additionalProperties: false,
        },
      },
    ],
  },
];

const anonymous = { alternatives: [{ allOf: [] }] };

function unary(path: string, cache?: { freshMs: number; staleMs?: number }): ClientContractOperation {
  return {
    stream: 'unary',
    transports: [{ protocol: 'rest-json', path, encoding: 'json' }],
    security: anonymous,
    errors: [],
    idempotency: { kind: 'safe' },
    ...(cache ? { resilience: { cache } } : {}),
  };
}

const operations: Record<string, ClientContractOperation> = {
  getFidelity: unary('/fidelity'),
  getCached: unary('/cached', { freshMs: 5000, staleMs: 300_000 }),
  getBlob: unary('/blob'),
  deleteThing: unary('/thing'),
  watchThings: {
    stream: 'server',
    messages: { output: { type: 'object', properties: { value: { type: 'string' } } } },
    transports: [{ protocol: 'sse', path: '/watch', encoding: 'json' }],
    security: anonymous,
    errors: [],
    idempotency: { kind: 'safe' },
  },
  adjustThings: {
    stream: 'client',
    messages: {
      input: { type: 'object', properties: { value: { type: 'string' } } },
      output: { type: 'object', properties: { value: { type: 'string' } } },
    },
    transports: [
      { protocol: 'websocket', path: '/adjust', encoding: 'json', websocket: { subprotocol: 'putnami.service.v1' } },
    ],
    security: anonymous,
    errors: [],
    idempotency: { kind: 'safe' },
  },
};

type Fidelity = { zeta: string; alpha: string };

class FidelityClient extends BaseClient {
  readonly serviceName = 'fidelity';

  fidelity(successBody?: SuccessBody): Promise<Fidelity> {
    return this.request('GET', '/fidelity', { operationId: 'getFidelity', successBody, successes: FIDELITY_SUCCESS });
  }

  cached(successBody?: SuccessBody): Promise<Fidelity> {
    return this.request('GET', '/cached', { operationId: 'getCached', successBody, successes: FIDELITY_SUCCESS });
  }

  blob(successBody?: SuccessBody) {
    return this.requestBinary<200>('GET', '/blob', {
      operationId: 'getBlob',
      successBody,
      responseMediaType: 'application/octet-stream',
      successes: [
        {
          status: 200,
          description: 'Blob',
          content: [{ mediaType: 'application/octet-stream', schema: { type: 'string', format: 'binary' } }],
        },
      ],
    });
  }

  remove(successBody?: SuccessBody): Promise<void> {
    return this.request('DELETE', '/thing', {
      operationId: 'deleteThing',
      successBody,
      successes: [{ status: 204, description: 'Deleted', content: [] }],
    });
  }

  watch(successBody?: SuccessBody): StreamObserver<{ value: string }> {
    return this.serviceStream('GET', '/watch', { operationId: 'watchThings', successBody });
  }

  adjust(successBody?: SuccessBody): DuplexStream<{ value: string }, { value: string }> {
    return this.serviceClientStream('GET', '/adjust', { operationId: 'adjustThings', successBody });
  }
}

/** A provider whose answers are chosen per path, counting the calls it saw. */
class Provider {
  calls = 0;
  answers: Record<string, () => Response> = {};
  private hold: { promise: Promise<void>; release: () => void } | undefined;
  private readonly server: ReturnType<typeof Bun.serve>;

  constructor() {
    this.server = Bun.serve({
      port: 0,
      fetch: async (request) => {
        this.calls++;
        await this.hold?.promise;
        const answer = this.answers[new URL(request.url).pathname];
        return answer ? answer() : new Response('', { status: 404 });
      },
    });
  }

  get url(): string {
    return `http://localhost:${this.server.port}`;
  }

  holdAnswers(): void {
    let release = () => {};
    const promise = new Promise<void>((resolve) => {
      release = resolve;
    });
    this.hold = { promise, release };
  }

  release(): void {
    this.hold?.release();
    this.hold = undefined;
  }

  stop(): void {
    this.release();
    this.server.stop(true);
  }
}

function json(body: string, status = 200, contentType = 'application/json'): () => Response {
  return () => new Response(body, { status, headers: { 'Content-Type': contentType } });
}

const providers: Provider[] = [];
const apps: ReturnType<typeof application>[] = [];

afterEach(async () => {
  for (const provider of providers.splice(0)) provider.stop();
  await Promise.all(apps.splice(0).map((app) => app.stop()));
});

function provider(): Provider {
  const created = new Provider();
  created.answers['/fidelity'] = json(FIDELITY_BODY);
  providers.push(created);
  return created;
}

async function bind(url: string, clock?: { now: number }): Promise<FidelityClient> {
  const descriptor: GeneratedServiceDescriptor = {
    contract,
    service: 'FidelityService',
    operations,
    transport: 'http',
  };
  const app = application();
  app.register(
    createServiceClientRegistration(
      FidelityClient,
      descriptor,
      { url, clientId: 'consumer', allowInsecure: true },
      undefined,
      clock ? new ServiceResponseCache({ now: () => clock.now }) : undefined,
    ),
  );
  await app.start();
  apps.push(app);
  return app.context.get(FidelityClient);
}

function streamError(stream: StreamObserver<unknown>): Promise<unknown> {
  return new Promise((resolve) => stream.onError(resolve));
}

describe('generated call success body', () => {
  specTest(
    'delivers the provider bytes after the call accepted them',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'the-success-body-is-delivered-byte-for-byte-after-the-calls-own-checks',
    },
    async () => {
      const client = await bind(provider().url);
      const sink = new SuccessBody();
      expect(sink.bytes).toBeUndefined();
      expect(await client.fidelity(sink)).toEqual({ zeta: 'café', alpha: 'a' });
      expect(decoder.decode(sink.bytes)).toBe(FIDELITY_BODY);
      // The proof is only meaningful because serializing the decoded value
      // again cannot give these bytes back.
      expect(JSON.stringify(await client.fidelity())).not.toBe(FIDELITY_BODY);
      // A call without a sink is unchanged, and the sink keeps the last body.
      await client.fidelity();
      expect(decoder.decode(sink.bytes)).toBe(FIDELITY_BODY);
    },
  );

  specTest(
    'replays the stored bytes from the response cache and hands each caller its own copy',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-cached-answer-delivers-the-bytes-the-provider-sent' },
    async () => {
      const upstream = provider();
      const cachedBody = '{ "zeta" :\t"z", "alpha":"a" }';
      upstream.answers['/cached'] = json(cachedBody);
      const clock = { now: Date.UTC(2026, 8, 18, 12) };
      const client = await bind(upstream.url, clock);
      const first = new SuccessBody();
      await client.cached(first);
      expect(decoder.decode(first.bytes)).toBe(cachedBody);
      // Writing into the delivered copy reaches neither the cache nor the next caller.
      first.bytes?.set(encoder.encode('XXXX'));
      const second = new SuccessBody();
      expect(await client.cached(second)).toEqual({ zeta: 'z', alpha: 'a' });
      expect(upstream.calls).toBe(1);
      expect(decoder.decode(second.bytes)).toBe(cachedBody);
      // A stale answer masking an outage is still the stored bytes.
      clock.now += 10_000;
      upstream.answers['/cached'] = json('{"code":"unavailable","error":"Unavailable","message":"down"}', 503);
      const stale = new SuccessBody();
      await client.cached(stale);
      expect(decoder.decode(stale.bytes)).toBe(cachedBody);
    },
  );

  specTest(
    'leaves nothing in the sink when the call is refused or fails',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-refused-or-failed-call-delivers-no-bytes' },
    async () => {
      const upstream = provider();
      const client = await bind(upstream.url);
      const cases: [string, () => Response, unknown][] = [
        ['undeclared status', json('{"zeta":"z","alpha":"a"}', 202), ClientResponseContractError],
        ['undeclared media type', json('{"zeta":"z","alpha":"a"}', 200, 'text/plain'), ClientResponseContractError],
        ['schema mismatch', json('{"zeta":"z"}'), ClientResponseContractError],
        ['trailing data', json('{"zeta":"z","alpha":"a"} {}'), ClientResponseContractError],
        ['empty body', json(''), ClientResponseContractError],
        ['provider error', json('{"code":"internal","error":"Internal Server Error","message":"boom"}', 500), Error],
      ];
      for (const [name, answer, expected] of cases) {
        const sink = new SuccessBody();
        // The sink first holds an earlier call's body: a failed call must not
        // leave it there for the caller to mistake for its own.
        upstream.answers['/fidelity'] = json(FIDELITY_BODY);
        await client.fidelity(sink);
        expect(sink.bytes).toBeDefined();
        upstream.answers['/fidelity'] = answer;
        await expect(client.fidelity(sink), name).rejects.toBeInstanceOf(expected as never);
        expect(sink.bytes, name).toBeUndefined();
      }
    },
  );

  specTest(
    'is refused before dispatch by every call that cannot deliver it',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-refused-or-failed-call-delivers-no-bytes' },
    async () => {
      const upstream = provider();
      const client = await bind(upstream.url);
      // Every refusal starts from a sink that already holds a body: what it
      // must prove is that the refused call empties it.
      const sink = new SuccessBody();
      const filled = async (): Promise<SuccessBody> => {
        await client.fidelity(sink);
        expect(sink.bytes).toBeDefined();
        return sink;
      };
      const refused = (): void => expect(sink.bytes).toBeUndefined();
      await expect(client.blob(await filled())).rejects.toBeInstanceOf(ClientServiceConfigError);
      refused();
      // A void operation declares no success body to deliver.
      await expect(client.remove(await filled())).rejects.toBeInstanceOf(ClientServiceConfigError);
      refused();
      await expect(streamError(client.watch(await filled()))).resolves.toBeInstanceOf(ClientServiceConfigError);
      refused();
      await expect(streamError(client.adjust(await filled()))).resolves.toBeInstanceOf(ClientServiceConfigError);
      refused();
      // A call with no generated operation has no declaration to check the body against.
      class BareClient extends BaseClient {
        readonly serviceName = 'fidelity';
        bare(successBody: SuccessBody): Promise<unknown> {
          return this.request('GET', '/fidelity', { successBody });
        }
      }
      await expect(
        new BareClient({ baseUrl: upstream.url, transport: 'http' }).bare(await filled()),
      ).rejects.toBeInstanceOf(ClientServiceConfigError);
      refused();
      // A transport that does not declare it keeps the body refuses the sink
      // before it runs: the refusal is a configuration error, not an answer.
      const executed: string[] = [];
      const bareTransport = {
        execute: async (request: ClientRequest) => {
          executed.push(request.path);
          return { data: { zeta: 'z', alpha: 'a' }, status: 200, headers: new Headers() };
        },
      };
      const custom = new FidelityClient({ baseUrl: upstream.url, transport: 'http', operationContracts: operations });
      Reflect.set(custom, 'transport', bareTransport);
      await filled();
      const before = upstream.calls;
      await expect(custom.fidelity(sink)).rejects.toThrow(ClientServiceConfigError);
      await expect(custom.fidelity(sink)).rejects.toThrow('transport does not declare');
      refused();
      expect(executed).toEqual([]);
      // The same transport still answers the call when no sink asks for the body.
      expect(await custom.fidelity()).toEqual({ zeta: 'z', alpha: 'a' });
      expect(executed).toEqual(['/fidelity']);
      expect(upstream.calls).toBe(before);
      // Defensive: an answer that reached the caller without the declared
      // document — here, an interceptor's — fails the call rather than
      // delivering other bytes.
      const shortCircuited = new FidelityClient({
        baseUrl: upstream.url,
        transport: 'http',
        operationContracts: operations,
        interceptors: [async () => ({ data: { zeta: 'z', alpha: 'a' }, status: 200, headers: new Headers() })],
      });
      await expect(shortCircuited.fidelity(await filled())).rejects.toBeInstanceOf(ClientResponseContractError);
      refused();
      // Only the fill reached the provider: the short-circuited call never did.
      expect(upstream.calls).toBe(before + 1);
    },
  );

  specTest(
    'belongs to one call at a time',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'a-sink-belongs-to-one-call-at-a-time' },
    async () => {
      const upstream = provider();
      upstream.holdAnswers();
      const client = await bind(upstream.url);
      const sink = new SuccessBody();
      const held = client.fidelity(sink);
      while (upstream.calls === 0) await new Promise<void>((resolve) => setImmediate(resolve));
      expect(sink.bytes).toBeUndefined();
      await expect(client.fidelity(sink)).rejects.toThrow('one call at a time');
      // A call refused for another reason while the sink is held leaves it to
      // the holder as well: the holder's answer is still delivered below.
      await expect(client.remove(sink)).rejects.toBeInstanceOf(ClientServiceConfigError);
      upstream.release();
      await held;
      expect(decoder.decode(sink.bytes)).toBe(FIDELITY_BODY);
      expect(upstream.calls).toBe(1);
      // Released: the same sink serves the next call.
      await client.fidelity(sink);
      expect(decoder.decode(sink.bytes)).toBe(FIDELITY_BODY);
    },
  );

  specTest(
    'is refused on a transport that re-encodes the body, before anything is sent',
    {
      feature: FEATURE,
      requirement: REQUIREMENT,
      check: 'a-transport-that-rewrites-the-body-is-refused-before-dispatch',
    },
    async () => {
      const upstream = provider();
      const connect = (encoding: 'proto' | 'json') => ({
        protocol: 'connect' as const,
        path: '/fidelity.v1.FidelityService/GetFidelity',
        encoding,
        protobufMethod: '/fidelity.v1.FidelityService/GetFidelity',
      });
      const descriptor = {
        syntax: 'proto3' as const,
        package: 'fidelity.v1',
        services: [
          {
            name: 'FidelityService',
            methods: [
              {
                name: 'GetFidelity',
                input: 'GetFidelityRequest',
                output: 'GetFidelityResponse',
                clientStreaming: false,
                serverStreaming: false,
              },
            ],
          },
        ],
        messages: [
          { name: 'GetFidelityRequest', fields: [] },
          {
            name: 'GetFidelityResponse',
            fields: [
              { name: 'zeta', jsonName: 'zeta', number: 1, typeKind: 'scalar' as const, type: 'string' },
              { name: 'alpha', jsonName: 'alpha', number: 2, typeKind: 'scalar' as const, type: 'string' },
            ],
          },
        ],
        enums: [],
      };
      const rest = unary('/fidelity');
      const clientFor = (transports: ClientContractOperation['transports']) =>
        new FidelityClient({
          baseUrl: upstream.url,
          transport: 'http',
          clientProtobuf: descriptor,
          operationContracts: { getFidelity: { ...rest, transports } },
        });
      // Connect with the proto encoding re-encodes the reply from protobuf: the
      // sink is refused and the provider is never reached.
      const proto = clientFor([connect('proto'), connect('json'), ...rest.transports]);
      const sink = new SuccessBody();
      await clientFor(rest.transports).fidelity(sink);
      expect(sink.bytes).toBeDefined();
      await expect(proto.fidelity(sink)).rejects.toThrow('connect with the proto encoding');
      expect(sink.bytes).toBeUndefined();
      expect(upstream.calls).toBe(1);
      // Connect with the JSON encoding carries the declared document itself.
      const connectBody = '{ "zeta" : "z",\n "alpha" : "a" }';
      upstream.answers['/fidelity.v1.FidelityService/GetFidelity'] = json(connectBody);
      const jsonClient = clientFor([connect('json'), ...rest.transports]);
      expect(await jsonClient.fidelity(sink)).toEqual({ zeta: 'z', alpha: 'a' });
      expect(decoder.decode(sink.bytes)).toBe(connectBody);
      expect(upstream.calls).toBe(2);
    },
  );

  test('the generated call option survives endpoint selection', async () => {
    const upstream = provider();
    const fleet = provider();
    fleet.answers['/fidelity'] = json('{"alpha":"fleet","zeta":"z"}');
    const client = await bind(upstream.url);
    const sink = new SuccessBody();
    const view = (client as unknown as { forEndpoint(endpoint: string): FidelityClient }).forEndpoint(fleet.url);
    expect(await view.fidelity(sink)).toEqual({ alpha: 'fleet', zeta: 'z' });
    expect(decoder.decode(sink.bytes)).toBe('{"alpha":"fleet","zeta":"z"}');
    expect(upstream.calls).toBe(0);
  });
});
