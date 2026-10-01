import type { ClientSecurityContext, ClientSecurityRequirement } from '../../shared/security.types';

/**
 * Check whether a security context satisfies a requirement.
 *
 * Uses the same AND/OR semantics as the server-side `SecurityMiddleware`:
 * - `roles`  / `scopes`    → user must have **ALL**
 * - `rolesAny` / `scopesAny` → user must have **at least one**
 *
 * When the requirement is `indeterminate` (the server enforces a custom guard
 * function that cannot be evaluated client-side), this default-denies so
 * guard-gated UI is hidden rather than shown to every authenticated user. The
 * server is always the source of truth; this only governs optimistic UI hints.
 *
 * Returns `true` if all checks pass, `false` otherwise.
 *
 * @example
 * ```ts
 * const { authenticated, roles, scopes } = useSecurityContext();
 * const allowed = checkAccess({ authenticated, roles, scopes }, { roles: ['admin'] });
 * ```
 */
export function checkAccess(ctx: ClientSecurityContext, requirement: ClientSecurityRequirement): boolean {
  // Server-side guard functions can't be replayed on the client. Default-deny
  // so guard-gated UI stays hidden instead of leaking to all authenticated users.
  if (requirement.indeterminate) {
    return false;
  }

  if (requirement.authenticated && !ctx.authenticated) {
    return false;
  }

  if (requirement.roles?.length && !requirement.roles.every((r) => ctx.roles.includes(r))) {
    return false;
  }

  if (requirement.rolesAny?.length && !requirement.rolesAny.some((r) => ctx.roles.includes(r))) {
    return false;
  }

  if (requirement.scopes?.length && !requirement.scopes.every((s) => ctx.scopes.includes(s))) {
    return false;
  }

  if (requirement.scopesAny?.length && !requirement.scopesAny.some((s) => ctx.scopes.includes(s))) {
    return false;
  }

  return true;
}
