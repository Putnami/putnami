import type { AotValidators } from '@putnami/runtime';
import { isAbsolute } from 'node:path';
import { BadRequestException, type InferConfig, type SchemaDefinition, useConfig, useLogger } from '@putnami/runtime';
import {
  fileExists,
  getDirectoryName,
  getExternalCaller,
  getProjectRoot,
  joinPath,
  relativePath,
} from '@putnami/utils';
import type { GenerateResult, Module, Plugin } from '../application';
import { type DesignBuilder, type DesignEdgeKind, designServiceNode } from '../features/design-graph';
import { HttpPlugin } from '../http/http.plugin';
import type { HttpRequestContext } from '../http/http-context.type';
import type { HttpMethod } from '../http/http-method.type';
import type { HttpMiddleware } from '../http/http-middleware.type';
import type { SourceDesc } from '../http/source-description';
import { SecurityMiddleware } from '../security/security.middleware';
import type { SecurityOptions } from '../security/security.types';
import {
  type ClientOperationPolicy,
  type ClientServiceContract,
  externalClientPolicyError,
  isExternalClientPolicy,
} from './client-contract';
import { clientOperationId } from './client-operation-id';
import { ApiConfig, type ApiConfigExtras } from './api.config';
import { applyPrefix } from './api.utils';
import { generateApiLoader, resolveApiModule } from './api-codegen';
import { generatedLoaderKey, generatedLoaderSlot } from '../bundled/loader-slot';
import { registerWsHandlers, upgradeToWebSocket } from './api-ws.utils';
import type { DiscoveredRoute } from './discovered-route.type';
import { normalizeGeneratedHttpRoutePath, type GeneratedHttpRoute } from '../http-routes/generation';
import { isEndpointDefinition } from './route';
import { buildAotValidatedHandler, buildDefinition } from './route/endpoint-helpers';
import type { EndpointMeta, ResponseDeclarations, ResponseMeta } from './route/response-meta';
import type { StreamEndpointDefinition } from './route/stream-endpoint';
import { isStreamEndpointDefinition } from './route/stream-endpoint';
import { createSseHandler } from './stream/sse-handler';
import {
  createProviderWireUpgrade,
  resolveProviderWireBudgets,
  resolveProviderWireHandlers,
} from './stream/provider-wire-dispatcher';
import type { ProviderWire } from './route/byte-stream';
import { resolveStreamHandlers } from './stream/stream-dispatcher';
import {
  negotiateFirstPartySubprotocol,
  resolveFirstPartyStreamAdmission,
  resolveFirstPartyStreamHandlers,
} from './stream/first-party-stream-dispatcher';

const API_ACCEPT = [
  'application/json',
  'text/plain',
  'text/html',
  'application/xml',
  'text/xml',
  'application/yaml',
  'text/yaml',
  '*/*',
];
const HTTP_METHODS: HttpMethod[] = ['GET', 'POST', 'PUT', 'DELETE', 'PATCH'];

/**
 * API Plugin for JSON API routes.
 * Supports both manual registration and file-based auto-discovery.
 *
 * @example Manual registration
 * ```typescript
 * const apiPlugin = api({ autoScan: false });
 * apiPlugin.register('/users', endpoint().handle(() => ({ users: [] })), 'GET');
 * app.use(apiPlugin);
 * ```
 *
 * @example File-based routing (default)
 * ```typescript
 * app.use(api()); // Scans ./api folder for route files
 * ```
 *
 * @example With prefix
 * ```typescript
 * app.use(api({ prefix: '/v1' })); // All routes prefixed with /v1
 * ```
 */
export class ApiPlugin implements Plugin {
  private readonly config: InferConfig<typeof ApiConfig>;
  private readonly httpPlugin: HttpPlugin;
  private apiLoaderPath: string | undefined;
  private _scanPath: string | undefined;
  /**
   * Pre-loaded module to use instead of dynamic import.
   * When provided, warmup() skips the `import()` call and uses this module directly.
   * This enables bundled builds where dynamic imports cannot be resolved.
   */
  private preloadedModule?: Record<string, unknown>;

  readonly discovered: ({ method?: HttpMethod; route: string } & SourceDesc)[] = [];
  private readonly _routes: DiscoveredRoute[] = [];
  private _generatedRoutes: DiscoveredRoute[] = [];
  private readonly _streamDefs = new Map<string, StreamEndpointDefinition>();
  private readonly _globalThrows: ResponseMeta[] = [];
  private _moduleSecurity?: SecurityOptions;
  private readonly _clientContract?: ClientServiceContract;

