import type { HttpMiddleware } from '@putnami/application';
import type React from 'react';
import type { ClientSecurityRequirement } from '../shared/security.types';
import { withMiddleware } from './builder-mixin';

// ---------------------------------------------------------------------------
// ErrorDefinition — the object produced by error().render(Component)
// ---------------------------------------------------------------------------

const ERROR_MARKER = 'putnami:error' as const;

export interface ErrorDefinition {
  readonly __error: typeof ERROR_MARKER;
  readonly component: React.ComponentType;
  readonly middleware: readonly HttpMiddleware[];
  readonly statusCode?: number;
  /**
   * Declarative security requirement, populated when `.secure()` was called.
   * Surfaced for the SSR generator so it can attach it to the client route.
   */
  readonly security?: ClientSecurityRequirement;
}

export function isErrorDefinition(value: unknown): value is ErrorDefinition {
  return typeof value === 'object' && value !== null && (value as ErrorDefinition).__error === ERROR_MARKER;
}

// ---------------------------------------------------------------------------
// ErrorBuilder — fluent API built by error()
// ---------------------------------------------------------------------------

class ErrorBuilderBase {
  _middleware: HttpMiddleware[] = [];
  _security?: ClientSecurityRequirement;
}

export class ErrorBuilder extends withMiddleware(ErrorBuilderBase) {
  private _statusCode?: number;

  /** Set the HTTP status code for this error boundary */
  status(code: number): this {
    this._statusCode = code;
    return this;
  }

  /** Finalise the error definition with the error component */
  render(component: React.ComponentType): ErrorDefinition {
    return {
      __error: ERROR_MARKER,
      component,
      middleware: [...this._middleware],
      statusCode: this._statusCode,
      ...(this._security ? { security: this._security } : {}),
    };
  }
}

// ---------------------------------------------------------------------------
// error() — entry point
// ---------------------------------------------------------------------------

/**
 * Declare an error boundary with its configuration (security, CORS,
 * rate limiting) and component in a single file.
 *
 * Export the result as the default export of your `error.tsx`.
 *
 * **Example:**
 * ```ts
 * // src/app/error.tsx
 * import { error } from '@putnami/web';
 *
 * export default error()
 *   .status(500)
 *   .render(function ErrorPage() {
 *     return <h1>Something went wrong</h1>;
 *   });
 * ```
 */
export function error(): ErrorBuilder {
  return new ErrorBuilder();
}
