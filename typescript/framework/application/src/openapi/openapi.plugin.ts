import { file, Glob, gzipSync } from 'bun';
import { mkdirSync, writeFileSync } from 'node:fs';
import { fileExists, getDirectoryName, getProjectRoot, joinPath, readPackageJson } from '@putnami/utils';
import {
  ApiPlugin,
  applyPrefix,
  type ClientServiceContract,
  type DiscoveredRoute,
  isEndpointDefinition,
  isStreamEndpointDefinition,
} from '../api';
import type { GenerateResult, Module, Plugin } from '../application';
import type { HttpMethod } from '../http';
import { HttpPlugin, HttpResponse } from '../http';
import { GrpcPlugin } from '../grpc';
import { ProtoPlugin } from '../proto';
import { generateOpenApiSpec, type OpenApiDocument, type OpenApiOptions } from './openapi';

const HTTP_METHODS: HttpMethod[] = ['GET', 'POST', 'PUT', 'DELETE', 'PATCH'];

/** File suffix to HTTP method mapping */
const FILE_METHOD_MAP: Record<string, HttpMethod | 'ws' | 'multi'> = {
  'get.ts': 'GET',
  'post.ts': 'POST',
  'put.ts': 'PUT',
  'patch.ts': 'PATCH',
  'delete.ts': 'DELETE',
  'ws.ts': 'ws',
  'route.ts': 'multi',
  'routes.ts': 'multi',
};

export interface OpenApiConfig {
  title: string;
  version: string;
  description?: string;
  servers?: { url: string; description?: string }[];
  /** If true, exposes the OpenAPI spec at a public route (default: false) */
  exposeRoute?: boolean;
  /**
   * Custom route path for the OpenAPI spec (default: `/_/openapi.json` for the
   * application's first document, `/_/openapi-<n>.json` for the others).
   */
  publicRoute?: string;
  /**
   * Output path for the generated `openapi.json`, relative to the project root.
   *
   * - `string` (default `'schema/openapi.json'`): emit the spec to a committed path so it can be
   *   reviewed in PR diffs and consumed by external tooling.
   * - `false`: keep the spec inside `.gen/schema/openapi.json` (gitignored, build-only).
   *
   * An application with several `openapi()` plugins gives each its own document, numbered by
   * slot: the one that publishes a client contract first, then the others in module-tree
   * registration order. Slot 0 keeps the paths above; slot `n` writes
   * `.gen/schema/openapi-<n>.json` unless `output` names a path, and is exposed on
   * `/_/openapi-<n>.json`.
   *
   * The compressed `.gz` companion always lives in `.gen/schema/` regardless of this option —
   * it is a build-time artifact, never committed.
   */
  output?: string | false;
}

const DEFAULT_OUTPUT_PATH = 'schema/openapi.json';
const FALLBACK_OUTPUT_PATH = '.gen/schema/openapi.json';
const GZ_PATH = '.gen/schema/openapi.json.gz';
const DEFAULT_PUBLIC_ROUTE = '/_/openapi.json';

/**
 * The names one document of an application writes and serves under. Slot 0 is
 * the project's contract and keeps the historical names every consumer reads
 * (`schema/openapi.json`, the `.gen` fallback and `.gz`, `/_/openapi.json`).
 */
interface DocumentNames {
  /** Asset key of the JSON document; the `.gz` companion's key appends `.gz`. */
  assetKey: string;
  /** Project-relative JSON path when `output` is left unset. */
  defaultOutput: string;
  /** Project-relative JSON path when `output` is `false`. */
  fallbackOutput: string;
  /** Project-relative path of the compressed companion. */
  gzPath: string;
  /** Route the document is exposed on when `publicRoute` is left unset. */
  publicRoute: string;
}

function documentNames(slot: number): DocumentNames {
  if (slot === 0) {
    return {
      assetKey: DEFAULT_OUTPUT_PATH,
      defaultOutput: DEFAULT_OUTPUT_PATH,
      fallbackOutput: FALLBACK_OUTPUT_PATH,
      gzPath: GZ_PATH,
      publicRoute: DEFAULT_PUBLIC_ROUTE,
    };
  }
  // A further document is not the project contract that the workspace's client
  // discovery, compose and the build's drift check read at `schema/openapi.json`,
  // so it is not committed unless `output` asks for it.
  const generated = `.gen/schema/openapi-${slot}.json`;
  return {
    assetKey: `schema/openapi-${slot}.json`,
    defaultOutput: generated,
    fallbackOutput: generated,
    gzPath: `${generated}.gz`,
    publicRoute: `/_/openapi-${slot}.json`,
  };
}

