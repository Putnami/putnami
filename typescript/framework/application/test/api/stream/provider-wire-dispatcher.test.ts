import { afterEach, describe, expect, test } from 'bun:test';
import { connect } from 'node:net';
import { specTest } from '@putnami/runtime/spectest';
import { NotFoundException, resetConfigLoader, Stream } from '@putnami/runtime';
import { api, type ApiPlugin } from '../../../src/api/api.plugin';
import { ByteStream, endpoint } from '../../../src/api/route';
import { application } from '../../../src/application';
import { http, type HttpPlugin } from '../../../src/http/http.plugin';
import { openapi } from '../../../src/openapi/openapi.plugin';
import { authenticate } from '../../../src/security/identity-resolver.middleware';
import { apiKeyStrategy } from '../../../src/security/strategies/api-key.strategy';

const API_KEY = 'gateway-secret';
const CONTRACT = {
  service: { id: 'gateway', audience: 'api://gateway' },
  credentials: { 'gateway-key': { kind: 'api-key' as const, header: 'X-Gateway-Key' } },
  defaults: { resilience: { stream: { maxFrameBytes: 64 } } },
};
const SECURED = { security: { alternatives: [{ allOf: [{ profile: 'gateway-key' }] }] } } as const;

const EventIn = { type: String, topic: String };
const EventOut = { type: String, data: String };

const running: (() => Promise<void>)[] = [];

afterEach(async () => {
  for (const stop of running.splice(0)) await stop();
  resetConfigLoader();
});

/** The two provider-owned wires a db-gateway and an event-server declare. */
function registerGateway(plugin: ApiPlugin): void {
  plugin.register(
    '/v1/databases/connect',
    endpoint()
      .query({ database: String })
      .body(ByteStream())
      .returns(ByteStream())
      .secure({ principalKind: 'apikey' })
      .client(SECURED)
      .handle(async (context) => {
        context.send(new TextEncoder().encode(`db=${context.queryParams().database}`));
        for await (const chunk of context.messages()) context.send(chunk);
      }),
    'GET',
  );
  plugin.register(
    '/events/ws',
    endpoint()
      .body(Stream(EventIn))
      .returns(Stream(EventOut))
      .subprotocol('putnami.events.v1')
      .secure({ principalKind: 'apikey' })
      .client(SECURED)
      .handle(async (context) => {
        for await (const frame of context.messages()) {
          if (frame.type === 'fail') throw new NotFoundException('topic is unknown');
          if (frame.type === 'done') return;
          context.send({ type: 'event', data: frame.topic });
        }
      }),
    'GET',
  );
}

async function startGateway(): Promise<{ port: number; document: () => Record<string, unknown> | undefined }> {
  const providerHttp: HttpPlugin = http({ port: 0 });
  providerHttp.prepend(authenticate({ anyOf: [apiKeyStrategy({ keys: [API_KEY], header: 'X-Gateway-Key' })] }));
  const providerApi = api({ autoScan: false, client: CONTRACT });
  registerGateway(providerApi);
  const providerOpenApi = openapi({ title: 'Gateway', version: '1.0.0' });
  const app = application().use(providerHttp).use(providerApi).use(providerOpenApi);
  await app.start();
  running.push(async () => {
    await app.stop();
  });
  return {
    port: providerHttp.getServer()?.port as number,
    document: () => providerOpenApi.spec() as Record<string, unknown> | undefined,
  };
}

/** The status line a hand-made upgrade request is answered with. */
function upgradeStatus(port: number, path: string, headers: Record<string, string>): Promise<number> {
  return new Promise((resolve, reject) => {
    const socket = connect(port, '127.0.0.1');
    let received = '';
    socket.on('data', (chunk) => {
      received += chunk.toString('latin1');
      const line = received.split('\r\n')[0] ?? '';
      if (line.includes(' ')) {
        socket.destroy();
        resolve(Number(line.split(' ')[1]));
      }
    });
    socket.on('error', reject);
    const lines = [
      `GET ${path} HTTP/1.1`,
      `Host: 127.0.0.1:${port}`,
      'Upgrade: websocket',
      'Connection: Upgrade',
      'Sec-WebSocket-Version: 13',
      'Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==',
      ...Object.entries(headers).map(([name, value]) => `${name}: ${value}`),
    ];
    socket.write(`${lines.join('\r\n')}\r\n\r\n`);
  });
}

