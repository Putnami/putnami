import type React from 'react';
import { normalizeClientSecurity } from '../shared/security-builder';
import type { ClientSecurityRequirement } from '../shared/security.types';

// ---------------------------------------------------------------------------
// Client-side stubs — lightweight versions without server middleware
//
// These stubs exist so that shared page/layout definitions can be imported
// in browser bundles without pulling in server-only dependencies.
// Middleware calls (cors, rateLimit, etc.) are no-ops on the client.
//
// secure() is special: it captures declarative security requirements so the
// client can expose route metadata via React Router's `handle` property.
// This enables conditional rendering (e.g. hiding admin links) without
// pulling in server-only dependencies.
//
// Method parity with the SSR builders is intentional — shared page.tsx
// modules are compiled into both bundles, so any method available on one
// builder must also exist on the other.
// ---------------------------------------------------------------------------

const CLIENT_MW_WARNING =
  '[putnami] Middleware methods (cors, rateLimit, etc.) are server-only and have no effect on the client.';
let warned = false;
function warnOnce(): void {
  if (!warned) {
    warned = true;
    // biome-ignore lint/suspicious/noConsole: surfacing a server-only-method misuse warning to the developer
    console.warn(CLIENT_MW_WARNING);
  }
}

/**
 * @internal Reset the module-scoped warn-once latch. The latch is process-global,
 * so any test asserting the "warn exactly once" behavior must reset it first to
 * stay independent of which other test file tripped it earlier in the same
 * `bun test` process.
 */
export function __resetClientMiddlewareWarning(): void {
  warned = false;
}

export interface ClientDefinition {
  readonly component: React.ComponentType;
  readonly security?: ClientSecurityRequirement;
}

class ClientBuilder {
  private _security?: ClientSecurityRequirement;

  cors(_options?: unknown): this {
    warnOnce();
    return this;
  }
  rateLimit(_options?: unknown): this {
    warnOnce();
    return this;
  }

  /**
   * Capture security requirements for client-side route metadata.
   *
   * Accepts the same argument shapes as the server-side `secure()` for
   * type compatibility in shared page.tsx files:
   * - No args → requires authentication only
   * - Object with roles/scopes → captures declarative requirements
   * - Guard function → can't be serialized, marks the requirement indeterminate
   */
  // biome-ignore lint/suspicious/noExplicitAny: must accept server-side SecurityOptions | SecurityGuard without importing @putnami/application
  secure(optionsOrGuard?: any): this {
    this._security = normalizeClientSecurity(optionsOrGuard);
    return this;
  }

  status(_code?: unknown): this {
    return this;
  }
  cache(_options?: unknown): this {
    warnOnce();
    return this;
  }
  use(_middleware?: unknown): this {
    warnOnce();
    return this;
  }

  render(component: React.ComponentType): ClientDefinition {
    return {
      component,
      ...(this._security ? { security: this._security } : {}),
    };
  }
}

export function page(): ClientBuilder {
  return new ClientBuilder();
}

export function layout(): ClientBuilder {
  return new ClientBuilder();
}

export function error(): ClientBuilder {
  return new ClientBuilder();
}

export function notFound(): ClientBuilder {
  return new ClientBuilder();
}
