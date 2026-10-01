// Stable public surface of the inject module.
//
// Internal escape hatches — `createScopeProxy`, `useContainer`, `resolve`,
// `resolveInjection`, `SCOPE_CONTAINER_KEY` — intentionally do NOT live here
// so they don't leak through the root `@putnami/runtime` barrel. They remain
// available from `@putnami/runtime/inject` for framework packages that need
// to wire scope plumbing.
//
// Stable types defined adjacent to these escape hatches are still re-exported
// (e.g. `ScopeContext`, `TraceSink`) — they are part of the public type
// contract used by application code.

export * from './inject.type';
export * from './token';
export * from './errors';
export * from './provider';
export {
  Container,
  ScopedContainer,
  type ContainerDescription,
  type DescribeOptions,
  type ProviderDescription,
} from './container';
export { ContainerContext } from './container-context';
export { ContainerContextFork } from './container-context-fork';
export { createTracingProxy, noopTraceSink } from './proxy';
export { analyzeScopeReachability } from './static-analysis';
export type { ProviderLookup, ProviderLookupContext, ScopeReachability, ScopeReachabilityHit } from './static-analysis';

// Internal escape hatches — re-exported here so consumers that explicitly
// import from `@putnami/runtime/inject` keep working. These names are
// excluded from the root `@putnami/runtime` barrel; treat them as
// `@internal` with no semver guarantees.
export { createScopeProxy } from './proxy';
export { resolve, useContainer, resolveInjection, SCOPE_CONTAINER_KEY } from './scope';