/** One socket driven by the test, opened the way a server consumer opens it. */
class Peer {
  private readonly inbox: (string | Uint8Array)[] = [];
  private readonly waiting: ((message: string | Uint8Array) => void)[] = [];
  closedWith?: { code: number; reason: string };
  private readonly closeWaiters: (() => void)[] = [];

  private constructor(readonly socket: WebSocket) {}

  static async open(url: string, init: { protocols?: string[]; headers?: Record<string, string> }): Promise<Peer> {
    const socket = new WebSocket(url, init as unknown as string[]);
    socket.binaryType = 'arraybuffer';
    const peer = new Peer(socket);
    socket.addEventListener('message', (event) =>
      peer.receive(typeof event.data === 'string' ? event.data : new Uint8Array(event.data as ArrayBuffer)),
    );
    socket.addEventListener('close', (event) => peer.closed(event.code, event.reason));
    await new Promise<void>((resolve, reject) => {
      socket.addEventListener('open', () => resolve(), { once: true });
      socket.addEventListener('close', () => reject(new Error('websocket upgrade was refused')), { once: true });
    });
    return peer;
  }

  async next(): Promise<string | Uint8Array> {
    const buffered = this.inbox.shift();
    if (buffered !== undefined) return buffered;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => reject(new Error('no message arrived within the test budget')), 4000);
      this.waiting.push((message) => {
        clearTimeout(timer);
        resolve(message);
      });
    });
  }

  async closeCode(): Promise<number> {
    if (!this.closedWith) {
      await new Promise<void>((resolve, reject) => {
        const timer = setTimeout(() => reject(new Error('the provider never closed the socket')), 4000);
        this.closeWaiters.push(() => {
          clearTimeout(timer);
          resolve();
        });
      });
    }
    return (this.closedWith as { code: number }).code;
  }

  private receive(message: string | Uint8Array): void {
    const waiter = this.waiting.shift();
    if (waiter) waiter(message);
    else this.inbox.push(message);
  }

  private closed(code: number, reason: string): void {
    this.closedWith = { code, reason };
    for (const waiter of this.closeWaiters.splice(0)) waiter();
  }
}

const KEY = { 'X-Gateway-Key': API_KEY };

