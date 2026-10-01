import { file, Glob } from 'bun';
import { mkdirSync, writeFileSync } from 'node:fs';
import { fileExists, getDirectoryName, getProjectRoot, joinPath, readPackageJson } from '@putnami/utils';
import { ApiPlugin, applyPrefix, type DiscoveredRoute, isEndpointDefinition, isStreamEndpointDefinition } from '../api';
import type { GenerateResult, Module, Plugin } from '../application';
import type { HttpMethod } from '../http';
import { HttpPlugin, HttpResponse } from '../http';
import { generateProto, type ProtoDocument, type ProtoOptions } from './proto';

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

export interface ProtoConfig {
  /** Proto package name (e.g. "myapp.v1"). Defaults to package.json name */
  packageName?: string;
  /** Optional Go package option */
  goPackage?: string;
  /** If true, exposes the .proto file at a public route (default: false) */
  exposeRoute?: boolean;
  /** Custom route path for the proto file (default: /_/api.proto) */
  publicRoute?: string;
  /**
   * Output path for the generated `api.proto`, relative to the project root.
   *
   * - `string` (default `'schema/api.proto'`): emit the proto to a committed path so it can be
   *   reviewed in PR diffs and consumed by gRPC clients in other languages.
   * - `false`: keep the proto inside `.gen/schema/api.proto` (gitignored, build-only).
   */
  output?: string | false;
}

const DEFAULT_OUTPUT_PATH = 'schema/api.proto';
const FALLBACK_OUTPUT_PATH = '.gen/schema/api.proto';

/**
 * Proto plugin for generating Protocol Buffer definitions from API routes.
 *
 * Discovers routes from ApiPlugin and generates a `.proto` file at build time to
 * `<project>/schema/api.proto` (committed). The generated proto defines messages and services
 * that mirror the HTTP API, enabling gRPC clients in any language.
 *
 * @example
 * ```typescript
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(proto({ packageName: 'myapp.v1', exposeRoute: true }));
 * ```
 *
 * @example Opt out of committing the proto
 * ```typescript
 * proto({ output: false });
 * // Proto is written to .gen/schema/api.proto instead.
 * ```
 */
export class ProtoPlugin implements Plugin {
  private readonly config: Required<Omit<ProtoConfig, 'output'>> & Pick<ProtoConfig, 'output'>;
  private _proto: ProtoDocument | undefined;

  constructor(config: Required<Omit<ProtoConfig, 'output'>> & Pick<ProtoConfig, 'output'>) {
    this.config = config;
  }

  /**
   * Resolve the absolute path where the proto should be written / read.
   * Honors the `output` config option; `false` falls back to `.gen/`.
   */
  private resolveProtoPath(): string {
    const projectRoot = getProjectRoot();
    if (this.config.output === false) {
      return joinPath(projectRoot, FALLBACK_OUTPUT_PATH);
    }
    const relative = this.config.output ?? DEFAULT_OUTPUT_PATH;
    return joinPath(projectRoot, relative);
  }

  /**
   * Build-time: scan API route files, generate api.proto.
   */
  async generate(app: Module): Promise<GenerateResult> {
    const apiPlugin = this.findApiPlugin(app);
    if (!apiPlugin) {
      return {};
    }

    const scanPath = apiPlugin.scanPath;
    if (!scanPath) {
      return {};
    }

    const protoPath = this.resolveProtoPath();
    mkdirSync(getDirectoryName(protoPath), { recursive: true });

    const routes = await this.extractRoutesFromFiles(scanPath, apiPlugin.prefix);
    const proto = generateProto(routes, this.buildOptions());
    writeFileSync(protoPath, proto.content);

    this._proto = proto;

    return {
      assets: {
        'schema/api.proto': protoPath,
      },
    };
  }

  /**
   * Runtime: generate proto from registered routes and optionally expose via HTTP route.
   */
  async warmup(app: Module): Promise<void> {
    const apiPlugin = this.findApiPlugin(app);
    if (!apiPlugin) {
      return;
    }

    // Generate proto at runtime for the proto() method
    this._proto = generateProto([...apiPlugin.routes], this.buildOptions());

    // If exposeRoute is enabled, register HTTP route to serve the proto file
    if (this.config.exposeRoute) {
      const httpPlugin = await app.ensurePlugin(HttpPlugin);
      const publicRoute = this.config.publicRoute;

      const protoPath = this.resolveProtoPath();

      httpPlugin.route('GET', publicRoute, () => {
        if (fileExists(protoPath)) {
          return new HttpResponse(file(protoPath), {
            headers: {
              'Content-Type': 'text/plain',
              'Cache-Control': 'public, max-age=86400',
            },
          });
        }

        // Fallback to runtime-generated content
        if (this._proto) {
          return new HttpResponse(this._proto.content, {
            headers: { 'Content-Type': 'text/plain' },
          });
        }

        return new HttpResponse('Not Found', { status: 404 });
      });
    }
  }

