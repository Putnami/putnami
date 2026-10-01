import { ClaimName } from './identity.constants';
import type { SecurityOptions } from './security.types';

const TIMING_SAFE_ENCODER = new TextEncoder();

/**
 * Constant-time comparison of two byte sequences. Every byte is compared and a
 * length difference is folded into the result, so neither the first mismatching
 * position nor a length difference is revealed through an early exit. This is the
 * shared primitive for any secret/digest comparison (credential digests, HMACs,
 * OIDC nonces) — mirror of Go's `crypto/subtle.ConstantTimeCompare`.
 */
export const timingSafeEqualBytes = (a: Uint8Array, b: Uint8Array): boolean => {
  const len = Math.max(a.length, b.length);
  let diff = a.length ^ b.length;
  for (let i = 0; i < len; i++) diff |= (a[i] ?? 0) ^ (b[i] ?? 0);
  return diff === 0;
};

/**
 * Constant-time string comparison. Both arguments are compared as their UTF-8
 * bytes via {@link timingSafeEqualBytes}, so the length-leak via early exit is
 * also avoided.
 */
export const timingSafeEqual = (a: string, b: string): boolean =>
  timingSafeEqualBytes(TIMING_SAFE_ENCODER.encode(a), TIMING_SAFE_ENCODER.encode(b));

export interface Claims {
  iss?: string;
  aud?: string | string[];
  scope?: string;
  scp?: string[];
  scopes?: string[];
  roles?: string[] | string;
  role?: string | string[];
  azp?: string;
  client_id?: string;
  clientId?: string;
  realm_access?: { roles?: string[] };
  resource_access?: Record<string, { roles?: string[] }>;
  [key: string]: unknown;
}

// Alias-first adoption: the contract-owned wire names (ClaimName.Scope ===
// 'scope', ClaimName.Roles === 'roles') replace the prior string literals, so
// the default claim paths stay byte-identical while tracking the vocabulary.
// The non-standard fallbacks ('scp'/'scopes', 'role', 'realm_access.roles') are
// not part of the contract and remain plain literals.
const DEFAULT_SCOPE_CLAIMS = [ClaimName.Scope, 'scp', 'scopes'];
const DEFAULT_ROLE_CLAIMS = [ClaimName.Roles, 'role', 'realm_access.roles'];

export const toArray = (value?: string | string[]): string[] => {
  if (!value) return [];
  return Array.isArray(value) ? value : [value];
};

export const toNonEmptyTuple = <T>(value?: T | T[]): T | [T, ...T[]] | undefined => {
  if (value === undefined) return undefined;
  if (Array.isArray(value)) {
    return value.length ? [value[0], ...value.slice(1)] : undefined;
  }
  return value;
};

export const splitTokens = (value: string, pattern: RegExp): string[] =>
  value
    .split(pattern)
    .map((item) => item.trim())
    .filter(Boolean);

export const readClaim = (claims: Claims, path: string): unknown => {
  const parts = path.split('.');
  let current: unknown = claims;
  for (const part of parts) {
    if (!current || typeof current !== 'object') return undefined;
    current = (current as Record<string, unknown>)[part];
  }
  return current;
};

export const resolveScopes = (claims: Claims, options: SecurityOptions): string[] => {
  const scopes: string[] = [];
  const paths = options.scopeClaim ? toArray(options.scopeClaim) : DEFAULT_SCOPE_CLAIMS;

  for (const path of paths) {
    const value = readClaim(claims, path);
    if (typeof value === 'string') {
      scopes.push(...splitTokens(value, /\s+/));
    } else if (Array.isArray(value)) {
      scopes.push(...value.filter((item): item is string => typeof item === 'string'));
    }
  }

  return [...new Set(scopes)];
};

export const resolveRoles = (claims: Claims, options: SecurityOptions): string[] => {
  const roles: string[] = [];
  const paths = options.roleClaim ? toArray(options.roleClaim) : DEFAULT_ROLE_CLAIMS;

  for (const path of paths) {
    const value = readClaim(claims, path);
    if (typeof value === 'string') {
      roles.push(...splitTokens(value, /[\s,]+/));
    } else if (Array.isArray(value)) {
      roles.push(...value.filter((item): item is string => typeof item === 'string'));
    }
  }

  const clients = toArray(options.client);
  if (clients.length && claims.resource_access && typeof claims.resource_access === 'object') {
    for (const client of clients) {
      const access = claims.resource_access[client];
      if (access?.roles) {
        roles.push(...access.roles);
      }
    }
  }

  return [...new Set(roles)];
};

export const hasAll = (values: string[], required: string[]): boolean =>
  required.every((item) => values.includes(item));

export const hasAny = (values: string[], required: string[]): boolean => required.some((item) => values.includes(item));

/**
 * Fail-closed check that the token's `iss` claim is one of the expected issuers.
 * Returns `false` when the claim is missing or does not match.
 */
export const claimMatchesIssuer = (claims: Claims, expected: string | string[]): boolean => {
  const issuers = toArray(expected);
  if (!issuers.length) return true;
  return typeof claims.iss === 'string' && issuers.includes(claims.iss);
};

/**
 * Fail-closed check that the token's `aud` claim contains one of the expected
 * audiences. Returns `false` when the claim is missing or does not match.
 */
export const claimMatchesAudience = (claims: Claims, expected: string | string[]): boolean => {
  const audiences = toArray(expected);
  if (!audiences.length) return true;
  const tokenAud = Array.isArray(claims.aud) ? claims.aud : claims.aud ? [claims.aud] : [];
  return tokenAud.some((aud) => typeof aud === 'string' && audiences.includes(aud));
};

export const resolveClientClaims = (claims: Claims): string[] => {
  const clients: string[] = [];
  if (typeof claims.azp === 'string') clients.push(claims.azp);
  if (typeof claims.client_id === 'string') clients.push(claims.client_id);
  if (typeof claims.clientId === 'string') clients.push(claims.clientId);
  if (typeof claims.aud === 'string') clients.push(claims.aud);
  if (Array.isArray(claims.aud)) {
    clients.push(...claims.aud.filter((item): item is string => typeof item === 'string'));
  }
  return [...new Set(clients)];
};
