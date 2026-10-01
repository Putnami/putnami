# ADR 0005 — A pre-rendered page gets its tracker at serve time

- **Status**: accepted
- **Scope**: `@putnami/analytics` (`typescript/framework/analytics`), `@putnami/web`

## Context

The server records a page view for every rendered page, so a view survives an
ad blocker. The browser tracker completes that row (session, engagement,
viewport, language) and is the only source of sessions, client navigations and
declared actions. The React renderer serializes the tracker bootstrap and script
URL from request-context slots into the document.

A `page().static()` route never reaches that renderer: it serves HTML rendered
at build time, with no security context, CSP nonce or client bootstrap. The
middleware still records the server row, but the document has no tracker. A
fully pre-rendered site, such as `putnami.dev`, would get raw view counts only.

## Decision

The page-view middleware injects the tracker into any HTML response the renderer
did not produce, carrying that request's own bootstrap.

- `@putnami/web` marks the request context when its renderer emits the bootstrap
  (`markClientBootstrapEmitted`). The mark, not a guess about the handler, tells
  the two shapes apart.
- The injection appends one
  `<script type="module" src="…" data-putnami-analytics="…">` before `</body>`.
  Nothing is injected when the mark is set, when the body is not a string (a
  streamed SSR response), when no tracker bundle was built, or when the document
  has no closing body tag; `analytics.page_view.tracker_not_injected` counts the
  last case.
- The bootstrap rides in a data attribute of an external script, never an inline
  script. A pre-rendered page has no CSP nonce, and this package never asks an
  application to allow `'unsafe-inline'`.
- The injected id is the page-view id the server just recorded, so the browser
  enriches that row through the `ON CONFLICT` path.

## Rejected alternatives

- **Bake the tracker into the build output.** A pre-rendered file is identical
  for every visitor and cacheable anywhere, so it cannot carry a per-request id.
  A client-minted view beside the server row double-counts; dropping the server
  row loses every visitor who blocks scripts.
- **Publish the id in a response header** (`Server-Timing`). A page replayed
  from the HTTP cache or bfcache reports against an earlier visit's header.
- **Document the gap.** Every fully pre-rendered site would have no sessions,
  engagement or declared actions.

## Consequences

- One string search and one concatenation per statically served HTML response;
  `staticServeHandler` already holds the body as a string.
- An endpoint returning HTML also gets the tracker, matching its server row.
- A response replayed from a browser or CDN cache replays the injected id, so
  the enrichment updates that older view instead of creating one.
- `@putnami/web` exports one slot pair that states a fact about its renderer and
  knows nothing about analytics.
