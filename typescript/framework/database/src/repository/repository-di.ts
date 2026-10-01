import { named, type NamedToken, provide, type Registration, type Scope } from '@putnami/runtime';
import { entityHelper } from '../metadata';
import type { TableDefinition } from '../table';
import { Repository } from './repository';

/**
 * Stable DI token per {@link TableDefinition}, memoized by the table object so
 * `provideRepository(T)` and any later `resolve(repositoryToken(T))` share the
 * exact same token instance (the DI container keys providers by token identity,
 * not by name string). Table definitions are module-level constants, so a
 * `WeakMap` keyed on the definition returns the identical token on every call.
 */
const repositoryTokens = new WeakMap<TableDefinition, NamedToken<Repository<TableDefinition>>>();

/**
 * The DI token a repository for `tableDef` is registered under by
 * {@link provideRepository}. Use it to resolve or inject the repository, e.g.
 * `resolve(repositoryToken(UserTable))` or `.inject({ users: repositoryToken(UserTable) })`.
 *
 * The token is derived from the table's datasource + name so two tables never
 * collide, and is stable across calls for a given table definition.
 */
export function repositoryToken<T extends TableDefinition>(tableDef: T): NamedToken<Repository<T>> {
  const existing = repositoryTokens.get(tableDef);
  if (existing) {
    return existing as NamedToken<Repository<T>>;
  }
  const helper = entityHelper(tableDef);
  const token = named<Repository<TableDefinition>>(`putnami.repository:${helper.db ?? 'default'}.${helper.tableName}`);
  repositoryTokens.set(tableDef, token);
  return token as NamedToken<Repository<T>>;
}

/**
 * The repository identity already minted for `tableDef`, or `undefined` when no
 * native declaration in the loaded composition names one.
 *
 * {@link repositoryToken} memoizes per table definition, so this memo *is* the
 * record of which tables a repository was declared over: `provideRepository(T)`,
 * an endpoint's `.inject({ x: repositoryToken(T) })`, and a
 * `resolve(repositoryToken(T))` call site all mint it. Build-time consumers read
 * it to project a repository that was actually declared instead of inventing one
 * for every table — a `Table()` on its own declares a relation, not a gateway.
 * Reading the memo mints nothing and cannot change resolution.
 */
export function declaredRepositoryToken<T extends TableDefinition>(tableDef: T): NamedToken<Repository<T>> | undefined {
  return repositoryTokens.get(tableDef) as NamedToken<Repository<T>> | undefined;
}

/**
 * Minimal repository DI factory: returns a provider registration for a
 * {@link Repository} over `tableDef`, keyed by {@link repositoryToken}. This is a
 * small, additive convenience — it does NOT replace the `new Repository(tableDef)`
 * path, which keeps working and still joins the ambient transaction / request
 * UnitOfWork. Register it like any other provider:
 *
 * ```ts
 * app.register(provideRepository(UserTable));
 * // ...later, in a handler:
 * const users = resolve(repositoryToken(UserTable));
 * ```
 *
 * A repository is a stateless gateway (it resolves its connection per call from
 * the ambient context), so it defaults to a `singleton`; pass `{ scope: 'scoped' }`
 * to get a fresh instance per request when that is preferred.
 */
export function provideRepository<T extends TableDefinition>(
  tableDef: T,
  options: { scope?: Scope } = {},
): Registration<Repository<T>> {
  return provide(repositoryToken(tableDef), () => new Repository(tableDef), {
    scope: options.scope ?? 'singleton',
  });
}
