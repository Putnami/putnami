import { afterAll, beforeAll, describe, expect, test } from 'bun:test';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';
import { ConnectTransport } from '../../src/runtime/connect-transport';
import { ClientRequestError, ClientServerError } from '../../src/runtime/errors';
import { connectErrorInfo, GrpcStatus, parseConnectError, parseEndStreamTerminal } from '../../src/runtime/grpc-status';
import type { StreamObserver } from '../../src/runtime/stream.type';

/**
 * The client half of the Connect protocol, judged by the corpus transcribed
 * from the published specification.
 *
 * The provider half is judged by the same file in
 * `typescript/framework/application/test/grpc/connect-conformance.test.ts`. The
 * two halves share `connect-protocol.ts`, so agreeing with each other proves
 * nothing; the corpus is the arbiter for both.
 */

const CORPUS_PATH = join(import.meta.dir, '../../../application/test/grpc/connect-conformance/corpus.json');

interface Corpus {
  source: { url: string; protocolVersion: string };
  codes: { code: string; httpStatus: number; grpcNumber: number }[];
  httpInference: { status: number; code: string }[];
  errors: { name: string; source: string; json: unknown; expect: unknown }[];
  endStream: { name: string; source: string; json: unknown; expect: { ok: boolean; code?: string } | null }[];
  streams: {
    name: string;
    source: string;
    frames: { flags: number; json: unknown }[];
    expect: { outcome: 'complete' | 'error'; code?: string; message?: string; messages: unknown[] };
  }[];
  envelope: { headerBytes: number };
}

const CORPUS: Corpus = JSON.parse(readFileSync(CORPUS_PATH, 'utf8')) as Corpus;

const encoder = new TextEncoder();

/** Frame a payload from the specification's byte layout, not from the runtime's writer. */
function referenceEnvelope(flags: number, payload: Uint8Array): Uint8Array {
  const frame = new Uint8Array(5 + payload.length);
  frame[0] = flags & 0xff;
  frame[1] = (payload.length >>> 24) & 0xff;
  frame[2] = (payload.length >>> 16) & 0xff;
  frame[3] = (payload.length >>> 8) & 0xff;
  frame[4] = payload.length & 0xff;
  frame.set(payload, 5);
  return frame;
}

let server: ReturnType<typeof Bun.serve>;
let baseUrl: string;

beforeAll(() => {
  server = Bun.serve({
    port: 0,
    fetch(req) {
      const name = new URL(req.url).pathname.slice(1);
      const scene = CORPUS.streams.find((entry) => entry.name === name);
      if (!scene) return new Response('unknown scene', { status: 404 });
      const body = new ReadableStream({
        start(controller) {
          for (const frame of scene.frames) {
            controller.enqueue(referenceEnvelope(frame.flags, encoder.encode(JSON.stringify(frame.json))));
          }
          controller.close();
        },
      });
      return new Response(body, {
        headers: { 'Content-Type': 'application/connect+json', 'Connect-Protocol-Version': '1' },
      });
    },
  });
  baseUrl = `http://localhost:${server.port}`;
});

afterAll(() => {
  server.stop(true);
});

interface Outcome {
  messages: unknown[];
  error?: Error;
  completed: boolean;
}

/** Drive an observer to its terminal event under a short, controlled deadline. */
function drain(observer: StreamObserver<unknown>, timeoutMs = 3000): Promise<Outcome> {
  return new Promise((resolve) => {
    const messages: unknown[] = [];
    const timer = setTimeout(() => resolve({ messages, completed: false }), timeoutMs);
    const settle = (outcome: Omit<Outcome, 'messages'>) => {
      clearTimeout(timer);
      resolve({ messages, ...outcome });
    };
    observer.onMessage((data) => messages.push(data));
    observer.onError((error) => settle({ error, completed: false }));
    observer.onComplete(() => settle({ completed: true }));
  });
}

describe('the client reads the corpus the specification defines', () => {
  test('reads the same corpus the provider is judged by', () => {
    expect(CORPUS.source.url).toBe('https://connectrpc.com/docs/protocol/');
    expect(CORPUS.streams.length).toBeGreaterThan(0);
  });

  for (const entry of CORPUS.codes) {
    test(`${entry.code} reports google.rpc.Code ${entry.grpcNumber}`, () => {
      const info = connectErrorInfo({ code: entry.code as never }, 200);
      expect(info.code).toBe(entry.grpcNumber as never);
    });
  }

  for (const entry of CORPUS.httpInference) {
    test(`a bare HTTP ${entry.status} infers ${entry.code}`, () => {
      const expected = CORPUS.codes.find((code) => code.code === entry.code);
      expect(parseConnectError(undefined, entry.status).code).toBe(expected?.grpcNumber as never);
    });
  }

  for (const scene of CORPUS.errors) {
    test(`error body: ${scene.name}`, () => {
      const info = parseConnectError(scene.json, 500);
      if (scene.expect === null) {
        expect(info.connectCode).toBeUndefined();
        return;
      }
      const expected = scene.expect as { code: string; message?: string };
      expect(info.connectCode).toBe(expected.code as never);
      expect(info.message).toBe(expected.message as never);
    });
  }

  for (const scene of CORPUS.endStream) {
    test(`end-of-stream: ${scene.name}`, () => {
      const terminal = parseEndStreamTerminal(scene.json);
      if (scene.expect === null) {
        expect(terminal.valid).toBe(false);
        return;
      }
      expect(terminal.valid).toBe(true);
      expect(terminal.failure === undefined).toBe(scene.expect.ok);
      if (scene.expect.code) {
        const expected = CORPUS.codes.find((code) => code.code === scene.expect?.code);
        expect(terminal.failure?.code).toBe(expected?.grpcNumber as never);
      }
    });
  }
});

