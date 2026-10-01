import { describe, expect, it } from 'bun:test';
import { endpoint } from '../../../src/api/route/endpoint';
import type { ResponseMeta } from '../../../src/api/route/response-meta';
import { isStreamEndpointDefinition } from '../../../src/api/route/stream-endpoint';
import { ArrayOf, Stream } from '@putnami/runtime';
import type { SecurityOptions } from '../../../src/security/security.types';

describe('EndpointBuilder .returns() and .response()', () => {
  it('should set primary returns schema with .returns(schema)', () => {
    const def = endpoint()
      .returns({ id: String, name: String })
      .handle(() => ({ id: '1', name: 'Alice' }));

    expect(def.schemas?.returns).toEqual({ id: String, name: String });
    expect(def.responses).toBeUndefined();
  });

  it('should support .response(status, schema) for additional status entries', () => {
    const def = endpoint()
      .response(201, { id: String })
      .handle(() => ({ id: '1' }));

    expect(def.responses?.returns).toHaveLength(1);
    expect(def.responses?.returns?.[0]).toEqual({
      status: 201,
      description: undefined,
      schema: { id: String },
    });
  });

  it('should support .response(status, description, schema)', () => {
    const def = endpoint()
      .response(201, 'Created', { id: String })
      .handle(() => ({ id: '1' }));

    expect(def.responses?.returns).toHaveLength(1);
    expect(def.responses?.returns?.[0]).toEqual({
      status: 201,
      description: 'Created',
      schema: { id: String },
    });
  });

  it('should support .response(status, description) without schema', () => {
    const def = endpoint()
      .response(204, 'No content')
      .handle(() => undefined);

    expect(def.responses?.returns).toHaveLength(1);
    expect(def.responses?.returns?.[0]).toEqual({
      status: 204,
      description: 'No content',
      schema: undefined,
    });
  });

  it('should store multiple .response() entries', () => {
    const def = endpoint()
      .response(200, 'List of users', { users: String })
      .response(201, 'Created', { id: String })
      .handle(() => ({ users: '[]' }));

    expect(def.responses?.returns).toHaveLength(2);
    expect(def.responses?.returns?.[0].status).toBe(200);
    expect(def.responses?.returns?.[1].status).toBe(201);
  });

  it('should combine .returns(schema) with .response(status, ...)', () => {
    const def = endpoint()
      .returns({ id: String, name: String })
      .response(201, 'Created', { id: String })
      .handle(() => ({ id: '1', name: 'Alice' }));

    expect(def.schemas?.returns).toEqual({ id: String, name: String });
    expect(def.responses?.returns).toHaveLength(1);
    expect(def.responses?.returns?.[0].status).toBe(201);
  });
});

