import { describe, expect, it, mock } from 'bun:test';
import { HttpException, NotFoundException } from '@putnami/runtime';
import {
  acceptContains,
  buildGrpcWebErrorResponse,
  buildGrpcWebResponse,
  clientAcceptsGzip,
  compressGzip,
  compressResponse,
  connectResponseHeaders,
  decompressGzip,
  handleGrpcError,
  mapErrorToGrpcStatus,
  parseGrpcTimeout,
  parseMediaType,
  projectConnectError,
} from '../../src/grpc/grpc-protocol';
import { decodeBadRequestDetail, findFrameworkErrorDetail } from '../../src/grpc/connect-protocol';
import { snakeToCamel } from '../../src/grpc/proto-wire';
import { HttpResponse } from '../../src/http/http-response';
import { pascalCase } from '../../src/proto/proto-render';

describe('parseMediaType', () => {
  it('strips parameters from content type', () => {
    expect(parseMediaType('application/json; charset=utf-8')).toBe('application/json');
  });

  it('returns media type as-is when no params', () => {
    expect(parseMediaType('application/proto')).toBe('application/proto');
  });
});

describe('acceptContains', () => {
  it('matches exact media type', () => {
    expect(acceptContains('application/json', 'application/json')).toBe(true);
  });

  it('matches in multi-value accept', () => {
    expect(acceptContains('text/html, application/json', 'application/json')).toBe(true);
  });

  it('matches with quality factor', () => {
    expect(acceptContains('application/proto;q=0.9, application/json', 'application/proto')).toBe(true);
  });

  it('matches wildcard', () => {
    expect(acceptContains('application/*', 'application/proto')).toBe(true);
  });

  it('returns false for no match', () => {
    expect(acceptContains('text/html', 'application/json')).toBe(false);
  });

  it('handles multiple target types', () => {
    expect(acceptContains('application/proto', 'application/json', 'application/proto')).toBe(true);
  });
});

describe('parseGrpcTimeout', () => {
  it('returns undefined when no grpc-timeout header', () => {
    const headers = new Headers();
    expect(parseGrpcTimeout(headers)).toBeUndefined();
  });

  it('returns undefined for invalid format', () => {
    const headers = new Headers({ 'grpc-timeout': 'invalid' });
    expect(parseGrpcTimeout(headers)).toBeUndefined();
  });

  it('parses hours', () => {
    const headers = new Headers({ 'grpc-timeout': '2H' });
    const result = parseGrpcTimeout(headers);
    expect(result).toBeDefined();
    expect(result?.timeoutMs).toBe(7_200_000);
  });

  it('parses minutes', () => {
    const headers = new Headers({ 'grpc-timeout': '5M' });
    const result = parseGrpcTimeout(headers);
    expect(result?.timeoutMs).toBe(300_000);
  });

  it('parses seconds', () => {
    const headers = new Headers({ 'grpc-timeout': '30S' });
    const result = parseGrpcTimeout(headers);
    expect(result?.timeoutMs).toBe(30_000);
  });

  it('parses milliseconds', () => {
    const headers = new Headers({ 'grpc-timeout': '500m' });
    const result = parseGrpcTimeout(headers);
    expect(result?.timeoutMs).toBe(500);
  });

  it('parses microseconds (rounds up to at least 1ms)', () => {
    const headers = new Headers({ 'grpc-timeout': '100u' });
    const result = parseGrpcTimeout(headers);
    expect(result?.timeoutMs).toBe(1);
  });

  it('parses nanoseconds (rounds up to at least 1ms)', () => {
    const headers = new Headers({ 'grpc-timeout': '500000n' });
    const result = parseGrpcTimeout(headers);
    expect(result?.timeoutMs).toBe(1);
  });

  it('returns AbortController', () => {
    const headers = new Headers({ 'grpc-timeout': '10S' });
    const result = parseGrpcTimeout(headers);
    expect(result?.controller).toBeInstanceOf(AbortController);
  });
});

describe('clientAcceptsGzip', () => {
  it('checks grpc-accept-encoding first', () => {
    const headers = new Headers({ 'grpc-accept-encoding': 'gzip, identity' });
    expect(clientAcceptsGzip(headers)).toBe(true);
  });

  it('falls back to accept-encoding', () => {
    const headers = new Headers({ 'accept-encoding': 'gzip, deflate' });
    expect(clientAcceptsGzip(headers)).toBe(true);
  });

  it('returns false when no gzip support', () => {
    const headers = new Headers({ 'accept-encoding': 'deflate' });
    expect(clientAcceptsGzip(headers)).toBe(false);
  });

  it('returns false when no encoding headers', () => {
    const headers = new Headers();
    expect(clientAcceptsGzip(headers)).toBe(false);
  });
});

