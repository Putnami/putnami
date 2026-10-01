import { afterEach, describe, expect } from 'bun:test';
import type { ServerWebSocket } from 'bun';
import type { ClientContractOperation, ClientResiliencePolicy } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import { BaseClient } from '../../src/runtime/base-client';
import {
  ClientDeadlineError,
  ClientResponseContractError,
  ClientTransportUnavailableError,
} from '../../src/runtime/errors';
import type { ByteStream, FrameStream } from '../../src/runtime/provider-ws-transport';

const stops: (() => void)[] = [];
const originalWebSocket = globalThis.WebSocket;

afterEach(() => {
  for (const stop of stops.splice(0)) stop();
  globalThis.WebSocket = originalWebSocket;
});

const BYTES: ClientContractOperation = {
  stream: 'bidirectional',
  transports: [
    { protocol: 'websocket', path: '/tunnel', encoding: 'binary', websocket: { resume: false, wire: 'provider' } },
  ],
  security: { alternatives: [{ allOf: [] }] },
  errors: [],
  idempotency: { kind: 'non-idempotent' },
};

const FRAMES: ClientContractOperation = {
  stream: 'bidirectional',
  messages: {
    input: {
      type: 'object',
      properties: { type: { type: 'string' } },
      required: ['type'],
      additionalProperties: false,
    },
    output: {
      type: 'object',
      properties: { type: { type: 'string' } },
      required: ['type'],
      additionalProperties: false,
    },
  },
  transports: [
    {
      protocol: 'websocket',
      path: '/events',
      encoding: 'json',
      websocket: { subprotocol: 'acme.events.v1', resume: false, wire: 'provider' },
    },
  ],
  security: { alternatives: [{ allOf: [] }] },
  errors: [],
  idempotency: { kind: 'non-idempotent' },
};

class TunnelClient extends BaseClient {
  readonly serviceName = 'gateway';

  tunnel(): Promise<ByteStream> {
    return this.serviceByteStream('GET', '/tunnel', { operationId: 'openTunnel' });
  }

  events(): FrameStream<{ type: string }, { type: string }> {
    return this.serviceFrameStream('GET', '/events', { operationId: 'openEvents' });
  }
}

interface ScriptedProvider {
  readonly baseUrl: string;
  readonly closes: { code: number; reason: string }[];
  readonly upgrades: Headers[];
}

/** A scripted provider: `open` runs once per admitted socket. */
function scriptedProvider(open: (ws: ServerWebSocket<unknown>) => void): ScriptedProvider {
  const closes: { code: number; reason: string }[] = [];
  const upgrades: Headers[] = [];
  const server = Bun.serve({
    port: 0,
    fetch(request, bun) {
      upgrades.push(request.headers);
      const offered = request.headers.get('sec-websocket-protocol');
      const headers = offered ? { 'Sec-WebSocket-Protocol': offered } : undefined;
      if (bun.upgrade(request, { data: {}, ...(headers ? { headers } : {}) })) return undefined;
      return new Response('refused', { status: 400 });
    },
    websocket: {
      open,
      message() {},
      close(_ws, code, reason) {
        closes.push({ code, reason });
      },
    },
  });
  stops.push(() => server.stop(true));
  return { baseUrl: `http://localhost:${server.port}`, closes, upgrades };
}

function client(baseUrl: string, defaults?: ClientResiliencePolicy): TunnelClient {
  return new TunnelClient({
    baseUrl,
    transport: 'http',
    serviceId: 'gateway',
    operationContracts: { openTunnel: BYTES, openEvents: FRAMES },
    ...(defaults ? { clientDefaults: defaults } : {}),
  });
}

async function waitFor(condition: () => boolean): Promise<void> {
  const deadline = Date.now() + 4000;
  while (!condition()) {
    if (Date.now() > deadline) throw new Error('condition never became true');
    await Bun.sleep(10);
  }
}

