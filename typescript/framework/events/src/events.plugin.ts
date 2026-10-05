import type {
  DesignBuilder,
  DesignContributor,
  DesignProvenance,
  GenerateResult,
  InfraContributor,
  Module,
  Plugin,
  SourceScopedDesignContributor,
} from '@putnami/application';
import {
  Application,
  HttpPlugin,
  generatedLoaderKey,
  generatedLoaderPlugins,
  loadRegisteredModule,
} from '@putnami/application';
import type { DetachedScope, Logger } from '@putnami/runtime';
import { type ConfigDefinition, useLogger } from '@putnami/runtime';
import { fileExists, getDirectoryName, getExternalCaller, getProjectRoot, relativePath } from '@putnami/utils';
import { emitInfraRequirementsForScans, resolveEventsLoaderPath, writeHandlerLoader } from './events-discovery';
import {
  createLocalServerTransport,
  isAddressInUse,
  resolveTransport,
  validateTransportConfig,
} from './events-transport';
import { EventsRuntimeConfig, resolveEventsRuntimeConfig } from './events.config';
import type { HandlerDefinition } from './handler/handler';
import { isHandlerDefinition } from './handler/handler';
import type { OutboxDefinition } from './outbox/outbox';
import { clearTransport, getDesignPublications, setTransport } from './publisher/publisher';
import { EVENTS_DEFAULT_PORT, MemoryServer } from './server/memory-server';
import { type Delivery, PUSH_RECEIVER_PATH, type PushConfig, createPushReceiver } from './server/push-receiver';
import type {
  EventServerTokenSource,
  EventServerTransportConfig,
  Transport,
  TransportRoute,
  TransportTarget,
} from './transport';

// ---------------------------------------------------------------------------
// Events plugin configuration
// ---------------------------------------------------------------------------

export interface EventsConfig {
  /** Explicit transport or the managed `eventserver` config selector. */
  transport?: Transport | 'eventserver';
  /** Managed Event Server binding and provider-owned token source. */
  eventServer?: Partial<Omit<EventServerTransportConfig, 'tokenSource'>> & { tokenSource?: EventServerTokenSource };
  /** Named transports used by plugin-level routing. */
  transports?: Record<string, Transport>;
  /** Ordered routes. Topic channel routes are evaluated before topic name matches. */
  routes?: TransportRoute[];
  /** Fallback named transport when no route matches. */
  defaultTransport?: TransportTarget;
  /** The endpoint for the events service. If unset, starts a local in-memory broker. */
  endpoint?: string;
  /** Bearer token for the configured events endpoint. Falls back to EVENTS_TOKEN. */
  token?: string;
  /** Port for the local events server (when no endpoint is configured). Default: 4222 */
  port?: number;
  /** Maximum time to wait for in-flight messages during shutdown. Default: 10_000 */
  drainTimeout?: number;
  /**
   * For the local in-memory broker only: deliberately redeliver ~2% of messages
   * to surface non-idempotent handlers during development. Off by default so the
   * default local path is deterministic. Has no effect when an endpoint or
   * explicit transport is configured. Default: false.
   */
  simulateDuplicates?: boolean;
  /**
   * How handlers receive events: `pull` (the default) or `stream` hold a
   * long-lived process; `push` registers an HTTP receiver route and the provider
   * POSTs each event to it, so the workload can scale to zero. In push mode the
   * broker/transport pull loop is not started and `push` config is required.
   */
  delivery?: Delivery;
  /** OIDC verification for the push receiver; required when `delivery` is `push`. */
  push?: PushConfig;
  /** Registered handlers (discovered or explicitly provided) */
  handlers?: HandlerDefinition[];
  /**
   * Declared transactional outboxes. Runtime-inert: the plugin never writes,
   * claims, or relays a row. The declarations only let the build-time design
   * graph model commit-time enqueue separately from relay-time publication.
   */
  outboxes?: OutboxDefinition[];
  /** Directory name to scan for handler files (relative to caller). Default: 'events' */
  scanFolder?: string;
  /** Explicit path to scan for handler files. Overrides scanFolder. */
  scanPath?: string;
  /** Enable automatic file-based handler discovery. Default: true */
  autoScan?: boolean;
  /**
   * Pre-loaded module to use instead of dynamic import in warmup.
   * Pass the already-imported generated `.events-application.gen.ts` module
   * to enable bundled builds where dynamic imports cannot be resolved.
   *
   * @example
   * ```typescript
   * import * as eventsModule from './.gen/src/events/.events-application.gen.ts';
   * app.use(events({ preloadedModule: eventsModule }));
   * ```
   */
  preloadedModule?: Record<string, unknown>;
}