  /**
   * Return the generated proto document.
   */
  proto(): ProtoDocument | undefined {
    return this._proto;
  }

  /** Exact package used by the Proto and Connect route emitters. */
  get packageName(): string {
    return this.config.packageName;
  }

  private buildOptions(): ProtoOptions {
    return {
      packageName: this.config.packageName,
      goPackage: this.config.goPackage || undefined,
    };
  }

  private findApiPlugin(app: Module): ApiPlugin | undefined {
    try {
      return app.getPlugin(ApiPlugin);
    } catch {
      return undefined;
    }
  }

  /**
   * Scan route files and extract DiscoveredRoute metadata for proto generation.
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

// ---------------------------------------------------------------------------
// Internal helpers (mirrored from openapi.plugin.ts)
// ---------------------------------------------------------------------------

function asRoute(filePath: string): string {
  let route = getDirectoryName(filePath);
  const lastChar = route.charAt(route.length - 1);
  if (lastChar === '.' || lastChar === '/') {
    route = route.substring(0, route.length - 1);
  }
  return route.length === 0 ? '/' : route;
}

function extractMultiMethodRoutes(mod: Record<string, unknown>, path: string, routes: DiscoveredRoute[]): void {
  for (const m of HTTP_METHODS) {
    const target = mod[m];
    if (isEndpointDefinition(target)) {
      routes.push({ method: m, path, schemas: target.schemas, meta: target.meta });
    } else if (typeof target === 'function') {
      routes.push({ method: m, path });
    }
  }
}

function extractStreamRoute(mod: Record<string, unknown>, path: string, routes: DiscoveredRoute[]): void {
  const target = mod['default'];
  if (isStreamEndpointDefinition(target)) {
    routes.push({ method: 'GET', path, streamMode: target.mode, schemas: target.schemas, meta: target.meta });
  }
}

function extractSingleMethodRoute(
  mod: Record<string, unknown>,
  method: HttpMethod,
  path: string,
  routes: DiscoveredRoute[],
): void {
  const target = mod[method] ?? mod['default'];
  if (isEndpointDefinition(target)) {
    routes.push({ method, path, schemas: target.schemas, meta: target.meta });
  } else if (typeof target === 'function') {
    routes.push({ method, path });
  }
}

/**
 * Compute an idiomatic proto package name from a package.json `name` field.
 *
 * Proto package names use dots as separators and lowercase alphanumeric words.
 * The result is always suffixed with `.v1`.
 *
 * Conversion rules:
 * - Scoped packages: `@scope/name` → `scope.name.v1`
 * - Dashes/underscores become dots: `my-app` → `my.app.v1`
 * - Multiple separators collapse: `@org/my--app` → `org.my.app.v1`
 * - Falls back to `api.v1` if the name is empty
 *
 * @example
 * ```typescript
 * computeProtoPackageName('@putnami/application') // → 'putnami.application.v1'
 * computeProtoPackageName('my-cool-api')           // → 'my.cool.api.v1'
 * computeProtoPackageName()                         // → 'api.v1'
 * ```
 */
export function computeProtoPackageName(packageJsonName?: string): string {
  if (!packageJsonName) {
    return 'api.v1';
  }

  const cleaned = packageJsonName
    .replace(/^@/, '') // Strip leading scope @
    .replace(/[/@\-_]+/g, '.') // Replace separators with dots
    .replace(/^\.+|\.+$/g, '') // Trim leading/trailing dots
    .toLowerCase();

  if (!cleaned) {
    return 'api.v1';
  }

  return `${cleaned}.v1`;
}

/**
 * Factory function for creating a ProtoPlugin.
 *
 * If `packageName` is not provided, it is computed from `package.json` `name`
 * using proto naming conventions (dots as separators, `.v1` suffix).
 *
 * The proto file is generated to `<project>/schema/api.proto` (committed) by default.
 * By default, the proto is NOT publicly exposed.
 *
 * Set `exposeRoute: true` to serve the proto file at `/_/api.proto`.
 * Set `output: false` to keep the proto in `.gen/schema/api.proto` (gitignored).
 *
 * @example
 * ```typescript
 * // Auto-detect package name from package.json
 * // @putnami/my-api → package "putnami.my.api.v1"
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(proto());
 *
 * // Explicit package name and expose route
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(proto({ packageName: 'myapp.v1', exposeRoute: true }));
 * ```
 */
export function proto(config: ProtoConfig = {}): ProtoPlugin {
  const packageJson = readPackageJson(joinPath(getProjectRoot(), 'package.json'));

  const fullConfig: Required<Omit<ProtoConfig, 'output'>> & Pick<ProtoConfig, 'output'> = {
    packageName: config.packageName ?? computeProtoPackageName(packageJson?.name),
    goPackage: config.goPackage ?? '',
    exposeRoute: config.exposeRoute ?? false,
    publicRoute: config.publicRoute ?? '/_/api.proto',
    output: config.output,
  };

  return new ProtoPlugin(fullConfig);
}
