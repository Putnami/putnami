/**
 * Adoptable identity vocabulary: the well-known token-claim names the framework
 * extracts and the principal kinds it recognizes.
 *
 * These constants are the in-project twin of the generated identity contract at
 * `protocols/identity/schema/contracts.gen.ts` (itself emitted from the
 * canonical IR by `emitTypeScript`, the same shape the Go twin adopts).
 * TypeScript cannot import that generated twin across the Go protocol project
 * boundary without tripping `tsc --rootDir` (TS6059), so the vocabulary is
 * mirrored here and pinned to the contract enums by
 * `test/contracts/identity-twin.test.ts` — a rename or wire-value change fails
 * that test rather than silently drifting.
 */

/** Wire names of the well-known token claims the framework extracts. */
export const ClaimName = {
  Sub: 'sub',
  Iss: 'iss',
  ClientId: 'client_id',
  Roles: 'roles',
  Scope: 'scope',
  Aud: 'aud',
  Exp: 'exp',
} as const;
export type ClaimName = (typeof ClaimName)[keyof typeof ClaimName];

/** The recognized kinds of authenticated principal. */
export const PrincipalKind = {
  User: 'user',
  ApiKey: 'apikey',
} as const;
export type PrincipalKind = (typeof PrincipalKind)[keyof typeof PrincipalKind];

/**
 * The outcome label for an authorization check. The values double as the metric
 * counter keys and the `decision` log attribute, so this TypeScript framework
 * and the Go framework (which aliases the generated `AuthDecision*` constants in
 * `go/framework/security` observe.go) share one authorization vocabulary. Pinned
 * to the manifest enum by `test/contracts/identity-twin.test.ts`.
 */
export const AuthDecision = {
  Allow: 'allow',
  DenyUnauthenticated: 'deny_unauthenticated',
  DenyClient: 'deny_client',
  DenyScope: 'deny_scope',
  DenyRole: 'deny_role',
  DenyGuard: 'deny_guard',
} as const;
export type AuthDecision = (typeof AuthDecision)[keyof typeof AuthDecision];
