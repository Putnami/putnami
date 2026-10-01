import { describe, expect, it } from 'bun:test';
import { runInContext, Uuid } from '@putnami/runtime';
import { SCOPE_CONTAINER_KEY } from '@putnami/runtime/inject';
import { handler, isHandlerDefinition } from '../../src/handler/handler';
import { DEFAULT_HANDLER_OPTIONS } from '../../src/handler/handler.type';
import { topic } from '../../src/topic/topic';

const TestTopic = topic('test.event', { id: Uuid, value: String });

describe('handler()', () => {
  it('should create a HandlerDefinition with defaults', () => {
    const def = handler(TestTopic).handle(async () => {});

    expect(def.__handler).toBe('putnami:event-handler');
    expect(def.topic).toBe(TestTopic);
    expect(def.options.distribution).toBe('competing');
    expect(def.options.maxRetries).toBe(10);
    expect(def.options.backoff).toBe('exponential');
    expect(def.options.maxBackoff).toBe(60_000);
    expect(def.options.timeout).toBe(30_000);
    expect(def.options.concurrency).toBe(0);
    expect(def.options.queueLimit).toBe(0);
    expect(def.options.overflow).toBe('throw');
    expect(def.options.dlq).toBe(true);
    expect(def.options.ack).toBe('auto');
    expect(def.filter).toBeUndefined();
  });

  it('should allow custom options', () => {
    const def = handler(TestTopic)
      .options({ maxRetries: 3, distribution: 'broadcast', timeout: 5000 })
      .handle(async () => {});

    expect(def.options.distribution).toBe('broadcast');
    expect(def.options.maxRetries).toBe(3);
    expect(def.options.timeout).toBe(5000);
    // Unspecified options keep defaults
    expect(def.options.dlq).toBe(true);
    expect(def.options.ack).toBe('auto');
  });

  it('should support attribute filters', () => {
    const def = handler(TestTopic)
      .filter({ attributes: { region: 'eu' } })
      .handle(async () => {});

    expect(def.filter).toEqual({ attributes: { region: 'eu' } });
  });

  it('should be detected by isHandlerDefinition', () => {
    const def = handler(TestTopic).handle(async () => {});

    expect(isHandlerDefinition(def)).toBe(true);
    expect(isHandlerDefinition({})).toBe(false);
    expect(isHandlerDefinition(null)).toBe(false);
  });

  it('should preserve the handler function', () => {
    const fn = async () => {};
    const def = handler(TestTopic).handle(fn);

    expect(def.handler).toBe(fn);
  });

  it('should match DEFAULT_HANDLER_OPTIONS when no options given', () => {
    const def = handler(TestTopic).handle(async () => {});

    expect(def.options).toEqual(DEFAULT_HANDLER_OPTIONS);
  });

  it('should resolve injected dependencies from the active DI scope', async () => {
    const token = Symbol('test-token');
    const scope = {
      get(value: unknown) {
        expect(value).toBe(token);
        return { service: 'injected-service' };
      },
      list() {
        return [];
      },
    };

    const def = handler(TestTopic)
      .inject({ dependency: token })
      .handle(async ({ dependency }, msg) => {
        expect(dependency).toEqual({ service: 'injected-service' });
        expect(msg.topic).toBe(TestTopic.name);
      });

    await runInContext(
      { [SCOPE_CONTAINER_KEY]: scope },
      async () =>
        await def.handler({
          id: 'message-1',
          topic: TestTopic.name,
          payload: { id: crypto.randomUUID(), value: 'hello' },
          timestamp: new Date(),
          attributes: {},
          attempt: 1,
          signal: new AbortController().signal,
          ack() {},
          nack() {},
        }),
    );

    expect(def.inject).toEqual({ dependency: token });
  });
});
