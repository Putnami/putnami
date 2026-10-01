import type { FilterOptions, NamedToken, TagSelector, Token, Type } from './inject.type';

/**
 * Creates a named token for type-safe DI resolution of non-class values.
 *
 * @typeParam T - The type of the value associated with this token
 * @param name - A unique string identifier
 * @returns A branded named token
 *
 * @example
 * ```typescript
 * const DbUrl = named<string>('db-url');
 * provide(DbUrl, () => process.env.DATABASE_URL!);
 * const url = app.get(DbUrl); // string
 * ```
 */
export function named<T>(name: string): NamedToken<T> {
  return { __brand: 'NamedToken', name } as NamedToken<T>;
}

/**
 * Creates a tag selector for type-safe multi-resolution via `list()`.
 *
 * TagSelector extends FilterOptions, so it can be used directly
 * with `resolveInjection()` for automatic single-vs-multi dispatch.
 *
 * @typeParam T - The type of each tagged instance
 * @param tag - The tag string to match
 * @returns A tag selector (also a valid FilterOptions)
 *
 * @example
 * ```typescript
 * provide(PluginA, { tags: ['plugin'] });
 * provide(PluginB, { tags: ['plugin'] });
 * const plugins = context.list<Plugin>({ tags: 'plugin' }); // Plugin[]
 * // Or with tagged() for type safety in resolveInjection:
 * resolveInjection({ plugins: tagged<Plugin>('plugin') }, scope);
 * ```
 */
export function tagged<T = unknown>(tag: string): TagSelector<T> {
  return { __brand: 'TagSelector', tags: tag } as TagSelector<T>;
}

/**
 * Returns a human-readable name for a token, used in error messages.
 */
export function tokenName(token: Token | TagSelector | FilterOptions): string {
  if (isNamedToken(token)) {
    return `named('${token.name}')`;
  }
  if (isTagSelector(token)) {
    return `tagged('${token.tags}')`;
  }
  if (isFilterOptions(token)) {
    const tags = typeof token.tags === 'string' ? token.tags : token.tags?.join(', ');
    return `filter({ tags: '${tags}' })`;
  }
  if (isClassToken(token as Token)) {
    return (token as Type).name || 'AnonymousClass';
  }
  if (typeof token === 'symbol') {
    return token.toString();
  }
  return String(token);
}

/**
 * Type guard for NamedToken.
 */
export function isNamedToken(value: unknown): value is NamedToken {
  return (
    typeof value === 'object' && value !== null && '__brand' in value && (value as NamedToken).__brand === 'NamedToken'
  );
}

/**
 * Type guard for TagSelector.
 */
export function isTagSelector(value: unknown): value is TagSelector {
  return (
    typeof value === 'object' &&
    value !== null &&
    '__brand' in value &&
    (value as TagSelector).__brand === 'TagSelector'
  );
}

/**
 * Type guard for FilterOptions (non-branded filter object).
 */
export function isFilterOptions(value: unknown): value is FilterOptions {
  return typeof value === 'object' && value !== null && 'tags' in value && !isTagSelector(value);
}

/**
 * Type guard for class (constructor) tokens.
 */
export function isClassToken(value: unknown): value is Type {
  return typeof value === 'function';
}