// ---------------------------------------------------------------------------
// EventsPlugin — Application lifecycle plugin
// ---------------------------------------------------------------------------

/**
 * Events plugin for the Putnami application framework.
 *
 * Manages the event transport lifecycle:
 * - **Local dev** (no endpoint): starts an in-memory broker + local HTTP server
 * - **Production** (endpoint set): connects to the cloud events service
 *
 * Supports file-based handler auto-discovery. Place handler files with the
 * `.on.ts` suffix in the scan directory:
 *
 * ```
 * src/events/
 * ├── user-created.on.ts      → export default handler(UserCreated).handle(...)
 * ├── order-placed.on.ts      → export default handler(OrderPlaced).handle(...)
 * └── payment/
 *     └── failed.on.ts        → export default handler(PaymentFailed).handle(...)
 * ```
 */
export class EventsPlugin implements Plugin, DesignContributor, InfraContributor, SourceScopedDesignContributor {
  readonly designAttribution = 'source' as const;
  private transport: Transport | undefined;
  private memoryServer: MemoryServer | undefined;
  private config: EventsConfig;
  private readonly handlers: HandlerDefinition[] = [];
  private readonly outboxes: OutboxDefinition[] = [];
  private eventsLoaderPath: string | undefined;
  private designHandlers: HandlerDefinition[] = [];

  constructor(config: EventsConfig = {}) {
    this.config = {
      autoScan: true,
      scanFolder: 'events',
      ...config,
    };
    validateTransportConfig(this.config);
    if (config.handlers) {
      this.handlers.push(...config.handlers);
    }
    if (config.outboxes) {
      this.outboxes.push(...config.outboxes);
    }

    // Computed eagerly so warmup() finds it when generate() ran in another process.
    if (this.config.scanPath) {
      this.eventsLoaderPath = resolveEventsLoaderPath(this.config.scanPath, getProjectRoot());
    }
  }

  /** Project the same declared topics used by event design and infra emission. */
  designInfraRequirements() {
    const requirements = new Map<string, DesignProvenance[]>();
    const add = (name: string, ...provenance: Array<DesignProvenance | undefined>): void => {
      const sources = requirements.get(name) ?? [];
      for (const source of provenance) {
        if (source && !sources.some(({ path }) => path === source.path)) sources.push(source);
      }
      requirements.set(name, sources);
    };
    for (const publication of getDesignPublications()) {
      add(publication.topic.name, publication.topic.__source, publication.source);
    }
    for (const definition of [...this.handlers, ...this.designHandlers]) {
      add(definition.topic.name, definition.topic.__source, definition.provenance);
    }
    for (const declaration of this.outboxes) {
      for (const topic of declaration.topics) add(topic.name, topic.__source, declaration.__source);
    }
    return [...requirements]
      .sort(([left], [right]) => left.localeCompare(right))
      .map(([name, sources]) => ({
        name,
        kind: 'events' as const,
        ...(sources.length > 0 ? { sources: sources.sort((left, right) => left.path.localeCompare(right.path)) } : {}),
      }));
  }

  /**
   * Register a handler definition with the plugin.
   * Can be called before or during warmup.
   */
  register(definition: HandlerDefinition): this {
    this.handlers.push(definition);
    return this;
  }