/**
 * OpenAPI plugin for generating OpenAPI 3.0.3 specifications.
 *
 * Discovers routes from ApiPlugin and generates an OpenAPI spec at build time
 * to `<project>/schema/openapi.json` (committed) — the compressed `.gz` companion stays in
 * `.gen/schema/` (not committed). At runtime, generates the spec from loaded routes and
 * optionally exposes it via an HTTP route.
 *
 * @example
 * ```typescript
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(openapi({ title: 'My API', version: '1.0.0', exposeRoute: true }));
 * ```
 *
 * @example Opt out of committing the spec
 * ```typescript
 * openapi({ title: 'My API', version: '1.0.0', output: false });
 * // Spec is written to .gen/schema/openapi.json instead.
 * ```
 */
export class OpenApiPlugin implements Plugin {
  private readonly config: OpenApiConfig;
  private _spec: OpenApiDocument | undefined;

  constructor(config: OpenApiConfig) {
    this.config = config;
  }

  /**
   * Resolve the absolute path where the JSON spec should be written / read.
   * Honors the `output` config option; `false` falls back to `.gen/`.
   */
  private resolveJsonPath(names: DocumentNames): string {
    const projectRoot = getProjectRoot();
    if (this.config.output === false) {
      return joinPath(projectRoot, names.fallbackOutput);
    }
    const relative = this.config.output ?? names.defaultOutput;
    return joinPath(projectRoot, relative);
  }

  /**
   * This document's slot among the application's `openapi()` documents: the one
   * that publishes a client contract first, then the others, each group in
   * module-tree registration order. The slot names the document's output, asset
   * key, `.gz` companion and default route, so two documents never overwrite each
   * other, and a build and the runtime compute the same names because they
   * compose the same tree.
   *
   * The contract comes first because every consumer of a project's contract —
   * `clientGenerator()`, workspace client discovery, compose, the capability
   * manifest — reads the one document at slot 0. For the same reason an
   * application publishes at most one client contract: two documents that do
   * are refused rather than one of them silently never reaching a client.
   */
  private documentSlot(app: Module): number {
    if (typeof app?.getRoot !== 'function') return 0;
    const root = app.getRoot();
    if (typeof root.collectPlugins !== 'function') return 0;
    const documents = root
      .collectPlugins()
      .filter((entry): entry is { plugin: OpenApiPlugin; owner: Module } => entry.plugin instanceof OpenApiPlugin);
    if (documents.length <= 1) return 0;
    const contracts = documents.filter(({ plugin, owner }) => plugin.publishesClientContract(owner));
    if (contracts.length > 1) {
      throw new Error(
        `openapi(): ${contracts.length} openapi() documents of this application publish a client contract, and a ` +
          'project publishes one: its generated client and every dependant read one document. Document the ' +
          'contract-bearing api() plugins with a single openapi().',
      );
    }
    const ordered = [...contracts, ...documents.filter((document) => !contracts.includes(document))];
    return Math.max(
      ordered.findIndex(({ plugin }) => plugin === this),
      0,
    );
  }

  /** Whether the document this plugin writes for `owner` carries an `x-putnami-client` contract. */
  private publishesClientContract(owner: Module): boolean {
    return documentClientContract(this.findApiPlugins(owner).plugins) !== undefined;
  }

