import { describe, expect, test } from 'bun:test';
import type { ClientContractOperation, ClientSchema } from '@putnami/application';
import { specTest } from '@putnami/runtime/spectest';
import {
  ClientError,
  ClientFrameworkError,
  ClientRequestError,
  ClientResponseContractError,
  ClientRetryExhaustedError,
  ClientServerError,
  ClientTimeoutError,
  decodeFrameworkError,
  isClientFrameworkError,
  readFirstPartyErrorEnvelope,
} from '../../src/runtime/errors';
import { parseJsonValue } from '../../src/runtime/json-codec';

function operation(errors: ClientContractOperation['errors']): ClientContractOperation {
  return {
    stream: 'unary',
    transports: [{ protocol: 'rest-json', path: '/items', encoding: 'json' }],
    security: { alternatives: [{ allOf: [] }] },
    errors,
    idempotency: { kind: 'safe' },
  };
}

describe('ClientError', () => {
  test('sets all properties from constructor options', () => {
    const err = new ClientError({
      service: 'users',
      method: '/GetUser',
      status: 500,
      message: 'Internal error',
      responseBody: { detail: 'db failure' },
    });
    expect(err.service).toBe('users');
    expect(err.method).toBe('/GetUser');
    expect(err.status).toBe(500);
    expect(err.message).toBe('Internal error');
    expect(err.name).toBe('ClientError');
    expect(err).toBeInstanceOf(Error);
  });

  test('responseBody is accessible but non-enumerable', () => {
    const body = { detail: 'sensitive' };
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 400,
      message: 'bad',
      responseBody: body,
    });
    expect(err.responseBody).toBe(body);
    // Non-enumerable: should not appear in Object.keys or JSON.stringify
    expect(Object.keys(err)).not.toContain('responseBody');
    const json = JSON.stringify(err);
    expect(json).not.toContain('responseBody');
    expect(json).not.toContain('sensitive');
  });

  test('responseBody defaults to undefined when not provided', () => {
    const err = new ClientError({ service: 's', method: 'm', status: 400, message: 'bad' });
    expect(err.responseBody).toBeUndefined();
  });

  test('responseBody truncates large string payloads', () => {
    const largeBody = 'x'.repeat(5000);
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: largeBody,
    });
    expect(typeof err.responseBody).toBe('string');
    expect((err.responseBody as string).length).toBeLessThan(largeBody.length);
    expect(err.responseBody as string).toEndWith('… [truncated]');
  });

  test('responseBody truncates large object payloads', () => {
    const largeBody = { data: 'y'.repeat(5000) };
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: largeBody,
    });
    // Large objects get stringified and truncated
    expect(typeof err.responseBody).toBe('string');
    expect(err.responseBody as string).toEndWith('… [truncated]');
  });

  test('responseBody preserves small payloads unchanged', () => {
    const body = { error: 'not found' };
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 404,
      message: 'missing',
      responseBody: body,
    });
    expect(err.responseBody).toBe(body);
  });
});

