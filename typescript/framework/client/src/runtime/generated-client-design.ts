/**
 * An exact API operation embedded in generated client code, and the unit
 * producer attribution is keyed by.
 *
 * `operationId` is the canonical operation identity taken from the API
 * contract, deliberately independent from the generated TypeScript symbol: the
 * symbol must be a legal identifier (`getV1_Operator_Cli_usage`) while the
 * canonical id keeps its punctuation (`getV1_Operator_Cli-usage`), and lineage
 * must survive that rewrite.
 *
 * `producerProject` and `producerFeature` are present together or not at all:
 * an operation whose endpoint owner declares no feature stays unattributed
 * rather than inheriting the lineage of a neighbour operation that happens to
 * share the client.
 */
export interface GeneratedClientOperation {
  readonly operationId: string;
  readonly method: string;
  readonly path: string;
  readonly producerProject?: string;
  readonly producerFeature?: string;
}

/**
 * Build-time identity embedded in every generated client. It is intentionally
 * independent from deployment URLs, and carries producer identity per operation
 * because one generated client routinely spans several producer features.
 */
export interface GeneratedClientDesign {
  readonly language: 'ts' | 'go';
  readonly client: string;
  readonly service: string;
  readonly specHash?: string;
  readonly operations: readonly GeneratedClientOperation[];
}

/**
 * Exact functional identity attached to an outgoing generated-client call.
 * Every field is resolved from the invoked operation; the producer fields are
 * absent for an unattributed operation, which is a first-class state rather
 * than a value to guess at.
 */
export interface ClientFeatureTrace {
  readonly generatedClient: string;
  readonly operationId: string;
  readonly method: string;
  readonly path: string;
  readonly specHash?: string;
  readonly producerProject?: string;
  readonly producerFeature?: string;
}

/** Source location where a runtime instantiated a generated client. */
export interface GeneratedClientUsageSource {
  readonly path: string;
  readonly line?: number;
  readonly symbol?: string;
}

/** Runtime evidence that application code constructed a generated client. */
export interface GeneratedClientUsage {
  readonly design: GeneratedClientDesign;
  readonly source?: GeneratedClientUsageSource;
}

const MAX_RECORDED_USAGES = 1024;
const generatedClientUsages = new Map<string, GeneratedClientUsage>();

/** @internal Called by ClientBuilder once the client is actually constructed. */
export function recordGeneratedClientUsage(design: GeneratedClientDesign, source?: GeneratedClientUsageSource): void {
  // The key is the client's own identity (plus the spec it was generated from)
  // and the call site — never a feature, which is per operation.
  const key = [design.client, design.language, design.specHash ?? '', source?.path ?? '', source?.line ?? ''].join(
    '\0',
  );
  if (!generatedClientUsages.has(key) && generatedClientUsages.size >= MAX_RECORDED_USAGES) {
    const oldest = generatedClientUsages.keys().next().value;
    if (oldest !== undefined) generatedClientUsages.delete(oldest);
  }
  generatedClientUsages.set(key, { design, ...(source ? { source } : {}) });
}

/** Snapshot of generated-client usages observed by this runtime. */
export function getGeneratedClientUsages(): readonly GeneratedClientUsage[] {
  return [...generatedClientUsages.values()];
}

/** Clear runtime usage evidence. Primarily useful for isolated tests. */
export function clearGeneratedClientUsages(): void {
  generatedClientUsages.clear();
}

/**
 * Resolve one operation from the exact generated descriptor by its canonical
 * operation id — the same key the Go runtime and the design graph's
 * `generatedFrom` edges use. Returns `undefined` for an unknown operation (or
 * for a hand-written call that names none) instead of guessing from the URL,
 * and a trace with no producer fields for a known but unattributed operation.
 */
export function resolveClientFeatureTrace(
  design: GeneratedClientDesign | undefined,
  operationId: string | undefined,
): ClientFeatureTrace | undefined {
  if (!design || !operationId) return undefined;
  const operation = design.operations.find((candidate) => candidate.operationId === operationId);
  if (!operation) return undefined;
  return {
    generatedClient: design.client,
    operationId: operation.operationId,
    method: operation.method,
    path: operation.path,
    ...(design.specHash ? { specHash: design.specHash } : {}),
    ...(operation.producerProject ? { producerProject: operation.producerProject } : {}),
    ...(operation.producerFeature ? { producerFeature: operation.producerFeature } : {}),
  };
}
