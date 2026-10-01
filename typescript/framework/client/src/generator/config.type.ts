/**
 * `.gen/clientgen/config.json` — the shared client-generation contract.
 *
 * This is the single source of truth that drives every per-language client
 * emitter. The `clientGenerator()` plugin writes it during `postGenerate()`;
 * both the TypeScript and Go emitters read it (mirrored by a Go struct with
 * identical field names). Keep this schema and
 * {@link resolveClientGenConfig}'s defaults in lockstep with the Go reader so
 * the two extensions can never disagree on where/how to emit.
 */

/** A code-generation target language. */
export type ClientTarget = 'ts' | 'go';

/** TypeScript client target options. */
export interface TsClientGenConfig {
  /** Output directory, relative to the project root. */
  output: string;
  /** npm package name for the generated client package. */
  packageName: string;
}

/** Go client target options. */
export interface GoClientGenConfig {
  /** Output directory, relative to the project root. */
  output: string;
  /** Go module path of the generated client module (required to enable the `go` target). */
  modulePath: string;
  /** Go package name for the generated client. */
  packageName: string;
  /** Generated client struct name. */
  clientName: string;
  /**
   * Operations, by `operationId`, the Go target leaves out instead of failing
   * the whole client: the escape for an operation the Go emitter cannot
   * represent. The generated client's doc comment and its
   * `client.putnami.json` name every operation left out, and a name the
   * contract does not declare fails Go generation. Omitted from the emitted
   * contract when empty, exactly as the Go describer omits it.
   */
  omitOperations?: string[];
}

/**
 * One operation's producer attribution, keyed by HTTP method and route path.
 *
 * The canonical operation id is deliberately absent: it belongs to the OpenAPI
 * contract, and repeating it here would create a second identity to keep in
 * sync. Key order fixes the emitted JSON and must stay identical to the Go
 * struct's field order (`clientGenConfigOperation` in `go/framework/api`).
 */
export interface ClientGenConfigOperation {
  method: string;
  path: string;
  producerProject: string;
  producerFeature: string;
}

/** The fully-resolved on-disk shape of `.gen/clientgen/config.json`. */
export interface ClientGenConfig {
  /** Explicit compatibility mode for external, unmarked specifications. */
  thirdParty?: boolean;
  /** Targets to generate. */
  targets: ClientTarget[];
  /** TypeScript target configuration. */
  ts: TsClientGenConfig;
  /** Go target configuration. */
  go: GoClientGenConfig;
  /**
   * Producer attribution for the operations the application declares a feature
   * for, copied into every generated target. Omitted entirely when nothing is
   * attributed; an operation missing from the table is generated unattributed
   * rather than inheriting the client generator's own feature.
   */
  design?: {
    operations: ClientGenConfigOperation[];
  };
}

/** Documented defaults for every field, applied by {@link resolveClientGenConfig}. */
export const CLIENTGEN_DEFAULTS = {
  targets: ['ts'] as ClientTarget[],
  ts: { output: 'clients/ts' },
  go: { output: 'clients/go', modulePath: '', packageName: 'client', clientName: 'Client' },
} as const;

/** User-supplied overrides for the resolver — only fields a caller wants to change. */
export interface ClientGenConfigInput {
  thirdParty?: boolean;
  targets?: ClientTarget[];
  ts?: Partial<TsClientGenConfig>;
  go?: Partial<GoClientGenConfig>;
}

/** Context the resolver needs to fill in computed defaults. */
export interface ClientGenConfigContext {
  /** Default npm package name when `ts.packageName` is not supplied (e.g. `<pkg>-client`). */
  defaultTsPackageName: string;
}

/**
 * Apply {@link CLIENTGEN_DEFAULTS} to a partial input, producing the canonical
 * resolved config. Pure and deterministic — same input + context always yields
 * the same object, so the emitted `config.json` stays byte-stable across runs.
 */
export function resolveClientGenConfig(input: ClientGenConfigInput, ctx: ClientGenConfigContext): ClientGenConfig {
  return {
    ...(input.thirdParty === true ? { thirdParty: true } : {}),
    targets: input.targets ?? [...CLIENTGEN_DEFAULTS.targets],
    ts: {
      output: input.ts?.output ?? CLIENTGEN_DEFAULTS.ts.output,
      packageName: input.ts?.packageName ?? ctx.defaultTsPackageName,
    },
    go: {
      output: input.go?.output ?? CLIENTGEN_DEFAULTS.go.output,
      modulePath: input.go?.modulePath ?? CLIENTGEN_DEFAULTS.go.modulePath,
      packageName: input.go?.packageName ?? CLIENTGEN_DEFAULTS.go.packageName,
      clientName: input.go?.clientName ?? CLIENTGEN_DEFAULTS.go.clientName,
      ...(input.go?.omitOperations?.length ? { omitOperations: [...input.go.omitOperations] } : {}),
    },
  };
}

/** Serialize a resolved config to the exact JSON written to `.gen/clientgen/config.json`. */
export function serializeClientGenConfig(config: ClientGenConfig): string {
  return `${JSON.stringify(config, null, 2)}\n`;
}