describe('first-party framework errors', () => {
  specTest(
    'rejects an original payload that violates the declared schema with a safe local response error',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-error-safety',
      check: 'a-malformed-declared-payload-is-a-safe-local-response-error',
    },
    () => {
      const secretKey = 'remote-secret-key';
      let thrown: unknown;
      try {
        decodeFrameworkError({
          service: 'catalog.items',
          method: 'findItem',
          status: 404,
          payload: { code: 'NotFound', [secretKey]: 'untrusted' },
          operation: operation([
            {
              status: 404,
              code: 'NotFound',
              schema: {
                type: 'object',
                properties: { code: { type: 'string', enum: ['NotFound'] }, reason: { type: 'string' } },
                required: ['code', 'reason'],
                additionalProperties: false,
              },
            },
          ]),
        });
      } catch (error) {
        thrown = error;
      }
      expect(thrown).toBeInstanceOf(ClientResponseContractError);
      expect(thrown).toMatchObject({ code: 'client.response' });
      expect(String(thrown)).not.toContain(secretKey);
      expect('responseBody' in (thrown as object)).toBe(false);
    },
  );

  test.each([
    {
      name: 'enum value',
      schema: { type: 'string', enum: ['credential-value'] } satisfies ClientSchema,
      payload: 'credential-value',
      secret: 'credential-value',
    },
    {
      name: 'pattern value',
      schema: { type: 'string', pattern: '^credential-value$' } satisfies ClientSchema,
      payload: 'credential-value',
      secret: 'credential-value',
    },
    {
      name: 'required key',
      schema: {
        type: 'object',
        properties: { 'credential-value': { type: 'string' } },
        required: ['credential-value'],
        additionalProperties: false,
      } satisfies ClientSchema,
      payload: { 'credential-value': 'safe' },
      secret: 'credential-value',
    },
    {
      name: 'byte value',
      schema: { type: 'string', format: 'byte' } satisfies ClientSchema,
      payload: 'Y3JlZGVudGlhbC12YWx1ZQ==',
      secret: 'credential-value',
    },
  ])('omits details when redaction invalidates a typed $name', ({ schema, payload, secret }) => {
    const error = decodeFrameworkError({
      service: 'catalog.items',
      method: 'findItem',
      status: 404,
      payload: { code: 'NotFound' },
      remoteCode: 'NotFound',
      detailsPayload: payload,
      operation: operation([{ status: 404, code: 'NotFound', schema }]),
      secrets: [secret],
    });
    expect(error).toMatchObject({ code: 'NotFound', status: 404 });
    expect(error.details).toBeUndefined();
  });

  specTest(
    'omits details when credential redaction breaks the declared schema',
    {
      feature: 'typescript/service-clients',
      requirement: 'declared-error-safety',
      check: 'redaction-that-breaks-the-declared-schema-omits-details',
    },
    () => {
      const error = decodeFrameworkError({
        service: 'catalog.items',
        method: 'findItem',
        status: 404,
        payload: { code: 'NotFound' },
        remoteCode: 'NotFound',
        detailsPayload: 'credential-value',
        operation: operation([
          { status: 404, code: 'NotFound', schema: { type: 'string', enum: ['credential-value'] } },
        ]),
        secrets: ['credential-value'],
      });
      expect(error).toMatchObject({ code: 'NotFound', details: undefined });
    },
  );

  test('preserves a typed byte detail when it contains no credential', () => {
    const error = decodeFrameworkError({
      service: 'catalog.items',
      method: 'findItem',
      status: 404,
      payload: { code: 'NotFound' },
      remoteCode: 'NotFound',
      detailsPayload: 'AAH/',
      operation: operation([{ status: 404, code: 'NotFound', schema: { type: 'string', format: 'byte' } }]),
      secrets: ['different-secret'],
    });
    expect(error.details).toEqual(Uint8Array.of(0, 1, 255));
  });

  test('revalidates exact wide integer details without routing them through number', () => {
    const error = decodeFrameworkError({
      service: 'catalog.items',
      method: 'findItem',
      status: 409,
      payload: { code: 'Conflict' },
      remoteCode: 'Conflict',
      detailsPayload: parseJsonValue('9223372036854775807'),
      operation: operation([{ status: 409, code: 'Conflict', schema: { type: 'integer', format: 'int64' } }]),
      secrets: ['different-secret'],
    });
    expect(error.details).toBe(9_223_372_036_854_775_807n);
  });

  test('selects two schema-less codes at one status and guards exact service, operation, status, and code', () => {
    const declared = operation([
      { status: 409, code: 'Conflict' },
      { status: 409, code: 'AlreadyExists' },
    ]);
    const error = decodeFrameworkError({
      service: 'catalog.items',
      method: 'createItem',
      status: 409,
      payload: { code: 'AlreadyExists', ignored: 'remote-secret' },
      operation: declared,
    });
    type ExactError = ClientFrameworkError<'AlreadyExists', undefined, 'catalog.items', 'createItem', 409>;
    const isExactError = (caught: unknown): caught is ExactError =>
      isClientFrameworkError(caught, {
        service: 'catalog.items',
        method: 'createItem',
        status: 409,
        code: 'AlreadyExists',
      });
    const consumeCaughtError = (): boolean => {
      try {
        throw error;
      } catch (caught: unknown) {
        if (!isExactError(caught)) return false;
        const code: 'AlreadyExists' = caught.code;
        const service: 'catalog.items' = caught.service;
        const method: 'createItem' = caught.method;
        const status: 409 = caught.status;
        const details: undefined = caught.details;
        return (
          code === 'AlreadyExists' &&
          service === 'catalog.items' &&
          method === 'createItem' &&
          status === 409 &&
          details === undefined
        );
      }
    };
    expect(isExactError(error)).toBe(true);
    expect(consumeCaughtError()).toBe(true);
    expect(error.details).toBeUndefined();
    expect(
      isClientFrameworkError(error, { service: 'other', method: 'createItem', status: 409, code: 'AlreadyExists' }),
    ).toBe(false);
    expect(
      isClientFrameworkError(error, { service: 'catalog.items', method: 'other', status: 409, code: 'AlreadyExists' }),
    ).toBe(false);
    expect(
      isClientFrameworkError(error, {
        service: 'catalog.items',
        method: 'createItem',
        status: 400,
        code: 'AlreadyExists',
      }),
    ).toBe(false);
    expect(
      isClientFrameworkError(error, { service: 'catalog.items', method: 'createItem', status: 409, code: 'Conflict' }),
    ).toBe(false);

    const forged = new ClientFrameworkError({
      service: 'catalog.items',
      method: 'createItem',
      status: 409,
      code: 'AlreadyExists',
      details: { reason: 42 },
    });
    expect(
      isClientFrameworkError(forged, {
        service: 'catalog.items',
        method: 'createItem',
        status: 409,
        code: 'AlreadyExists',
        detailsSchema: {
          type: 'object',
          properties: { reason: { type: 'string' } },
          required: ['reason'],
          additionalProperties: false,
        },
      }),
    ).toBe(false);
  });

  // The provider's envelope `message` is carried only when the consuming
  // binding sets `carryRemoteMessage`. The default keeps the local synthetic
  // message, so a consumer that only logs or forwards a failure widens nothing.
  // The fixture body is byte-identical to the Go half,
  // TestRemoteErrorDropsTheEnvelopeMessageWithoutTheBindingOptIn and
  // TestRemoteErrorCarriesTheEnvelopeMessageWhenTheBindingOptsIn
  // (go/framework/client/remote_error_test.go).
  const CARRY_MESSAGE_BODY = JSON.parse(
    '{"code":"not_found","error":"Not Found","message":"widget 4f0c8f4e does not exist","details":{"resource":"widget"}}',
  );
  const carryMessageDetailsSchema: ClientSchema = {
    type: 'object',
    properties: { resource: { type: 'string' } },
    required: ['resource'],
    additionalProperties: false,
  };

  test('does not carry the envelope message without the binding opt-in', () => {
    const error = decodeFrameworkError({
      service: 'widgets',
      method: 'getWidget',
      status: 404,
      payload: CARRY_MESSAGE_BODY,
      detailsPayload: CARRY_MESSAGE_BODY.details,
      operation: operation([{ status: 404, code: 'not_found', schema: carryMessageDetailsSchema }]),
    });
    expect(error.code).toBe('not_found');
    expect(error.message).toBe('widgets request failed with not_found');
    expect(error.details).toEqual({ resource: 'widget' });
  });

  test('does not carry the envelope message of an undeclared error without the opt-in', () => {
    const error = decodeFrameworkError({
      service: 'widgets',
      method: 'getWidget',
      status: 502,
      payload: {
        code: 'upstream.unreachable',
        error: 'Bad Gateway',
        message: 'upstream inventory service is unreachable',
      },
      operation: operation([]),
    });
    expect(error.code).toBe('client.remote');
    expect(error.message).toBe('widgets request failed with client.remote');
  });

  test('carries the envelope message onto a declared error when the binding opts in', () => {
    const error = decodeFrameworkError({
      service: 'widgets',
      method: 'getWidget',
      status: 404,
      payload: CARRY_MESSAGE_BODY,
      detailsPayload: CARRY_MESSAGE_BODY.details,
      operation: operation([{ status: 404, code: 'not_found', schema: carryMessageDetailsSchema }]),
      carryRemoteMessage: true,
    });
    expect(error.code).toBe('not_found');
    expect(error.message).toBe('widget 4f0c8f4e does not exist');
    expect(error.details).toEqual({ resource: 'widget' });
  });

  test('carries the envelope message onto an undeclared error when the binding opts in', () => {
    const error = decodeFrameworkError({
      service: 'widgets',
      method: 'getWidget',
      status: 502,
      payload: {
        code: 'upstream.unreachable',
        error: 'Bad Gateway',
        message: 'upstream inventory service is unreachable',
      },
      operation: operation([]),
      carryRemoteMessage: true,
    });
    expect(error.code).toBe('client.remote');
    expect(error.message).toBe('upstream inventory service is unreachable');
  });

  test('falls back to the synthetic message when the envelope carries none', () => {
    const error = decodeFrameworkError({
      service: 'widgets',
      method: 'getWidget',
      status: 404,
      payload: { code: 'not_found' },
      operation: operation([{ status: 404, code: 'not_found' }]),
      carryRemoteMessage: true,
    });
    expect(error.message).toBe('widgets request failed with not_found');
  });

  test('redacts this call secrets from the envelope message it was asked to carry', () => {
    const error = decodeFrameworkError({
      service: 'inventory',
      method: 'putItem',
      status: 400,
      payload: { code: 'errors.invalid', error: 'Bad Request', message: 'invalid token active-service-token' },
      operation: operation([{ status: 400, code: 'errors.invalid' }]),
      secrets: ['active-service-token'],
      carryRemoteMessage: true,
    });
    expect(error.message).toBe('invalid token [REDACTED]');
    expect(error.message).not.toContain('active-service-token');
  });

  // The two rules both runtimes share for the message, as distinct from
  // `details`: a secret in prose — raw or in either base64 form — is
  // substituted inline and the words around it survive; a message that is
  // itself one base64 value whose bytes disclose a secret is replaced whole.
  // The Go half is TestSanitizedErrorMessageRedactsInlineAndReplacesAnOpaqueBlobWhole
  // (remote_error_test.go); the expectations are byte-identical.
  const bearer = 'active-service-token';
  const bearerBytes = new TextEncoder().encode(bearer);
  for (const scene of [
    { name: 'inline raw', message: `credential ${bearer} expired`, expected: 'credential [REDACTED] expired' },
    { name: 'inline every hit', message: `${bearer} and again ${bearer}`, expected: '[REDACTED] and again [REDACTED]' },
    {
      name: 'inline std base64',
      message: `credential ${bearerBytes.toBase64({ alphabet: 'base64' })} expired`,
      expected: 'credential [REDACTED] expired',
    },
    {
      name: 'inline url base64',
      message: `credential ${bearerBytes.toBase64({ alphabet: 'base64url', omitPadding: true })} expired`,
      expected: 'credential [REDACTED] expired',
    },
    {
      name: 'opaque blob',
      message: new TextEncoder().encode(`credential ${bearer} expired`).toBase64({ alphabet: 'base64' }),
      expected: '[REDACTED]',
    },
    { name: 'clean prose', message: 'widget 4f0c does not exist', expected: 'widget 4f0c does not exist' },
  ]) {
    test(`redacts the carried message inline or whole: ${scene.name}`, () => {
      const decode = (carryRemoteMessage: boolean) =>
        decodeFrameworkError({
          service: 'inventory',
          method: 'putItem',
          status: 400,
          payload: { code: 'errors.invalid', error: 'Bad Request', message: scene.message },
          operation: operation([{ status: 400, code: 'errors.invalid' }]),
          secrets: ['', bearer],
          carryRemoteMessage,
        });
      expect(decode(true).message).toBe(scene.expected);
      expect(decode(false).message).toBe('inventory request failed with errors.invalid');
    });
  }
});