describe('compressGzip / decompressGzip', () => {
  it('round-trips data', () => {
    const data = new TextEncoder().encode('hello world test data');
    const compressed = compressGzip(data);
    const decompressed = decompressGzip(compressed);
    expect(new TextDecoder().decode(decompressed)).toBe('hello world test data');
  });

  it('bounds decompressed output to prevent a decompression bomb', () => {
    // 256 KiB of zeros compresses to a tiny gzip payload but inflates far past a
    // small cap — the classic decompression-bomb shape.
    const bomb = compressGzip(new Uint8Array(256 * 1024));
    expect(bomb.length).toBeLessThan(2048);

    let thrown: unknown;
    try {
      decompressGzip(bomb, 4096);
    } catch (error) {
      thrown = error;
    }
    expect(thrown).toBeInstanceOf(HttpException);
    expect((thrown as HttpException).getStatus()).toBe(413);
    // 413 maps to gRPC RESOURCE_EXHAUSTED (8).
    expect(mapErrorToGrpcStatus(thrown).grpcCode).toBe(8);
    expect(mapErrorToGrpcStatus(thrown).code).toBe('resource_exhausted');

    // Within the default cap it still round-trips.
    expect(decompressGzip(bomb).length).toBe(256 * 1024);
  });
});

describe('compressResponse', () => {
  it('compresses string body', () => {
    const body = 'x'.repeat(100);
    const response = new HttpResponse(body, { headers: { 'Content-Type': 'application/json' } });
    const compressed = compressResponse(response);
    expect(compressed.getHeader('Content-Encoding')).toBe('gzip');
  });

  it('skips tiny payloads', () => {
    const response = new HttpResponse('hi', { headers: { 'Content-Type': 'text/plain' } });
    const result = compressResponse(response);
    expect(result.getHeader('Content-Encoding')).toBeUndefined();
  });
});

describe('connectResponseHeaders', () => {
  it('returns protocol version and accept-encoding', () => {
    const headers = connectResponseHeaders();
    expect(headers['Connect-Protocol-Version']).toBe('1');
    expect(headers['Accept-Encoding']).toBe('gzip, identity');
  });
});

describe('buildGrpcWebResponse', () => {
  it('builds response with data and trailer frames', () => {
    const payload = new TextEncoder().encode('{"items":[]}');
    const response = buildGrpcWebResponse(payload, false);
    expect(response.getHeader('Content-Type')).toBe('application/grpc-web+json');
    const bodyInit = response.getBodyInit();
    expect(bodyInit).toBeInstanceOf(ArrayBuffer);
    expect((bodyInit as ArrayBuffer).byteLength).toBeGreaterThan(payload.length);
  });

  it('uses binary content type when useBinary=true', () => {
    const payload = new Uint8Array([1, 2, 3]);
    const response = buildGrpcWebResponse(payload, true);
    expect(response.getHeader('Content-Type')).toBe('application/grpc-web+proto');
  });

  it('includes grpc-message in trailers when provided', () => {
    const payload = new TextEncoder().encode('{}');
    const response = buildGrpcWebResponse(payload, false, 3, 'Invalid argument');
    const body = new Uint8Array(response.getBodyInit() as ArrayBuffer);
    const text = new TextDecoder().decode(body);
    expect(text).toContain('grpc-status: 3');
    expect(text).toContain('Invalid%20argument');
  });
});

describe('buildGrpcWebErrorResponse', () => {
  it('builds trailers-only error response', () => {
    const response = buildGrpcWebErrorResponse(5, 'Not found', false);
    expect(response.getHeader('Content-Type')).toBe('application/grpc-web+json');
    const body = new Uint8Array(response.getBodyInit() as ArrayBuffer);
    const text = new TextDecoder().decode(body);
    expect(text).toContain('grpc-status: 5');
    expect(text).toContain('Not%20found');
  });
});

