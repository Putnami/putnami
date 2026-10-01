import { describe, expect, it } from 'bun:test';
import {
  type Claims,
  hasAll,
  hasAny,
  readClaim,
  resolveClientClaims,
  resolveRoles,
  resolveScopes,
  splitTokens,
  toArray,
  toNonEmptyTuple,
} from '../../src/security/security.utils';

describe('security.utils', () => {
  describe('toArray()', () => {
    it('should return empty array for undefined', () => {
      expect(toArray(undefined)).toEqual([]);
    });

    it('should return empty array for empty string', () => {
      expect(toArray('')).toEqual([]);
    });

    it('should wrap a string in an array', () => {
      expect(toArray('hello')).toEqual(['hello']);
    });

    it('should return array as-is', () => {
      expect(toArray(['a', 'b'])).toEqual(['a', 'b']);
    });
  });

  describe('toNonEmptyTuple()', () => {
    it('should return undefined for undefined', () => {
      expect(toNonEmptyTuple(undefined)).toBeUndefined();
    });

    it('should return undefined for empty array', () => {
      expect(toNonEmptyTuple([])).toBeUndefined();
    });

    it('should return value for non-array', () => {
      expect(toNonEmptyTuple('hello')).toBe('hello');
    });

    it('should return tuple for non-empty array', () => {
      expect(toNonEmptyTuple(['a', 'b'])).toEqual(['a', 'b']);
    });
  });

  describe('splitTokens()', () => {
    it('should split by whitespace', () => {
      expect(splitTokens('read write delete', /\s+/)).toEqual(['read', 'write', 'delete']);
    });

    it('should trim items', () => {
      expect(splitTokens('  read   write  ', /\s+/)).toEqual(['read', 'write']);
    });

    it('should filter empty strings', () => {
      expect(splitTokens('', /\s+/)).toEqual([]);
    });

    it('should split by comma', () => {
      expect(splitTokens('admin,editor', /[,\s]+/)).toEqual(['admin', 'editor']);
    });
  });

  describe('readClaim()', () => {
    it('should read a top-level claim', () => {
      expect(readClaim({ scope: 'read' }, 'scope')).toBe('read');
    });

    it('should read a nested claim', () => {
      const claims: Claims = {
        realm_access: { roles: ['admin'] },
      };
      expect(readClaim(claims, 'realm_access.roles')).toEqual(['admin']);
    });

    it('should return undefined for missing path', () => {
      expect(readClaim({}, 'missing.path')).toBeUndefined();
    });

    it('should return undefined when intermediate is not an object', () => {
      expect(readClaim({ foo: 'bar' }, 'foo.baz')).toBeUndefined();
    });
  });

  describe('resolveScopes()', () => {
    it('should resolve from "scope" string claim', () => {
      const claims: Claims = { scope: 'read write' };
      expect(resolveScopes(claims, {})).toEqual(['read', 'write']);
    });

    it('should resolve from "scp" array claim', () => {
      const claims: Claims = { scp: ['read', 'write'] };
      expect(resolveScopes(claims, {})).toEqual(['read', 'write']);
    });

    it('should resolve from "scopes" array claim', () => {
      const claims: Claims = { scopes: ['openid', 'profile'] };
      expect(resolveScopes(claims, {})).toEqual(['openid', 'profile']);
    });

    it('should merge scopes from multiple claim sources', () => {
      const claims: Claims = { scope: 'read', scp: ['write'] };
      expect(resolveScopes(claims, {})).toEqual(['read', 'write']);
    });

    it('should deduplicate scopes', () => {
      const claims: Claims = { scope: 'read write', scp: ['read'] };
      expect(resolveScopes(claims, {})).toEqual(['read', 'write']);
    });

    it('should use custom scopeClaim', () => {
      const claims: Claims = { permissions: 'read write' } as Claims;
      expect(resolveScopes(claims, { scopeClaim: 'permissions' })).toEqual(['read', 'write']);
    });

    it('should return empty array when no scopes found', () => {
      expect(resolveScopes({}, {})).toEqual([]);
    });
  });

  describe('resolveRoles()', () => {
    it('should resolve from "roles" array claim', () => {
      const claims: Claims = { roles: ['admin', 'editor'] };
      expect(resolveRoles(claims, {})).toEqual(['admin', 'editor']);
    });

    it('should resolve from "role" string claim', () => {
      const claims: Claims = { role: 'admin' };
      expect(resolveRoles(claims, {})).toEqual(['admin']);
    });

    it('should resolve from "realm_access.roles"', () => {
      const claims: Claims = { realm_access: { roles: ['admin'] } };
      expect(resolveRoles(claims, {})).toEqual(['admin']);
    });

    it('should resolve client-scoped roles from resource_access', () => {
      const claims: Claims = {
        resource_access: {
          'my-app': { roles: ['editor'] },
        },
      };
      expect(resolveRoles(claims, { client: 'my-app' })).toEqual(['editor']);
    });

    it('should merge roles from multiple sources', () => {
      const claims: Claims = {
        roles: ['admin'],
        realm_access: { roles: ['user'] },
      };
      expect(resolveRoles(claims, {})).toEqual(['admin', 'user']);
    });

    it('should deduplicate roles', () => {
      const claims: Claims = {
        roles: ['admin'],
        role: 'admin',
      };
      expect(resolveRoles(claims, {})).toEqual(['admin']);
    });

    it('should use custom roleClaim', () => {
      const claims: Claims = { groups: ['admin'] } as Claims;
      expect(resolveRoles(claims, { roleClaim: 'groups' })).toEqual(['admin']);
    });
  });

  describe('hasAll()', () => {
    it('should return true when all required values are present', () => {
      expect(hasAll(['read', 'write', 'delete'], ['read', 'write'])).toBe(true);
    });

    it('should return false when some required values are missing', () => {
      expect(hasAll(['read'], ['read', 'write'])).toBe(false);
    });

    it('should return true for empty required list', () => {
      expect(hasAll(['read'], [])).toBe(true);
    });
  });

  describe('hasAny()', () => {
    it('should return true when at least one required value is present', () => {
      expect(hasAny(['read'], ['read', 'write'])).toBe(true);
    });

    it('should return false when no required values are present', () => {
      expect(hasAny(['delete'], ['read', 'write'])).toBe(false);
    });

    it('should return false for empty required list', () => {
      expect(hasAny(['read'], [])).toBe(false);
    });
  });

  describe('resolveClientClaims()', () => {
    it('should resolve from azp', () => {
      expect(resolveClientClaims({ azp: 'my-app' })).toEqual(['my-app']);
    });

    it('should resolve from client_id', () => {
      expect(resolveClientClaims({ client_id: 'my-app' })).toEqual(['my-app']);
    });

    it('should resolve from clientId', () => {
      expect(resolveClientClaims({ clientId: 'my-app' })).toEqual(['my-app']);
    });

    it('should resolve from aud string', () => {
      expect(resolveClientClaims({ aud: 'my-app' })).toEqual(['my-app']);
    });

    it('should resolve from aud array', () => {
      expect(resolveClientClaims({ aud: ['app-a', 'app-b'] })).toEqual(['app-a', 'app-b']);
    });

    it('should merge and deduplicate client claims', () => {
      expect(resolveClientClaims({ azp: 'my-app', client_id: 'my-app' })).toEqual(['my-app']);
    });

    it('should return empty array when no client claims', () => {
      expect(resolveClientClaims({})).toEqual([]);
    });
  });
});
