import { describe, expect, test } from 'bun:test';
import { ClientError, ClientRequestError, ClientServerError } from '../../src/runtime/errors';

describe('ClientError edge cases', () => {
  test('responseBody handles null', () => {
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: null,
    });
    expect(err.responseBody).toBeNull();
  });

  test('responseBody handles number', () => {
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: 42,
    });
    expect(err.responseBody).toBe(42);
  });

  test('responseBody handles boolean', () => {
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: false,
    });
    expect(err.responseBody).toBe(false);
  });

  test('responseBody handles array', () => {
    const arr = [1, 2, 3];
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: arr,
    });
    // Array serializes to short JSON string, so preserved
    expect(err.responseBody).toBe(arr);
  });

  test('responseBody handles empty string', () => {
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: '',
    });
    expect(err.responseBody).toBe('');
  });

  test('responseBody truncates exactly at 4096 chars', () => {
    const body = 'a'.repeat(4096);
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: body,
    });
    // 4096 chars is at the boundary — should be preserved
    expect(err.responseBody).toBe(body);
  });

  test('responseBody truncates at 4097 chars', () => {
    const body = 'a'.repeat(4097);
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: body,
    });
    expect(typeof err.responseBody).toBe('string');
    // Truncated to 4096 chars + "… [truncated]" suffix
    expect(err.responseBody as string).toEndWith('… [truncated]');
    expect((err.responseBody as string).startsWith('a'.repeat(4096))).toBe(true);
  });

  test('responseBody is read-only', () => {
    const err = new ClientError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'fail',
      responseBody: 'original',
    });

    expect(() => {
      (err as { responseBody: unknown }).responseBody = 'modified';
    }).toThrow();
  });

  test('is instanceof Error', () => {
    const err = new ClientError({ service: 's', method: 'm', status: 500, message: 'fail' });
    expect(err).toBeInstanceOf(Error);
  });

  test('stack trace is captured', () => {
    const err = new ClientError({ service: 's', method: 'm', status: 500, message: 'fail' });
    expect(err.stack).toBeDefined();
    expect(err.stack).toContain('ClientError');
  });
});

describe('ClientRequestError edge cases', () => {
  test('is instanceof ClientError and Error', () => {
    const err = new ClientRequestError({
      service: 's',
      method: 'm',
      status: 400,
      message: 'bad',
    });
    expect(err).toBeInstanceOf(ClientError);
    expect(err).toBeInstanceOf(Error);
    expect(err.name).toBe('ClientRequestError');
  });

  test('status codes in 4xx range', () => {
    for (const status of [400, 401, 403, 404, 409, 422, 429]) {
      const err = new ClientRequestError({
        service: 's',
        method: 'm',
        status,
        message: `HTTP ${status}`,
      });
      expect(err.status).toBe(status);
    }
  });
});

describe('ClientServerError edge cases', () => {
  test('is instanceof ClientError and Error', () => {
    const err = new ClientServerError({
      service: 's',
      method: 'm',
      status: 500,
      message: 'internal',
    });
    expect(err).toBeInstanceOf(ClientError);
    expect(err).toBeInstanceOf(Error);
    expect(err.name).toBe('ClientServerError');
  });

  test('status codes in 5xx range', () => {
    for (const status of [500, 501, 502, 503, 504]) {
      const err = new ClientServerError({
        service: 's',
        method: 'm',
        status,
        message: `HTTP ${status}`,
      });
      expect(err.status).toBe(status);
    }
  });
});