  /**
   * Build-time: collect the documented api() plugins' routes — registered ones
   * and scanned route files — generate openapi.json, and create compressed .gz version.
   */
  async generate(app: Module): Promise<GenerateResult> {
    const { owner, plugins: apiPlugins } = this.findApiPlugins(app);
    if (apiPlugins.length === 0) {
      return {};
    }
    const client = documentClientContract(apiPlugins);

    const routes: DiscoveredRoute[] = [];
    for (const apiPlugin of apiPlugins) {
      routes.push(...apiPlugin.routes);
      if (apiPlugin.scanPath) {
        // Scanned routes are served under the owning module's `.path()` when the
        // plugin sets no prefix, as the generated loader registers them.
        // biome-ignore lint/performance/noAwaitInLoops: plugin order keeps duplicate-route resolution deterministic
        routes.push(...(await this.extractRoutesFromFiles(apiPlugin.scanPath, apiPlugin.scanPrefix(owner))));
      }
    }
    // A plugin that scans nothing and registered nothing documents nothing, as
    // before: no spec is written rather than an empty one.
    if (routes.length === 0 && apiPlugins.every((apiPlugin) => !apiPlugin.scanPath)) {
      return {};
    }

    const names = documentNames(this.documentSlot(app));
    const projectRoot = getProjectRoot();
    const openapiPath = this.resolveJsonPath(names);
    const openapiGzPath = joinPath(projectRoot, names.gzPath);

    mkdirSync(getDirectoryName(openapiPath), { recursive: true });
    mkdirSync(getDirectoryName(openapiGzPath), { recursive: true });

    const spec = generateOpenApiSpec(routes, this.buildOptions(app, client));
    const specJson = `${JSON.stringify(spec, null, 2).trimEnd()}\n`;

    // Write the human-readable spec (committed by default). End it with a
    // trailing newline so the committed file satisfies editorconfig/formatter
    // rules and stays byte-stable when regenerated during lint/build — otherwise
    // a `generate` step that runs before `format --no-fix` leaves the file dirty
    // and fails the format gate.
    writeFileSync(openapiPath, specJson);

    // Write the compressed companion (always in .gen/, for runtime serving).
    writeFileSync(openapiGzPath, gzipSync(specJson));

    this._spec = spec;

    return {
      assets: {
        [names.assetKey]: openapiPath,
        [`${names.assetKey}.gz`]: openapiGzPath,
      },
    };
  }

  /**
   * Runtime: generate spec from registered routes and optionally expose via HTTP route.
   */
  async warmup(app: Module): Promise<void> {
    const { plugins: apiPlugins } = this.findApiPlugins(app);
    if (apiPlugins.length === 0) {
      return;
    }

    // Generate spec at runtime for the spec() method, from the same plugins
    // and under the same contract the build-time document used.
    this._spec = generateOpenApiSpec(
      apiPlugins.flatMap((apiPlugin) => [...apiPlugin.routes]),
      this.buildOptions(app, documentClientContract(apiPlugins)),
    );

    // If exposeRoute is enabled, register HTTP route to serve the compressed spec
    if (this.config.exposeRoute) {
      const httpPlugin = await app.ensurePlugin(HttpPlugin);
      const names = documentNames(this.documentSlot(app));
      const publicRoute = this.config.publicRoute ?? names.publicRoute;

      const openapiPath = this.resolveJsonPath(names);
      const openapiGzPath = joinPath(getProjectRoot(), names.gzPath);

      // Register route with handler that checks file existence at request time
      httpPlugin.route('GET', publicRoute, () => {
        // Prefer compressed (.gz from .gen/), fallback to the JSON spec.
        const useCompressed = fileExists(openapiGzPath);
        const useUncompressed = fileExists(openapiPath);

        if (!useCompressed && !useUncompressed) {
          return new HttpResponse('Not Found', { status: 404 });
        }

        const filePath = useCompressed ? openapiGzPath : openapiPath;

        const headers: Record<string, string> = {
          'Content-Type': 'application/json',
          'Cache-Control': 'public, max-age=86400',
        };

        if (useCompressed) {
          headers['Content-Encoding'] = 'gzip';
        }

        return new HttpResponse(file(filePath), { headers });
      });
    }
  }

  /**
   * Return the generated OpenAPI specification.
   */
  spec(): OpenApiDocument | undefined {
    return this._spec;
  }

  private buildOptions(app: Module, client: ClientServiceContract | undefined): OpenApiOptions {
    const connect = this.resolveConnect(app);
    return {
      info: {
        title: this.config.title,
        version: this.config.version,
        description: this.config.description,
      },
      servers: this.config.servers,
      client,
      ...(connect ? { connect } : {}),
    };
  }

  /** Advertise Connect only when both the Proto contract and serving plugin are installed. */
  private resolveConnect(app: Module): OpenApiOptions['connect'] | undefined {
    try {
      const grpcPlugin = app.getPlugin(GrpcPlugin);
      const protoPlugin = app.getPlugin(ProtoPlugin);
      return {
        packageName: protoPlugin.packageName,
        unaryEncodings: grpcPlugin.connectEncodings(false),
        serverStreamEncodings: grpcPlugin.connectEncodings(true),
      };
    } catch {
      return undefined;
    }
  }

