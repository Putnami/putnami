/**
 * Minimal security context serialized from SSR to the client.
 * Contains ONLY non-sensitive derived claims — never tokens or PII.
 *
 * This is a UX convenience, not real security. The server always enforces
 * via {@link SecurityMiddleware}.
 */
export interface ClientSecurityContext {
  /** Whether the user has a valid authenticated session. */
  readonly authenticated: boolean;
  /** Normalized role names resolved from JWT claims. */
  readonly roles: readonly string[];
  /** Normalized scope names resolved from JWT claims. */
  readonly scopes: readonly string[];
}

/**
 * Declarative security requirement attached to a client route.
 * Only includes fields that can be evaluated client-side.
 *
 * JWT-specific fields (issuer, audience, client, scopeClaim, roleClaim)
 * are excluded — they are enforced server-side only.
 */
export interface ClientSecurityRequirement {
  /** Whether authentication is required. Always `true` when present. */
  readonly authenticated: boolean;
  /** Require ALL of these roles. */
  readonly roles?: readonly string[];
  /** Require ANY of these roles. */
  readonly rolesAny?: readonly string[];
  /** Require ALL of these scopes. */
  readonly scopes?: readonly string[];
  /** Require ANY of these scopes. */
  readonly scopesAny?: readonly string[];
  /**
   * The server enforces this route with a custom guard function that cannot be
   * evaluated on the client (e.g. `secure((user, ctx) => ...)`). Set when
   * `secure()` receives a function so client-side gating can default-deny rather
   * than silently widen access to every authenticated user. The server remains
   * the source of truth — this only governs optimistic UI hints like hiding links.
   */
  readonly indeterminate?: boolean;
}

/** Default context for unauthenticated users. */
export const ANONYMOUS_SECURITY_CONTEXT: ClientSecurityContext = {
  authenticated: false,
  roles: [],
  scopes: [],
};
