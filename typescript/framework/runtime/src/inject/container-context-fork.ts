import type { ContainerContextOptions, Registration, SyncFactory, Token } from './inject.type';
import { ContainerContext, registerForkFactory } from './container-context';
import { provide } from './provider';

/**
 * A test fork of a ContainerContext. Copies all root registrations and the
 * mounted module-container structure, then allows overriding specific providers
 * before starting. Module-scoped providers resolve in the fork, so a `module()`
 * -composed app can be forked and overridden in tests without losing its modules.
 */
export class ContainerContextFork extends ContainerContext {
  private overrides = new Map<Token, Registration>();

  constructor(source: ContainerContext, options: ContainerContextOptions) {
    super('fork', options);

    // Copy root-level registrations
    for (const reg of source.getRegistrations()) {
      super.register(
        provide(reg.provider.token, reg.provider.factory as SyncFactory<unknown>, {
          deps: reg.provider.deps,
          depsComplete: reg.provider.depsComplete,
          scope: reg.provider.scope,
          visibility: reg.provider.visibility,
          tags: reg.provider.tags,
          onClose: reg.provider.onClose,
          proxy: reg.provider.proxy,
          dynamic: reg.provider.dynamic,
          lazy: reg.provider.lazy,
        }),
      );
    }

    // Recreate each mounted module container as a child of the fork's root, so
    // module-scoped providers resolve in the fork too. Module requirements were
    // already validated when the source was mounted and the same root providers
    // were just copied above, so re-running mount()'s requirement check would be
    // redundant — recreate the children directly. Providers are inert data, so
    // wrapping each in a fresh Registration (matching ContainerContext.scope())
    // faithfully preserves async/depsComplete/tags without re-deriving them.
    const forkRoot = this.getRoot();
    for (const moduleContainer of source.getModuleContainers()) {
      const child = forkRoot.createChild(moduleContainer.name);
      for (const [, provider] of moduleContainer.getProviders()) {
        child.register({ __brand: 'Registration', provider });
      }
      this.getModuleContainers().push(child);
    }
  }

  /**
   * Override a provider with a test double.
   * Must be called before `start()`.
   */
  override<T>(token: Token<T>, factory: SyncFactory<T>): this;
  override<T>(token: Token<T>, factory: (resolve: never) => Promise<T>): this;
  override<T>(token: Token<T>, factory: SyncFactory<T>): this {
    const registration = provide(token, factory) as Registration;
    this.overrides.set(token, registration);
    return this;
  }

  override async start(): Promise<void> {
    // Apply overrides by replacing providers in the root container before starting.
    // This ensures the overridden factories run during resolveAll() instead of originals.
    if (this.overrides.size > 0) {
      const root = this.getRoot();
      for (const [, override] of this.overrides) {
        root.replaceProvider(override);
      }
    }
    return super.start();
  }
}

// Self-register to break circular dependency
registerForkFactory((parent, options) => new ContainerContextFork(parent, options));