  constructor(
    config: Partial<InferConfig<typeof ApiConfig>>,
    scanPath?: string,
    preloadedModule?: Record<string, unknown>,
    clientContract?: ClientServiceContract,
  ) {
    this.config = useConfig(ApiConfig, { confInit: config });
    this._scanPath = scanPath && !isAbsolute(scanPath) ? joinPath(getProjectRoot(), scanPath) : scanPath;
    this.httpPlugin = new HttpPlugin();
    this.preloadedModule = preloadedModule;
    this._clientContract = clientContract;

    // Eagerly compute the gen file path so warmup() can find it even when
    // generate() ran in a different process (e.g. pre-build hook subprocess).
    if (this._scanPath) {
      const relativeScanPath = relativePath(getProjectRoot(), this._scanPath);
      const genDirPath = joinPath(getProjectRoot(), '.gen', relativeScanPath);
      this.apiLoaderPath = joinPath(genDirPath, '.api-application.gen.ts');
    }
  }

  /**
   * Declare error responses inherited by all endpoints registered on this plugin.
   * Endpoint-level `.throws()` overrides global throws for the same status code.
   *
   * @example
   * ```ts
   * api({ autoScan: false })
   *   .throws(401, 'Unauthorized', { message: String })
   *   .throws(500, 'Internal error')
   *   .register('/users', ...)
   * ```
   */
  throws(status: number, description: string, schema?: SchemaDefinition): this {
    this._globalThrows.push({ status, description, schema });
    return this;
  }

  /**
   * Apply prefix to a route path.
   */
  private prefixPath(path: string): string {
    return applyPrefix(path, this.config.prefix);
  }

  /**
   * Apply a middleware to all requests whose path matches `pattern`.
   *
   * Pattern syntax:
   * - exact path (`/admin`) — only matches exactly that path
   * - prefix wildcard (`/admin/*`) — matches `/admin` and any path starting with `/admin/`
   * - global wildcard (`*`) — matches every path
   *
   * The pattern is evaluated against the post-prefix path, so a plugin configured with
   * `prefix: '/v1'` and `.use('/admin/*', mw)` matches requests to `/v1/admin/...`.
   *
   * @example Auth on a sub-tree
   * ```ts
   * api()
   *   .use('/admin/*', SecurityMiddleware({ roles: ['admin'] }))
   *   .use('/internal/*', RateLimitMiddleware({ max: 10 }))
   * ```
   */
  use(pattern: string, middleware: HttpMiddleware): this {
    const matchPath = compilePathPattern(this.prefixPath(pattern));
    this.httpPlugin.use(async (ctx, next) => {
      if (!matchPath(ctx.path())) {
        return next();
      }
      return middleware(ctx, next);
    });
    return this;
  }

