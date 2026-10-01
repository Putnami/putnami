# ADR 0001 — Keep rendering explicit across separate runtime graphs

- **Status**: accepted
- **Scope**: `@putnami/web` (`typescript/framework/web`)

## Context

A route module is imported by a server renderer and a browser router with
different authority. The server reads request context, resolves services, and
streams; the browser hydrates and navigates and must not import server
facilities. In one bundle graph, a shared chunk can pull a server import into
the browser package. Rendering mode cannot be inferred from source: a pure-looking
page may reach request state through a loader, and a static page that silently
hydrates changes cost and security without an API change.

## Decision

- **Two graphs.** The package publishes separate default and browser entrypoints,
  compiled as separate dependency graphs. The default graph holds SSR,
  generation, loaders, and actions. The browser graph holds components, router
  hydration, form and navigation handlers, islands, document helpers, and
  lightweight page/layout builders, and cannot reach SSR or server-only runtime
  APIs. Route source is an input to both graphs, never a shared emitted chunk.
- **File discovery builds the route graph.** Pages, layouts, loaders, actions,
  errors, not-found components, dynamic segments, route groups, and multiple scan
  roots get deterministic identifiers and matching nesting on both sides.
- **Rendering mode is explicit.** Pages default to live SSR with full-page
  hydration. `page().static()` selects SSG; a revalidation interval or tags
  select ISR. Static loaders run at build time in a request-free context; reading
  request data throws `StaticRenderViolation`. DI analysis proves static safety
  where it can and marks dynamic selectors conservatively. Dynamic static routes
  need finite `paths()` output, and every output path is checked against
  traversal or normalization outside the static root.
- **Static pages ship no full-page hydration.** Only `*.island.tsx` components
  emit hydration boundaries and client code, with an explicit load strategy.
  Island props are serialized script-safe. Missing observer APIs fall back to
  immediate hydration.
- **Every build emits a sorted, byte-stable route manifest** with mode,
  hydration, DI proof, and client-JavaScript bytes. Diff and budget tools make a
  mode or bundle-size change reviewable.

## Rejected alternatives

- **One graph with conditional branches.** Tree shaking is not an authority
  boundary, and code splitting can mix both runtimes in one chunk.
- **Infer static pages from source or traffic.** The proof is incomplete and can
  flip after an unrelated refactor.
- **Hydrate every static page.** Removes the zero-JavaScript contract.
- **Write whatever path `paths()` returns.** A splat could escape or alias the
  output directory.
- **An SSR throughput benchmark as the CI verdict.** Throughput varies by host;
  emitted bytes are stable, and throughput stays a same-host diagnostic.

## Consequences

- An API used by route modules needs a browser stub, or stays server-only and out
  of browser-imported route source.
- Static loaders accept stricter inputs than live SSR loaders.
- A manifest baseline changes only for a reviewed, deliberate cost.
- Browser hydration cost is measured separately from the client-JavaScript
  budget.