describe('provider-owned websocket wires in the TypeScript runtime', () => {
  specTest(
    'refuses a wire this runtime cannot carry before anything is dialed',
    {
      feature: 'typescript/service-clients',
      requirement: 'provider-owned-websocket-wires',
      check: 'a-provider-owned-wire-this-runtime-cannot-carry-is-refused-before-dialing',
    },
    async () => {
      const provider = scriptedProvider(() => undefined);
      // A WHATWG WebSocket takes a URL and a protocol list, nothing else: the
      // admission headers of a provider-owned wire cannot travel on it.
      let dialed = 0;
      globalThis.WebSocket = class {
        constructor(_url: string, protocols?: unknown) {
          dialed += 1;
          if (typeof protocols === 'object' && protocols !== null && !Array.isArray(protocols)) {
            throw new SyntaxError('invalid subprotocol');
          }
        }
      } as unknown as typeof WebSocket;
      await expect(client(provider.baseUrl).tunnel()).rejects.toBeInstanceOf(ClientTransportUnavailableError);
      expect(provider.upgrades).toHaveLength(0);
      globalThis.WebSocket = originalWebSocket;

      // An operation that is not a byte stream is refused by name, before a dial.
      const refused = new TunnelClient({
        baseUrl: provider.baseUrl,
        transport: 'http',
        serviceId: 'gateway',
        operationContracts: { openTunnel: FRAMES, openEvents: BYTES },
      });
      await expect(refused.tunnel()).rejects.toBeInstanceOf(ClientTransportUnavailableError);
      const failure = await new Promise<Error>((resolve) => refused.events().onError(resolve));
      expect(failure).toBeInstanceOf(ClientTransportUnavailableError);
      expect(provider.upgrades).toHaveLength(0);
      expect(dialed).toBe(1);
    },
  );

  specTest(
    'ends a provider-owned wire on its close code and its declared budgets',
    {
      feature: 'typescript/service-clients',
      requirement: 'provider-owned-websocket-wires',
      check: 'a-provider-owned-wire-ends-on-its-close-code-and-its-budgets',
    },
    async () => {
      const failing = scriptedProvider((ws) => {
        ws.sendBinary(new Uint8Array([1, 2]));
        setTimeout(() => ws.close(1011, 'provider failed'), 20);
      });
      const stream = await client(failing.baseUrl).tunnel();
      expect(failing.upgrades[0]?.get('sec-websocket-protocol')).toBeNull();
      const reader = stream.readable.getReader();
      expect([...((await reader.read()).value as Uint8Array)]).toEqual([1, 2]);
      await expect(reader.read()).rejects.toThrow(/code 1011/);
      await expect(stream.closed).rejects.toBeInstanceOf(ClientResponseContractError);

      const wrongKind = scriptedProvider((ws) => ws.sendBinary(new Uint8Array([1])));
      const frames = client(wrongKind.baseUrl).events();
      const wrongKindError = await new Promise<Error>((resolve) => frames.onError(resolve));
      expect(wrongKindError).toBeInstanceOf(ClientResponseContractError);
      expect(wrongKind.upgrades[0]?.get('sec-websocket-protocol')).toBe('acme.events.v1');
      await waitFor(() => wrongKind.closes.length > 0);
      expect(wrongKind.closes[0]?.code).toBe(1003);

      const invalid = scriptedProvider((ws) => ws.sendText('{"unexpected":true}'));
      const invalidError = await new Promise<Error>((resolve) => client(invalid.baseUrl).events().onError(resolve));
      expect(invalidError).toBeInstanceOf(ClientResponseContractError);
      await waitFor(() => invalid.closes.length > 0);
      expect(invalid.closes[0]?.code).toBe(1007);

      const silent = scriptedProvider(() => undefined);
      const quiet = await client(silent.baseUrl, { stream: { idleTimeoutMs: 100 } }).tunnel();
      await expect(quiet.closed).rejects.toBeInstanceOf(ClientDeadlineError);
      await waitFor(() => silent.closes.length > 0);
      expect(silent.closes[0]?.code).toBe(1001);

      const normal = scriptedProvider((ws) => {
        ws.sendText('{"type":"event"}');
        setTimeout(() => ws.close(1000), 20);
      });
      const received: { type: string }[] = [];
      await new Promise<void>((resolve, reject) => {
        const events = client(normal.baseUrl).events();
        events.onMessage((frame) => received.push(frame));
        events.onError(reject);
        events.onComplete(() => resolve());
      });
      expect(received).toEqual([{ type: 'event' }]);
    },
    20_000,
  );
});