  /**
   * Register a route from an imported module or endpoint definition.
   *
   * Auto-detects the endpoint type:
   * - `StreamEndpointDefinition` (from `endpoint().returns(Stream(…)).handle(…)`) → stream (WebSocket / SSE)
   * - `EndpointDefinition` (from `endpoint().handle(…)`) → HTTP route
   * - Object with named method exports (`{ GET, POST, … }`) → multi-method HTTP routes
   *
   * @param path    Route path (e.g. '/users', '/chat/[roomId]')
   * @param handler Module exports or endpoint definition
   * @param method  Optional HTTP method hint (from file name convention)
   */
  register(
    path: string,
    handler: Record<string, unknown>,
    method?: HttpMethod,
    aot?: AotValidators,
    provenance?: DiscoveredRoute['provenance'],
  ): this {
    // Resolve default export if present
    const target = handler?.['default'] ?? handler;

    // Stream endpoint (WebSocket / SSE)
    if (isStreamEndpointDefinition(target)) {
      return this.registerStream(path, target);
    }

    const prefixedPath = this.prefixPath(path);

    // API routes are CSRF-exempt by default; opt in with api({ csrf: true }).
    const csrfExempt = !this.config.csrf;

    if (method) {
      // Single-method file (get.ts, post.ts, etc.)
      const endpointTarget = handler[method] ?? handler['default'] ?? handler;
      // A file-based route file exports its definition under the method name,
      // so a stream declared in `get.ts` arrives here rather than as the
      // module's default export. Streams negotiate SSE and WebSocket on GET.
      if (isStreamEndpointDefinition(endpointTarget)) {
        if (method !== 'GET') {
          throw new Error(`stream endpoint at '${path}' must be declared on GET, not ${method}`);
        }
        return this.registerStream(path, endpointTarget);
      }
      if (isEndpointDefinition(endpointTarget)) {
        this.assertExternalContract(method, prefixedPath, endpointTarget.meta?.client);
        // Build-time AOT: swap the generic validation for compiled validators when
        // the codegen emitted them and the route has no middleware to preserve.
        const routeHandler =
          aot && !endpointTarget.middleware ? buildAotValidatedHandler(endpointTarget, aot) : endpointTarget.handler;
        this.httpPlugin.route(method, prefixedPath, routeHandler, {
          accept: API_ACCEPT,
          // Per-endpoint `.csrfExempt()` opts this single route out of CSRF;
          // otherwise fall back to the api-level default. Never widens siblings.
          csrfExempt: endpointTarget.csrfExempt ?? csrfExempt,
          minimal: endpointTarget.minimal,
          firstPartyErrors: this.servesFirstParty(endpointTarget.meta?.client),
        });
        this.trackRoute(method, prefixedPath, endpointTarget, provenance);
      }
    } else {
      // Multi-method file (route.ts / routes.ts)
      for (const m of HTTP_METHODS) {
        const ep = handler[m];
        if (isEndpointDefinition(ep)) {
          this.assertExternalContract(m, prefixedPath, ep.meta?.client);
          this.httpPlugin.route(m, prefixedPath, ep.handler, {
            accept: API_ACCEPT,
            // Per-endpoint `.csrfExempt()` opts this single route out of CSRF;
            // otherwise fall back to the api-level default. Never widens siblings.
            csrfExempt: ep.csrfExempt ?? csrfExempt,
            minimal: ep.minimal,
            firstPartyErrors: this.servesFirstParty(ep.meta?.client),
          });
          this.trackRoute(m, prefixedPath, ep, provenance);
        }
      }
    }

    return this;
  }

  /**
   * Register a StreamEndpointDefinition (produced by `endpoint().returns(Stream(…)).handle(…)`).
   *
   * - **bidirectional** / **client-stream**: registered as WebSocket
   * - **server-stream**: registered as both WebSocket and SSE (protocol negotiation)
   */
  private registerStream(path: string, def: StreamEndpointDefinition): this {
    let route = path;
    if (route === '') {
      route = '/';
    } else if (route[0] !== '/') {
      route = `/${route}`;
    }
    route = this.prefixPath(route.trim());

    this.assertExternalContract('GET', route, def.meta?.client);

    // API routes are CSRF-exempt by default; opt in with api({ csrf: true }).
    const csrfExempt = !this.config.csrf;

    // Track for OpenAPI generation (stream endpoints use GET for SSE/WS upgrade)
    this.trackRoute('GET', route, def);

    // Store stream definition so GrpcPlugin can bridge streaming RPCs
    this._streamDefs.set(route, def);

    // A provider-owned wire speaks its own vocabulary whether or not this API
    // declares a client contract: the upgrade request is its admission.
    if (def.wire) {
      return this.registerProviderWire(route, def, def.wire, csrfExempt);
    }

    // A first-party contract turns the socket into the declared service stream:
    // the upgrade negotiates `putnami.service.v1` and the first frame admits.
    // Without a contract the legacy raw-JSON bridge stays exactly as it was,
    // and so does a stream an external authority owns.
    const contract = this.servesFirstParty(def.meta?.client) ? this._clientContract : undefined;
    const wsHandlers = contract
      ? resolveFirstPartyStreamHandlers(
          def,
          resolveFirstPartyStreamAdmission({
            operationId: clientOperationId('GET', route),
            path: route,
            contract,
            definition: def,
          }),
        )
      : resolveStreamHandlers(def);
    const upgrade = (context: HttpRequestContext) => {
      if (!contract) return upgradeToWebSocket(context);
      const negotiated = negotiateFirstPartySubprotocol(context.req.headers.get('sec-websocket-protocol'));
      if (negotiated.refused) {
        throw new BadRequestException('websocket subprotocol is not one this route speaks');
      }
      return upgradeToWebSocket(context, negotiated.subprotocol ? { subprotocol: negotiated.subprotocol } : undefined);
    };

    if (def.mode === 'server') {
      // Server-stream: register both SSE (HTTP GET) and WebSocket.
      //
      // The declared stream bounds reach SSE too, so a provider that states
      // maxFrameBytes or heartbeatMs gets them on whichever transport the
      // consumer picked. Only what the declaration states is passed: the
      // WebSocket admission floors are its own — heartbeatMs floors at 0 there,
      // which would silence an SSE heartbeat nobody asked to remove.
      //
      // A first-party stream that declares a continuation also speaks the
      // negotiated SSE wire (ADR 0013 of protocols/clientcontract); every
      // other route keeps the legacy framing for every request. The drain a
      // negotiated stream follows is the serving HTTP plugin's, read from the
      // request: a scanned route folder is loaded once per process and merged
      // into every application instance that scans it, so nothing this
      // plugin owns could tell one instance's stop from another's.
      const sseOptions = {
        ...declaredStreamBounds(def, contract),
        negotiatesWire: contract !== undefined && def.meta?.client?.sseContinuation !== undefined,
      };
      const sseHandler = buildDefinition(createSseHandler(def, sseOptions), undefined, [
        ...(def.middleware ?? []),
      ]).handler;

      // GET route: negotiate between SSE and WebSocket upgrade
      this.httpPlugin.route(
        'GET',
        route,
        (context: HttpRequestContext) => {
          if (context.req.headers.has('upgrade')) {
            return upgrade(context);
          }
          const accept = context.req.headers.get('accept') ?? '';
          if (accept.includes('text/event-stream')) {
            return sseHandler(context);
          }
          throw new BadRequestException('stream endpoint requires "Upgrade: websocket" or "Accept: text/event-stream"');
        },
        { accept: [...API_ACCEPT, 'text/event-stream'], csrfExempt, firstPartyErrors: Boolean(contract) },
      );

      registerWsHandlers(this.httpPlugin, route, wsHandlers);
    } else {
      // Bidirectional / client-stream: WebSocket only
      this.httpPlugin.route('GET', route, (context: HttpRequestContext) => {
        if (context.req.headers.has('upgrade')) return upgrade(context);
        throw new BadRequestException('websocket, expected header "upgrade"');
      });
      registerWsHandlers(this.httpPlugin, route, wsHandlers);
    }

    return this;
  }

