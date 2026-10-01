import {
  ConfigService,
  ContainerContext,
  type ContainerContextOptions,
  getRegisteredConfigDefinitions,
  provide,
  provideConfig,
  type Registration,
} from '@putnami/runtime';
import { SessionStoreService, setActiveStoreService } from '../session/session.store';
import type { Module } from './module';

/**
 * Build the application's {@link ContainerContext} from its registrations and
 * modules, wiring the framework-owned services (ConfigService, SessionStoreService)
 * and every tokenized config provider.
 *
 * Extracted from Application so the DI assembly is isolated from lifecycle code.
 */
export function buildAppContainerContext(
  contextOptions: ContainerContextOptions,
  registrations: readonly Registration[],
  modules: Module[],
): ContainerContext {
  const ctx = new ContainerContext('app', contextOptions);

  // Auto-register ConfigService for DI-aware config resolution.
  // This makes useConfig() delegate to ConfigService when inside the DI context,
  // providing instance-scoped caching and config.describe() support.
  ctx.register(
    provide(ConfigService, () => new ConfigService(), {
      onClose: (svc) => svc.close(),
    }),
  );

  // Auto-register SessionStoreService for DI-managed session store instances.
  // When active, getRegisteredStore() delegates to this service.
  ctx.register(
    provide(
      SessionStoreService,
      () => {
        const service = new SessionStoreService();
        setActiveStoreService(service);
        return service;
      },
      { onClose: (svc) => svc.close() },
    ),
  );

  // Auto-register providers for all configs that have been tokenized via configToken().
  // This ensures that resolve(configToken(X)) works when inside a DI scope.
  for (const config of getRegisteredConfigDefinitions()) {
    ctx.register(provideConfig(config));
  }

  // Register app-level providers
  for (const registration of registrations) {
    ctx.register(registration);
  }

  // Mount all modules (depth-first order)
  for (const mod of modules) {
    ctx.mount(mod, mod.name);
  }

  return ctx;
}