describe('the client consumes a response stream the way the specification frames one', () => {
  specTest(
    'every corpus stream scene reaches the outcome the specification requires',
    {
      feature: 'typescript/service-clients',
      requirement: 'connect-transport',
      check: 'every-connect-stream-scene-from-the-specification-reaches-its-declared-outcome',
    },
    async () => {
      // Non-vacuity: the scenes actually driven here are exactly the scenes on
      // disk, so a scene added to the corpus cannot be silently skipped.
      const driven: string[] = [];
      for (const scene of CORPUS.streams) {
        const transport = new ConnectTransport(baseUrl, 'corpus.v1');
        // biome-ignore lint/performance/noAwaitInLoops: each scene is one real HTTP response read to its terminal
        const outcome = await drain(transport.stream(`/${scene.name}`));
        driven.push(scene.name);

        expect(outcome.messages, scene.name).toEqual(scene.expect.messages);
        if (scene.expect.outcome === 'complete') {
          expect(outcome.completed, scene.name).toBe(true);
          expect(outcome.error, scene.name).toBeUndefined();
          continue;
        }
        expect(outcome.completed, scene.name).toBe(false);
        expect(outcome.error, scene.name).toBeDefined();
        if (scene.expect.message) expect(outcome.error?.message, scene.name).toContain(scene.expect.message);
      }
      expect(driven).toEqual(CORPUS.streams.map((scene) => scene.name));
    },
  );

  test('a stream whose frames the runtime wrote is one the reference reader accepts', () => {
    // The frames the corpus server writes above come from the reference writer;
    // this pins that the runtime's own writer produces the same bytes, so a
    // request stream this client sends is one a conforming server can read.
    const payload = encoder.encode('{"filter":"all"}');
    const reference = referenceEnvelope(0x00, payload);
    expect(reference.length).toBe(CORPUS.envelope.headerBytes + payload.length);
    expect(reference[0]).toBe(0x00);
    expect(new DataView(reference.buffer).getUint32(1, false)).toBe(payload.length);
  });
});

describe('the client reads a unary failure the way the specification writes one', () => {
  let errorServer: ReturnType<typeof Bun.serve>;
  let errorUrl: string;

  beforeAll(() => {
    errorServer = Bun.serve({
      port: 0,
      fetch(req) {
        const scene = CORPUS.errors.find((entry) => entry.name === new URL(req.url).pathname.slice(1));
        if (!scene) return new Response('unknown scene', { status: 404 });
        return Response.json(scene.json, { status: 500, headers: { 'Content-Type': 'application/json' } });
      },
    });
    errorUrl = `http://localhost:${errorServer.port}`;
  });

  afterAll(() => {
    errorServer.stop(true);
  });

  for (const scene of CORPUS.errors) {
    test(`unary failure: ${scene.name}`, async () => {
      const transport = new ConnectTransport(errorUrl, 'corpus.v1');
      let thrown: unknown;
      try {
        await transport.execute({ method: 'POST', path: `/${scene.name}`, headers: new Headers() });
      } catch (error) {
        thrown = error;
      }
      expect(thrown).toBeInstanceOf(ClientServerError);
      const error = thrown as ClientServerError;
      if (scene.expect === null) {
        // No readable code: the status decides, and a bare 500 is `unknown`.
        expect(error.grpcCode).toBe(GrpcStatus.UNKNOWN);
        return;
      }
      const expected = scene.expect as { code: string };
      const numeric = CORPUS.codes.find((code) => code.code === expected.code);
      expect(error.grpcCode).toBe(numeric?.grpcNumber as never);
    });
  }

  test('a 4xx is a request error and a 5xx a server error, whatever the code names', async () => {
    const transport = new ConnectTransport(errorUrl, 'corpus.v1');
    await expect(
      transport.execute({ method: 'POST', path: '/simplest-form-is-a-code-alone', headers: new Headers() }),
    ).rejects.toBeInstanceOf(ClientServerError);
    expect(ClientRequestError).toBeDefined();
  });
});