  /**
   * Register a stream whose WebSocket wire the provider owns.
   *
   * The upgrade request is the admission, so it takes the path SSE takes: the
   * endpoint's middleware and params/query validation run on it and refuse
   * with an ordinary HTTP response before any socket exists. The route then
   * speaks exactly its declared subprotocol, or none, and hands the admitted
   * socket to the provider-wire dispatcher.
   */
  private registerProviderWire(
    route: string,
    def: StreamEndpointDefinition,
    wire: ProviderWire,
    csrfExempt: boolean,
  ): this {
    const upgrade = buildDefinition(createProviderWireUpgrade(def, wire), undefined, [
      ...(def.middleware ?? []),
    ]).handler;
    this.httpPlugin.route(
      'GET',
      route,
      (context: HttpRequestContext) => {
        if (!context.req.headers.has('upgrade')) {
          throw new BadRequestException('websocket, expected header "upgrade"');
        }
        return upgrade(context);
      },
      { csrfExempt, firstPartyErrors: Boolean(this._clientContract) },
    );
    registerWsHandlers(
      this.httpPlugin,
      route,
      resolveProviderWireHandlers(def, wire, resolveProviderWireBudgets(this._clientContract, def.meta?.client)),
    );
    return this;
  }

  /**
   * Track a route for OpenAPI spec generation.
   */
  private trackRoute(
    method: HttpMethod,
    path: string,
    target: unknown,
    provenance?: DiscoveredRoute['provenance'],
  ): void {
    const entry: DiscoveredRoute = { method, path };
    if (isEndpointDefinition(target)) {
      if (target.schemas) {
        entry.schemas = target.schemas;
      }
      entry.responses = this.mergeResponses(target.responses);
      entry.meta = this.mergeMeta(target.meta);
      entry.dependencies = target.dependencies;
      entry.provenance = provenance ?? target.provenance;
    } else if (isStreamEndpointDefinition(target)) {
      if (target.schemas) {
        entry.schemas = target.schemas;
      }
      entry.streamMode = target.mode;
      if (target.wire) entry.wire = target.wire;
      entry.responses = this.mergeResponses(target.responses);
      entry.meta = this.mergeMeta(target.meta);
    }
    this._routes.push(entry);
  }

