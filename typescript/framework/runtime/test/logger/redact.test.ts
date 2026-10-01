import { afterEach, describe, expect, it } from 'bun:test';
import { REDACTED, addSensitiveKeys, redact, setSensitiveKeys } from '../../src/logger/redact';

describe('redact', () => {
  afterEach(() => {
    // Restore the default denylist after tests that mutate it.
    setSensitiveKeys();
  });

  it('masks default sensitive keys', () => {
    expect(redact({ password: 'hunter2' })).toEqual({ password: REDACTED });
    expect(redact({ token: 'abc' })).toEqual({ token: REDACTED });
    expect(redact({ apiKey: 'k' })).toEqual({ apiKey: REDACTED });
    expect(redact({ authorization: 'Bearer x' })).toEqual({ authorization: REDACTED });
    expect(redact({ secret: 's' })).toEqual({ secret: REDACTED });
  });

  it('matches keys case-insensitively', () => {
    expect(redact({ Password: 'p', APIKEY: 'k', Authorization: 'a' })).toEqual({
      Password: REDACTED,
      APIKEY: REDACTED,
      Authorization: REDACTED,
    });
  });

  it('matches exact key names only (does not normalize separators)', () => {
    // 'apiKey' is on the denylist; 'api_key' / 'api-key' are distinct keys.
    expect(redact({ apiKey: 'k', api_key: 'k2', 'api-key': 'k3' })).toEqual({
      apiKey: REDACTED,
      api_key: 'k2',
      'api-key': 'k3',
    });
  });

  it('leaves non-sensitive fields untouched', () => {
    expect(redact({ host: 'localhost', port: 5432, user: 'admin' })).toEqual({
      host: 'localhost',
      port: 5432,
      user: 'admin',
    });
  });

  it('redacts nested objects', () => {
    expect(redact({ db: { host: 'localhost', password: 'pw' } })).toEqual({
      db: { host: 'localhost', password: REDACTED },
    });
  });

  it('redacts inside arrays', () => {
    expect(redact([{ token: 'a' }, { token: 'b' }])).toEqual([{ token: REDACTED }, { token: REDACTED }]);
  });

  it('masks the key regardless of value type', () => {
    expect(redact({ password: { nested: 'x' } })).toEqual({ password: REDACTED });
    expect(redact({ token: 123 })).toEqual({ token: REDACTED });
    expect(redact({ secret: null })).toEqual({ secret: REDACTED });
  });

  it('returns primitives unchanged', () => {
    expect(redact('plain')).toBe('plain');
    expect(redact(42)).toBe(42);
    expect(redact(null)).toBe(null);
    expect(redact(undefined)).toBe(undefined);
  });

  it('does not mutate the input object', () => {
    const input = { password: 'pw', nested: { token: 'tk' } };
    const out = redact(input) as Record<string, unknown>;
    expect(input.password).toBe('pw');
    expect(input.nested.token).toBe('tk');
    expect(out).not.toBe(input);
  });

  it('leaves Error and Date values intact (not treated as plain objects)', () => {
    const err = new Error('boom');
    const date = new Date('2024-01-01T00:00:00.000Z');
    const out = redact({ err, date }) as Record<string, unknown>;
    expect(out['err']).toBe(err);
    expect(out['date']).toBe(date);
  });

  it('breaks circular references without retaining the original object', () => {
    const circular: Record<string, unknown> = { name: 'a', password: 'pw' };
    circular['self'] = circular;
    expect(() => redact(circular)).not.toThrow();
    const out = redact(circular) as Record<string, unknown>;
    expect(out['name']).toBe('a');
    expect(out['password']).toBe(REDACTED);
    expect(out['self']).toBe('[Circular]');
    expect(out['self']).not.toBe(circular);
  });

  it('redacts repeated references independently when they are not circular', () => {
    const shared = { host: 'localhost', token: 'tk' };
    expect(redact({ first: shared, second: shared })).toEqual({
      first: { host: 'localhost', token: REDACTED },
      second: { host: 'localhost', token: REDACTED },
    });
  });

  it('supports adding extra sensitive keys', () => {
    addSensitiveKeys('ssn');
    expect(redact({ ssn: '123-45-6789', name: 'Jane' })).toEqual({ ssn: REDACTED, name: 'Jane' });
  });

  it('supports replacing the denylist', () => {
    setSensitiveKeys(['custom']);
    // 'password' is no longer redacted once the list is replaced.
    expect(redact({ password: 'pw', custom: 'c' })).toEqual({ password: 'pw', custom: REDACTED });
  });
});
