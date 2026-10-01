import { describe, expect, it } from 'bun:test';
import { HttpResponse } from '../../src/http/http-response';
import type { ProtoFieldMeta } from '../../src/proto';
import { encodeResponse, formatGrpcResponse, formatResult } from '../../src/grpc/grpc-response';

const emptyMessages: Record<string, ProtoFieldMeta[]> = {};

describe('formatResult', () => {
  it('returns empty JSON for undefined result', () => {
    const res = formatResult(undefined, false, undefined, emptyMessages);
    expect(res).toBeInstanceOf(HttpResponse);
    const response = (res as HttpResponse).get();
    expect(response.headers.get('Content-Type')).toContain('application/json');
  });

  it('returns empty JSON for null result', () => {
    const res = formatResult(null, false, undefined, emptyMessages);
    expect(res).toBeInstanceOf(HttpResponse);
  });

  it('encodes plain object as JSON', () => {
    const res = formatResult({ id: 1, name: 'test' }, false, undefined, emptyMessages);
    expect(res).toBeInstanceOf(HttpResponse);
  });

  it('returns HttpResponse.json for primitive result', () => {
    const res = formatResult('hello', false, undefined, emptyMessages);
    expect(res).toBeInstanceOf(HttpResponse);
  });

  it('delegates to formatGrpcResponse for HttpResponse result', async () => {
    const input = HttpResponse.json({ id: 1 });
    const res = await formatResult(input, false, undefined, emptyMessages);
    expect(res).toBeInstanceOf(HttpResponse);
    const body = await res.get().text();
    expect(body).toContain('"id"');
  });

  it('builds gRPC-Web response for undefined result when isGrpcWeb', () => {
    const res = formatResult(undefined, false, undefined, emptyMessages, undefined, true);
    expect(res).toBeInstanceOf(HttpResponse);
  });
});

describe('encodeResponse', () => {
  it('returns JSON response when useBinary is false', () => {
    const data = { name: 'test', count: 5 };
    const res = encodeResponse(data, false, undefined, emptyMessages);
    const response = res.get();
    expect(response.headers.get('Content-Type')).toContain('application/json');
  });

  it('returns binary response when useBinary is true and responseMeta provided', () => {
    const meta: ProtoFieldMeta[] = [{ name: 'name', number: 1, type: 'string', optional: false, repeated: false }];
    const data = { name: 'test' };
    const res = encodeResponse(data, true, meta, emptyMessages);
    const response = res.get();
    expect(response.headers.get('Content-Type')).toContain('application/proto');
  });

  it('falls back to JSON when useBinary is true but no responseMeta', () => {
    const data = { name: 'test' };
    const res = encodeResponse(data, true, undefined, emptyMessages);
    const response = res.get();
    expect(response.headers.get('Content-Type')).toContain('application/json');
  });

  it('builds gRPC-Web JSON response when isGrpcWeb is true', () => {
    const data = { name: 'test' };
    const res = encodeResponse(data, false, undefined, emptyMessages, undefined, true);
    const response = res.get();
    expect(response.headers.get('Content-Type')).toContain('grpc-web');
  });

  it('builds gRPC-Web binary response when isGrpcWeb and useBinary', () => {
    const meta: ProtoFieldMeta[] = [{ name: 'id', number: 1, type: 'int32', optional: false, repeated: false }];
    const data = { id: 42 };
    const res = encodeResponse(data, true, meta, emptyMessages, undefined, true);
    const response = res.get();
    expect(response.headers.get('Content-Type')).toContain('grpc-web');
  });
});

describe('formatGrpcResponse', () => {
  it('uses raw data fast path when available', async () => {
    const input = HttpResponse.json({ id: 1, name: 'fast' });
    const res = await formatGrpcResponse(input, false, undefined, emptyMessages);
    const body = await res.get().text();
    expect(body).toContain('"name"');
  });

  it('falls back to parsing response body', async () => {
    const input = new HttpResponse('{"id":2}', {
      headers: { 'Content-Type': 'application/json' },
    });
    const res = await formatGrpcResponse(input, false, undefined, emptyMessages);
    expect(res).toBeInstanceOf(HttpResponse);
  });

  it('handles non-JSON body gracefully', async () => {
    const input = new HttpResponse('not-json', {
      headers: { 'Content-Type': 'text/plain' },
    });
    const res = await formatGrpcResponse(input, false, undefined, emptyMessages);
    expect(res).toBeInstanceOf(HttpResponse);
  });

  it('encodes as binary when useBinary and responseMeta provided', async () => {
    const meta: ProtoFieldMeta[] = [{ name: 'name', number: 1, type: 'string', optional: false, repeated: false }];
    const input = HttpResponse.json({ name: 'binary' });
    const res = await formatGrpcResponse(input, true, meta, emptyMessages);
    const response = res.get();
    expect(response.headers.get('Content-Type')).toContain('application/proto');
  });
});
