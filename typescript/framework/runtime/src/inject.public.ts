// Stable inject surface — re-exported from the root `@putnami/runtime`.
//
// The escape hatches (`createScopeProxy`, `useContainer`, `resolve`,
// `resolveInjection`, `SCOPE_CONTAINER_KEY`) are deliberately omitted here;
// they remain available from `@putnami/runtime/inject` for framework
// packages that need to wire scope plumbing.

export * from './inject/inject.type';
export * from './inject/token';
export * from './inject/errors';
export * from './inject/provider';
export {
  Container,
  ScopedContainer,
  type ContainerDescription,
  type DescribeOptions,
  type ProviderDescription,
} from './inject/container';
export { ContainerContext } from './inject/container-context';
export { ContainerContextFork } from './inject/container-context-fork';
export { createTracingProxy, noopTraceSink } from './inject/proxy';
export { analyzeScopeReachability } from './inject/static-analysis';
export type {
  ProviderLookup,
  ProviderLookupContext,
  ScopeReachability,
  ScopeReachabilityHit,
} from './inject/static-analysis';
