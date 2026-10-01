import { useLogger } from '../logger';
import type { Container, DiDebugLogger } from './container';
import type { Provider, TraceSink } from './inject.type';
import { createTracingProxy } from './proxy';
import { tokenName } from './token';

const IS_PRODUCTION = process.env.NODE_ENV === 'production';

/**
 * Creates a debug logger for DI events. One-time-per-token-pair dedup for scope proxy logs.
 * Returns undefined in production or when debug is disabled.
 * @internal
 */
export function createDiDebugLogger(debug?: boolean): DiDebugLogger | undefined {
  if (!debug || IS_PRODUCTION) return undefined;

  const logger = useLogger('putnami:di');
  const loggedScopeProxies = new Set<string>();

  return {
    onResolve(token, container, cached) {
      const label = cached ? 'cached' : 'new';
      logger.debug(`[resolve] ${tokenName(token)} (${label}) from '${container}'`);
    },
    onScopeProxy(singletonToken, scopedToken) {
      const key = `${tokenName(singletonToken)}->${tokenName(scopedToken)}`;
      if (loggedScopeProxies.has(key)) return;
      loggedScopeProxies.add(key);
      logger.debug(
        `[scope-proxy] Singleton ${tokenName(singletonToken)} depends on scoped ${tokenName(scopedToken)}. ` +
          'A scope proxy will delegate to the current scope at access time.',
      );
    },
    onScopeCreate() {
      logger.debug('[scope] Created new scope');
    },
    onScopeClose() {
      logger.debug('[scope] Scope closed');
    },
  };
}

/**
 * Apply tracing proxy to all providers in a container.
 * Wraps resolved instances with a proxy that reports method calls to the trace sink.
 * @internal
 */
export function applyTracing(container: Container, sink: TraceSink, moduleName?: string): void {
  for (const [token] of container.getProviders()) {
    const provider = container.findProvider(token);
    if (!provider) continue;

    const providerProxy = provider.proxy;
    if (providerProxy === false) continue;
    const providerTracing =
      typeof providerProxy === 'object' ? providerProxy.tracing : providerProxy === true || undefined;
    if (providerTracing === false) continue;

    const instance = container.getInstance(token);
    if (instance && typeof instance === 'object') {
      const wrapped = createTracingProxy(instance as object, { token: tokenName(token), module: moduleName }, sink);
      container.setInstance(token, wrapped);
    }
  }
}

/**
 * Collect scoped providers from the root and all module containers.
 * Computed once at start() to avoid O(M×P) iteration on every scope() call.
 * @internal
 */
export function collectScopedProviders(root: Container, moduleContainers: Container[]): Array<{ provider: Provider }> {
  const cache: Array<{ provider: Provider }> = [];

  for (const [, provider] of root.getProviders()) {
    if (provider.scope === 'scoped') {
      cache.push({ provider });
    }
  }

  for (const container of moduleContainers) {
    for (const [, provider] of container.getProviders()) {
      if (provider.scope === 'scoped') {
        cache.push({ provider });
      }
    }
  }

  return cache;
}
