import { describe, expect, test } from 'bun:test';
import { runInContext } from '@putnami/runtime';
import { contextInterceptor } from '../../src/interceptors/context.interceptor';
import type { ClientRequest, ClientResponse } from '../../src/runtime/transport.type';

function makeRequest(headers?: Record<string, string>): ClientRequest {
  const h = new Headers(headers);
  return { method: 'GET', path: '/test', headers: h };
}

function makeResponse(): ClientResponse {
  return { data: null, status: 200, headers: new Headers() };
}

async function runInterceptor(request: ClientRequest, context?: Record<string, unknown>): Promise<ClientRequest> {
  const interceptor = contextInterceptor();
  let captured: ClientRequest | undefined;

  const run = async () => {
    await interceptor(request, async (req) => {
      captured = req;
      return makeResponse();
    });
  };

  if (context) {
    await runInContext(context, run);
  } else {
    await run();
  }

  // captured is always set because the interceptor calls next synchronously
  return captured as ClientRequest;
}

describe('contextInterceptor', () => {
  test('does nothing when no async context is set', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req);
    expect(result.headers.has('X-Trace-Id')).toBe(false);
    expect(result.headers.has('X-Request-Id')).toBe(false);
    expect(result.headers.has('X-Region')).toBe(false);
    expect(result.headers.has('X-Experiments')).toBe(false);
  });

  test('injects X-Trace-Id from context', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req, { traceId: 'trace-abc123' });
    expect(result.headers.get('X-Trace-Id')).toBe('trace-abc123');
  });

  test('injects X-Request-Id from context', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req, { requestId: 'req-xyz' });
    expect(result.headers.get('X-Request-Id')).toBe('req-xyz');
  });

  test('injects X-Region from context', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req, { __region: 'us-east1' });
    expect(result.headers.get('X-Region')).toBe('us-east1');
  });

  test('injects X-Experiments from context', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req, { __experiments: 'exp-v2,exp-dark' });
    expect(result.headers.get('X-Experiments')).toBe('exp-v2,exp-dark');
  });

  test('injects all routing context headers at once', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req, {
      traceId: 'trace-1',
      requestId: 'req-2',
      __region: 'eu-west1',
      __experiments: 'exp-a',
    });
    expect(result.headers.get('X-Trace-Id')).toBe('trace-1');
    expect(result.headers.get('X-Request-Id')).toBe('req-2');
    expect(result.headers.get('X-Region')).toBe('eu-west1');
    expect(result.headers.get('X-Experiments')).toBe('exp-a');
  });

  test('does not override existing X-Trace-Id header', async () => {
    const req = makeRequest({ 'X-Trace-Id': 'pre-existing-trace' });
    const result = await runInterceptor(req, { traceId: 'new-trace' });
    expect(result.headers.get('X-Trace-Id')).toBe('pre-existing-trace');
  });

  test('does not override existing X-Request-Id header', async () => {
    const req = makeRequest({ 'X-Request-Id': 'existing-req' });
    const result = await runInterceptor(req, { requestId: 'new-req' });
    expect(result.headers.get('X-Request-Id')).toBe('existing-req');
  });

  test('does not override existing X-Region header', async () => {
    const req = makeRequest({ 'X-Region': 'us-west1' });
    const result = await runInterceptor(req, { __region: 'eu-west1' });
    expect(result.headers.get('X-Region')).toBe('us-west1');
  });

  test('does not override existing X-Experiments header', async () => {
    const req = makeRequest({ 'X-Experiments': 'existing-exp' });
    const result = await runInterceptor(req, { __experiments: 'new-exp' });
    expect(result.headers.get('X-Experiments')).toBe('existing-exp');
  });

  test('strips CR/LF from traceId to prevent header injection', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req, { traceId: 'trace\r\nX-Injected: evil' });
    const value = result.headers.get('X-Trace-Id') ?? '';
    expect(value).not.toContain('\r');
    expect(value).not.toContain('\n');
    expect(value).toBe('traceX-Injected: evil');
  });

  test('strips NUL from requestId to prevent header injection', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req, { requestId: 'req\0id' });
    expect(result.headers.get('X-Request-Id')).toBe('reqid');
  });

  test('skips injection when context field is empty string', async () => {
    const req = makeRequest();
    const result = await runInterceptor(req, { traceId: '' });
    // Empty string is falsy — should not set the header
    expect(result.headers.has('X-Trace-Id')).toBe(false);
  });

  test('calls next and returns its result', async () => {
    const interceptor = contextInterceptor();
    const req = makeRequest();
    const expected: ClientResponse = { data: { value: 42 }, status: 200, headers: new Headers() };

    const result = await interceptor(req, async () => expected);
    expect(result).toBe(expected);
  });
});