  /**
   * Register a transactional outbox declaration with the plugin.
   *
   * The declaration is build-time metadata only. It starts no relay, opens no
   * transaction, and changes no delivery path — registering one must leave
   * warmup, subscription, publication, retry, and shutdown byte-identical.
   */
  registerOutbox(definition: OutboxDefinition): this {
    this.outboxes.push(definition);
    return this;
  }

  /**
   * Register all HandlerDefinition exports from a module.
   * Used by the generated loader to register discovered handlers.
   */
  registerModule(mod: Record<string, unknown>): this {
    for (const value of Object.values(mod)) {
      if (isHandlerDefinition(value)) {
        this.handlers.push(value);
      }
    }
    return this;
  }

  /** Publish the deploy-time `events` config block through application composition. */
  configDefinitions(): ConfigDefinition[] {
    return [EventsRuntimeConfig];
  }

  /**
   * Generate handler loader during build (if scanning is enabled).
   * Scans for `*.on.ts` files and produces a generated TypeScript file
   * that imports and registers all discovered handlers.
   */
  async generate(owner: Module): Promise<GenerateResult> {
    if (!this.config.scanPath) {
      return {};
    }

    const projectRoot = getProjectRoot();
    this.eventsLoaderPath = resolveEventsLoaderPath(this.config.scanPath, projectRoot);

    writeHandlerLoader(this.eventsLoaderPath, this.config.scanPath, projectRoot);
    await this.emitApplicationInfraRequirements(owner, projectRoot);

    return {
      exports: {
        [this.loaderKey(owner)]: this.eventsLoaderPath,
      },
    };
  }

  /**
   * Whether this plugin owns a generated handler loader: it names a `scanPath`,
   * or leaves `autoScan` on. Read from configuration, never from the filesystem,
   * so a build and the packaged binary it produces agree even though the binary
   * has no handler folder for the default `events()` to find.
   */
  private get scansHandlers(): boolean {
    return this.config.scanPath !== undefined || this.config.autoScan !== false;
  }

  /** The application's events plugins that own a generated handler loader, in registration order. */
  private loaderSiblings(owner: Module | undefined): EventsPlugin[] {
    return generatedLoaderPlugins(
      owner,
      this,
      (plugin): plugin is EventsPlugin => plugin instanceof EventsPlugin && plugin.scansHandlers,
    );
  }

  /**
   * The key this plugin's generated handler loader is exported and registered
   * under: `events-loader` for the application's first scanning plugin in
   * module-tree registration order, `events-<n>-loader` for the others, so two
   * scanning plugins never share one (@putnami/application ADR 0006).
   */
  private loaderKey(owner: Module | undefined): string {
    return generatedLoaderKey('events', this.loaderSiblings(owner).indexOf(this));
  }

  /**
   * Write the project's events infra sidecar once for the whole application:
   * the first plugin with a scan folder scans every sibling's folder in turn and
   * writes their union. Plugins generate concurrently and each scan resets the
   * process-wide publisher record, so one writer per plugin would race and the
   * last one would replace the others' topics.
   */
  private async emitApplicationInfraRequirements(owner: Module | undefined, projectRoot: string): Promise<void> {
    const scanned = this.loaderSiblings(owner).filter((plugin) => plugin.config.scanPath !== undefined);
    if (scanned[0] !== this) return;
    const handlers = await emitInfraRequirementsForScans(
      projectRoot,
      scanned.map((plugin) => ({ scanPath: plugin.config.scanPath, delivery: plugin.deliveryMode() })),
    );
    scanned.forEach((plugin, index) => {
      plugin.designHandlers = handlers[index] ?? [];
    });
  }

