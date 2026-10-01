import { describe, expect, it } from 'bun:test';
import { api } from '../../../src/api/api.plugin';
import { EndpointBuilder, endpoint, isEndpointDefinition } from '../../../src/api/route/endpoint';
import { Optional, Stream, Uuid, isStreamSchema } from '@putnami/runtime';
import { isStreamEndpointDefinition } from '../../../src/api/route/stream-endpoint';
import type { StreamEndpointDefinition } from '../../../src/api/route/stream-endpoint';

// ---------------------------------------------------------------------------
// Stream() combinator
// ---------------------------------------------------------------------------

describe('Stream() combinator', () => {
  it('should wrap a schema definition', () => {
    const s = Stream({ type: String, data: String });
    expect(isStreamSchema(s)).toBe(true);
    expect(s.schema).toEqual({ type: String, data: String });
  });

  it('should not be a schema descriptor', () => {
    const s = Stream({ type: String });
    expect(isStreamSchema(s)).toBe(true);
    expect(isStreamSchema(String)).toBe(false);
    expect(isStreamSchema(null)).toBe(false);
    expect(isStreamSchema({})).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// endpoint() with Stream() — StreamEndpointDefinition
// ---------------------------------------------------------------------------

describe('endpoint() with Stream()', () => {
  describe('server-stream (returns only)', () => {
    it('should produce a StreamEndpointDefinition with mode "server"', () => {
      const def = endpoint()
        .returns(Stream({ event: String, payload: String }))
        .handle(async (ctx) => {
          ctx.send({ event: 'hello', payload: 'world' });
        });
      expect(isStreamEndpointDefinition(def)).toBe(true);
      expect((def as StreamEndpointDefinition).mode).toBe('server');
    });

    it('should not be an EndpointDefinition', () => {
      const def = endpoint()
        .returns(Stream({ event: String }))
        .handle(async (ctx) => {
          ctx.send({ event: 'test' });
        });
      expect(isEndpointDefinition(def)).toBe(false);
    });

    it('should carry schemas', () => {
      const def = endpoint()
        .params({ id: Uuid })
        .query({ token: String })
        .returns(Stream({ event: String }))
        .handle(async () => {}) as StreamEndpointDefinition;
      expect(def.schemas?.params).toBeDefined();
      expect(def.schemas?.query).toBeDefined();
      expect(def.schemas?.returns).toBeDefined();
    });
  });

  describe('client-stream (body only)', () => {
    it('should produce a StreamEndpointDefinition with mode "client"', () => {
      const def = endpoint()
        .body(Stream({ type: String, data: String }))
        .returns({ result: String })
        .handle(async (ctx) => {
          for await (const _msg of ctx.messages()) {
            // consume
          }
          return { result: 'done' };
        });
      expect(isStreamEndpointDefinition(def)).toBe(true);
      expect((def as StreamEndpointDefinition).mode).toBe('client');
    });

    it('should carry body and returns schemas', () => {
      const def = endpoint()
        .body(Stream({ data: String }))
        .returns({ result: String })
        .handle(async () => ({ result: 'ok' })) as StreamEndpointDefinition;
      expect(def.schemas?.body).toBeDefined();
      expect(def.schemas?.returns).toBeDefined();
    });
  });

  describe('bidirectional (both streamed)', () => {
    it('should produce a StreamEndpointDefinition with mode "bidirectional"', () => {
      const def = endpoint()
        .body(Stream({ type: String, data: String }))
        .returns(Stream({ event: String, payload: String }))
        .handle(async (ctx) => {
          ctx.send({ event: 'welcome', payload: 'hello' });
          for await (const msg of ctx.messages()) {
            ctx.send({ event: 'echo', payload: msg.data });
          }
        });
      expect(isStreamEndpointDefinition(def)).toBe(true);
      expect((def as StreamEndpointDefinition).mode).toBe('bidirectional');
    });

    it('should carry all schemas', () => {
      const def = endpoint()
        .params({ roomId: Uuid })
        .query({ token: Optional(String) })
        .body(Stream({ data: String }))
        .returns(Stream({ event: String }))
        .handle(async () => {}) as StreamEndpointDefinition;
      expect(def.schemas?.params).toBeDefined();
      expect(def.schemas?.query).toBeDefined();
      expect(def.schemas?.body).toBeDefined();
      expect(def.schemas?.returns).toBeDefined();
    });
  });

  describe('unary (no Stream())', () => {
    it('should produce a standard EndpointDefinition', () => {
      const def = endpoint()
        .body({ name: String })
        .returns({ id: String })
        .handle(async () => ({ id: '123' }));
      expect(isEndpointDefinition(def)).toBe(true);
      expect(isStreamEndpointDefinition(def)).toBe(false);
    });
  });

  describe('builder type', () => {
    it('should return an EndpointBuilder when called with no arguments', () => {
      const builder = endpoint();
      expect(builder).toBeInstanceOf(EndpointBuilder);
    });

    it('should support simple mode (handler directly)', () => {
      const def = endpoint(() => ({ hello: 'world' }));
      expect(isEndpointDefinition(def)).toBe(true);
    });
  });
});

// ---------------------------------------------------------------------------
// isStreamEndpointDefinition
// ---------------------------------------------------------------------------

describe('isStreamEndpointDefinition', () => {
  it('should detect StreamEndpointDefinition objects', () => {
    const def = endpoint()
      .returns(Stream({ event: String }))
      .handle(async (ctx) => {
        ctx.send({ event: 'test' });
      });
    expect(isStreamEndpointDefinition(def)).toBe(true);
  });

  it('should reject non-StreamEndpointDefinition objects', () => {
    expect(isStreamEndpointDefinition({})).toBe(false);
    expect(isStreamEndpointDefinition(null)).toBe(false);
    expect(isStreamEndpointDefinition(() => {})).toBe(false);
    expect(isStreamEndpointDefinition({ __streamEndpoint: 'wrong' })).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Integration with ApiPlugin
// ---------------------------------------------------------------------------

describe('ApiPlugin stream registration', () => {
  it('should register StreamEndpointDefinition via register()', () => {
    const plugin = api({ autoScan: false });
    const def = endpoint()
      .returns(Stream({ event: String }))
      .handle(async (ctx) => {
        ctx.send({ event: 'test' });
      });

    const result = plugin.register('/events', { default: def });
    expect(result).toBe(plugin);
  });

  it('should register StreamEndpointDefinition directly', () => {
    const plugin = api({ autoScan: false });
    const def = endpoint()
      .body(Stream({ data: String }))
      .returns(Stream({ event: String }))
      .handle(async () => {});

    const result = plugin.register('/ws', def);
    expect(result).toBe(plugin);
  });

  it('should register stream with params', () => {
    const plugin = api({ autoScan: false });
    const def = endpoint()
      .params({ roomId: Uuid })
      .body(Stream({ data: String }))
      .returns(Stream({ event: String }))
      .handle(async () => {}) as StreamEndpointDefinition;

    const result = plugin.register('/chat/[roomId]', def);
    expect(result).toBe(plugin);
  });
});
