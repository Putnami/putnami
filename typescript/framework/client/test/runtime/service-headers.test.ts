import { afterEach, describe, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  application,
  type ClientContractOperation,
  type ClientCredentialProfile,
  type ClientSchema,
  SERVICE_WEBSOCKET_SUBPROTOCOL,
} from '@putnami/application';
import { resetConfigLoader, runInContext } from '@putnami/runtime';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import { ClientCredentialError, ClientServiceConfigError } from '../../src/runtime/errors';
import type { ByteStream } from '../../src/runtime/provider-ws-transport';
import {
  bindServiceClient,
  type GeneratedServiceDescriptor,
  registerServiceClient,
  type ServiceBinding,
} from '../../src/runtime/service-binding';
import {
  RESERVED_BINDING_HEADER_PREFIXES,
  RESERVED_BINDING_HEADERS,
  snapshotBindingHeaders,
} from '../../src/runtime/service-headers';
import type { StreamObserver } from '../../src/runtime/stream.type';

const FEATURE = 'typescript/service-clients';
const REQUIREMENT = 'binding-headers';
const CORPUS = resolve(import.meta.dir, '../../../../../protocols/clientcontract/fixtures/binding/headers.json');

const USER = { alternatives: [{ allOf: [{ profile: 'user' }] }] } as const;
const ANONYMOUS = { alternatives: [{ allOf: [] }] } as const;

const output = {
  type: 'object',
  properties: { value: { type: 'string' } },
  required: ['value'],
  additionalProperties: false,
} as const satisfies ClientSchema;

function unary(path: string, overrides: Partial<ClientContractOperation> = {}): ClientContractOperation {
  return {
    stream: 'unary',
    transports: [{ protocol: 'rest-json', path, encoding: 'json' }],
    security: ANONYMOUS,
    errors: [],
    idempotency: { kind: 'safe' },
    ...overrides,
  };
}

function serverStream(path: string, overrides: Partial<ClientContractOperation> = {}): ClientContractOperation {
  return {
    stream: 'server',
    messages: { output },
    transports: [{ protocol: 'sse', path, encoding: 'json' }],
    security: USER,
    errors: [],
    idempotency: { kind: 'safe' },
    ...overrides,
  };
}

const operations: Record<string, ClientContractOperation> = {
  getRevision: unary('/revision', { security: USER, resilience: { retry: { maxAttempts: 2, statuses: [503] } } }),
  getCached: unary('/cached', { resilience: { cache: { freshMs: 60_000, keyFields: ['header.x-revision'] } } }),
  putItem: unary('/items', { idempotency: { kind: 'idempotent', keyHeader: 'Idempotency-Key' } }),
  watchSse: serverStream('/watch-sse'),
  watchKeyed: serverStream('/watch-keyed', { idempotency: { kind: 'idempotent', keyHeader: 'Idempotency-Key' } }),
  watchSocket: serverStream('/watch-socket', {
    transports: [
      {
        protocol: 'websocket',
        path: '/watch-socket',
        encoding: 'json',
        websocket: { subprotocol: SERVICE_WEBSOCKET_SUBPROTOCOL, resume: false },
      },
    ],
  }),
  openTunnel: {
    stream: 'bidirectional',
    transports: [
      { protocol: 'websocket', path: '/tunnel', encoding: 'binary', websocket: { resume: false, wire: 'provider' } },
    ],
    security: USER,
    errors: [],
    idempotency: { kind: 'non-idempotent' },
  },
};

function descriptor(credentials: Record<string, ClientCredentialProfile> = {}): GeneratedServiceDescriptor {
  return {
    contract: {
      protocolVersion: 1,
      service: { id: 'revisions', audience: 'api://revisions' },
      credentials: { user: { kind: 'forwarded-user-token' }, ...credentials },
    },
    service: 'RevisionsService',
    operations,
    transport: 'http',
  };
}

class RevisionsClient extends BaseClient {
  readonly serviceName = 'revisions';

  read(): Promise<{ value: string }> {
    return this.request('GET', '/revision', { operationId: 'getRevision' });
  }

