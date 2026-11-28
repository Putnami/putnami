import { shouldExposeErrorStack } from '@putnami/runtime';
import React, { type ReactNode } from 'react';
import { isRouteErrorResponse, useRouteError } from 'react-router';

/**
 * Error boundary component that renders the not-found page for 404 errors
 * thrown by child route loaders, and falls back to the existing error element.
 */
export function NotFoundErrorBoundary({
  notFoundElement,
  fallbackErrorElement,
}: {
  notFoundElement: ReactNode;
  fallbackErrorElement?: ReactNode;
}) {
  const error = useRouteError();
  if (isRouteErrorResponse(error) && error.status === 404) {
    return notFoundElement;
  }
  return fallbackErrorElement ?? React.createElement(DefaultErrorBoundary);
}

/**
 * Lazy version of NotFoundErrorBoundary for lazy-loaded not-found components.
 */
export function LazyNotFoundErrorBoundary({
  lazyNotFoundElement,
  fallbackErrorElement,
}: {
  lazyNotFoundElement: ReactNode;
  fallbackErrorElement?: ReactNode;
}) {
  const error = useRouteError();
  if (isRouteErrorResponse(error) && error.status === 404) {
    return lazyNotFoundElement;
  }
  return fallbackErrorElement ?? React.createElement(DefaultErrorBoundary);
}

/** The message and stack a default boundary shows in development, for an `Error` or a route error response. */
function describeRouteError(error: unknown): { message: string; stack?: string } {
  if (isRouteErrorResponse(error)) {
    const data = error.data as { message?: unknown } | null | undefined;
    const message = typeof data?.message === 'string' ? data.message : undefined;
    return { message: `${error.status} ${error.statusText}${message ? `: ${message}` : ''}` };
  }
  if (error instanceof Error) return { message: error.message, stack: error.stack };
  // A hydrated error arrives as the `{ message, status }` `sanitizeErrors` kept of it.
  const message = (error as { message?: unknown } | null | undefined)?.message;
  return { message: typeof message === 'string' ? message : String(error) };
}

export function DefaultErrorBoundary() {
  const error = describeRouteError(useRouteError());
  // Only expose the error message and stack in development. In production this
  // boundary is the default fallback, so rendering `error.stack` would disclose
  // file paths and internal structure over the wire. Mirrors the client
  // ErrorBoundary and the server-side handleRenderError guard.
  //
  // The decision must be taken at render time: written inline,
  // `process.env.NODE_ENV !== 'production'` is constant-folded to `true` when
  // this package is published, so the consumer's `NODE_ENV` never reached it.
  const isDev = shouldExposeErrorStack();
  return React.createElement(
    React.Fragment,
    null,
    React.createElement('h2', null, 'Oups, we met an issue!'),
    React.createElement(
      'div',
      null,
      React.createElement('h3', null, isDev ? error.message : 'An unexpected error occurred'),
      isDev && error.stack ? React.createElement('pre', null, error.stack) : null,
    ),
  );
}
