import { mkdirSync, readFileSync, writeFileSync } from 'node:fs';
import { dirname } from 'node:path';
import type {
  DesignBuilder,
  DesignContributor,
  DesignProvenance,
  GenerateResult,
  Module,
  Plugin,
  ProtoDocument,
} from '@putnami/application';
import { ApiPlugin, canonicalDesignPath } from '@putnami/application';
import {
  fileExists,
  getCurrentProject,
  getExternalCaller,
  getLogger,
  getProjectRoot,
  joinPath,
  readPackageJson,
} from '@putnami/utils';
import {
  type ClientGenConfig,
  type ClientGenConfigOperation,
  type ClientTarget,
  type GoClientGenConfig,
  resolveClientGenConfig,
  serializeClientGenConfig,
} from './config.type';
import type { SpecIR } from './ir.type';
import { ClientGenerationError, readOpenApiSource } from './openapi-reader';
import { generateProjectClients } from './project-clients';
import { readProtoSpec } from './proto-reader';

const logger = getLogger('client-generator');

/** Relative path of the emitted client-generation contract. */
const CONFIG_OUTPUT_PATH = '.gen/clientgen/config.json';

function uniquePaths(paths: Array<string | undefined>): string[] {
  return [...new Set(paths.filter((path): path is string => path !== undefined))];
}

/**
 * Derive, for every endpoint in the application, the feature that produced it.
 *
 * Ownership is the module that owns the {@link ApiPlugin} the endpoint was
 * registered on — never this generator's module. An endpoint whose owner
 * declares no feature is simply absent from the table, so the emitters leave it
 * unattributed instead of letting it borrow the generator's lineage. This
 * mirrors the Go describer (`ClientsPlugin.operationProducers`) exactly.
 *
 * The walk is the module tree in registration order and the result is sorted,
 * so the emitted contract is byte-stable across runs. Two api plugins cannot
 * legitimately serve the same method and path, so the first owner in tree order
 * wins rather than producing an ambiguous second attribution.
 */
function resolveOperationProducers(app: Module, project: string): ClientGenConfigOperation[] {
  const seen = new Set<string>();
  const operations: ClientGenConfigOperation[] = [];
  if (typeof app.collectPlugins !== 'function') return operations;
  for (const { plugin, owner } of app.collectPlugins()) {
    if (!(plugin instanceof ApiPlugin)) continue;
    const feature = typeof owner.getEffectiveFeature === 'function' ? owner.getEffectiveFeature() : undefined;
    if (!feature) continue;
    for (const route of plugin.designRoutes) {
      const method = route.method.toUpperCase();
      const path = canonicalDesignPath(route.path);
      const key = `${method} ${path}`;
      if (seen.has(key)) continue;
      seen.add(key);
      operations.push({ method, path, producerProject: project, producerFeature: feature.id });
    }
  }
  // Code-unit comparison, not localeCompare: the emitted contract must be
  // byte-identical to the Go describer's output on any machine locale.
  operations.sort((left, right) =>
    left.method === right.method
      ? compareCodeUnits(left.path, right.path)
      : compareCodeUnits(left.method, right.method),
  );
  return operations;
}

function compareCodeUnits(left: string, right: string): number {
  if (left < right) return -1;
  return left > right ? 1 : 0;
}

/**
 * Configuration for the client generator plugin.
 */
export interface ClientGeneratorConfig {
  /** Allow an external, unmarked OpenAPI document. Provider generation is strict by default. */
  thirdParty?: boolean;
  /** Technology targets for generation. Default: ['ts'] */
  targets?: ClientTarget[];
  /** TS output directory relative to project root. Default: "clients/ts" */
  output?: string;
  /** npm package name for the generated TS client. Auto-computed if not provided. */
  packageName?: string;
  /** Go target options (consumed by the Go client emitter in Phase 2b). */
  go?: Partial<GoClientGenConfig>;
}

/**
 * Plugin that generates typed API client code from the generated OpenAPI spec.
 *
 * Runs in the `postGenerate()` lifecycle — *after* every plugin's `generate()`
 * has resolved — so the OpenAPI plugin has finished writing `openapi.json`
 * before this reader opens it. Reading the spec inside the parallel
 * `generate()` pass would race that write.
 *
 * @example
 * ```typescript
 * const app = application()
 *   .use(http({ port: 3000 }))
 *   .use(api())
 *   .use(openapi({ title: 'Users API', version: '1.0.0' }))
 *   .use(clientGenerator({ packageName: '@myorg/users-client' }));
 * ```
 */
export class ClientGeneratorPlugin implements Plugin, DesignContributor {
  private readonly config: ClientGeneratorConfig;
  private readonly provenance?: DesignProvenance;
  private generatedDesign?: {
    project: string;
    config: ClientGenConfig;
    spec: SpecIR;
  };

