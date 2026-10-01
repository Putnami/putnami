# ADR 0002 — Keep data and request security server-authoritative

- **Status**: accepted
- **Scope**: `@putnami/web` (`typescript/framework/web`)

## Context

A hydrated route has server endpoints that load, mutate, and authorize data, and
a browser router that renders the results. Treating client route metadata as
enforcement, or leaving generated JSON and action endpoints unchecked because the
page is protected, fails when a caller hits the endpoint directly or JavaScript
is off. SSR embeds data and bootstrap scripts in HTML: a loader value can contain
a closing script sequence, cached HTML can hold user-specific data, no-JavaScript
forms still need CSRF protection, and a strict CSP needs one nonce on every
framework script.

## Decision

- **Loaders and actions are server definitions.** Their builders validate
  declared params, query, and body and resolve `.inject()` tokens from the
  request scope before the handler runs. Browser page/layout builders keep only
  what importing shared route components needs; server middleware methods are
  no-ops with a developer warning, and `.secure()` keeps a declarative
  authentication, role, or scope hint for conditional UI only.
- **Authorization is attached to generated endpoints.** Layout middleware applies
  to every descendant page, loader, action, and not-found boundary, including
  lazy modules. A page's middleware protects the page and its colocated data
  endpoints. Routes outside the layout are not guarded. Function guards cannot be
  serialized; the browser treats them as indeterminate and denies the UI hint,
  while the server runs the real guard.
- **Hydration state is minimal and escaped.** SSR serializes only
  `authenticated`, resolved roles, and scopes, never tokens or general claims.
  Loader data, action data, errors, basename, and security hints are escaped for
  a script context. Each request owns its document metadata and CSP nonce through
  async context. If a browser chunk fails or hydration breaks, the router keeps
  or restores the server HTML.
- **Security headers and CSRF are on by default for React routes.** Every emitted
  script carries the nonce the CSP middleware publishes. Action POSTs require the
  token cookie echoed in the client header or the hidden `CsrfInput` field, so
  enhanced and no-JavaScript forms share one check. Opt-out is explicit and
  survives composition with a root HTTP plugin.
- **Cache identity includes the caller.** The default page-cache key includes a
  stable fingerprint of the authenticated user and the query string; loader keys
  include query values. Redirects are not cached. Successful actions apply their
  declared invalidations. A custom key is an application-owned escape hatch.

## Rejected alternatives

- **Guard only the rendered page.** Loader and action endpoints stay callable
  without it.
- **Browser route handle as authorization.** Any caller can modify browser state.
- **Serialize full user claims.** Widens disclosure; UI gating needs roles and
  scopes only.
- **React text escaping for hydration JSON.** Script content has different
  termination rules.
- **One cache entry per route.** Leaks user or query data across callers.
- **JavaScript-only CSRF.** Progressive forms need the same boundary.

## Consequences

- A new route-level security source needs propagation tests for page, loader,
  action, lazy descendants, and not-found.
- Opting out of CSRF or security headers is an explicit application decision.
- Custom cache keys must include every input that changes rendered data,
  especially caller identity and query.
- Hydration failures keep a readable server document and are reported.
