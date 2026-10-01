# ADR 0002 — An optional rule serves a caller that presents no credential

- **Status**: accepted
- **Scope**: `go.putnami.dev/security`, `go.putnami.dev/http`, `go.putnami.dev/openapi`,
  and their TypeScript twins in `@putnami/application` (`security`, `openapi`)

## Context

Some routes answer anonymous and authenticated callers alike: a registry's
resolve and download routes serve anyone for a public package and require a
credential for a private one. Generated clients choose the first security
alternative their binding satisfies, and an empty `allOf` is always satisfied.
The provider needs a way to state that the route serves a caller with no
credential.

## Decision

1. **The rule says so.** `security.Options{Optional: true}` in Go and
   `.secure({ optional: true })` in TypeScript serve a request with no
   credential. Every other requirement of the rule (roles, scopes, client,
   issuer, audience, principal kind) applies to authenticated callers only. A
   custom Go rule opts in through `phttp.SecurityOptionalAuthentication`. With
   a TypeScript `verify`, the verifier is the route's only identity source: a
   request without a bearer token is anonymous, even when another resolver set
   `ctx.user`.
2. **A presented credential is never downgraded.** A non-empty
   `Authorization` header that resolved no identity gets 401. A header empty
   after trimming counts as absent. An expired token is reported, so a client
   can refresh it.
3. **The contract follows the rule.** A first-party contract offers the
   anonymous alternative only for an optional rule, and only last. The rule's
   scopes and roles merge into the credentialed alternatives only. Go derives
   `[{allOf: [{profile}]}, {allOf: []}]` when the provider declares exactly one
   credential profile and no alternatives; with several profiles the provider
   declares them. The plain OpenAPI operation adds the empty requirement `{}`
   beside its schemes.
4. **The refusals stay.** An anonymous alternative on a rule that requires
   authentication, and a credential alternative on a route with no rule, fail
   publication with an error naming the route and the optional rule.

## Rejected alternatives

- **Treat an unresolved credential as anonymous.** An expired or forged token
  gets the public answer, and a service token is never refreshed.
- **Declare the anonymous alternative in the contract alone.** It advertises a
  call the server refuses.
- **Anonymous alternative first.** No credential would ever be presented.
- **An `optional` flag in the protocol.** The empty `allOf` already says it.

## Consequences

- A module- or server-level rule runs before the route's rule; one that
  requires authentication refuses anonymous callers.
- The 401 covers only `Authorization`. A credential in another header (an API
  key) that no resolver accepts is served anonymously.
- A TypeScript route declares its alternatives with `.client({ security })`;
  only Go derives them from a single profile.