  /**
   * The api() plugins this document describes: every ApiPlugin registered on
   * the module the nearest one belongs to (this module, else the closest
   * ancestor that uses api()), in registration order.
   *
   * The scope is that one module rather than the whole tree, so a module that
   * declares its own `api({ client })` and `openapi()` publishes a contract for
   * its routes only, beside a public surface registered elsewhere.
   */
  private findApiPlugins(app: Module): { owner: Module; plugins: ApiPlugin[] } {
    let nearest: ApiPlugin;
    try {
      nearest = app.getPlugin(ApiPlugin);
    } catch {
      return { owner: app, plugins: [] };
    }
    const owner =
      typeof app.getRoot === 'function'
        ? app
            .getRoot()
            .collectPlugins()
            .find(({ plugin }) => plugin === nearest)?.owner
        : undefined;
    if (!owner) return { owner: app, plugins: [nearest] };
    return { owner, plugins: owner.getPlugins().filter((plugin): plugin is ApiPlugin => plugin instanceof ApiPlugin) };
  }

  /**
   * Scan route files and extract DiscoveredRoute metadata for OpenAPI generation.
   */
  private async extractRoutesFromFiles(scanPath: string, prefix?: string): Promise<DiscoveredRoute[]> {
    const scanner = new Glob('**/{get,post,put,patch,delete,route,routes,ws}.ts');
    const scannedFiles: { file: string; route: string; method: string | undefined }[] = [];

    for (const f of scanner.scanSync(scanPath)) {
      const method = Object.entries(FILE_METHOD_MAP).find(([suffix]) => f.endsWith(suffix))?.[1];
      const route = asRoute(f);
      scannedFiles.push({ file: f, route, method });
    }

    const imports = await Promise.all(
      scannedFiles.map(async ({ file, route, method }) => {
        try {
          const mod = await import(joinPath(scanPath, file));
          const normalizedRoute = route.startsWith('/') ? route : `/${route}`;
          const prefixedPath = applyPrefix(normalizedRoute, prefix);
          return { mod, path: prefixedPath, method };
        } catch {
          return undefined;
        }
      }),
    );

    const routes: DiscoveredRoute[] = [];
    for (const entry of imports) {
      if (!entry) continue;
      if (entry.method === 'ws') {
        extractStreamRoute(entry.mod, entry.path, routes);
      } else if (entry.method === 'multi') {
        extractMultiMethodRoutes(entry.mod, entry.path, routes);
      } else if (entry.method) {
        extractSingleMethodRoute(entry.mod, entry.method as HttpMethod, entry.path, routes);
      }
    }
    return routes;
  }
}

/**
 * The one client contract a document publishes for its api() plugins.
 *
 * A document carries one `x-putnami-client` service contract, and every
 * operation in it is published under that contract. Plugins that declare
 * different contracts — or a contract beside a plugin that declares none —
 * cannot share a document without publishing some routes under a contract
 * their plugin does not serve, so generation refuses rather than pick one.
 */
function documentClientContract(apiPlugins: readonly ApiPlugin[]): ClientServiceContract | undefined {
  const [first, ...rest] = apiPlugins;
  const expected = canonicalContract(first?.clientContract);
  for (const apiPlugin of rest) {
    if (canonicalContract(apiPlugin.clientContract) !== expected) {
      throw new Error(
        'openapi(): the api() plugins of one module declare different client contracts, and one OpenAPI document ' +
          'publishes one contract. Register the api() that declares `client` in its own module, with its own openapi().',
      );
    }
  }
  return first?.clientContract;
}

/** Key-order-independent identity of a client contract; `undefined` for none. */
function canonicalContract(contract: ClientServiceContract | undefined): string | undefined {
  return contract === undefined ? undefined : JSON.stringify(sortKeys(contract));
}

function sortKeys(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(sortKeys);
  if (value === null || typeof value !== 'object') return value;
  return Object.fromEntries(
    Object.keys(value)
      .sort()
      .map((key) => [key, sortKeys((value as Record<string, unknown>)[key])]),
  );
}

/**
 * Convert a file path to a route path.
 * e.g. `users/[id]/get.ts` → `users/[id]`
 */
