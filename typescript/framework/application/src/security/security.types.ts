import type { HttpRequestContext } from '../http/http-context.type';
import type { PrincipalKind } from './identity.constants';

/**
 * The authenticated principal exposed as `ctx.user`.
 *
 * Typed slots for the well-known claims the framework consumes (mirroring the
 * `protocols/identity` vocabulary) sit alongside an index signature that keeps
 * every other claim readable by key. The index signature is load-bearing:
 * existing raw-claims readers (`ctx.user['someClaim']`) and writers that assign
 * a plain claims record keep working unchanged after the typed retype.
 */
export interface Principal {
  /** Subject: the principal identifier (`sub`). */
  sub?: string;
  /** Issuer that minted the credential (`iss`). */
  iss?: string;
  /** OAuth2 client the credential was issued to (`client_id`). */
  client_id?: string;
  /** Audience(s) the credential is intended for (`aud`). */
  aud?: string | string[];
  /** Role names assigned to the principal (`roles`). */
  roles?: string[] | string;
  /** Scopes granted to the principal (`scope`). */
  scope?: string;
  /** Expiration in seconds since the Unix epoch (`exp`). */
  exp?: number;
  /** The recognized kind of principal, when known. */
  kind?: PrincipalKind;
  /** Any other claim, read by key. */
  [key: string]: unknown;
}

/**
 * Custom token verifier for endpoints that manage their own authentication.
 *
 * Extracts a Bearer token from the request and returns the verified claims,
 * or `undefined` if the token is invalid. When provided, the global
 * identity resolver is bypassed and this function is used instead.
 */
export type TokenVerifier = (
  token: string,
) => Record<string, unknown> | undefined | Promise<Record<string, unknown> | undefined>;

/**
 * Declarative access requirements for a handler or module.
 *
 * When applied via `.secure(options)`, the security middleware checks
 * the authenticated user's claims against these requirements:
 * - Missing identity → 401 Unauthorized
 * - Insufficient permissions → 403 Forbidden
 * - All checks pass → request proceeds
 */
export interface SecurityOptions {
  /**
   * Custom token verifier. When set, the middleware extracts the Bearer token
   * from the Authorization header and calls this function to verify it.
   * The returned claims are set as `ctx.user`. If verification returns
   * `undefined`, the request gets 401 Unauthorized.
   *
   * Use this when your app is its own auth server and cannot rely on the
   * global identity resolver (e.g., OAuth/OIDC providers).
   */
  verify?: TokenVerifier;
  /** Expected issuer claim(s). */
  issuer?: string | string[];
  /** Expected audience claim(s). */
  audience?: string | string[];
  /**
   * Require the authenticated principal to be one of these kinds (bearer `user`
   * vs static `apikey`). A request whose principal kind does not match is denied
   * (`deny_client`). When unset, any principal kind is accepted.
   */
  principalKind?: PrincipalKind | PrincipalKind[];
  /** Expected client id(s). Checked against azp/client_id/aud. */
  client?: string | string[];
  /** Require ALL of these scopes. */
  scopes?: string[];
  /** Require ANY of these scopes. */
  scopesAny?: string[];
  /** Require ALL of these roles. */
  roles?: string[];
  /** Require ANY of these roles. */
  rolesAny?: string[];
  /** Override the claim path(s) used to resolve scopes. */
  scopeClaim?: string | string[];
  /** Override the claim path(s) used to resolve roles. */
  roleClaim?: string | string[];
  /**
   * Serve a request that presents no credential: no resolved identity and an
   * absent or empty `Authorization` header. Every other requirement applies to an
   * authenticated caller only. A request whose non-empty `Authorization` header
   * resolved no identity is still refused with 401, never served anonymously.
   * With `verify`, the verifier is the only identity source: a request without a
   * bearer token is anonymous even when another resolver set `ctx.user`. A first-party client
   * contract may then offer an anonymous alternative (`{ allOf: [] }`) after its
   * credentialed ones. The Go twin is `security.Options.Optional`.
   */
  optional?: boolean;
}

/**
 * Custom guard function for complex authorization logic.
 *
 * Receives the authenticated user's claims and the request context.
 * Return `true` to allow the request, `false` to deny with 403 Forbidden.
 *
 * @example
 * ```ts
 * endpoint()
 *   .secure((user, ctx) => user.organizationId === ctx.params?.orgId)
 *   .handle((ctx) => { ... });
 * ```
 */
export type SecurityGuard = (user: Record<string, unknown>, ctx: HttpRequestContext) => boolean | Promise<boolean>;