describe('EndpointBuilder .throws()', () => {
  it('should add a throws entry with status and description', () => {
    const def = endpoint()
      .throws(404, 'Not found')
      .handle(() => ({ ok: true }));

    expect(def.responses?.throws).toHaveLength(1);
    expect(def.responses?.throws?.[0]).toEqual({
      status: 404,
      description: 'Not found',
      schema: undefined,
    });
  });

  it('should add a throws entry with status, description, and schema', () => {
    const def = endpoint()
      .throws(400, 'Validation failed', { message: String, fields: String })
      .handle(() => ({ ok: true }));

    expect(def.responses?.throws).toHaveLength(1);
    expect(def.responses?.throws?.[0]).toEqual({
      status: 400,
      description: 'Validation failed',
      schema: { message: String, fields: String },
    });
  });

  it('should chain multiple .throws() calls', () => {
    const def = endpoint()
      .throws(400, 'Bad request')
      .throws(401, 'Unauthorized')
      .throws(404, 'Not found')
      .handle(() => ({ ok: true }));

    expect(def.responses?.throws).toHaveLength(3);
    expect(def.responses?.throws?.[0].status).toBe(400);
    expect(def.responses?.throws?.[1].status).toBe(401);
    expect(def.responses?.throws?.[2].status).toBe(404);
  });

  it('should combine .response() and .throws()', () => {
    const def = endpoint()
      .response(200, { id: String })
      .response(201, 'Created', { id: String })
      .throws(400, 'Validation failed')
      .throws(404, 'Not found')
      .handle(() => ({ id: '1' }));

    expect(def.responses?.returns).toHaveLength(2);
    expect(def.responses?.throws).toHaveLength(2);
  });

  it('should accept spread of ResponseMeta on .throws(...specs)', () => {
    const authErrors: ResponseMeta[] = [
      { status: 401, description: 'Unauthorized' },
      { status: 403, description: 'Forbidden', schema: { reason: String } },
    ];
    const def = endpoint()
      .throws(...authErrors)
      .throws(404, 'Not found')
      .handle(() => ({ ok: true }));

    expect(def.responses?.throws).toHaveLength(3);
    expect(def.responses?.throws?.[0].status).toBe(401);
    expect(def.responses?.throws?.[1].status).toBe(403);
    expect(def.responses?.throws?.[1].schema).toEqual({ reason: String });
    expect(def.responses?.throws?.[2].status).toBe(404);
  });

  it('should include throws in StreamEndpointDefinition', () => {
    const def = endpoint()
      .returns(Stream({ event: String }))
      .throws(401, 'Unauthorized')
      .handle(async (ctx) => {
        ctx.send({ event: 'hello' });
      });

    expect(isStreamEndpointDefinition(def)).toBe(true);
    if (isStreamEndpointDefinition(def)) {
      expect(def.responses?.throws).toHaveLength(1);
      expect(def.responses?.throws?.[0].status).toBe(401);
      // No multi-status returns for streams
      expect(def.responses?.returns).toBeUndefined();
    }
  });
});

describe('EndpointBuilder .mayThrow()', () => {
  it('preserves an explicit retryability decision beside the stable code', () => {
    const def = endpoint()
      .mayThrowWith('Unavailable', { retryable: true })
      .mayThrowWith('Conflict', { retryable: false })
      .handle(() => ({ ok: true }));

    expect(def.responses?.errorCodes).toEqual(['Unavailable', 'Conflict']);
    expect(def.responses?.errorOptions).toEqual({
      Unavailable: { retryable: true },
      Conflict: { retryable: false },
    });
  });

  it('declares a details schema per code without a retry classification', () => {
    const details = { rejections: ArrayOf({ project: String, error: String }), retryable: Boolean };
    const def = endpoint()
      .mayThrowDetails('Conflict', details)
      .mayThrow('NotFound')
      .handle(() => ({ ok: true }));

    expect(def.responses?.errorCodes).toEqual(['Conflict', 'NotFound']);
    expect(def.responses?.errorDetails).toEqual({ Conflict: details });
    expect(def.responses?.errorOptions).toEqual({});

    const stream = endpoint()
      .returns(Stream({ event: String }))
      .mayThrowDetails('NotFound', { resource: String })
      .handle(async (ctx) => {
        ctx.send({ event: 'hello' });
      });
    expect(isStreamEndpointDefinition(stream)).toBe(true);
    if (isStreamEndpointDefinition(stream)) {
      expect(stream.responses?.errorDetails).toEqual({ NotFound: { resource: String } });
    }
  });

  it('should add framework-known error codes to response metadata', () => {
    const def = endpoint()
      .mayThrow('NotFound', 'Conflict')
      .handle(() => ({ ok: true }));

    expect(def.responses?.errorCodes).toEqual(['NotFound', 'Conflict']);
    expect(def.responses?.throws).toBeUndefined();
  });

  it('should combine .mayThrow() and .throws()', () => {
    const def = endpoint()
      .mayThrow('NotFound')
      .throws(409, 'Conflict', { message: String })
      .handle(() => ({ ok: true }));

    expect(def.responses?.errorCodes).toEqual(['NotFound']);
    expect(def.responses?.throws).toHaveLength(1);
    expect(def.responses?.throws?.[0].status).toBe(409);
  });

  it('should include error codes in StreamEndpointDefinition', () => {
    const def = endpoint()
      .returns(Stream({ event: String }))
      .mayThrow('Unauthorized')
      .handle(async (ctx) => {
        ctx.send({ event: 'hello' });
      });

    expect(isStreamEndpointDefinition(def)).toBe(true);
    if (isStreamEndpointDefinition(def)) {
      expect(def.responses?.errorCodes).toEqual(['Unauthorized']);
      expect(def.responses?.throws).toBeUndefined();
    }
  });
});