describe('ClientRequestError', () => {
  test('is a ClientError with name ClientRequestError', () => {
    const err = new ClientRequestError({
      service: 'orders',
      method: '/CreateOrder',
      status: 422,
      message: 'Validation failed',
    });
    expect(err).toBeInstanceOf(ClientError);
    expect(err).toBeInstanceOf(ClientRequestError);
    expect(err.name).toBe('ClientRequestError');
    expect(err.status).toBe(422);
  });

  test('carries responseBody', () => {
    const body = { field: 'quantity', issue: 'must be positive' };
    const err = new ClientRequestError({
      service: 's',
      method: 'm',
      status: 400,
      message: 'bad',
      responseBody: body,
    });
    expect(err.responseBody).toBe(body);
  });
});

describe('ClientServerError', () => {
  test('is a ClientError with name ClientServerError', () => {
    const err = new ClientServerError({
      service: 'payments',
      method: '/Charge',
      status: 503,
      message: 'Service unavailable',
    });
    expect(err).toBeInstanceOf(ClientError);
    expect(err).toBeInstanceOf(ClientServerError);
    expect(err.name).toBe('ClientServerError');
    expect(err.status).toBe(503);
  });
});

describe('ClientTimeoutError', () => {
  test('is a ClientError carrying timeoutMs', () => {
    const err = new ClientTimeoutError({ service: 'users', method: 'GET /users/123', timeoutMs: 10_000 });
    expect(err).toBeInstanceOf(ClientError);
    expect(err).toBeInstanceOf(ClientTimeoutError);
    expect(err.name).toBe('ClientTimeoutError');
    expect(err.timeoutMs).toBe(10_000);
    expect(err.status).toBe(0);
    expect(err.message).toContain('10000ms');
  });

  test('forwards the underlying error on the standard cause chain', () => {
    const underlying = new DOMException('The operation timed out.', 'TimeoutError');
    const err = new ClientTimeoutError({ service: 'users', method: 'GET /x', timeoutMs: 5000, cause: underlying });
    expect(err.cause).toBe(underlying);
  });
});