function asRoute(filePath: string): string {
  let route = getDirectoryName(filePath);
  const lastChar = route.charAt(route.length - 1);
  if (lastChar === '.' || lastChar === '/') {
    route = route.substring(0, route.length - 1);
  }
  return route.length === 0 ? '/' : route;
}

/**
 * Extract routes from a multi-method module (route.ts / routes.ts).
 */
// biome-ignore lint/suspicious/noExplicitAny: Module type is dynamic
function extractMultiMethodRoutes(mod: any, path: string, routes: DiscoveredRoute[]): void {
  for (const m of HTTP_METHODS) {
    const target = mod[m];
    if (isEndpointDefinition(target)) {
      routes.push({ method: m, path, schemas: target.schemas, responses: target.responses, meta: target.meta });
    } else if (typeof target === 'function') {
      routes.push({ method: m, path });
    }
  }
}

/**
 * Extract a route from a stream module (ws.ts).
 * Stream endpoints use GET for SSE/WebSocket upgrade.
 */
// biome-ignore lint/suspicious/noExplicitAny: Module type is dynamic
function extractStreamRoute(mod: any, path: string, routes: DiscoveredRoute[]): void {
  const target = mod.default;
  if (isStreamEndpointDefinition(target)) {
    routes.push({
      method: 'GET',
      path,
      streamMode: target.mode,
      ...(target.wire ? { wire: target.wire } : {}),
      schemas: target.schemas,
      responses: target.responses,
      meta: target.meta,
    });
  }
}

/**
 * Extract a route from a single-method module (get.ts, post.ts, etc.).
 */
// biome-ignore lint/suspicious/noExplicitAny: Module type is dynamic
function extractSingleMethodRoute(mod: any, method: HttpMethod, path: string, routes: DiscoveredRoute[]): void {
  const target = mod[method] ?? mod.default;
  // A stream declared in a file-based route file (`get.ts`) is exported under
  // the method name, not as the module default, so it never reaches
  // extractStreamRoute. Without this it is silently absent from the spec — and
  // therefore from every generated client.
  if (isStreamEndpointDefinition(target)) {
    if (method === 'GET') {
      routes.push({
        method,
        path,
        streamMode: target.mode,
        ...(target.wire ? { wire: target.wire } : {}),
        schemas: target.schemas,
        responses: target.responses,
        meta: target.meta,
      });
    }
    return;
  }
  if (isEndpointDefinition(target)) {
    routes.push({ method, path, schemas: target.schemas, responses: target.responses, meta: target.meta });
  } else if (typeof target === 'function') {
    routes.push({ method, path });
  }
}

/**
 * Factory function for creating an OpenApiPlugin.
 *
 * If title, version, or description are not provided, they will be read from package.json:
 * - title defaults to package.json "name"
 * - version defaults to package.json "version"
 * - description defaults to package.json "description"
 *
 * The OpenAPI spec is generated to `.gen/schema/openapi.json` (and compressed to `.gz`)
 * at build time. By default, the spec is NOT publicly exposed.
 *
 * Set `exposeRoute: true` to register an HTTP route serving the spec at runtime.
 * The compressed version is served automatically with `Content-Encoding: gzip` header.
 *
 * @example
 * ```typescript
 * // Using explicit values (spec not exposed)
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(openapi({ title: 'My API', version: '1.0.0' }));
 *
 * // Using defaults from package.json (spec not exposed)
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(openapi({}));
 *
 * // Exposing the OpenAPI spec at /_/openapi.json
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(openapi({ exposeRoute: true }));
 *
 * // Custom route path
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(openapi({ exposeRoute: true, publicRoute: '/api/docs.json' }));
 * ```
 */
export function openapi(config: Partial<OpenApiConfig> = {}): OpenApiPlugin {
  const packageJson = readPackageJson(joinPath(getProjectRoot(), 'package.json'));

  const fullConfig: OpenApiConfig = {
    title: config.title ?? packageJson?.name ?? 'API',
    version: config.version ?? packageJson?.version ?? '1.0.0',
    description: config.description ?? packageJson?.description,
    servers: config.servers,
    exposeRoute: config.exposeRoute ?? false,
    // Left unset, the route is resolved from the document's slot at warmup.
    publicRoute: config.publicRoute,
    output: config.output,
  };

  return new OpenApiPlugin(fullConfig);
}
