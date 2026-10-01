import { beforeEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import * as browserEntry from '../../src/index.browser';
import { checkAccess } from '../../src/client/security/check-access';
import {
  getSecurityContext,
  SecurityContextProvider,
  setSecurityContext,
  useSecurityContext,
} from '../../src/client/security/security-context';
import { ANONYMOUS_SECURITY_CONTEXT } from '../../src/shared/security.types';

describe('client security runtime', () => {
  beforeEach(() => {
    setSecurityContext(ANONYMOUS_SECURITY_CONTEXT);
  });

  it('re-exports security utilities through the browser entrypoint', () => {
    expect(browserEntry.checkAccess).toBe(checkAccess);
    expect(browserEntry.SecurityContextProvider).toBe(SecurityContextProvider);
  });

  it('stores the module-level security context', () => {
    const context = {
      authenticated: true,
      roles: ['admin'],
      scopes: ['docs:read'],
    };

    setSecurityContext(context);

    expect(getSecurityContext()).toBe(context);
  });

  specTest(
    'provides the current security context to React consumers',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'browser-security-hint',
      check: 'the-serialized-context-is-what-react-consumers-read',
    },
    () => {
      const context = {
        authenticated: true,
        roles: ['admin'],
        scopes: ['docs:read'],
      };

      const Consumer = () => {
        const value = useSecurityContext();
        return React.createElement('pre', null, JSON.stringify(value));
      };

      const html = renderToStaticMarkup(
        React.createElement(SecurityContextProvider, { value: context }, React.createElement(Consumer)),
      );

      expect(html).toContain('&quot;authenticated&quot;:true');
      expect(html).toContain('admin');
      expect(html).toContain('docs:read');
    },
  );

  specTest(
    'evaluates authentication, role, and scope requirements',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'browser-security-hint',
      check: 'the-client-evaluates-authentication-role-and-scope-facts',
    },
    () => {
      const context = {
        authenticated: true,
        roles: ['admin', 'editor'],
        scopes: ['docs:read', 'docs:write'],
      };

      expect(checkAccess(context, { authenticated: true })).toBe(true);
      expect(checkAccess(ANONYMOUS_SECURITY_CONTEXT, { authenticated: true })).toBe(false);
      expect(checkAccess(context, { roles: ['admin'] })).toBe(true);
      expect(checkAccess(context, { roles: ['admin', 'ops'] })).toBe(false);
      expect(checkAccess(context, { rolesAny: ['viewer', 'editor'] })).toBe(true);
      expect(checkAccess(context, { scopes: ['docs:read', 'docs:write'] })).toBe(true);
      expect(checkAccess(context, { scopesAny: ['billing:read', 'docs:write'] })).toBe(true);
      expect(checkAccess(context, { scopesAny: ['billing:read'] })).toBe(false);
    },
  );

  specTest(
    'default-denies indeterminate (guard-gated) requirements even for authenticated users',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'browser-security-hint',
      check: 'a-guard-gated-requirement-is-indeterminate-and-denied-on-the-client',
    },
    () => {
      const admin = {
        authenticated: true,
        roles: ['admin'],
        scopes: ['docs:read'],
      };

      // A server-side guard function can't be replayed on the client, so the
      // requirement is marked indeterminate. checkAccess must hide the UI rather
      // than reveal it to every authenticated user.
      expect(checkAccess(admin, { authenticated: true, indeterminate: true })).toBe(false);
      expect(checkAccess(ANONYMOUS_SECURITY_CONTEXT, { authenticated: true, indeterminate: true })).toBe(false);

      // Sanity: without the flag, an authenticated-only requirement still passes.
      expect(checkAccess(admin, { authenticated: true })).toBe(true);
    },
  );
});