  readAt(endpoint: string): Promise<{ value: string }> {
    return this.forEndpoint(endpoint).read();
  }

  cached(headers?: Record<string, string | undefined>): Promise<{ value: string }> {
    return this.request('GET', '/cached', { operationId: 'getCached', headers });
  }

  put(): Promise<{ value: string }> {
    return this.request('PUT', '/items', { operationId: 'putItem', body: { value: 'input' } });
  }

  watchSse(): StreamObserver<{ value: string }> {
    return this.serviceStream('GET', '/watch-sse', { operationId: 'watchSse' });
  }

  watchKeyed(): StreamObserver<{ value: string }> {
    return this.serviceStream('GET', '/watch-keyed', { operationId: 'watchKeyed' });
  }

  watchSocket(): StreamObserver<{ value: string }> {
    return this.serviceStream('GET', '/watch-socket', { operationId: 'watchSocket' });
  }

  tunnel(): Promise<ByteStream> {
    return this.serviceByteStream('GET', '/tunnel', { operationId: 'openTunnel' });
  }
}

/** What one request that reached the provider carried. */
interface Seen {
  readonly path: string;
  readonly revision: string | null;
  readonly authorization: string | null;
  readonly idempotencyKey: string | null;
}

interface Provider {
  readonly url: string;
  readonly seen: Seen[];
  readonly inits: Record<string, unknown>[];
}

const stops: (() => void)[] = [];
const apps: ReturnType<typeof application>[] = [];

afterEach(async () => {
  for (const stop of stops.splice(0)) stop();
  await Promise.all(apps.splice(0).map((app) => app.stop()));
  delete process.env.CONFIG_DATA;
  resetConfigLoader();
});

/** One provider for every carrier: REST, SSE, first-party and provider-owned WebSocket. */
function provider(options: { failFirstRevision?: boolean } = {}): Provider {
  const seen: Seen[] = [];
  const inits: Record<string, unknown>[] = [];
  let revisionCalls = 0;
  const server = Bun.serve<{ path: string }, Record<string, never>>({
    port: 0,
    fetch(request, target) {
      const path = new URL(request.url).pathname;
      const revision = request.headers.get('x-revision');
      seen.push({
        path,
        revision,
        authorization: request.headers.get('authorization'),
        idempotencyKey: request.headers.get('idempotency-key'),
      });
      if (request.headers.get('upgrade')?.toLowerCase() === 'websocket') {
        const offered = request.headers.get('sec-websocket-protocol');
        const headers = offered ? { 'Sec-WebSocket-Protocol': offered } : undefined;
        if (target.upgrade(request, { data: { path }, ...(headers ? { headers } : {}) })) return undefined;
        return new Response('expected an upgrade', { status: 400 });
      }
      if (path === '/revision' && options.failFirstRevision && revisionCalls++ === 0) {
        return new Response('busy', { status: 503 });
      }
      if (path.startsWith('/watch')) {
        return new Response(`data: ${JSON.stringify({ value: revision ?? '' })}\n\n`, {
          headers: { 'Content-Type': 'text/event-stream' },
        });
      }
      return Response.json({ value: revision ?? '' });
    },
    websocket: {
      message(socket, message) {
        const frame = JSON.parse(String(message)) as Record<string, unknown>;
        if (frame['type'] !== 'init') return;
        inits.push(frame);
        socket.send(JSON.stringify({ v: 1, type: 'ready', resumed: false }));
        socket.send(
          JSON.stringify({
            v: 1,
            type: 'message',
            sequence: '1',
            payload: { encoding: 'json', value: { value: 'socket' } },
          }),
        );
        socket.send(JSON.stringify({ v: 1, type: 'result' }));
      },
    },
  });
  stops.push(() => server.stop(true));
  return { url: `http://localhost:${server.port}`, seen, inits };
}

function binding(url: string, overrides: Partial<ServiceBinding> = {}): ServiceBinding {
  return {
    url,
    clientId: 'revision-consumer',
    allowInsecure: true,
    headers: { 'X-Revision': 'instance' },
    credentials: { user: { source: 'forwarded-user' } },
    ...overrides,
  };
}