describe('EndpointBuilder no responses when unused', () => {
  it('should have no responses field when neither returns(status) nor throws is used', () => {
    const def = endpoint().handle(() => ({ ok: true }));
    expect(def.responses).toBeUndefined();
  });

  it('should have no responses field with bare .returns(schema) only', () => {
    const def = endpoint()
      .returns({ id: String })
      .handle(() => ({ id: '1' }));

    expect(def.responses).toBeUndefined();
  });
});

describe('EndpointBuilder .description()', () => {
  it('should store description in meta', () => {
    const def = endpoint()
      .description('List all users')
      .handle(() => ({ users: [] }));

    expect(def.meta?.description).toBe('List all users');
  });

  it('should include description alongside other meta', () => {
    const def = endpoint()
      .description('Get user by ID')
      .cache({ maxAge: 60 })
      .handle(() => ({ id: '1' }));

    expect(def.meta?.description).toBe('Get user by ID');
    expect(def.meta?.cache).toEqual({ maxAge: 60 });
  });
});

describe('EndpointBuilder meta capture', () => {
  it('should store security options from .secure(options)', () => {
    const opts: SecurityOptions = { roles: ['admin'], scopes: ['write'] };
    const def = endpoint()
      .secure(opts)
      .handle(() => ({ ok: true }));

    expect(def.meta?.security).toEqual(opts);
  });

  it('should store empty security from .secure() with no args', () => {
    const def = endpoint()
      .secure()
      .handle(() => ({ ok: true }));

    expect(def.meta?.security).toEqual({});
  });

  it('should store empty security from .secure(guardFn)', () => {
    const def = endpoint()
      .secure(() => true)
      .handle(() => ({ ok: true }));

    // Guard is opaque — meta.security is {} (auth required, no specific claims)
    expect(def.meta?.security).toEqual({});
  });

  it('should store cache options from .cache()', () => {
    const def = endpoint()
      .cache({ maxAge: 3600, etag: true })
      .handle(() => ({ ok: true }));

    expect(def.meta?.cache).toEqual({ maxAge: 3600, etag: true });
  });

  it('should store cors options from .cors()', () => {
    const def = endpoint()
      .cors({ origin: 'https://example.com', credentials: true })
      .handle(() => ({ ok: true }));

    expect(def.meta?.cors).toEqual({ origin: 'https://example.com', credentials: true });
  });

  it('should store rate limit options from .rateLimit()', () => {
    const def = endpoint()
      .rateLimit({ windowMs: 60_000, max: 100 })
      .handle(() => ({ ok: true }));

    expect(def.meta?.rateLimit).toEqual({ windowMs: 60_000, max: 100 });
  });

  it('should have no meta when no options are used', () => {
    const def = endpoint().handle(() => ({ ok: true }));
    expect(def.meta).toBeUndefined();
  });

  it('should combine all meta options', () => {
    const def = endpoint()
      .description('Admin endpoint')
      .secure({ roles: ['admin'] })
      .cache({ maxAge: 60 })
      .cors({ origin: '*' })
      .rateLimit({ max: 10 })
      .handle(() => ({ ok: true }));

    expect(def.meta?.description).toBe('Admin endpoint');
    expect(def.meta?.security).toEqual({ roles: ['admin'] });
    expect(def.meta?.cache).toEqual({ maxAge: 60 });
    expect(def.meta?.cors).toEqual({ origin: '*' });
    expect(def.meta?.rateLimit).toEqual({ max: 10 });
  });

  it('should pass meta through to StreamEndpointDefinition', () => {
    const def = endpoint()
      .description('Live events stream')
      .secure({ scopes: ['events:read'] })
      .returns(Stream({ event: String }))
      .handle(async (ctx) => {
        ctx.send({ event: 'hello' });
      });

    expect(isStreamEndpointDefinition(def)).toBe(true);
    if (isStreamEndpointDefinition(def)) {
      expect(def.meta?.description).toBe('Live events stream');
      expect(def.meta?.security).toEqual({ scopes: ['events:read'] });
    }
  });
});