  /**
   * Merge endpoint-level responses with plugin-level global throws.
   * Endpoint-level throws override global throws for the same status code.
   */
  private mergeResponses(endpointResponses?: ResponseDeclarations): ResponseDeclarations | undefined {
    const endpointThrows = endpointResponses?.throws ?? [];
    const endpointReturns = endpointResponses?.returns;
    const endpointErrorCodes =
      endpointResponses?.errorCodes && endpointResponses.errorCodes.length > 0
        ? endpointResponses.errorCodes
        : undefined;
    const endpointErrorOptions = endpointResponses?.errorOptions;
    const endpointErrorDetails = endpointResponses?.errorDetails;

    if (this._globalThrows.length === 0 && endpointThrows.length === 0 && !endpointReturns && !endpointErrorCodes) {
      return undefined;
    }

    // Merge throws: endpoint-level overrides global for same status
    let mergedThrows: readonly ResponseMeta[] | undefined;
    if (this._globalThrows.length > 0 || endpointThrows.length > 0) {
      const throwsMap = new Map<number, ResponseMeta>();

      // Global first (lower priority)
      for (const t of this._globalThrows) {
        throwsMap.set(t.status, t);
      }
      // Endpoint overrides (higher priority)
      for (const t of endpointThrows) {
        throwsMap.set(t.status, t);
      }

      mergedThrows = [...throwsMap.values()];
    }

    return {
      returns: endpointReturns,
      errorCodes: endpointErrorCodes,
      errorOptions: endpointErrorOptions,
      errorDetails: endpointErrorDetails,
      throws: mergedThrows,
    };
  }

  /**
   * Merge endpoint-level meta with module-level security.
   * Endpoint-level security takes precedence over module-level security.
   */
  private mergeMeta(endpointMeta?: EndpointMeta): EndpointMeta | undefined {
    if (!endpointMeta && !this._moduleSecurity) {
      return undefined;
    }

    // Endpoint has its own security → use endpoint meta as-is
    if (endpointMeta?.security) {
      return endpointMeta;
    }

    // No endpoint security but module-level security → inject it
    if (this._moduleSecurity) {
      return { ...endpointMeta, security: this._moduleSecurity };
    }

    return endpointMeta;
  }

  /**
   * Return all discovered routes (including schemas) for this plugin.
   */
  get routes(): readonly DiscoveredRoute[] {
    return this._routes;
  }

  /**
   * Routes this plugin owns for build-time projections: the ones registered on
   * it plus the ones its scanned route modules declared.
   *
   * Build-time consumers (the design graph, the client generator's producer
   * attribution) must attribute an operation to the module that owns the plugin
   * which declared it. Once child plugins are merged at warmup, the root's
   * `routes` can no longer answer that, so ownership is read per plugin here.
   */
  get designRoutes(): readonly DiscoveredRoute[] {
    return [...this._routes, ...this._generatedRoutes];
  }

  /**
   * @internal Scanned routes read back from the generated loader for the design
   * graph. Empty when the loader was never imported because no feature could
   * attribute these routes.
   */
  get generatedRoutesForDesign(): readonly DiscoveredRoute[] {
    return this._generatedRoutes;
  }

  /** Derive API operations, schemas, and injected services from native routes. */
  contributeDesign(builder: DesignBuilder): void {
    const seen = new Set<string>();
    for (const route of this.designRoutes) {
      const canonicalPath = canonicalDesignPath(route.path);
      const operationId = `api.operation:${route.method}:${canonicalPath}`;
      if (seen.has(operationId)) continue;
      seen.add(operationId);
      builder.addNode({
        id: operationId,
        kind: 'api.operation',
        name: `${route.method} ${canonicalPath}`,
        properties: {
          method: route.method,
          operationId: clientOperationId(route.method, route.path),
          path: canonicalPath,
          ...(route.meta?.description ? { description: route.meta.description } : {}),
        },
        ...(route.provenance ? { provenance: route.provenance } : {}),
      });
      builder.relateFromModule(operationId, 'exposes');
      for (const dependency of route.dependencies ?? []) {
        // The endpoint records the injected token's label, so this is the same
        // identity the module registration and the database plugin mint for it.
        const service = designServiceNode(dependency);
        builder.addNode(service);
        builder.addEdge({ from: operationId, to: service.id, kind: 'injects', authority: 'exact' });
      }
      contributeRouteSchema(builder, operationId, 'params', route.schemas?.params, 'accepts');
      contributeRouteSchema(builder, operationId, 'query', route.schemas?.query, 'accepts');
      contributeRouteSchema(builder, operationId, 'body', route.schemas?.body, 'accepts');
      contributeRouteSchema(builder, operationId, 'response', route.schemas?.returns, 'returns');
    }
  }

