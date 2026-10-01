import { afterEach, beforeEach, describe, expect, test } from 'bun:test';
import { resolveServiceUrl } from '../../src/config/client.config';

describe('resolveServiceUrl (scheme validation on configured URL)', () => {
  const ENV_KEY = 'CLIENT_SERVICE_USERS_API_URL';
  let savedEnv: string | undefined;

  beforeEach(() => {
    savedEnv = process.env[ENV_KEY];
    delete process.env[ENV_KEY];
  });

  afterEach(() => {
    if (savedEnv === undefined) {
      delete process.env[ENV_KEY];
    } else {
      process.env[ENV_KEY] = savedEnv;
    }
  });

  test('returns a valid configured http URL', () => {
    expect(resolveServiceUrl('users-api', 'http://localhost:3000')).toBe('http://localhost:3000');
  });

  test('rejects a configured file:// URL (previously returned unchecked)', () => {
    expect(() => resolveServiceUrl('users-api', 'file:///etc/passwd')).toThrow(/scheme/);
  });

  test('rejects a configured non-URL string', () => {
    expect(() => resolveServiceUrl('users-api', 'definitely not a url')).toThrow();
  });

  test('validates the env-var fallback scheme', () => {
    process.env[ENV_KEY] = 'gopher://metadata/';
    expect(() => resolveServiceUrl('users-api')).toThrow(/scheme/);
  });

  test('accepts a valid env-var URL', () => {
    process.env[ENV_KEY] = 'https://users.internal:8443';
    expect(resolveServiceUrl('users-api')).toBe('https://users.internal:8443');
  });

  test('throws a helpful error when nothing is configured', () => {
    expect(() => resolveServiceUrl('users-api')).toThrow(/No URL configured/);
  });
});