  /**
   * Derive topics, outboxes, publishers, handlers, and injected consumers
   * natively.
   *
   * Three publication facts stay structurally distinct because they are not the
   * same claim: `module -enqueues-> event.outbox` is the commit-time write and
   * is exact (registering the outbox on this module is a native fact);
   * `event.outbox -publishes-> event.topic` is the relay-time publication a
   * claimed row produces and is exact (the declaration names those topics); and
   * `module -publishes-> event.topic` remains the derived direct transport
   * publish attributed from a `getPublisher()` call site.
   *
   * Only names and bounded identifiers reach the graph. Topic nodes expose
   * schema FIELD names, never a value, default, or metadata entry; outbox nodes
   * expose the table and datasource identifiers and nothing from a payload.
   */
  contributeDesign(builder: DesignBuilder): void {
    const topics = new Map<string, HandlerDefinition['topic']>();
    for (const publication of getDesignPublications()) topics.set(publication.topic.name, publication.topic);
    for (const definition of [...this.handlers, ...this.designHandlers])
      topics.set(definition.topic.name, definition.topic);
    // A declared outbox topic may have no local publisher or handler at all —
    // the relay is the only producer. Mint its node here or the exact
    // relay-publication edge would be dropped as dangling.
    for (const declaration of this.outboxes) for (const topic of declaration.topics) topics.set(topic.name, topic);

    for (const topic of topics.values()) {
      const topicId = `event.topic:${topic.name}`;
      builder.addNode({
        id: topicId,
        kind: 'event.topic',
        name: topic.name,
        properties: {
          fields: Object.keys(topic.schema).sort().join(','),
          ...(topic.version ? { version: topic.version } : {}),
        },
        ...(topic.__source ? { provenance: topic.__source } : {}),
      });
      if (topic.__source?.path) builder.relateFromSource(topic.__source.path, topicId, 'contains');
    }

    for (const publication of getDesignPublications()) {
      const topicId = `event.topic:${publication.topic.name}`;
      if (publication.source?.path) builder.relateFromSource(publication.source.path, topicId, 'publishes');
    }

    for (const declaration of this.outboxes) {
      const outboxId = `event.outbox:${declaration.name}`;
      const properties = {
        ...(declaration.table ? { table: declaration.table } : {}),
        ...(declaration.datasource ? { datasource: declaration.datasource } : {}),
      };
      builder.addNode({
        id: outboxId,
        kind: 'event.outbox',
        name: declaration.name,
        ...(Object.keys(properties).length > 0 ? { properties } : {}),
        ...(declaration.__source ? { provenance: declaration.__source } : {}),
      });
      // Registering the outbox on this module IS the native fact, so the
      // commit-time enqueue is exact rather than source-containment derived.
      if (builder.moduleId) builder.relateFromModule(outboxId, 'enqueues');
      else if (declaration.__source?.path) builder.relateFromSource(declaration.__source.path, outboxId, 'enqueues');
      for (const topic of declaration.topics) {
        builder.addEdge({
          from: outboxId,
          to: `event.topic:${topic.name}`,
          kind: 'publishes',
          authority: 'exact',
        });
      }
    }

    const seenHandlers = new Set<string>();
    for (const definition of [...this.handlers, ...this.designHandlers]) {
      const sourcePath = definition.provenance?.path;
      const relativeSource = sourcePath ? relativePath(getProjectRoot(), sourcePath).replaceAll('\\', '/') : 'inline';
      const handlerId = `event.handler:${relativeSource}:${definition.topic.name}`;
      if (seenHandlers.has(handlerId)) continue;
      seenHandlers.add(handlerId);
      builder.addNode({
        id: handlerId,
        kind: 'event.handler',
        name: relativeSource,
        properties: { topic: definition.topic.name },
        ...(definition.provenance ? { provenance: definition.provenance } : {}),
      });
      if (sourcePath) builder.relateFromSource(sourcePath, handlerId, 'contains');
      builder.addEdge({
        from: handlerId,
        to: `event.topic:${definition.topic.name}`,
        kind: 'subscribes',
        authority: 'exact',
      });
      for (const dependency of definition.dependencies ?? []) {
        const serviceId = `service:${dependency}`;
        builder.addNode({ id: serviceId, kind: 'service', name: dependency });
        builder.addEdge({ from: handlerId, to: serviceId, kind: 'injects', authority: 'exact' });
      }
    }
  }