describe('provider-owned websocket wires', () => {
  specTest(
    'a byte stream carries raw octets both ways after an upgrade admission',
    {
      feature: 'typescript/api-contracts',
      requirement: 'provider-owned-websocket-wires',
      check: 'a-byte-stream-carries-raw-octets-after-an-upgrade-admission',
    },
    async () => {
      const { port } = await startGateway();
      const path = '/v1/databases/connect';
      expect(await upgradeStatus(port, `${path}?database=main`, {})).toBe(401);
      expect(await upgradeStatus(port, path, KEY)).toBe(400);
      expect(
        await upgradeStatus(port, `${path}?database=main`, { ...KEY, 'Sec-WebSocket-Protocol': 'putnami.service.v1' }),
      ).toBe(400);
      expect(await upgradeStatus(port, `${path}?database=main`, KEY)).toBe(101);

      const peer = await Peer.open(`ws://localhost:${port}${path}?database=main`, { headers: KEY });
      expect(peer.socket.protocol).toBe('');
      expect(new TextDecoder().decode((await peer.next()) as Uint8Array)).toBe('db=main');
      const octets = new Uint8Array([0x00, 0xff, 0xfe, 0x80, 0x22, 0x5c, 0x0a]);
      peer.socket.send(octets);
      expect([...((await peer.next()) as Uint8Array)]).toEqual([...octets]);
      // A message at the declared 64-octet frame budget travels; one past it
      // ends the stream with message-too-big.
      peer.socket.send(new Uint8Array(64).fill(7));
      expect(((await peer.next()) as Uint8Array).byteLength).toBe(64);
      peer.socket.close(1000);
      expect(await peer.closeCode()).toBe(1000);

      const oversized = await Peer.open(`ws://localhost:${port}${path}?database=main`, { headers: KEY });
      await oversized.next();
      oversized.socket.send(new Uint8Array(65));
      expect(await oversized.closeCode()).toBe(1009);

      const text = await Peer.open(`ws://localhost:${port}${path}?database=main`, { headers: KEY });
      await text.next();
      text.socket.send('{"type":"not-bytes"}');
      expect(await text.closeCode()).toBe(1003);
    },
  );

  specTest(
    'a typed provider wire carries JSON frames under its declared subprotocol',
    {
      feature: 'typescript/api-contracts',
      requirement: 'provider-owned-websocket-wires',
      check: 'a-typed-provider-wire-carries-json-frames-under-its-declared-subprotocol',
    },
    async () => {
      const { port } = await startGateway();
      const path = '/events/ws';
      expect(await upgradeStatus(port, path, KEY)).toBe(400);
      expect(await upgradeStatus(port, path, { ...KEY, 'Sec-WebSocket-Protocol': 'putnami.service.v1' })).toBe(400);
      expect(await upgradeStatus(port, path, { 'Sec-WebSocket-Protocol': 'putnami.events.v1' })).toBe(401);

      const peer = await Peer.open(`ws://localhost:${port}${path}`, { protocols: ['putnami.events.v1'], headers: KEY });
      expect(peer.socket.protocol).toBe('putnami.events.v1');
      peer.socket.send(JSON.stringify({ type: 'subscribe', topic: 'orders' }));
      expect(await peer.next()).toBe('{"type":"event","data":"orders"}');
      peer.socket.send(JSON.stringify({ type: 'done', topic: '' }));
      expect(await peer.closeCode()).toBe(1000);

      const binary = await Peer.open(`ws://localhost:${port}${path}`, {
        protocols: ['putnami.events.v1'],
        headers: KEY,
      });
      binary.socket.send(new Uint8Array([1]));
      expect(await binary.closeCode()).toBe(1003);

      const invalid = await Peer.open(`ws://localhost:${port}${path}`, {
        protocols: ['putnami.events.v1'],
        headers: KEY,
      });
      invalid.socket.send('{"type":1}');
      expect(await invalid.closeCode()).toBe(1007);

      const failing = await Peer.open(`ws://localhost:${port}${path}`, {
        protocols: ['putnami.events.v1'],
        headers: KEY,
      });
      failing.socket.send(JSON.stringify({ type: 'fail', topic: '' }));
      expect(await failing.closeCode()).toBe(1008);
    },
  );

  specTest(
    'publishes the provider-owned wire as the only transport of its operation',
    {
      feature: 'typescript/api-contracts',
      requirement: 'provider-owned-websocket-wires',
      check: 'the-contract-publishes-the-provider-owned-wire-as-the-only-transport',
    },
    async () => {
      const { document } = await startGateway();
      const paths = document()?.['paths'] as Record<string, { get: Record<string, unknown> }>;
      const connectOperation = paths['/v1/databases/connect']?.get as Record<string, unknown>;
      const connectContract = connectOperation['x-putnami-client'] as Record<string, unknown>;
      expect(connectContract['stream']).toBe('bidirectional');
      expect(connectContract['messages']).toBeUndefined();
      expect(connectContract['transports']).toEqual([
        {
          protocol: 'websocket',
          path: '/v1/databases/connect',
          encoding: 'binary',
          websocket: { resume: false, wire: 'provider' },
        },
      ]);
      expect(String(connectOperation['description'])).toContain('Raw octets in binary messages');
      const events = paths['/events/ws'] as { get: Record<string, unknown> };
      const eventsContract = events.get['x-putnami-client'] as Record<string, unknown>;
      expect(eventsContract['transports']).toEqual([
        {
          protocol: 'websocket',
          path: '/events/ws',
          encoding: 'json',
          websocket: { subprotocol: 'putnami.events.v1', resume: false, wire: 'provider' },
        },
      ]);
      expect(Object.keys(eventsContract['messages'] as object).sort()).toEqual(['input', 'output']);
    },
  );

  specTest(
    'refuses a provider-owned wire that cannot be published at registration',
    {
      feature: 'typescript/api-contracts',
      requirement: 'provider-owned-websocket-wires',
      check: 'a-provider-owned-wire-that-cannot-be-published-is-refused-at-registration',
    },
    () => {
      const refusals: [string, () => unknown][] = [
        [
          'a subprotocol on a unary endpoint',
          () =>
            endpoint()
              .returns(EventOut)
              .subprotocol('acme.v1')
              .handle(async () => undefined),
        ],
        [
          'a subprotocol on a server stream',
          () =>
            endpoint()
              .returns(Stream(EventOut))
              .subprotocol('acme.v1')
              .handle(async () => undefined),
        ],
        [
          'a malformed token',
          () =>
            endpoint()
              .body(Stream(EventIn))
              .returns(Stream(EventOut))
              .subprotocol('acme v1')
              .handle(async () => undefined),
        ],
        [
          'a first-party token',
          () =>
            endpoint()
              .body(Stream(EventIn))
              .returns(Stream(EventOut))
              .subprotocol('putnami.service.v2')
              .handle(async () => undefined),
        ],
        [
          'octets one way only',
          () =>
            endpoint()
              .body(ByteStream())
              .returns(Stream(EventOut))
              .handle(async () => undefined),
        ],
        [
          'a declared resume',
          () =>
            endpoint()
              .body(ByteStream())
              .returns(ByteStream())
              .client({ resume: true })
              .handle(async () => undefined),
        ],
      ];
      for (const [name, register] of refusals) {
        expect(register, name).toThrow();
      }
      const definition = endpoint()
        .body(ByteStream())
        .returns(ByteStream())
        .subprotocol('pg.tunnel.v1')
        .handle(async () => undefined);
      expect(definition.wire).toEqual({ bytes: true, subprotocol: 'pg.tunnel.v1' });
    },
  );
});