function bound(url: string, overrides: Partial<ServiceBinding> = {}): RevisionsClient {
  const client = bindServiceClient(RevisionsClient, descriptor(), binding(url, overrides));
  stops.push(() => client.dispose());
  return client;
}

function collect<T>(stream: StreamObserver<T>): Promise<{ values: T[]; failure?: unknown }> {
  return new Promise((done) => {
    const values: T[] = [];
    stream.onMessage((value) => values.push(value));
    stream.onError((failure) => done({ values, failure }));
    stream.onComplete(() => done({ values }));
  });
}

function asUser<T>(user: string, run: () => Promise<T>): Promise<T> {
  return runInContext({ __authorizationHeader: `Bearer ${user}` }, run);
}

describe('static service binding headers', () => {
  specTest(
    'accompany each call and retry beside its own forwarded user',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'static-headers-accompany-each-calls-credentials' },
    async () => {
      const upstream = provider({ failFirstRevision: true });
      const app = application();
      apps.push(app);
      registerServiceClient(app, RevisionsClient, descriptor(), binding(upstream.url));
      await app.start();
      const client = app.context.get(RevisionsClient);
      expect(await asUser('first-user', () => client.read())).toEqual({ value: 'instance' });
      expect(await asUser('second-user', () => client.read())).toEqual({ value: 'instance' });
      // The first call was retried after a 503: every attempt carried the
      // static value once, beside the user of its own call.
      expect(upstream.seen.map(({ revision, authorization }) => [revision, authorization])).toEqual([
        ['instance', 'Bearer first-user'],
        ['instance', 'Bearer first-user'],
        ['instance', 'Bearer second-user'],
      ]);
      // A static header never stands in for a missing credential.
      await expect(client.read()).rejects.toBeInstanceOf(ClientCredentialError);
      expect(upstream.seen).toHaveLength(3);
      // An endpoint view of the same binding carries the same defaults.
      const replica = provider();
      expect(await asUser('first-user', () => client.readAt(replica.url))).toEqual({ value: 'instance' });
      expect(replica.seen).toEqual([
        { path: '/revision', revision: 'instance', authorization: 'Bearer first-user', idempotencyKey: null },
      ]);
    },
  );

  specTest(
    'refuse invalid, reserved and credential headers, and a static idempotency key, before dispatch',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'invalid-or-reserved-headers-fail-before-dispatch' },
    async () => {
      const upstream = provider();
      const refusals: unknown[] = [];
      const refuse = (run: () => unknown) => {
        try {
          run();
        } catch (error) {
          refusals.push(error);
          return;
        }
        throw new Error('the binding was accepted');
      };
      for (const headers of [{ authorization: 'private-value' }, { 'X-TRACE-ID': 'private-value' }]) {
        refuse(() =>
          registerServiceClient(application(), RevisionsClient, descriptor(), binding(upstream.url, { headers })),
        );
        refuse(() => bindServiceClient(RevisionsClient, descriptor(), binding(upstream.url, { headers })));
      }
      // A header a provider credential profile declares is a secret's place.
      const keyed = descriptor({ key: { kind: 'named-header', header: 'X-Key' } });
      refuse(() =>
        bindServiceClient(RevisionsClient, keyed, binding(upstream.url, { headers: { 'x-key': 'private-value' } })),
      );
      refuse(() =>
        registerServiceClient(
          application(),
          RevisionsClient,
          keyed,
          binding(upstream.url, { headers: { 'x-key': 'private-value' } }),
        ),
      );
      for (const error of refusals) {
        expect(error).toBeInstanceOf(ClientServiceConfigError);
        expect(String((error as Error).message)).not.toContain('private-value');
      }

      // Deployment config is held to the same rules when DI resolves it.
      process.env.CONFIG_DATA = JSON.stringify({
        clients: {
          clientId: 'revision-consumer',
          services: { revisions: { url: upstream.url, allowInsecure: true, headers: { Cookie: 'private-value' } } },
        },
      });
      resetConfigLoader();
      const configured = application();
      apps.push(configured);
      registerServiceClient(configured, RevisionsClient, descriptor());
      const failure = await (async () => {
        await configured.start();
        configured.context.get(RevisionsClient);
      })().catch((error: unknown) => error);
      expect(failure).toBeInstanceOf(ClientServiceConfigError);
      expect(String((failure as Error).message)).not.toContain('private-value');

      // A static idempotency key fails the one operation that declares it, for
      // a unary call and for a stream, while the binding stays usable.
      const client = bound(upstream.url, { headers: { 'idempotency-key': 'private-value' } });
      const unaryFailure = await client.put().catch((error: unknown) => error);
      expect(unaryFailure).toBeInstanceOf(ClientServiceConfigError);
      expect(String((unaryFailure as Error).message)).not.toContain('private-value');
      const streamFailure = await asUser('first-user', () => collect(client.watchKeyed()));
      expect(streamFailure.failure).toBeInstanceOf(ClientServiceConfigError);
      expect(upstream.seen).toEqual([]);
      expect(await client.cached()).toEqual({ value: '' });
      expect(upstream.seen.map((request) => request.path)).toEqual(['/cached']);
    },
  );

  specTest(
    'snapshot the caller map, so later changes cannot reach a constructed client',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'binding-headers-are-an-immutable-snapshot' },
    async () => {
      const upstream = provider();
      const headers: Record<string, string> = { 'X-Revision': 'original' };
      const direct = bound(upstream.url, { headers });
      const app = application();
      apps.push(app);
      registerServiceClient(app, RevisionsClient, descriptor(), binding(upstream.url, { headers }));
      await app.start();
      const registered = app.context.get(RevisionsClient);
      const calls: Promise<{ value: string }>[] = [];
      for (let index = 0; index < 10; index++) {
        calls.push(direct.cached({ 'x-call': String(index) }), registered.cached({ 'x-call': String(index) }));
        headers['X-Revision'] = `changed-${index}`;
        headers['X-Added'] = 'added';
      }
      headers['X-Revision'] = undefined;
      calls.push(direct.cached({ 'x-call': 'last' }), registered.cached({ 'x-call': 'last' }));
      // The caller still owns a mutable object: the binding copied it rather
      // than freezing it in place.
      expect(Object.isFrozen(headers)).toBe(false);
      expect((await Promise.all(calls)).map((answer) => answer.value)).toEqual(Array(22).fill('original'));
      expect(upstream.seen.every((request) => request.revision === 'original')).toBe(true);
    },
  );

  specTest(
    'let operation headers win, and key the response cache on the value that is sent',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'operation-headers-win-and-cache-keys-see-the-sent-value' },
    async () => {
      const upstream = provider();
      const client = bound(upstream.url);
      const values: string[] = [];
      for (const header of [undefined, 'operation', undefined, 'operation']) {
        const operationHeaders = header === undefined ? undefined : { 'X-Revision': header };
        values.push((await client.cached(operationHeaders)).value);
      }
      expect(values).toEqual(['instance', 'operation', 'instance', 'operation']);
      // Two sent values, two cache entries: the default is part of the key.
      expect(upstream.seen.map((request) => request.revision)).toEqual(['instance', 'operation']);
      // An explicit empty value wins too, case-insensitively, and is its own key.
      expect(await client.cached({ 'x-revision': '' })).toEqual({ value: '' });
      expect(await client.cached({ 'x-revision': '' })).toEqual({ value: '' });
      expect(upstream.seen.map((request) => request.revision)).toEqual(['instance', 'operation', '']);
    },
  );

  specTest(
    'reach every stream carrier the runtime opens, beside the stream credentials',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'stream-headers-use-each-transports-declared-carrier' },
    async () => {
      const upstream = provider();
      const client = bound(upstream.url);

      // SSE: an ordinary request header beside the forwarded user.
      expect(await asUser('first-user', () => collect(client.watchSse()))).toEqual({ values: [{ value: 'instance' }] });
      expect(upstream.seen.at(-1)).toMatchObject({
        path: '/watch-sse',
        revision: 'instance',
        authorization: 'Bearer first-user',
      });

      // First-party WebSocket: the handshake carries no application header;
      // the init frame carries the default apart from the credentials.
      expect(await asUser('first-user', () => collect(client.watchSocket()))).toEqual({
        values: [{ value: 'socket' }],
      });
      expect(upstream.seen.at(-1)).toMatchObject({ path: '/watch-socket', revision: null, authorization: null });
      const init = upstream.inits.at(-1) as {
        headers: { name: string; values: string[] }[];
        credentials: { profile: string; value: string }[];
      };
      expect(init.headers).toEqual([{ name: 'x-revision', values: ['instance'] }]);
      expect(init.credentials).toEqual([{ profile: 'user', value: 'Bearer first-user' }]);

      // Provider-owned WebSocket: the upgrade is the admission, so it carries
      // the default and the credential as request headers.
      const tunnel = await asUser('second-user', () => client.tunnel());
      await tunnel.close();
      expect(upstream.seen.at(-1)).toMatchObject({
        path: '/tunnel',
        revision: 'instance',
        authorization: 'Bearer second-user',
      });
    },
  );

  specTest(
    'accept, canonicalize and refuse exactly the maps of the shared corpus',
    { feature: FEATURE, requirement: REQUIREMENT, check: 'binding-headers-match-the-shared-corpus' },
    () => {
      const corpus = JSON.parse(readFileSync(CORPUS, 'utf8')) as {
        reservedNames: string[];
        reservedPrefixes: string[];
        accepted: { name: string; headers: Record<string, string>; canonical: Record<string, string> }[];
        refused: { name: string; headers: Record<string, string>; declaredCredentialHeaders?: string[] }[];
      };
      expect(corpus.accepted.length * corpus.refused.length * corpus.reservedNames.length).toBeGreaterThan(0);
      // This runtime reserves exactly the shared set, no more and no less.
      expect([...RESERVED_BINDING_HEADERS].sort()).toEqual(
        corpus.reservedNames.map((name) => name.toLowerCase()).sort(),
      );
      expect([...RESERVED_BINDING_HEADER_PREFIXES].sort()).toEqual(
        corpus.reservedPrefixes.map((prefix) => prefix.toLowerCase()).sort(),
      );

      const bind = (headers: Record<string, string>, credentialHeaders: readonly string[] = []) => {
        const profiles = Object.fromEntries(
          credentialHeaders.map((header, index) => [`key-${index}`, { kind: 'api-key' as const, header }]),
        );
        const client = bindServiceClient(RevisionsClient, descriptor(profiles), {
          url: 'https://service.example',
          clientId: 'consumer',
          headers,
        });
        client.dispose();
      };
      for (const accepted of corpus.accepted) {
        expect({ name: accepted.name, snapshot: { ...snapshotBindingHeaders(accepted.headers) } }).toEqual({
          name: accepted.name,
          snapshot: accepted.canonical,
        });
        bind(accepted.headers);
      }
      const refuse = (name: string, headers: Record<string, string>, credentialHeaders?: readonly string[]) => {
        let failure: unknown;
        try {
          bind(headers, credentialHeaders);
        } catch (error) {
          failure = error;
        }
        expect({ name, refused: failure instanceof ClientServiceConfigError }).toEqual({ name, refused: true });
        expect(String((failure as Error).message)).not.toContain('private');
      };
      for (const refused of corpus.refused) refuse(refused.name, refused.headers, refused.declaredCredentialHeaders);
      for (const reserved of corpus.reservedNames) {
        for (const name of [reserved, reserved.toLowerCase(), reserved.toUpperCase()]) {
          refuse(name, { [name]: 'private-value' });
        }
      }
      for (const prefix of corpus.reservedPrefixes) {
        for (const name of [`${prefix}Hint`, `${prefix.toLowerCase()}hint`, `${prefix.toUpperCase()}HINT`]) {
          refuse(name, { [name]: 'private-value' });
        }
      }
    },
  );
});