  constructor(config: ClientGeneratorConfig = {}) {
    this.config = config;
    const caller = getExternalCaller(getProjectRoot());
    this.provenance = caller
      ? {
          path: caller.filePath,
          line: caller.lineNumber,
          ...(caller.functionName ? { symbol: caller.functionName } : {}),
        }
      : undefined;
  }

  async postGenerate(app: Module, generated?: GenerateResult): Promise<GenerateResult> {
    this.generatedDesign = undefined;
    const projectRoot = getProjectRoot();
    const assets: Record<string, string> = {};

    // Always emit the client-generation contract first, even when no client
    // code is produced this run: the Go emitter (Phase 2b) and any downstream
    // tooling read this single source of truth for targets, output dirs, and
    // package/module naming.
    const project = getCurrentProject().name;
    const operations = resolveOperationProducers(app, project);
    const resolved: ClientGenConfig = {
      ...this.resolveConfig(projectRoot),
      ...(operations.length > 0 ? { design: { operations } } : {}),
    };
    assets[CONFIG_OUTPUT_PATH] = this.writeConfig(projectRoot, resolved);

    const openapiPath = this.resolveOpenApiPath(projectRoot, generated);
    const specIR = await this.readSpecs(projectRoot, app, generated, openapiPath);
    if (!specIR || specIR.services.length === 0) {
      return { assets };
    }

    if (resolved.design) {
      this.generatedDesign = { project, config: resolved, spec: specIR };
    }

    // This hook runs after the provider contract has been written but before
    // the TypeScript build's typecheck. It is therefore the bootstrap path for
    // a consumer that imports a newly generated client. The workspace command
    // snapshots tracked bytes before build, while this call shares the exact
    // manifest/hash/cleanup implementation used by explicit synchronization.
    if (resolved.targets.includes('ts')) {
      const result = generateProjectClients(projectRoot, { sourcePath: openapiPath });
      for (const file of result.files) {
        assets[`client/ts/${file.slice(`${result.outputDir}/`.length)}`] = joinPath(projectRoot, file);
      }
    }

    return { assets };
  }

  contributeDesign(builder: DesignBuilder): void {
    const generated = this.generatedDesign;
    if (!generated) return;
    if (generated.config.targets.includes('ts')) {
      for (const service of generated.spec.services) {
        const nodeId = `client.generated:ts:${generated.config.ts.packageName}/${service.className}`;
        this.addDesignClient(builder, nodeId, service.className, 'ts', generated.config.ts.packageName);
        this.relateDesignOperations(builder, nodeId, service.methods);
      }
    }
    if (generated.config.targets.includes('go')) {
      const identity = generated.config.go.modulePath || generated.config.go.packageName;
      const nodeId = `client.generated:go:${identity}/${generated.config.go.clientName}`;
      this.addDesignClient(builder, nodeId, generated.config.go.clientName, 'go', identity);
      this.relateDesignOperations(
        builder,
        nodeId,
        generated.spec.services.flatMap((service) => service.methods),
      );
    }
  }

  /**
   * Record the generated client itself. It deliberately carries no `feature`
   * property: the client is emitted by one project but its operations may be
   * produced by several features, and a client-wide feature would attribute
   * every one of them to whichever feature happened to own the generator.
   */
  private addDesignClient(
    builder: DesignBuilder,
    nodeId: string,
    name: string,
    language: string,
    packageName: string,
  ): void {
    const generated = this.generatedDesign;
    if (!generated) return;
    builder.addNode({
      id: nodeId,
      kind: 'client.generated',
      name,
      properties: {
        language,
        package: packageName,
        producer: generated.project,
        ...(generated.spec.specHash ? { specHash: generated.spec.specHash } : {}),
      },
      ...(this.provenance ? { provenance: this.provenance } : {}),
    });
    builder.relateFromModule(nodeId, 'contains');
  }

  /**
   * Emit one `generatedFrom` edge per operation, keyed by the canonical
   * operation id the generated descriptor and the runtime trace use, and
   * carrying that operation's producer. An operation the attribution table does
   * not name keeps its edge (the client really was generated from it) without
   * producer properties.
   */
  private relateDesignOperations(
    builder: DesignBuilder,
    nodeId: string,
    methods: SpecIR['services'][number]['methods'],
  ): void {
    const producers = this.generatedDesign?.config.design?.operations ?? [];
    for (const method of methods) {
      const httpMethod = method.httpMethod.toUpperCase();
      const producer = producers.find((entry) => entry.method === httpMethod && entry.path === method.path);
      builder.addEdge({
        from: nodeId,
        to: `api.operation:${httpMethod}:${method.path}`,
        kind: 'generatedFrom',
        authority: 'exact',
        properties: {
          operationId: method.operationId,
          ...(producer ? { producerProject: producer.producerProject, producerFeature: producer.producerFeature } : {}),
        },
      });
    }
  }