describe('ClientRetryExhaustedError', () => {
  test('is a ClientError carrying attempts and lastError', () => {
    const lastError = new TypeError('connection refused');
    const err = new ClientRetryExhaustedError({
      service: 'users',
      method: 'GET /users',
      attempts: 4,
      lastError,
    });
    expect(err).toBeInstanceOf(ClientError);
    expect(err).toBeInstanceOf(ClientRetryExhaustedError);
    expect(err.name).toBe('ClientRetryExhaustedError');
    expect(err.attempts).toBe(4);
    expect(err.lastError).toBe(lastError);
    // The last error is also exposed on the standard Error.cause chain.
    expect(err.cause).toBe(lastError);
    expect(err.status).toBe(0);
    expect(err.message).toContain('connection refused');
  });
});

describe("ADR 0006 — a declared error's schema describes details", () => {
  // The TypeScript half of the cross-language fixture. These are the same bytes
  // `go/framework/client/remote_error_test.go` decodes in
  // `TestDeclaredErrorSchemaDescribesTheDetailsMember`, and the schema is the
  // corpus `not_found` error of `GET /widgets/{id}`
  // (protocols/clientcontract/fixtures/openapi/valid/full.openapi.json).
  const WIRE_BODY =
    '{"code":"not_found","error":"Not Found",' +
    '"message":"widget 4f0c8f4e-0e8a-4d1e-9a2b-6f5a1c0d3b77 does not exist",' +
    '"details":{"resource":"widget","metadata":{"tenant":"acme"}}}';
  const detailsSchema: ClientSchema = {
    type: 'object',
    properties: {
      resource: { type: 'string', minLength: 1, maxLength: 128 },
      metadata: { type: 'object', additionalProperties: { type: 'string' } },
    },
    required: ['resource'],
    additionalProperties: false,
  };

  test('accepts a well-formed provider error and types its details member', () => {
    const payload = JSON.parse(WIRE_BODY);
    const envelope = readFirstPartyErrorEnvelope(payload);
    expect(envelope.remoteCode).toBe('not_found');

    const error = decodeFrameworkError({
      service: 'widgets',
      method: 'getWidget',
      status: 404,
      payload,
      remoteCode: envelope.remoteCode,
      detailsPayload: envelope.detailsPayload,
      operation: operation([{ status: 404, code: 'not_found', schema: detailsSchema }]),
    });
    expect(error).toMatchObject({ code: 'not_found', status: 404 });
    expect(error.details).toEqual({ resource: 'widget', metadata: { tenant: 'acme' } });
  });

  test('rejects an envelope-only body where a details schema is declared', () => {
    const payload = JSON.parse('{"code":"not_found","error":"Not Found","message":"gone"}');
    const envelope = readFirstPartyErrorEnvelope(payload);
    expect(envelope.detailsPayload).toBeUndefined();

    expect(() =>
      decodeFrameworkError({
        service: 'widgets',
        method: 'getWidget',
        status: 404,
        payload,
        remoteCode: envelope.remoteCode,
        detailsPayload: envelope.detailsPayload,
        operation: operation([{ status: 404, code: 'not_found', schema: detailsSchema }]),
      }),
    ).toThrow(ClientResponseContractError);
  });
});