  /**
   * Return stream endpoint definitions by path.
   * Used by GrpcPlugin to bridge streaming RPCs without server.fetch().
   */
  get streamDefinitions(): ReadonlyMap<string, StreamEndpointDefinition> {
    return this._streamDefs;
  }

  /**
   * Return the resolved scan path, if file-based routing is enabled.
   */
  get scanPath(): string | undefined {
    return this._scanPath;
  }

  /**
   * Whether this plugin serves a scanned route folder: it names a `scanPath`, or
   * leaves `autoScan` on. Read from configuration, never from the filesystem, so
   * a build and the packaged binary it produces agree even though the binary has
   * no route folder for the default `api()` to find.
   */
  private get scansRoutes(): boolean {
    return this._scanPath !== undefined || this.config.autoScan;
  }

  /**
   * The key this plugin's generated route loader is exported and registered
   * under: its slot among the scanning `api()` plugins of the application, in
   * module-tree registration order. Two scanned plugins therefore never share a
   * key, and the build and the packaged binary compute the same one because
   * they compose the same tree. A plugin outside any application tree keeps
   * the default `api-loader` key.
   */
  private loaderKey(owner: Module): string {
    const slot = generatedLoaderSlot(
      owner,
      this,
      (plugin): plugin is ApiPlugin => plugin instanceof ApiPlugin && plugin.scansRoutes,
    );
    return generatedLoaderKey('api', slot);
  }

  /**
   * Return the route prefix, if configured.
   */
  get prefix(): string | undefined {
    return this.config.prefix;
  }

  /**
   * The prefix this plugin's scanned routes are served under: its own `prefix`,
   * else the `.path()` of the module that owns it. The generated route loader
   * and the build-time OpenAPI document both read it here, so the document
   * names the paths the workload serves.
   */
  scanPrefix(owner: Module): string | undefined {
    return this.config.prefix ?? (typeof owner?.getPath === 'function' ? owner.getPath() : undefined);
  }

  /** First-party service contract declared on `api({ client })`, when enabled. */
  get clientContract(): ClientServiceContract | undefined {
    return this._clientContract;
  }

  /**
   * Whether a route is served by the first-party pipeline: the API publishes a
   * client contract, and no external authority owns the route. A route that
   * `.client({ external })` marks keeps the standard error body and the raw
   * stream bridge, because the standard decides its wire format.
   */
  private servesFirstParty(policy: ClientOperationPolicy | undefined): boolean {
    return this._clientContract !== undefined && !isExternalClientPolicy(policy);
  }

  /**
   * Refuse a contradictory `external` declaration before the route is bound,
   * the way the Go framework refuses it in `Configure`. The OpenAPI generation
   * applies the same rule, so a provider that publishes no document is refused
   * too — and one that does is refused before it serves anything.
   */
  private assertExternalContract(method: string, path: string, policy: ClientOperationPolicy | undefined): void {
    const contradiction = externalClientPolicyError(policy, this._clientContract !== undefined);
    if (contradiction) throw new Error(`api.client operation ${method} ${path}: ${contradiction}`);
  }

  /**
   * Generate route loader during build (if scanning is enabled).
   */
  async generate(app: Module): Promise<GenerateResult> {
    const httpPlugin = await app.ensurePlugin(HttpPlugin);
    const implicitHead = httpPlugin.servesImplicitHead();
    const registeredRoutes: GeneratedHttpRoute[] = this._routes.map((route) => ({
      ...normalizeGeneratedHttpRoutePath(route.path),
      methods: route.method === 'GET' && implicitHead ? ['GET', 'HEAD'] : [route.method],
      publicEdge: true,
      provenance: { package: '@putnami/application', sourceKind: 'typed-api' },
    }));
    if (!this._scanPath) {
      return { httpRoutes: registeredRoutes };
    }

    const effectivePrefix = this.scanPrefix(app);
    const loaderKey = this.loaderKey(app);
    const { apiLoaderPath, result } = await generateApiLoader(
      this._scanPath,
      effectivePrefix,
      this.config.csrf,
      this.config.aot,
      implicitHead,
      this._clientContract,
      loaderKey,
    );
    this.apiLoaderPath = apiLoaderPath;
    await this.collectGeneratedRoutesForDesign(app, apiLoaderPath, loaderKey);
    result.httpRoutes = [...(result.httpRoutes ?? []), ...registeredRoutes];
    return result;
  }