  async warmup(app: Module): Promise<void> {
    const logger = useLogger('events');

    this.config = resolveEventsRuntimeConfig(this.config);
    validateTransportConfig(this.config);

    // Load auto-discovered handlers from generated file
    await this.loadDiscoveredHandlers(app, logger);

    const { transport, memoryServer } = await resolveTransport(this.config, logger);
    this.transport = transport;
    this.memoryServer = memoryServer;

    // Register transport globally for publishers
    setTransport(this.transport);

    if (this.deliveryMode() === 'push') {
      // Push delivery: the receiver route owns delivery. Do not subscribe
      // handlers to the transport (it would double-deliver); keep the transport
      // available for publishing. The pull/stream loop is skipped in start().
      await this.registerPushReceiver(app, logger);
      return;
    }

    await this.subscribeHandlers(logger);
  }

  async start(app: Module): Promise<void> {
    // Wire DI scope factory into the broker when a ContainerContext is available.
    // Each handler invocation will get its own scope, enabling .inject() on handlers.
    this.setScopeFactory(app);

    if (this.deliveryMode() === 'push') {
      // The provider POSTs to the receiver route; there is no pull/stream loop
      // and no local HTTP pull server. But when the local in-memory broker is
      // the resolved transport it still needs starting, or publishers fail with
      // "Broker is not running". A remote/explicit transport needs no start for
      // publishing (and start() would launch the pull loops push mode skips),
      // so only the local broker is started here.
      if (this.memoryServer) {
        await this.transport?.start();
      }
      return;
    }

    if (this.memoryServer) {
      try {
        await this.memoryServer.start();
      } catch (error) {
        if (!isAddressInUse(error)) {
          throw error;
        }

        const logger = useLogger('events');
        const port = this.config.port ?? EVENTS_DEFAULT_PORT;
        if (!(await MemoryServer.isRunning(port))) {
          throw error;
        }

        logger.debug(`Local events server won startup race on port ${port}, connecting...`);
        this.memoryServer = undefined;
        this.transport = await createLocalServerTransport(port, this.config.drainTimeout, logger);
        this.setScopeFactory(app);
        setTransport(this.transport);
        await this.subscribeHandlers(logger);
        await this.transport.start();
      }
    } else if (this.transport) {
      await this.transport.start();
    }
  }

  async stop(_app: Module): Promise<void> {
    const logger = useLogger('events');
    logger.debug('Shutting down events...');

    if (this.memoryServer) {
      await this.memoryServer.stop();
    } else if (this.transport) {
      await this.transport.stop();
    }

    clearTransport();
    this.transport = undefined;
    this.memoryServer = undefined;
  }

  // ---------------------------------------------------------------------------
  // Internal — handler subscription and DI wiring
  // ---------------------------------------------------------------------------

  private async subscribeHandlers(logger: Logger): Promise<void> {
    const subscriptions = this.handlers.map(async (def) => {
      await this.transport?.subscribe(def, async (msg) => {
        await def.handler(msg);
      });
      logger.debug(`Subscribed handler to '${def.topic.name}' (${def.options.distribution})`);
    });
    await Promise.all(subscriptions);
  }

  private setScopeFactory(app: Module): void {
    const containerContext = app instanceof Application ? app.getActiveContext() : undefined;
    if (
      containerContext &&
      this.transport &&
      'setScopeFactory' in this.transport &&
      typeof this.transport.setScopeFactory === 'function'
    ) {
      this.transport.setScopeFactory(() => containerContext.createScope());
    }
  }

  /** Configured delivery mode, defaulting to pull. */
  private deliveryMode(): Delivery {
    return this.config.delivery ?? 'pull';
  }

