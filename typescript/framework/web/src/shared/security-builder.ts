import type { ClientSecurityRequirement } from './security.types';

// ---------------------------------------------------------------------------
// Shared `secure()` normalization
//
// Both the SSR and browser bundles need to derive a serializable
// {@link ClientSecurityRequirement} from the same argument shapes that
// `secure()` accepts:
//
// - No args → "requires authentication only"
// - Object with roles/scopes → captures declarative requirements
// - Guard function → can't be serialized, marked `indeterminate` so client
//   gating default-denies instead of silently widening to "any authenticated"
//
// JWT-specific fields (issuer, audience, client, scopeClaim, roleClaim,
// verify) are intentionally stripped — those are enforced server-side
// only and must not leak into client metadata.
// ---------------------------------------------------------------------------

/**
 * Normalize `secure()` arguments into a serializable client-side requirement.
 *
 * Used by both `@putnami/web` server builders (to surface the requirement
 * via `PageDefinition.security`) and client builders (to attach the same
 * shape to `ClientDefinition.security`). Keeping a single normalizer ensures
 * the browser and SSR bundles produce equivalent metadata for the React
 * Router generator to consume.
 */
// biome-ignore lint/suspicious/noExplicitAny: must accept @putnami/application's SecurityOptions | SecurityGuard without importing it into browser bundles
export function normalizeClientSecurity(optionsOrGuard?: any): ClientSecurityRequirement {
  if (typeof optionsOrGuard === 'function') {
    // Custom guard functions can't be checked client-side. Mark the requirement
    // as indeterminate so client gating default-denies — degrading to plain
    // "requires authentication" would reveal guard-gated UI (e.g. admin links)
    // to every logged-in user. The server still enforces the real guard.
    return { authenticated: true, indeterminate: true };
  }
  if (optionsOrGuard && typeof optionsOrGuard === 'object') {
    return {
      authenticated: true,
      ...(optionsOrGuard.roles ? { roles: optionsOrGuard.roles } : {}),
      ...(optionsOrGuard.rolesAny ? { rolesAny: optionsOrGuard.rolesAny } : {}),
      ...(optionsOrGuard.scopes ? { scopes: optionsOrGuard.scopes } : {}),
      ...(optionsOrGuard.scopesAny ? { scopesAny: optionsOrGuard.scopesAny } : {}),
    };
  }
  return { authenticated: true };
}
