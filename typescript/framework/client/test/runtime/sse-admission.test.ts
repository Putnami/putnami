import { describe, expect, test } from 'bun:test';
import type { ClientContractOperation, ClientSchema } from '@putnami/application';
import { SseTransport } from '../../src/runtime/sse-transport';
import { StreamSession } from '../../src/runtime/stream-session';
import type { StreamObserver } from '../../src/runtime/stream.type';
import type { ClientRequest } from '../../src/runtime/transport.type';

const output = {
  type: 'object',
  properties: { value: { type: 'string' } },
  required: ['value'],
  additionalProperties: false,
} as const satisfies ClientSchema;

/**
 * The declared bounds these fixtures use. They belong to the session, not to
 * the transport: one place owns admission, the terminal, the budgets and the
 * caller's cancellation, whichever transport carries the stream.
 */
function session<T>(overrides: { idleMs?: number; callerSignal?: AbortSignal } = {}): StreamSession<T> {
  return new StreamSession<T>({
    serviceId: 'items',
    operationId: 'watch',
    protocol: 'sse',
    budgets: {
      handshakeMs: 2000,
      idleMs: overrides.idleMs ?? 500,
      sessionMs: 0,
      heartbeatMs: 0,
      maxFrameBytes: 4096,
      maxBufferedMessages: 8,
    },
    ...(overrides.callerSignal ? { callerSignal: overrides.callerSignal } : {}),
  });
}

describe('SSE admission, segmentation and liveness', () => {
  test('a non-2xx handshake reaches the caller before any message', async () => {
    for (const status of [401, 403]) {
      const server = Bun.serve({
        port: 0,
        fetch: () => Response.json({ status, code: 'denied' }, { status }),
      });
      try {
        const messages: unknown[] = [];
        const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream<{ value: string }>(
          request(),
          { output },
          session(),
        );
        stream.onMessage((value) => messages.push(value));
        await expect(drain(stream)).rejects.toMatchObject({ status });
        // Admission failed, so nothing the provider could have streamed is
        // observable: the caller never saw a partial stream.
        expect(messages).toEqual([]);
      } finally {
        server.stop(true);
      }
    }
  });

  test('parses events across arbitrary chunk and rune boundaries', async () => {
    // Two CRLF-terminated events with multi-byte runes, written one byte at a
    // time: every rune, every CRLF pair and every event boundary is split.
    const body = new TextEncoder().encode('data: {"value":"héllo→"}\r\n\r\ndata: {"value":"日本語"}\r\n\r\n');
    const server = Bun.serve({
      port: 0,
      fetch: () =>
        new Response(
          new ReadableStream({
            async pull(controller) {
              for (const byte of body) controller.enqueue(new Uint8Array([byte]));
              controller.close();
            },
          }),
          { headers: { 'Content-Type': 'text/event-stream' } },
        ),
    });
    try {
      const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream<{ value: string }>(
        request(),
        { output },
        session(),
      );
      await expect(collect(stream)).resolves.toEqual([{ value: 'héllo→' }, { value: '日本語' }]);
    } finally {
      server.stop(true);
    }
  });

  test('a heartbeat comment keeps an otherwise idle stream alive', async () => {
    const idleTimeoutMs = 90;
    const server = Bun.serve({
      port: 0,
      fetch: () =>
        new Response(
          new ReadableStream({
            async start(controller) {
              const encoder = new TextEncoder();
              for (let index = 0; index < 6; index++) {
                await Bun.sleep(20);
                // A comment carries no data: only the idle budget observes it.
                controller.enqueue(encoder.encode(': heartbeat\n\n'));
              }
              controller.enqueue(encoder.encode('data: {"value":"late"}\n\n'));
              controller.close();
            },
          }),
          { headers: { 'Content-Type': 'text/event-stream' } },
        ),
    });
    try {
      const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream<{ value: string }>(
        request(),
        { output },
        session({ idleMs: idleTimeoutMs }),
      );
      // The quiet period is longer than the idle budget; only the heartbeats
      // keep the stream from being declared dead.
      await expect(collect(stream)).resolves.toEqual([{ value: 'late' }]);
    } finally {
      server.stop(true);
    }
  });

  test('a caller cancellation ends the stream as a typed cancellation', async () => {
    const server = Bun.serve({
      port: 0,
      fetch: () =>
        new Response(
          new ReadableStream({
            async start(controller) {
              const encoder = new TextEncoder();
              controller.enqueue(encoder.encode('data: {"value":"one"}\n\n'));
              // The provider never ends the stream: only the caller can.
              await new Promise(() => {});
            },
          }),
          { headers: { 'Content-Type': 'text/event-stream' } },
        ),
    });
    try {
      const abort = new AbortController();
      const stream = new SseTransport(`http://localhost:${server.port}`, 'items').stream<{ value: string }>(
        { ...request(), signal: abort.signal },
        { output },
        session({ callerSignal: abort.signal }),
      );
      const first = await new Promise<{ value: string }>((resolve) => stream.onMessage(resolve));
      expect(first).toEqual({ value: 'one' });
      abort.abort();
      await expect(drain(stream)).rejects.toMatchObject({ code: 'client.canceled' });
    } finally {
      server.stop(true);
    }
  });
});

function request(operation = operationContract()): ClientRequest {
  return {
    method: 'GET',
    path: '/watch',
    headers: new Headers(),
    operationId: 'watch',
    clientOperation: operation,
  };
}

function operationContract(errors: ClientContractOperation['errors'] = []): ClientContractOperation {
  return {
    stream: 'server',
    messages: { output },
    transports: [{ protocol: 'sse', path: '/watch', encoding: 'json' }],
    security: { alternatives: [{ allOf: [] }] },
    errors,
    idempotency: { kind: 'safe' },
  };
}

function collect<T>(stream: StreamObserver<T>): Promise<T[]> {
  return new Promise((resolve, reject) => {
    const values: T[] = [];
    stream.onMessage((value) => values.push(value));
    stream.onError(reject);
    stream.onComplete(() => resolve(values));
  });
}

/** Resolve on the terminal, whatever it is, without collecting messages. */
function drain<T>(stream: StreamObserver<T>): Promise<void> {
  return new Promise((resolve, reject) => {
    stream.onError(reject);
    stream.onComplete(() => resolve());
  });
}
