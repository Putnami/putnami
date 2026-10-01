import { describe, expect, it } from 'bun:test';
import { Uuid } from '@putnami/runtime';
import { handler } from '../../src/handler/handler';
import { assertValidPayload, createTransportMessage, invokeWithTimeout } from '../../src/transport';
import { buildEnvelope } from '../../src/transport/transport';
import { topic } from '../../src/topic/topic';

const TestTopic = topic('transport.timeout', { id: Uuid });

describe('transport message utilities', () => {
  it('aborts the message signal when handler timeout expires', async () => {
    const def = handler(TestTopic)
      .options({ timeout: 5 })
      .handle(async () => {});
    const { message, abortController } = createTransportMessage(
      buildEnvelope(TestTopic, { id: crypto.randomUUID() }),
      def.options,
    );
    let sideEffect = false;

    await expect(
      invokeWithTimeout(
        async (msg) => {
          await new Promise<void>((resolve, reject) => {
            const timer = setTimeout(() => {
              sideEffect = true;
              resolve();
            }, 50);
            msg.signal.addEventListener(
              'abort',
              () => {
                clearTimeout(timer);
                reject(msg.signal.reason);
              },
              { once: true },
            );
          });
        },
        message,
        def.options.timeout,
        abortController,
      ),
    ).rejects.toThrow('Handler timed out after 5ms');

    await new Promise((resolve) => setTimeout(resolve, 60));
    expect(message.signal.aborted).toBe(true);
    expect(sideEffect).toBe(false);
  });
});

describe('assertValidPayload', () => {
  const PayloadTopic = topic('transport.validate', { id: Uuid, value: String });
  const def = handler(PayloadTopic)
    .options({ timeout: 5000 })
    .handle(async () => {});

  const messageFor = (payload: unknown) =>
    createTransportMessage(buildEnvelope(PayloadTopic, payload), def.options).message;

  it('accepts a payload that matches the topic schema', () => {
    const message = messageFor({ id: crypto.randomUUID(), value: 'ok' });
    expect(() => assertValidPayload(message, PayloadTopic)).not.toThrow();
  });

  it('throws for a payload that violates the topic schema', () => {
    // Every distributed transport (pubsub/redis/local-server) runs this before
    // invoking the handler, so producer/attacker-controlled payloads cannot reach
    // a handler unvalidated.
    const message = messageFor({ id: 'not-a-uuid', value: 123 });
    expect(() => assertValidPayload(message, PayloadTopic)).toThrow(/Invalid payload for topic 'transport\.validate'/);
  });
});
