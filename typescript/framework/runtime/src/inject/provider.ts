import type {
  AsyncFactory,
  Factory,
  Provider,
  ProvideOptions,
  Registration,
  ResolveFn,
  SyncFactory,
  Token,
  Type,
} from './inject.type';
import { isClassToken, isNamedToken } from './token';

/**
 * Creates a provider registration. Returns inert data — no instantiation
 * happens until the registration is added to a container and `start()` is called.
 *
 * @example
 * ```typescript
 * // Class, no deps
 * provide(AppConfig)
 *
 * // Class + deps
 * provide(UserService, { deps: [Database, EmailService] })
 *
 * // Sync factory
 * provide(Database, () => new Database('postgres://localhost'))
 *
 * // Async factory with resolve
 * provide(Database, async (resolve) => {
 *   const config = resolve(AppConfig);
 *   const db = new Database(config.url);
 *   await db.connect();
 *   return db;
 * }, { onClose: (db) => db.disconnect() })
 *
 * // Named token + factory
 * provide(named<string>('version'), () => '2.0.0')
 * ```
 */
export function provide<T extends object>(classRef: Type<T>): Registration<T>;
export function provide<T extends object>(classRef: Type<T>, options: ProvideOptions<T>): Registration<T>;
export function provide<T>(token: Token<T>, factory: SyncFactory<T>): Registration<T>;
export function provide<T>(token: Token<T>, factory: SyncFactory<T>, options: ProvideOptions<T>): Registration<T>;
export function provide<T>(token: Token<T>, factory: AsyncFactory<T>): Registration<T>;
export function provide<T>(token: Token<T>, factory: AsyncFactory<T>, options: ProvideOptions<T>): Registration<T>;
export function provide<T>(
  tokenOrClass: Token<T>,
  factoryOrOptions?: Factory<T> | ProvideOptions<T>,
  maybeOptions?: ProvideOptions<T>,
): Registration<T> {
  let token: Token<T>;
  let factory: Factory<T>;
  let isAsync = false;
  let options: ProvideOptions<T> = {};
  let deps: Token[] = [];
  // Class providers resolve exactly `deps` (synthesized factory), so their
  // dependency set is provably complete for static graph analysis. Factory
  // providers may resolve undeclared tokens, so they are complete only when the
  // caller opts in via `depsComplete`.
  let depsComplete = false;

  // Parse overloaded arguments
  if (isClassToken(tokenOrClass) && (factoryOrOptions === undefined || isProvideOptions(factoryOrOptions))) {
    // provide(Class) or provide(Class, options)
    token = tokenOrClass;
    options = (factoryOrOptions as ProvideOptions<T>) || {};
    deps = options.deps || [];
    depsComplete = true;
    factory = ((resolve: ResolveFn) => {
      const args = deps.map((dep) => resolve(dep));
      return new tokenOrClass(...args);
    }) as SyncFactory<T>;
  } else if (typeof factoryOrOptions === 'function') {
    // provide(token, factory) or provide(token, factory, options)
    token = tokenOrClass;
    factory = factoryOrOptions as Factory<T>;
    options = maybeOptions || {};
    deps = options.deps || [];
    depsComplete = options.depsComplete ?? false;
    isAsync = isAsyncFactory(factory);
  } else {
    throw new Error(
      'Invalid provide() arguments: expected (Class), (Class, options), (token, factory), or (token, factory, options)',
    );
  }

  const provider: Provider<T> = {
    token,
    factory,
    async: isAsync,
    deps,
    depsComplete,
    scope: options.scope || 'singleton',
    visibility: options.visibility || 'public',
    tags: options.tags || [],
    onClose: options.onClose,
    proxy: options.proxy,
    dynamic: options.dynamic || false,
    lazy: options.lazy || false,
  };

  return { __brand: 'Registration', provider } as Registration<T>;
}

/**
 * Type guard: checks if the value is a ProvideOptions object (not a function).
 */
function isProvideOptions<T>(value: unknown): value is ProvideOptions<T> {
  return typeof value === 'object' && value !== null && !isNamedToken(value);
}

/**
 * Detects if a factory function is async.
 * Checks if the function is declared with `async` keyword.
 */
function isAsyncFactory<T>(factory: Factory<T>): boolean {
  return factory.constructor.name === 'AsyncFunction';
}

/**
 * Type guard for Registration objects.
 */
export function isRegistration(value: unknown): value is Registration {
  return (
    typeof value === 'object' &&
    value !== null &&
    '__brand' in value &&
    (value as Registration).__brand === 'Registration'
  );
}