  /**
   * Importing the generated loader constructs an ApiPlugin per scanned route and
   * registers every one of them — work worth doing only when the result can
   * actually be read back. (The route modules themselves are already imported by
   * generateApiLoader, which needs their exports for the route contract; this
   * import is the loader and its registrations, not that discovery.)
   *
   * The result is readable in exactly two cases: this plugin's owner is inside a
   * feature scope, so its routes are attributed directly; or a feature elsewhere
   * selected a source path overlapping this scan — the cross-owner case where a
   * workload root owns the scan while the feature is declared further down. A
   * feature that selected nothing from this scan cannot see these routes, so the
   * graph builder would discard every one of them.
   *
   * Failing to import must not fail the build either: the graph is a disposable
   * projection, whereas generate() owns the loader and the route contract.
   */
  private async collectGeneratedRoutesForDesign(app: Module, apiLoaderPath: string, loaderKey: string): Promise<void> {
    this._generatedRoutes = [];
    if (!this.designGraphReadsScannedRoutes(app)) return;
    try {
      const generated = await resolveApiModule(undefined, apiLoaderPath, undefined, loaderKey);
      for (const value of Object.values(generated ?? {})) {
        if (value instanceof ApiPlugin) this._generatedRoutes.push(...value._routes);
      }
    } catch (error) {
      useLogger('design').warn(
        `skipped scanned-route design discovery for '${apiLoaderPath}': ${error instanceof Error ? error.message : String(error)}`,
      );
    }
  }

  /** Can any feature actually read the routes this plugin scanned? */
  private designGraphReadsScannedRoutes(app: Module): boolean {
    if (typeof app.getEffectiveFeature !== 'function') return false;
    if (app.getEffectiveFeature()) return true;
    if (!this._scanPath || typeof app.collectFeatureSourceSelections !== 'function') return false;
    const scan = trimTrailingSlash(relativePath(getProjectRoot(), this._scanPath).replaceAll('\\', '/'));
    // A selection matches either way round: it can name the scan root, a
    // directory inside it, or a directory that contains it.
    return app.collectFeatureSourceSelections().some((source) => {
      const selected = trimTrailingSlash(source.replaceAll('\\', '/'));
      if (!selected) return false;
      return selected === scan || selected.startsWith(`${scan}/`) || scan.startsWith(`${selected}/`);
    });
  }

  /**
   * Warmup: get HttpPlugin reference and load generated routes.
   */
  async warmup(app: Module): Promise<void> {
    // Apply module-level security as a middleware on this plugin's HttpPlugin.
    // All routes registered by this ApiPlugin instance will be protected.
    const security = app.getSecurity();
    if (security) {
      this.httpPlugin.prepend(SecurityMiddleware(security));
      // Store module-level security options for OpenAPI metadata propagation
      if (typeof security !== 'function') {
        this._moduleSecurity = security;
        // Backfill module-level security to routes already registered before warmup()
        for (const route of this._routes) {
          if (!route.meta?.security) {
            route.meta = { ...route.meta, security: this._moduleSecurity };
          }
        }
      }
    }

    const rootHttpPlugin = await app.ensurePlugin(HttpPlugin);

    // Merge this plugin's own routes into the root HttpPlugin
    rootHttpPlugin.merge(this.httpPlugin);

    // Resolve module: preloaded > this plugin's registered loader > dynamic
    // import. A plugin that scans nothing owns no generated loader, so it must
    // not adopt one another plugin registered.
    const routeLoaders =
      this.preloadedModule !== undefined || this.scansRoutes
        ? await resolveApiModule(this.preloadedModule, this.apiLoaderPath, this._scanPath, this.loaderKey(app))
        : undefined;

    if (routeLoaders) {
      for (const loadedPlugin of Object.values(routeLoaders)) {
        if (loadedPlugin instanceof ApiPlugin) {
          // Merge the loaded plugin's httpPlugin into the root httpPlugin
          rootHttpPlugin.merge(loadedPlugin.httpPlugin);
          // Apply parent global throws and module security to routes from loaded child plugins
          for (const route of loadedPlugin._routes) {
            if (this._globalThrows.length > 0) {
              route.responses = this.mergeResponses(route.responses);
            }
            if (this._moduleSecurity && !route.meta?.security) {
              route.meta = { ...route.meta, security: this._moduleSecurity };
            }
          }
          // Merge discovered routes for OpenAPI generation
          this._routes.push(...loadedPlugin._routes);
          // A scanned stream route registers on the loader's plugin, and the
          // gRPC plugin bridges a declared server stream from this plugin's
          // map: without it the Connect URL answers as a unary call.
          for (const [route, def] of loadedPlugin._streamDefs) this._streamDefs.set(route, def);
        }
      }
    }
  }
}