  /**
   * A per-delivery DI scope factory for the push receiver. It reads the app's
   * context when a message arrives, not when the receiver is registered:
   * warmup() registers the receiver, and Application.prepare() builds the
   * context only after every plugin's warmup(). With no context (an app with
   * no DI registrations), the handler runs with no scope.
   */
  private resolveScopeFactory(app: Module): (() => Promise<DetachedScope | undefined>) | undefined {
    if (!(app instanceof Application)) return undefined;
    return async () => app.getActiveContext()?.createScope();
  }

  /**
   * Register the push receiver route on the app's HTTP server. The route is
   * service-to-service (csrfExempt + minimal), and the receiver enforces its own
   * OIDC + service-account-allowlist auth fail-closed.
   */
  private async registerPushReceiver(app: Module, logger: Logger): Promise<void> {
    if (!this.config.push) {
      throw new Error("events delivery 'push' requires a 'push' config (issuer, audience, allowedServiceAccounts)");
    }
    const httpPlugin = await app.ensurePlugin(HttpPlugin);
    const receiver = createPushReceiver(this.config.push, this.handlers, this.resolveScopeFactory(app));
    httpPlugin.post(PUSH_RECEIVER_PATH, receiver, { csrfExempt: true, minimal: true });
    logger.debug(`Registered push receiver at ${PUSH_RECEIVER_PATH} (${this.handlers.length} handler(s))`);
  }

  private async loadDiscoveredHandlers(app: Module, logger: Logger): Promise<void> {
    // Resolve module: preloaded > registered loader > dynamic import
    let loaderModule: Record<string, unknown> | undefined;
    if (this.config.preloadedModule) {
      loaderModule = this.config.preloadedModule;
    } else if (this.scansHandlers) {
      // Only this plugin's own key: a plugin that scans nothing owns no
      // generated loader and must not adopt the one another plugin registered.
      loaderModule = await loadRegisteredModule(this.loaderKey(app));
      if (!loaderModule) {
        let loaderPath = this.eventsLoaderPath;
        if (!loaderPath && this.config.scanPath) {
          loaderPath = resolveEventsLoaderPath(this.config.scanPath, getProjectRoot());
        }

        if (loaderPath && fileExists(loaderPath)) {
          loaderModule = await import(loaderPath);
        }
      }
    }

    if (loaderModule) {
      for (const loadedPlugin of Object.values(loaderModule)) {
        if (loadedPlugin instanceof EventsPlugin) {
          this.handlers.push(...loadedPlugin.handlers);
          logger.debug(`Auto-discovered ${loadedPlugin.handlers.length} handler(s)`);
        }
      }
    }
  }
}

// ---------------------------------------------------------------------------
// events() — convenience factory
// ---------------------------------------------------------------------------

/**
 * Create an events plugin.
 *
 * By default, auto-discovers handler files (`*.on.ts`) in a sibling `events/`
 * directory relative to the calling file.
 *
 * @param config - Plugin configuration
 * @returns An EventsPlugin instance
 *
 * @example Auto-discovery (default)
 * ```typescript
 * // Scans ./events/ for *.on.ts files
 * const app = application()
 *   .use(events());
 * ```
 *
 * @example Explicit handlers
 * ```typescript
 * const app = application()
 *   .use(events({
 *     autoScan: false,
 *     handlers: [onUserCreated, onOrderPlaced],
 *   }));
 * ```
 *
 * @example Custom scan directory
 * ```typescript
 * const app = application()
 *   .use(events({ scanFolder: 'subscribers' }));
 * ```
 */
export function events(config: EventsConfig = {}): EventsPlugin {
  const autoScan = config.autoScan ?? true;
  const scanFolder = config.scanFolder ?? 'events';

  // Auto-detect scan path from caller location
  if (autoScan && !config.scanPath) {
    const caller = getExternalCaller();
    if (caller?.filePath) {
      const pathToScan = `${getDirectoryName(caller.filePath)}/${scanFolder}`;
      if (fileExists(pathToScan)) {
        config = { ...config, scanPath: pathToScan };
      }
    }
  }

  return new EventsPlugin(config);
}
