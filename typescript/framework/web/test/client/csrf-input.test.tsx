import { afterEach, describe, expect, it } from 'bun:test';
import { specTest } from '@putnami/runtime/spectest';
import React from 'react';
import { renderToString } from 'react-dom/server';
import { CsrfInput } from '../../src/client/form/csrf-input';
import { CsrfTokenContext } from '../../src/shared/csrf-context';

describe('CsrfInput', () => {
  afterEach(() => {
    Reflect.deleteProperty(globalThis, 'document');
  });

  specTest(
    'renders a hidden input with the CSRF token from context',
    {
      feature: 'typescript/web-application-delivery',
      requirement: 'csrf-and-csp',
      check: 'a-form-carries-the-csrf-token-as-a-hidden-input',
    },
    () => {
      const html = renderToString(
        React.createElement(CsrfTokenContext.Provider, { value: 'test-token-123' }, React.createElement(CsrfInput)),
      );
      expect(html).toContain('type="hidden"');
      expect(html).toContain('name="_csrf"');
      expect(html).toContain('value="test-token-123"');
    },
  );

  it('renders nothing when no token is available', () => {
    const html = renderToString(
      React.createElement(CsrfTokenContext.Provider, { value: undefined }, React.createElement(CsrfInput)),
    );
    expect(html).toBe('');
  });

  it('supports a custom field name', () => {
    const html = renderToString(
      React.createElement(
        CsrfTokenContext.Provider,
        { value: 'abc' },
        React.createElement(CsrfInput, { name: 'csrf_token' }),
      ),
    );
    expect(html).toContain('name="csrf_token"');
    expect(html).toContain('value="abc"');
  });
});