  /**
   * Resolve the plugin config into the canonical {@link ClientGenConfig},
   * applying documented defaults (including the auto-computed TS package name).
   */
  private resolveConfig(projectRoot: string): ClientGenConfig {
    return resolveClientGenConfig(
      {
        thirdParty: this.config.thirdParty,
        targets: this.config.targets,
        ts: { output: this.config.output, packageName: this.config.packageName },
        go: this.config.go,
      },
      { defaultTsPackageName: this.resolvePackageName(projectRoot) },
    );
  }

  /** Write the resolved config to `.gen/clientgen/config.json` and return its absolute path. */
  private writeConfig(projectRoot: string, config: ClientGenConfig): string {
    const configPath = joinPath(projectRoot, CONFIG_OUTPUT_PATH);
    mkdirSync(dirname(configPath), { recursive: true });
    writeFileSync(configPath, serializeClientGenConfig(config));
    return configPath;
  }

  /**
   * Read the API spec from the generate output on disk.
   *
   * OpenAPI is the authoritative contract and embeds the exact protobuf
   * descriptor for Connect transports. A proto-only artifact is therefore a
   * legacy external input and is never accepted for a first-party provider.
   */
  private resolveOpenApiPath(projectRoot: string, generated?: GenerateResult): string | undefined {
    return uniquePaths([
      generated?.assets?.['schema/openapi.json'],
      // Supported standalone discovery for direct postGenerate() calls.
      joinPath(projectRoot, '.gen', 'schema', 'openapi.json'),
      joinPath(projectRoot, 'schema', 'openapi.json'),
    ]).find((path) => fileExists(path));
  }

  // biome-ignore lint/complexity/noExcessiveCognitiveComplexity: fail-closed format selection keeps errors source-specific
  private readSpecs(
    projectRoot: string,
    _app: Module,
    generated?: GenerateResult,
    selectedOpenApiPath = this.resolveOpenApiPath(projectRoot, generated),
  ): SpecIR | undefined {
    const openapiPath = selectedOpenApiPath;
    if (openapiPath) {
      try {
        const source = readFileSync(openapiPath, 'utf8');
        return readOpenApiSource(source, { mode: this.config.thirdParty ? 'thirdParty' : 'firstParty' });
      } catch (error) {
        if (!this.config.thirdParty) throw error;
        logger.warn(
          `[client-generator] Failed to parse OpenAPI spec at ${openapiPath}: ${error instanceof Error ? error.message : String(error)}.`,
        );
        return undefined;
      }
    }

    // A native provider must publish its paired strict OpenAPI client contract,
    // which embeds the exact protobuf descriptor when Connect is supported.
    // The legacy proto-only reader remains available solely for explicitly
    // external contracts because it cannot preserve first-party semantics.
    const protoPath = joinPath(projectRoot, '.gen', 'schema', 'api.proto.json');
    if (fileExists(protoPath)) {
      if (!this.config.thirdParty) {
        throw new ClientGenerationError(
          'clientgen_first_party_required',
          'first-party Proto/Connect generation requires the paired x-putnami-client OpenAPI contract',
          { field: 'x-putnami-client' },
        );
      }
      try {
        const protoDoc: ProtoDocument = JSON.parse(readFileSync(protoPath, 'utf8'));
        return readProtoSpec(protoDoc);
      } catch (error) {
        logger.warn(
          `[client-generator] Failed to parse proto spec at ${protoPath}: ${error instanceof Error ? error.message : String(error)}.`,
        );
      }
    }

    if (!this.config.thirdParty) {
      throw new ClientGenerationError(
        'clientgen_first_party_required',
        'first-party client generation requires a generated x-putnami-client OpenAPI contract',
        { field: 'x-putnami-client' },
      );
    }
    return undefined;
  }

  /**
   * Resolve the package name for the generated client.
   * Uses config.packageName if set, otherwise derives from the service package.json.
   */
  private resolvePackageName(projectRoot: string): string {
    if (this.config.packageName) {
      return this.config.packageName;
    }

    const pkg = readPackageJson(joinPath(projectRoot, 'package.json'));
    const baseName = pkg?.name ?? 'api';

    // @scope/my-api → @scope/my-api-client
    // my-api → my-api-client
    return `${baseName}-client`;
  }
}

/**
 * Factory function for creating a ClientGeneratorPlugin.
 */
export function clientGenerator(config: ClientGeneratorConfig = {}): ClientGeneratorPlugin {
  return new ClientGeneratorPlugin(config);
}