/**
 * Canonical graph/spec form of a route path: file-based `[param]` segments
 * become OpenAPI `{param}` ones. Build-time consumers join operations on this
 * form, so it must be computed in exactly one place.
 */
export function canonicalDesignPath(path: string): string {
  return path.replace(/\[([^\]]+)]/g, '{$1}');
}

function contributeRouteSchema(
  builder: DesignBuilder,
  operationId: string,
  role: string,
  schema: SchemaDefinition | undefined,
  edge: DesignEdgeKind,
): void {
  if (!schema) return;
  const schemaId = `api.schema:${operationId.slice('api.operation:'.length)}:${role}`;
  builder.addNode({
    id: schemaId,
    kind: 'api.schema',
    name: `${role} schema`,
    properties: { fields: Object.keys(schema).sort().join(','), role },
  });
  builder.addEdge({ from: operationId, to: schemaId, kind: edge, authority: 'exact' });
}

/**
 * Factory function for creating an ApiPlugin.
 *
 * @example File-based routing (default)
 * ```typescript
 * app.use(api()); // Auto-scans ./api folder
 * ```
 *
 * @example Manual mode
 * ```typescript
 * const apiPlugin = api({ autoScan: false });
 * // Register routes manually after warmup
 * ```
 */
export function api(config: Partial<InferConfig<typeof ApiConfig>> & ApiConfigExtras = {}): ApiPlugin {
  const { scanPath: explicitScanPath, preloadedModule, client, ...configRest } = config;
  const resolvedConfig = useConfig(ApiConfig, { confInit: configRest });

  // Use explicit scanPath if provided, otherwise auto-detect from caller location
  let scanPath: string | undefined = explicitScanPath;
  if (!scanPath && resolvedConfig.autoScan) {
    const caller = getExternalCaller();
    if (caller?.filePath) {
      const pathToScan = `${getDirectoryName(caller.filePath)}/${resolvedConfig.scanFolder}`;
      if (fileExists(pathToScan)) {
        scanPath = pathToScan;
      }
    }
  }

  return new ApiPlugin(resolvedConfig, scanPath, preloadedModule, client);
}

/**
 * Compile a path pattern to a matcher function.
 *
 * - `*` matches every path.
 * - A trailing `/*` matches the prefix and any path beneath it.
 * - Otherwise the pattern matches the request path exactly.
 *
 * The matcher is invoked with `ctx.path()`, which returns the bare path with no leading
 * slash — patterns are normalised the same way so callers can write either `/admin/*`
 * or `admin/*` interchangeably.
 */
/** Compare a scan root and a selected source path on the same shape. */
function trimTrailingSlash(path: string): string {
  return path.replace(/\/+$/, '');
}

function compilePathPattern(pattern: string): (path: string) => boolean {
  if (pattern === '*' || pattern === '/*') {
    return () => true;
  }
  const bare = pattern.startsWith('/') ? pattern.slice(1) : pattern;
  if (bare.endsWith('/*')) {
    const prefix = bare.slice(0, -2);
    if (prefix === '') {
      return () => true;
    }
    return (path) => path === prefix || path.startsWith(`${prefix}/`);
  }
  return (path) => path === bare;
}

/**
 * The stream bounds a provider declared, operation first and document default
 * second, with nothing filled in. An undefined bound leaves the SSE handler on
 * its own default rather than adopting the WebSocket admission floor, which is
 * a different number for a different wire.
 */
function declaredStreamBounds(
  def: StreamEndpointDefinition,
  contract: ClientServiceContract | undefined,
): { maxBufferedMessages?: number; maxFrameBytes?: number; heartbeatMs?: number } {
  const stream = { ...contract?.defaults?.resilience?.stream, ...def.meta?.client?.resilience?.stream };
  return {
    maxBufferedMessages: stream.maxBufferedMessages,
    maxFrameBytes: stream.maxFrameBytes,
    heartbeatMs: stream.heartbeatMs,
  };
}
