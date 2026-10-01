import type { Token, TraceData, TraceSink } from './inject.type';
import { tokenName } from './token';

/**
 * A minimal interface for scope resolution. Both `Container` and `ScopeContext`
 * satisfy this, allowing scope proxies to work with either.
 */
interface ScopeResolver {
  get<T>(token: Token<T>): T;
}

/**
 * Creates a scope proxy that delegates property access to the current
 * scoped container's instance. Used when a singleton depends on a
 * scoped provider.
 *
 * Each property access resolves the token from the current scope,
 * ensuring the singleton always sees the correct scoped instance.
 *
 * @param token - The scoped token to proxy
 * @param scopeResolver - Returns the current scope's resolver (Container or ScopeContext)
 * @returns A proxy that delegates to the current scope's instance
 */
export function createScopeProxy<T = unknown>(token: Token<T>, scopeResolver: () => ScopeResolver): T {
  const handler: ProxyHandler<object> = {
    get(_, prop, receiver) {
      // Only shadow Symbol.toStringTag so `Object.prototype.toString.call(proxy)`
      // reads as `[object ScopeProxy<...>]` for diagnostics. Symbol.toPrimitive must
      // delegate: hijacking it would make every coercion (numeric, Date, default)
      // of a scoped instance yield the literal label instead of the real value.
      if (prop === Symbol.toStringTag) {
        return `ScopeProxy<${tokenName(token)}>`;
      }
      const current = scopeResolver().get(token);
      return Reflect.get(current as object, prop, receiver);
    },
    set(_, prop, value) {
      const current = scopeResolver().get(token);
      return Reflect.set(current as object, prop, value);
    },
    has(_, prop) {
      const current = scopeResolver().get(token);
      return Reflect.has(current as object, prop);
    },
    ownKeys() {
      const current = scopeResolver().get(token);
      return Reflect.ownKeys(current as object);
    },
    getOwnPropertyDescriptor(_, prop) {
      const current = scopeResolver().get(token);
      return Reflect.getOwnPropertyDescriptor(current as object, prop);
    },
    getPrototypeOf() {
      const current = scopeResolver().get(token);
      return Reflect.getPrototypeOf(current as object);
    },
  };

  return new Proxy({}, handler) as T;
}

/**
 * Creates a tracing proxy that wraps method calls to measure duration
 * and report via a TraceSink.
 *
 * Only wraps functions, not property access. Handles both sync and
 * async (Promise) returns.
 *
 * @param instance - The instance to wrap
 * @param metadata - Token and module info for trace reports
 * @param sink - The sink to emit trace data to
 * @returns A proxy that traces method calls
 */
export function createTracingProxy<T extends object>(
  instance: T,
  metadata: { token: string; module?: string },
  sink: TraceSink,
): T {
  return new Proxy(instance, {
    get(target, prop, receiver) {
      const value = Reflect.get(target, prop, receiver);
      if (typeof value !== 'function') {
        return value;
      }

      // Don't wrap built-in Object methods
      if (typeof prop === 'string' && ['constructor', 'toString', 'valueOf', 'toJSON'].includes(prop)) {
        return value;
      }

      return function traced(this: unknown, ...args: unknown[]) {
        const start = performance.now();
        try {
          const result = value.apply(target, args);
          if (result instanceof Promise) {
            return result.then(
              (resolved: unknown) => {
                emitTrace(sink, metadata, prop, performance.now() - start);
                return resolved;
              },
              (err: unknown) => {
                emitTrace(sink, metadata, prop, performance.now() - start, err);
                throw err;
              },
            );
          }
          emitTrace(sink, metadata, prop, performance.now() - start);
          return result;
        } catch (err) {
          emitTrace(sink, metadata, prop, performance.now() - start, err);
          throw err;
        }
      };
    },
  });
}

function emitTrace(
  sink: TraceSink,
  metadata: { token: string; module?: string },
  method: string | symbol,
  duration: number,
  error?: unknown,
): void {
  const data: TraceData = {
    token: metadata.token,
    module: metadata.module,
    method,
    duration,
    error,
  };
  sink.emit(data);
}

/**
 * A no-op trace sink. Default when no tracing is configured.
 */
export const noopTraceSink: TraceSink = {
  emit() {},
};
