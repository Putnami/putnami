import { afterAll, describe, expect, it, mock } from 'bun:test';
import React from 'react';
import { renderToStaticMarkup } from 'react-dom/server';
import NotFoundHtml from '../../src/ssr/not-found.html';

let routeError: unknown = new Error('boom');

// Capture the real react-router exports before installing the module mock so we
// can restore them afterwards. bun's `mock.restore()` does NOT undo a
// `mock.module()`, and module-mock isolation across test files is not
// guaranteed — without an explicit restore this stub leaks into other suites
// (e.g. the SSR not-found tests in react-application.test.ts, which since the
// react-router-dom → react-router consolidation share this very module).
const realReactRouter = { ...(await import('react-router')) };

mock.module('react-router', () => ({
  useRouteError: () => routeError,
  isRouteErrorResponse: (error: unknown) => Boolean((error as { status?: number } | undefined)?.status),
}));

const { ErrorBoundary } = await import('../../src/client/error/error-boundary');
const { NotFoundErrorBoundary: ClientNotFoundErrorBoundary } = await import(
  '../../src/client/error/not-found-error-boundary'
);
const clientErrorExports = await import('../../src/client/error');

afterAll(() => {
  mock.module('react-router', () => realReactRouter);
});

describe('error boundaries', () => {
  it('re-exports client error boundaries', () => {
    expect(clientErrorExports.ErrorBoundary).toBe(ErrorBoundary);
    expect(clientErrorExports.NotFoundErrorBoundary).toBe(ClientNotFoundErrorBoundary);
  });

  it('renders the client error boundary with the current route error', () => {
    routeError = Object.assign(new Error('client boom'), { stack: 'client stack' });

    const html = renderToStaticMarkup(React.createElement(ErrorBoundary));

    expect(html).toContain('Oups, we met an issue!');
    expect(html).toContain('client boom');
    expect(html).toContain('client stack');
  });

  it('renders the client not-found boundary for 404 route errors', () => {
    routeError = { status: 404 };

    const clientHtml = renderToStaticMarkup(
      React.createElement(ClientNotFoundErrorBoundary, {
        notFoundElement: React.createElement('p', null, 'Client 404'),
      }),
    );

    expect(clientHtml).toContain('Client 404');
  });

  it('falls back to error UIs when the route error is not a 404', () => {
    routeError = Object.assign(new Error('fallback boom'), { stack: 'fallback stack' });

    const clientHtml = renderToStaticMarkup(
      React.createElement(ClientNotFoundErrorBoundary, {
        notFoundElement: React.createElement('p', null, 'unused'),
      }),
    );
    const fallbackHtml = renderToStaticMarkup(
      React.createElement(ClientNotFoundErrorBoundary, {
        notFoundElement: React.createElement('p', null, 'unused'),
        fallbackErrorElement: React.createElement('p', null, 'Client fallback'),
      }),
    );

    expect(clientHtml).toContain('fallback boom');
    expect(fallbackHtml).toContain('Client fallback');
  });

  it('hides stack trace and error message in production mode', () => {
    const originalEnv = process.env.NODE_ENV;
    process.env.NODE_ENV = 'production';
    try {
      routeError = Object.assign(new Error('secret details'), { stack: 'secret stack' });

      const html = renderToStaticMarkup(React.createElement(ErrorBoundary));

      expect(html).toContain('An unexpected error occurred');
      expect(html).not.toContain('secret details');
      expect(html).not.toContain('secret stack');
    } finally {
      if (originalEnv === undefined) {
        delete process.env.NODE_ENV;
      } else {
        process.env.NODE_ENV = originalEnv;
      }
    }
  });

  it('renders the SSR not-found document shell', () => {
    const html = renderToStaticMarkup(React.createElement(NotFoundHtml));

    expect(html).toContain('404 - page not found');
  });
});
