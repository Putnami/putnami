// Re-export client utilities for server-side use (except form which conflicts with handlers)

export * from '../client/components';
export * from '../client/document';
export * from '../client/error';
export * from '../client/event';
export * from '../client/hooks';
export * from '../shared/context-slots';
export * from '../shared/csrf-context';
export * from '../shared/security.types';
// The DOM navigation seam is browser-only code with no imports; the server
// entry re-exports it so plugins can share one event name and one detail type.
export {
  dispatchNavigation,
  onNavigation,
  PUTNAMI_NAVIGATION_EVENT,
  type NavigationDetail,
} from '../client/router/navigation-event';
// SSR-specific exports
export * from './action';
export * from './csp';
export * from './error';
export * from './form';
export * from './handlers';
export * from './layout';
export * from './loader';
export * from './middleware';
export * from './not-found';
export * from './page';
export * from './react-application';
export * from './react-plugin';
export * from './static';
export * from './static-di';
export * from './budget';
export * from './manifest';
export { renderStaticDocument, staticHtmlResponse, type StaticRenderResult } from './static-render';
export { revalidateTag } from './static-serve.utils';

// Islands — server-safe builder, marker host, slot and id registry (no
// react-dom/client; the client hydration runtime ships only in the browser bundle).
export {
  island,
  IslandBuilder,
  IslandModeContext,
  registerIslandId,
  getIslandId,
  isIslandHost,
  Slot,
} from '../client/island/island';
export {
  type IslandComponent,
  type IslandConfig,
  type IslandStrategy,
  isIslandComponent,
  islandIdFromFile,
  serializeIslandProps,
  parseIslandProps,
} from '../client/island/island-types';

// Re-export cache utilities from @putnami/application for convenience
export { cache, clearCache, evictCache } from '@putnami/application';

// Re-export schema primitives from @putnami/runtime for convenience
export {
  Optional,
  ArrayOf,
  MapOf,
  Stream,
  isStreamSchema,
  Uuid,
  Email,
  Int,
  Url,
  DateIso,
  Min,
  Max,
  MinLength,
  MaxLength,
  Pattern,
  OneOf,
  Constrained,
  Default,
  Env,
  Resolve,
  Sensitive,
  schema,
  isSchemaDescriptor,
  isNestedSchema,
  baseTypeName,
} from '@putnami/runtime';
export type {
  SchemaDescriptor,
  SchemaConstraint,
  SchemaPrimitive,
  NestedSchema,
  SchemaDefinition,
  StreamSchema,
  InferPrimitive,
  InferSchema,
} from '@putnami/runtime';