test('a stopping instance ends only its own provider-owned sockets when instances share a loaded route folder', async () => {
  // A scanned route folder is loaded once per process and merged into every
  // application instance that scans it; the loaded plugin is never started
  // nor stopped, so each socket follows the drain of the instance serving it.
  const loaded = api({ autoScan: false });
  loaded.register(
    '/events/ws',
    endpoint()
      .body(Stream(EventIn))
      .returns(Stream(EventOut))
      .subprotocol('putnami.events.v1')
      .handle(async (context) => {
        for await (const frame of context.messages()) context.send({ type: 'event', data: frame.topic });
      }),
    'GET',
  );
  const instances = await Promise.all(
    [1, 2].map(async () => {
      const providerHttp: HttpPlugin = http({ port: 0 });
      const app = application()
        .use(providerHttp)
        .use(api({ autoScan: false, preloadedModule: { loaded } }));
      await app.start();
      return { app, port: providerHttp.getServer()?.port };
    }),
  );
  const open = async (port: number | undefined) => {
    const peer = await Peer.open(`ws://localhost:${port}/events/ws`, { protocols: ['putnami.events.v1'] });
    peer.socket.send(JSON.stringify({ type: 'subscribe', topic: 'orders' }));
    expect(await peer.next()).toBe('{"type":"event","data":"orders"}');
    return peer;
  };
  const [onFirst, onSecond] = await Promise.all([open(instances[0]?.port), open(instances[1]?.port)]);
  try {
    await instances[0]?.app.stop();
    expect(await onFirst.closeCode()).toBe(1001);
    await Bun.sleep(100);
    expect(onSecond.closedWith).toBeUndefined();
  } finally {
    await instances[1]?.app.stop();
  }
  expect(await onSecond.closeCode()).toBe(1001);
});

test('a provider-owned wire keeps the first-party conversation untouched', async () => {
  const providerHttp: HttpPlugin = http({ port: 0 });
  const providerApi = api({ autoScan: false, client: { service: CONTRACT.service, credentials: {} } });
  providerApi.register(
    '/widgets/chat',
    endpoint()
      .body(Stream(EventIn))
      .returns(Stream(EventOut))
      .client({ security: { alternatives: [{ allOf: [] }] } })
      .handle(async () => ({ type: 'done', data: '' })),
    'GET',
  );
  const providerOpenApi = openapi({ title: 'Widgets', version: '1.0.0' });
  const app = application().use(providerHttp).use(providerApi).use(providerOpenApi);
  await app.start();
  running.push(async () => {
    await app.stop();
  });
  const paths = providerOpenApi.spec()?.['paths'] as Record<string, { get: Record<string, unknown> }>;
  const chat = paths['/widgets/chat'] as { get: Record<string, unknown> };
  expect((chat.get['x-putnami-client'] as Record<string, unknown>)['transports']).toEqual([
    {
      protocol: 'websocket',
      path: '/widgets/chat',
      encoding: 'json',
      websocket: { subprotocol: 'putnami.service.v1', resume: false },
    },
  ]);
});
