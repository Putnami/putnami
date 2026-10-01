/**
 * @module session
 *
 * Pluggable session management with multiple storage backends.
 *
 * @example Basic usage
 * ```typescript
 * import { useSession, setSession, deleteSession } from '@putnami/application';
 *
 * // Store user ID in session
 * setSession('userId', 123);
 *
 * // Retrieve it later
 * const userId = useSession<number>('userId');
 *
 * // Remove from session
 * deleteSession('userId');
 * ```
 *
 * @example Configuration (config.yaml)
 * ```yaml
 * session:
 *   store: 'cookie'        # 'cookie' | 'memory' | 'database'
 *   cookieName: 'session'
 *   cookieSecret: '...'    # Required: crypto.randomBytes(32).toString('hex')
 *   ttl: 604800            # 1 week in seconds
 * ```
 *
 * @example Custom store registration
 * ```typescript
 * import { registerSessionStore, type SessionStore } from '@putnami/application';
 *
 * class RedisSessionStore implements SessionStore {
 *   // ... implementation
 * }
 *
 * registerSessionStore('redis', () => new RedisSessionStore());
 * ```
 */

/**
 * Session store interface for pluggable session storage strategies.
 *
 * Built-in implementations:
 * - `CookieSessionStore` - Encrypted client-side storage
 * - `MemorySessionStore` - Server-side with TTL expiration
 * - `DatabaseSessionStore` - PostgreSQL backend (via @putnami/database)
 *
 * @example Implementing a custom store
 * ```typescript
 * import { registerSessionStore, type SessionStore } from '@putnami/application';
 *
 * class MyCustomStore implements SessionStore {
 *   get<T>(sessionId: string, key: string): T | undefined { ... }
 *   set<T>(sessionId: string, key: string, value: T): void { ... }
 *   delete(sessionId: string, key: string): void { ... }
 *   getAll<S>(sessionId: string): S { ... }
 *   setAll<S>(sessionId: string, session: S): void { ... }
 *   deleteAll(sessionId: string): void { ... }
 *   exists(sessionId: string): boolean { ... }
 * }
 *
 * // Register on module load
 * registerSessionStore('custom', () => new MyCustomStore());
 * ```
 */
export interface SessionStore {
  /**
   * Get a specific value from the session.
   * @param sessionId - Unique session identifier
   * @param key - Key to retrieve
   * @returns The value or undefined if not found
   */
  get<T>(sessionId: string, key: string): T | undefined;

  /**
   * Set a specific value in the session.
   * @param sessionId - Unique session identifier
   * @param key - Key to store
   * @param value - Value to store
   */
  set<T>(sessionId: string, key: string, value: T): void;

  /**
   * Delete a specific key from the session.
   * @param sessionId - Unique session identifier
   * @param key - Key to delete
   */
  delete(sessionId: string, key: string): void;

  /**
   * Get all session data.
   * @param sessionId - Unique session identifier
   * @returns All session data as an object
   */
  getAll<S = Record<string, unknown>>(sessionId: string): S;

  /**
   * Replace all session data.
   * @param sessionId - Unique session identifier
   * @param session - New session data
   */
  setAll<S>(sessionId: string, session: S): void;

  /**
   * Delete the entire session.
   * @param sessionId - Unique session identifier
   */
  deleteAll(sessionId: string): void;

  /**
   * Check if session exists.
   * @param sessionId - Unique session identifier
   * @returns True if session exists
   */
  exists(sessionId: string): boolean;

  /**
   * Release any resources held by the store (timers, connections).
   *
   * Optional. Called during graceful shutdown so background work (e.g. the
   * in-memory cleanup interval) does not keep the process alive.
   */
  dispose?(): void;
}

// ---------------------------------------------------------------------------
// Store Registry
// ---------------------------------------------------------------------------

/** Registry of session store factories by name */
const storeRegistry = new Map<string, () => SessionStore>();

/** Cached store instances (fallback when SessionStoreService is not active) */
const storeInstances = new Map<string, SessionStore>();

/**
 * Register a session store factory.
 *
 * Called by store implementations to make themselves available.
 * Registration typically happens at module load time.
 *
 * @param name - Store name (e.g., 'cookie', 'memory', 'database', 'redis')
 * @param factory - Factory function that creates a store instance
 *
 * @example
 * ```typescript
 * registerSessionStore('redis', () => new RedisSessionStore());
 * ```
 */
export function registerSessionStore(name: string, factory: () => SessionStore): void {
  storeRegistry.set(name, factory);
}

/**
 * Get or create a session store instance by name.
 *
 * When a `SessionStoreService` is active (inside a DI-managed Application),
 * delegates to it for instance management. Otherwise, falls back to the
 * module-level instance cache.
 *
 * @param name - Store name to retrieve
 * @returns The session store instance
 * @throws Error if the store name is not registered
 *
 * @example
 * ```typescript
 * const store = getRegisteredStore('cookie');
 * store.set(sessionId, 'key', 'value');
 * ```
 */
export function getRegisteredStore(name: string): SessionStore {
  // Delegate to SessionStoreService when active
  if (_activeStoreService) {
    return _activeStoreService.get(name);
  }

  // Fallback: module-level instance cache
  const cached = storeInstances.get(name);
  if (cached) {
    return cached;
  }

  const factory = storeRegistry.get(name);
  if (!factory) {
    const available = Array.from(storeRegistry.keys()).join(', ');
    throw new Error(
      `Unknown session store: '${name}'. Available stores: ${available || 'none (import a store module first)'}`,
    );
  }

  const instance = factory();
  storeInstances.set(name, instance);
  return instance;
}

// ---------------------------------------------------------------------------
// SessionStoreService — DI-managed instance cache
// ---------------------------------------------------------------------------

let _activeStoreService: SessionStoreService | undefined;

/**
 * Sets the active SessionStoreService for module-level delegation.
 * @internal Called by Application during DI setup.
 */
export function setActiveStoreService(service: SessionStoreService | undefined): void {
  _activeStoreService = service;
}

/**
 * DI-managed service that owns session store instances.
 *
 * When registered in the DI container, `getRegisteredStore()` delegates
 * to this service. On `close()`, all cached store instances are cleared.
 *
 * This ensures store instances participate in the Application lifecycle
 * and are properly cleaned up during shutdown.
 */
export class SessionStoreService {
  private instances = new Map<string, SessionStore>();

  /**
   * Get or create a session store instance by name.
   */
  get(name: string): SessionStore {
    const cached = this.instances.get(name);
    if (cached) return cached;

    const factory = storeRegistry.get(name);
    if (!factory) {
      const available = Array.from(storeRegistry.keys()).join(', ');
      throw new Error(
        `Unknown session store: '${name}'. Available stores: ${available || 'none (import a store module first)'}`,
      );
    }

    const instance = factory();
    this.instances.set(name, instance);
    return instance;
  }

  /**
   * Disposes and clears all cached store instances and deactivates delegation.
   */
  close(): void {
    for (const instance of this.instances.values()) {
      instance.dispose?.();
    }
    this.instances.clear();
    if (_activeStoreService === this) {
      _activeStoreService = undefined;
    }
  }
}

/**
 * Disposes and clears the module-level store instance cache.
 *
 * This is the fallback cache used by {@link getRegisteredStore} when no
 * `SessionStoreService` is active (e.g. outside a DI-managed Application, or in
 * tests). Call it to release background timers held by cached stores.
 */
export function disposeRegisteredStores(): void {
  for (const instance of storeInstances.values()) {
    instance.dispose?.();
  }
  storeInstances.clear();
}
