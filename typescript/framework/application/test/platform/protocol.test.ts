import { describe, expect, it } from 'bun:test';
import {
  CANONICAL_PATHS,
  DEFAULT_PROBE_TIMEOUT_MS,
  type Envelope,
  HTTP_STATUS_OK,
  HTTP_STATUS_UNAVAILABLE,
  PATH_HEALTHZ,
  PATH_LIVEZ,
  PATH_READYZ,
  PATH_VERSION,
  PROTOCOL_VERSION,
  STATUS_DEGRADED,
  STATUS_OK,
  STATUS_UNAVAILABLE,
  httpStatusFor,
  joinPrefix,
  normalizePrefix,
  validateEnvelope,
  validatePrefix,
  validateProbeName,
  validateStatus,
} from '../../src/platform';

// --- conformance: the canonical contract ---

describe('protocol conformance', () => {
  it('pins protocol version', () => {
    // Bumping requires the matching Go-side bump + a migration story.
    expect(PROTOCOL_VERSION).toBe(1);
  });

  it('pins canonical paths and ordering', () => {
    expect(CANONICAL_PATHS).toEqual([PATH_LIVEZ, PATH_HEALTHZ, PATH_READYZ, PATH_VERSION]);
  });

  it('pins HTTP status mapping (200 / 503)', () => {
    expect(httpStatusFor(STATUS_OK)).toBe(HTTP_STATUS_OK);
    expect(httpStatusFor(STATUS_UNAVAILABLE)).toBe(HTTP_STATUS_UNAVAILABLE);
    expect(httpStatusFor(STATUS_DEGRADED)).toBe(HTTP_STATUS_UNAVAILABLE);
    expect(httpStatusFor('bogus' as 'ok')).toBeUndefined();
  });

  it('pins the default probe timeout (5s)', () => {
    expect(DEFAULT_PROBE_TIMEOUT_MS).toBe(5000);
  });
});

// --- normalizePrefix / joinPrefix ---

describe('normalizePrefix', () => {
  const cases: [string, string][] = [
    ['', ''],
    [' ', ''],
    ['/', ''],
    ['//', ''],
    ['/_', '/_'],
    ['/_/', '/_'],
    ['_', '/_'],
    ['/admin', '/admin'],
    ['/admin/', '/admin'],
    ['/admin//', '/admin'],
    ['//admin', '/admin'],
    [' admin', '/admin'],
    [' /admin/ ', '/admin'],
    ['admin/', '/admin'],
    ['/api/v1', '/api/v1'],
    ['/ /', ''],
    ['/ v1', '/v1'],
    ['v1 /', '/v1'],
    ['\t/v1/\n', '/v1'],
    ['/api//v1', '/api//v1'],
    ['\v/v1/\f\r', '/v1'],
    ['\u00a0v1', '/\u00a0v1'],
  ];

  it.each(cases)('normalizes %p to %p', (input, expected) => {
    expect(normalizePrefix(input)).toBe(expected);
  });
});

describe('joinPrefix', () => {
  it('produces canonical mount paths regardless of prefix shape', () => {
    expect(joinPrefix('', PATH_HEALTHZ)).toBe('/healthz');
    expect(joinPrefix('/', PATH_HEALTHZ)).toBe('/healthz');
    expect(joinPrefix('/_', PATH_HEALTHZ)).toBe('/_/healthz');
    expect(joinPrefix('/_/', PATH_HEALTHZ)).toBe('/_/healthz');
    expect(joinPrefix('_', PATH_HEALTHZ)).toBe('/_/healthz');
    expect(joinPrefix('/admin/', PATH_LIVEZ)).toBe('/admin/livez');
    expect(joinPrefix('//admin', PATH_LIVEZ)).toBe('/admin/livez');
    expect(joinPrefix(' admin', PATH_LIVEZ)).toBe('/admin/livez');
    expect(joinPrefix('/ admin /', PATH_LIVEZ)).toBe('/admin/livez');
  });
});

// --- validateProbeName ---

describe('validateProbeName', () => {
  it('accepts canonical names', () => {
    for (const name of ['db', 'db-pool', 'cache.l1', 'upstream/billing', 'a', 'x_y']) {
      expect(validateProbeName(name)).toEqual([]);
    }
  });

  it('rejects invalid names', () => {
    const cases = ['', '-leading-dash', '.leading-dot', 'UPPERCASE', 'db pool', 'db:pool', 'a'.repeat(65)];
    for (const name of cases) {
      expect(validateProbeName(name).length).toBeGreaterThan(0);
    }
  });
});

// --- validateStatus ---

describe('validateStatus', () => {
  it('accepts canonical statuses', () => {
    expect(validateStatus(STATUS_OK)).toEqual([]);
    expect(validateStatus(STATUS_UNAVAILABLE)).toEqual([]);
    expect(validateStatus(STATUS_DEGRADED)).toEqual([]);
  });

  it('rejects unknown statuses', () => {
    expect(validateStatus('bogus').length).toBe(1);
  });
});

// --- validateEnvelope ---

describe('validateEnvelope', () => {
  it('accepts ok with passing checks', () => {
    const env: Envelope = { status: STATUS_OK, checks: { db: 'ok', cache: 'ok' } };
    expect(validateEnvelope(env)).toEqual([]);
  });

  it('rejects ok when a probe is failing — runtime should have set degraded', () => {
    const env: Envelope = { status: STATUS_OK, checks: { db: 'ok', cache: 'conn refused' } };
    expect(validateEnvelope(env).length).toBeGreaterThan(0);
  });

  it('rejects degraded with no failing probes', () => {
    const env: Envelope = { status: STATUS_DEGRADED, checks: { db: 'ok' } };
    expect(validateEnvelope(env).length).toBeGreaterThan(0);
  });

  it('accepts degraded with at least one failure', () => {
    const env: Envelope = { status: STATUS_DEGRADED, checks: { db: 'conn refused', cache: 'ok' } };
    expect(validateEnvelope(env)).toEqual([]);
  });

  it('rejects unavailable when checks are present — runtime should have short-circuited', () => {
    const env: Envelope = { status: STATUS_UNAVAILABLE, checks: { db: 'ok' } };
    expect(validateEnvelope(env).length).toBeGreaterThan(0);
  });

  it('accepts unavailable without checks', () => {
    const env: Envelope = { status: STATUS_UNAVAILABLE };
    expect(validateEnvelope(env)).toEqual([]);
  });
});

// --- validatePrefix ---

describe('validatePrefix', () => {
  it('canonical inputs always pass', () => {
    for (const input of ['', '/', '/_', '/admin/', 'admin']) {
      expect(validatePrefix(input)).toEqual([]);
    }
  });
});