describe('mapErrorToGrpcStatus', () => {
  it('maps HttpException to the Connect code and the status the protocol pairs with it', () => {
    const err = new HttpException('Not Found', 404);
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(5);
    // Connect codes are the sixteen lower-case strings; the gRPC spelling
    // `NOT_FOUND` is not one of them and no conforming client would read it.
    expect(result.code).toBe('not_found');
    expect(result.httpStatus).toBe(404);
  });

  it('maps HttpException 401 to unauthenticated', () => {
    const err = new HttpException('Unauthorized', 401);
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(16);
    expect(result.code).toBe('unauthenticated');
  });

  it('maps HttpException 403 to PERMISSION_DENIED', () => {
    const err = new HttpException('Forbidden', 403);
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(7);
  });

  it('maps HttpException 409 to ALREADY_EXISTS', () => {
    const err = new HttpException('Conflict', 409);
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(6);
  });

  it('maps HttpException 429 to RESOURCE_EXHAUSTED', () => {
    const err = new HttpException('Too Many Requests', 429);
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(8);
    expect(result.code).toBe('resource_exhausted');
  });

  it('maps HttpException 503 to UNAVAILABLE', () => {
    const err = new HttpException('Service Unavailable', 503);
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(14);
  });

  it('maps a 2xx HttpException to unknown, because the protocol has no success code', () => {
    // An exception carrying a success status is not a category the protocol can
    // name; reporting `OK` would tell a gRPC-Web caller the RPC succeeded.
    const err = new HttpException('OK', 200);
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(2);
    expect(result.code).toBe('unknown');
  });

  it('maps unknown HTTP status to UNKNOWN', () => {
    const err = new HttpException("I'm a teapot", 418);
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(2);
  });

  it('maps plain Error to INTERNAL and redacts the message', () => {
    const err = new Error('connection refused at db.internal:5432');
    const result = mapErrorToGrpcStatus(err);
    expect(result.grpcCode).toBe(13);
    expect(result.code).toBe('internal');
    // Internal error detail must not leak to the client.
    expect(result.message).toBe('Internal error');
    expect(result.message).not.toContain('db.internal');
  });

  it('extracts validation errors as a binary google.rpc.BadRequest detail', () => {
    const err = new HttpException(
      { message: 'Validation failed', errors: [{ field: 'name', message: 'required' }] },
      400,
    );
    const result = mapErrorToGrpcStatus(err);
    expect(result.details).toBeDefined();
    const violation = result.details?.find((detail) => detail.type === 'google.rpc.BadRequest');
    expect(violation).toBeDefined();
    // The specification calls the binary `value` normative and forbids a client
    // from depending on `debug`, so the violations must survive a decode of the
    // bytes alone.
    expect(decodeBadRequestDetail(violation!)).toEqual([{ field: 'name', description: 'required' }]);
  });

  it('carries the first-party envelope as a typed detail', () => {
    const declaredBy = (projection: ReturnType<typeof projectConnectError>) =>
      findFrameworkErrorDetail({
        code: projection.connectCode,
        details: projection.details?.filter((entry) => entry.type === 'putnami.client.v1.FrameworkError') ?? [],
      });

    // The framework's stable code and its own status ride in the binary detail,
    // so a generated client rebuilds the same typed error a REST call raises.
    const declared = projectConnectError(new NotFoundException('missing'), { errorCodes: ['NotFound'] });
    expect(declaredBy(declared)).toEqual({ code: 'not_found', status: 404 });

    // An error the endpoint never declared is not presented as a declared one:
    // the detail carries the generic remote code and no payload.
    const undeclared = projectConnectError(new NotFoundException('missing'));
    expect(declaredBy(undeclared)).toEqual({ code: 'client.remote', status: 404 });
  });
});

describe('handleGrpcError', () => {
  it('returns JSON error for non-gRPC-Web', () => {
    const logger = { error: mock(() => {}) };
    const err = new HttpException('Not Found', 404);
    const response = handleGrpcError(logger, err);
    expect(response.status).toBe(404);
  });

  it('logs 500 errors', () => {
    const errorFn = mock(() => {});
    const logger = { error: errorFn };
    const err = new Error('server crash');
    handleGrpcError(logger, err);
    expect(errorFn).toHaveBeenCalledTimes(1);
  });

  it('does not log client errors', () => {
    const errorFn = mock(() => {});
    const logger = { error: errorFn };
    const err = new HttpException('Bad Request', 400);
    handleGrpcError(logger, err);
    expect(errorFn).not.toHaveBeenCalled();
  });

  it('returns gRPC-Web response when isGrpcWeb=true', async () => {
    const logger = { error: mock(() => {}) };
    const err = new HttpException('Not Found', 404);
    const response = handleGrpcError(logger, err, true, false);
    expect(response.getHeader('Content-Type')).toBe('application/grpc-web+json');
  });
});

describe('pascalCase', () => {
  it('converts dash-separated string', () => {
    expect(pascalCase('foo-bar')).toBe('FooBar');
  });

  it('converts underscore-separated string', () => {
    expect(pascalCase('foo_bar')).toBe('FooBar');
  });

  it('handles single word', () => {
    expect(pascalCase('users')).toBe('Users');
  });
});

describe('snakeToCamel', () => {
  it('converts snake_case to camelCase', () => {
    expect(snakeToCamel('user_name')).toBe('userName');
  });

  it('handles multiple underscores', () => {
    expect(snakeToCamel('first_last_name')).toBe('firstLastName');
  });

  it('handles no underscores', () => {
    expect(snakeToCamel('name')).toBe('name');
  });
});
