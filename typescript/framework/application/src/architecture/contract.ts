/**
 * The import-contract validator, mirroring the import half of
 * `protocols/architecture/validate.go` rule for rule.
 *
 * This is a second implementation of what a valid contract means: a TypeScript
 * workload declares and enforces its own contract rather than only describing
 * Go code that happens to sit in the same repository.
 *
 * What keeps the two honest is not care, it is
 * `protocols/architecture/fixtures/conformance/contract-validation.json`: one
 * corpus of contracts and the exact (code, field) pairs each must produce, run
 * by the Go validator and by this one. A rule that exists on one side only fails
 * there. Never fix a divergence by editing the corpus.
 */

import {
  ARCHITECTURE_DIAGNOSTIC_CODES as CODES,
  type ArchitectureDiagnostic,
  type ArchitectureImport,
  type Binding,
  type Consistency,
  type Deletion,
  type LocalModel,
  type Transport,
} from './contract.types';

const DOMAIN_PATTERN = /^[a-z][a-z0-9-]*(\/[a-z][a-z0-9-]*)*$/;
const SEMANTIC_ID_PATTERN = /^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9_-]*)+$/;
const CONTRACT_ID_PATTERN = /^[a-z][a-z0-9-]*(\.[a-z0-9][a-z0-9_-]*)+\.v([1-9][0-9]*)$/;
const FACT_PATTERN = /^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$/;

/** The units `time.ParseDuration` accepts, in the nanoseconds it counts. */
const DURATION_UNITS: Readonly<Record<string, bigint>> = {
  ns: 1n,
  us: 1_000n,
  µs: 1_000n,
  μs: 1_000n,
  ms: 1_000_000n,
  s: 1_000_000_000n,
  m: 60_000_000_000n,
  h: 3_600_000_000_000n,
};

const MAX_DURATION = (1n << 63n) - 1n;
const MIN_DURATION_MAGNITUDE = 1n << 63n;
const NANOSECONDS_PER_MILLISECOND = 1_000_000;

/**
 * Parse the fractional digits the way Go does before scaling them to a unit.
 *
 * `time.ParseDuration` retains as many leading digits as fit its unsigned
 * accumulator and ignores the remaining precision. Its scale is a float64, so
 * using `number` for the scale and conversion below intentionally reproduces
 * the same final truncation to a whole nanosecond.
 */
function durationFraction(digits: string): { fraction: bigint; scale: number } {
  let fraction = 0n;
  let scale = 1;
  let overflow = false;
  for (const character of digits) {
    if (overflow) continue;
    if (fraction > MAX_DURATION / 10n) {
      overflow = true;
      continue;
    }
    const candidate = fraction * 10n + BigInt(character);
    if (candidate > MIN_DURATION_MAGNITUDE) {
      overflow = true;
      continue;
    }
    fraction = candidate;
    scale *= 10;
  }
  return { fraction, scale };
}

/**
 * Parse one Go duration literal and return it in milliseconds, or `undefined`
 * when Go would refuse it.
 *
 * The grammar is `time.ParseDuration`'s, not an approximation of it: a sequence
 * of decimal-with-optional-fraction values each followed by a known unit, with
 * the bare `0` as the one unitless literal. Milliseconds rather than the
 * nanoseconds Go counts in. Parsing stays in `bigint` until the signed-int64
 * checks and per-component fractional-nanosecond truncation are complete;
 * converting earlier would accept overflow and round a duration such as
 * `0.1ns` above the zero value Go returns.
 */
export function parseDuration(text: string): number | undefined {
  let rest = text;
  let negative = false;
  if (rest.startsWith('-')) {
    negative = true;
    rest = rest.slice(1);
  } else if (rest.startsWith('+')) {
    rest = rest.slice(1);
  }
  if (rest === '0') return 0;
  if (rest === '') return undefined;
  let total = 0n;
  while (rest.length > 0) {
    const match = /^([0-9]*)(?:\.([0-9]*))?/.exec(rest);
    const literal = match?.[0] ?? '';
    // Go requires at least one digit; a bare "." carries no value.
    const integerDigits = match?.[1] ?? '';
    const fractionDigits = match?.[2];
    if (literal === '' || (integerDigits === '' && (fractionDigits === undefined || fractionDigits === ''))) {
      return undefined;
    }
    rest = rest.slice(literal.length);
    const unit = /^[^0-9.]+/.exec(rest)?.[0];
    if (unit === undefined) return undefined;
    const scale = DURATION_UNITS[unit];
    if (scale === undefined) return undefined;
    rest = rest.slice(unit.length);

    const integer = integerDigits === '' ? 0n : BigInt(integerDigits);
    if (integer > MIN_DURATION_MAGNITUDE / scale) return undefined;
    let component = integer * scale;
    if (fractionDigits !== undefined && fractionDigits !== '') {
      const fraction = durationFraction(fractionDigits);
      const fractionalNanoseconds = Math.trunc(Number(fraction.fraction) * (Number(scale) / fraction.scale));
      component += BigInt(fractionalNanoseconds);
    }
    if (component > MIN_DURATION_MAGNITUDE) return undefined;
    total += component;
    if (total > MIN_DURATION_MAGNITUDE) return undefined;
  }
  if (!negative && total > MAX_DURATION) return undefined;
  const signed = negative ? -total : total;
  return Number(signed) / NANOSECONDS_PER_MILLISECOND;
}

/** Byte length, because the protocol's size limits count bytes as Go's `len` does. */
function byteLength(value: string): number {
  return new TextEncoder().encode(value).length;
}

function validDomain(value: string): boolean {
  return DOMAIN_PATTERN.test(value);
}

function validSemanticId(value: string): boolean {
  return byteLength(value) <= 256 && SEMANTIC_ID_PATTERN.test(value);
}

function validContractId(value: string, version: number): boolean {
  if (byteLength(value) > 256) return false;
  const matches = CONTRACT_ID_PATTERN.exec(value);
  return matches !== null && Number(matches[2]) === version;
}

function validContractIdAnyVersion(value: string): boolean {
  return byteLength(value) <= 256 && CONTRACT_ID_PATTERN.test(value);
}

function validFactName(value: string): boolean {
  return byteLength(value) <= 256 && FACT_PATTERN.test(value);
}

/** Non-empty, bounded, trimmed, and free of the two control characters Go rejects. */
function validText(value: string, maximum: number): boolean {
  if (value === '' || byteLength(value) > maximum || value !== value.trim()) return false;
  for (const character of value) {
    const point = character.codePointAt(0);
    if (point === 0 || point === 0x7f) return false;
  }
  return true;
}

/**
 * One canonical Putnami project ID: absolute, bounded, and already clean.
 *
 * The segment walk is how `path.Clean(value) == value` is expressed without a
 * path library — an empty, `.`, or `..` segment is exactly what cleaning would
 * have removed, and rejecting them rejects `//`, `/./`, `/../`, and a trailing
 * slash in one pass.
 */
function validProjectId(value: string): boolean {
  if (value === '' || value === '/' || byteLength(value) > 1024) return false;
  if (!value.startsWith('/') || value.includes('\\') || value.includes('\u0000')) return false;
  return value
    .slice(1)
    .split('/')
    .every((segment) => segment !== '' && segment !== '.' && segment !== '..');
}

const LIFECYCLE_STATUSES = new Set(['planned', 'active', 'legacy', 'deprecated']);
const ACCESS_MODES = new Set(['reference', 'query', 'snapshot', 'projection', 'command']);
const TRANSPORT_KINDS = new Set(['none', 'in-process', 'api', 'event', 'file']);
const FAILURE_MODES = new Set(['fail-open', 'fail-closed', 'use-stale', 'unavailable']);
const ORDERING_STRATEGIES = new Set(['none', 'source-version', 'source-sequence', 'event-time']);
const LATE_EVENT_STRATEGIES = new Set(['ignore-older', 'reject', 'apply']);
const DELETION_STRATEGIES = new Set(['tombstone', 'hard-delete', 'retain', 'not-applicable']);
const LOCAL_MODEL_KINDS = new Set(['projection', 'snapshot', 'reference']);
const REBUILD_STRATEGIES = new Set(['bootstrap', 'replay', 'bootstrap-and-replay', 'not-applicable']);

function error(code: string, field: string, message: string): ArchitectureDiagnostic {
  return { severity: 'error', code, field, message };
}

/**
 * Validate one import contract and return every finding, in the shape the Go
 * protocol returns them.
 *
 * `field` is the dotted path the contract sits at in its document. The default
 * matches what a one-import manifest produces, which is what the conformance
 * corpus pins and what a component validating a single declared contract wants.
 */
export function validateArchitectureImport(
  contract: ArchitectureImport,
  field = 'imports[0]',
): ArchitectureDiagnostic[] {
  const diagnostics: ArchitectureDiagnostic[] = [];

  if (!validContractId(contract.id, contract.version)) {
    diagnostics.push(
      error(CODES.invalidId, `${field}.id`, `import ID must be a lower-case dotted ID ending in .v${contract.version}`),
    );
  }
  if (!(contract.version >= 1)) {
    diagnostics.push(error(CODES.invalidId, `${field}.version`, 'import version must be positive'));
  }
  if (!validDomain(contract.from?.domain ?? '')) {
    diagnostics.push(
      error(
        CODES.invalidDomain,
        `${field}.from.domain`,
        'producer domain must be a lower-case slash-separated semantic ID',
      ),
    );
  }
  if (!validContractIdAnyVersion(contract.from?.export ?? '')) {
    diagnostics.push(
      error(CODES.invalidId, `${field}.from.export`, 'producer export must be a versioned dotted contract ID'),
    );
  }
  if (!validSemanticId(contract.as)) {
    diagnostics.push(
      error(CODES.invalidId, `${field}.as`, 'local import name must be a lower-case dotted semantic ID'),
    );
  }
  if (!ACCESS_MODES.has(contract.mode)) {
    diagnostics.push(error(CODES.invalidMode, `${field}.mode`, `unknown access mode "${contract.mode}"`));
  }
  if (!LIFECYCLE_STATUSES.has(contract.status)) {
    diagnostics.push(error(CODES.invalidStatus, `${field}.status`, `unknown lifecycle status "${contract.status}"`));
  }
  if (!validText(contract.justification ?? '', 4096)) {
    diagnostics.push(error(CODES.invalidId, `${field}.justification`, 'import justification is required'));
  }
  const facts = contract.facts ?? [];
  if (facts.length === 0) {
    diagnostics.push(error(CODES.invalidFact, `${field}.facts`, 'import must minimize and name at least one fact'));
  }
  diagnostics.push(...validateFactNames(facts, `${field}.facts`));

  if (contract.transport) {
    diagnostics.push(...validateTransport(contract.transport, `${field}.transport`, contract.status));
  }
  if (contract.bootstrap) {
    diagnostics.push(...validateTransport(contract.bootstrap, `${field}.bootstrap`, contract.status));
  }
  if (contract.updates) {
    diagnostics.push(...validateTransport(contract.updates, `${field}.updates`, contract.status));
  }
  if (contract.consistency) {
    diagnostics.push(...validateConsistency(contract.consistency, `${field}.consistency`));
  }
  if (contract.deletion) {
    diagnostics.push(...validateDeletion(contract.deletion, `${field}.deletion`));
  }
  if (contract.localModel) {
    diagnostics.push(...validateLocalModel(contract.localModel, `${field}.localModel`));
  }

  diagnostics.push(...validateMode(contract, field));
  diagnostics.push(...validateBindings(contract, field));
  return diagnostics;
}

/** The per-mode requirements: what each access mode must and must not declare. */
function validateMode(contract: ArchitectureImport, field: string): ArchitectureDiagnostic[] {
  const diagnostics: ArchitectureDiagnostic[] = [];
  const mode = contract.mode;
  if (mode === 'projection') {
    if (contract.transport) {
      diagnostics.push(
        error(
          CODES.invalidProjection,
          `${field}.transport`,
          'a projection uses bootstrap and updates, not one ambiguous transport',
        ),
      );
    }
    if (!contract.bootstrap) {
      diagnostics.push(
        error(CODES.invalidProjection, `${field}.bootstrap`, 'a projection requires an explicit bootstrap transport'),
      );
    }
    if (!contract.updates) {
      diagnostics.push(
        error(CODES.invalidProjection, `${field}.updates`, 'a projection requires an explicit updates transport'),
      );
    }
    if (!contract.consistency) {
      diagnostics.push(
        error(
          CODES.invalidProjection,
          `${field}.consistency`,
          'a projection requires freshness, ordering, and idempotency guarantees',
        ),
      );
    }
    if (!contract.deletion) {
      diagnostics.push(
        error(CODES.invalidProjection, `${field}.deletion`, 'a projection requires a deletion strategy'),
      );
    }
    if (!contract.localModel) {
      diagnostics.push(
        error(CODES.invalidProjection, `${field}.localModel`, 'a projection requires an explicit local model'),
      );
    } else {
      diagnostics.push(...validateProjection(contract, contract.localModel, field));
    }
    return diagnostics;
  }
  if (mode === 'query' || mode === 'command' || mode === 'snapshot') {
    if (!contract.transport) {
      diagnostics.push(
        error(CODES.invalidTransport, `${field}.transport`, `${mode} access requires one explicit transport`),
      );
    }
    if (contract.bootstrap || contract.updates) {
      diagnostics.push(
        error(CODES.invalidTransport, field, `${mode} access cannot declare projection bootstrap or updates`),
      );
    }
    if ((mode === 'query' || mode === 'snapshot') && !contract.consistency) {
      diagnostics.push(
        error(
          CODES.invalidConsistency,
          `${field}.consistency`,
          `${mode} access requires explicit freshness and failure behavior`,
        ),
      );
    }
    return diagnostics;
  }
  if (mode === 'reference' && (contract.bootstrap || contract.updates)) {
    diagnostics.push(
      error(CODES.invalidTransport, field, 'reference access cannot declare projection bootstrap or updates'),
    );
  }
  return diagnostics;
}

/**
 * Bindings are the reviewed half: a planned target may not claim one, each names
 * two canonical project IDs, and a repeat is reported on the repeat.
 */
function validateBindings(contract: ArchitectureImport, field: string): ArchitectureDiagnostic[] {
  const diagnostics: ArchitectureDiagnostic[] = [];
  const seen = new Map<string, number>();
  (contract.bindings ?? []).forEach((binding: Binding, index: number) => {
    const bindingField = `${field}.bindings[${index}]`;
    if (contract.status === 'planned') {
      diagnostics.push(
        error(CODES.invalidBinding, bindingField, 'a planned target cannot claim a current observed project binding'),
      );
    }
    if (binding.kind !== 'project-dependency') {
      diagnostics.push(
        error(CODES.invalidBinding, `${bindingField}.kind`, `unsupported binding kind "${binding.kind}"`),
      );
    }
    if (!validProjectId(binding.consumerProject)) {
      diagnostics.push(
        error(
          CODES.invalidProject,
          `${bindingField}.consumerProject`,
          'consumer project must be one canonical Putnami project ID',
        ),
      );
    }
    if (!validProjectId(binding.producerProject)) {
      diagnostics.push(
        error(
          CODES.invalidProject,
          `${bindingField}.producerProject`,
          'producer project must be one canonical Putnami project ID',
        ),
      );
    }
    const key = `${binding.kind} ${binding.consumerProject} ${binding.producerProject}`;
    const previous = seen.get(key);
    if (previous === undefined) {
      seen.set(key, index);
    } else {
      diagnostics.push(error(CODES.duplicateBinding, bindingField, `binding is repeated (also bindings[${previous}])`));
    }
  });
  return diagnostics;
}

function validateTransport(transport: Transport, field: string, contractStatus: string): ArchitectureDiagnostic[] {
  const diagnostics: ArchitectureDiagnostic[] = [];
  if (!TRANSPORT_KINDS.has(transport.kind)) {
    diagnostics.push(error(CODES.invalidTransport, `${field}.kind`, `unknown transport kind "${transport.kind}"`));
  }
  if (!LIFECYCLE_STATUSES.has(transport.availability)) {
    diagnostics.push(
      error(CODES.invalidStatus, `${field}.availability`, `unknown transport availability "${transport.availability}"`),
    );
  }
  if (transport.kind === 'none') {
    if ((transport.contract ?? '') !== '') {
      diagnostics.push(error(CODES.invalidTransport, `${field}.contract`, 'none transport cannot name a contract'));
    }
  } else if (!validContractIdAnyVersion(transport.contract ?? '')) {
    diagnostics.push(
      error(CODES.invalidTransport, `${field}.contract`, 'transport contract must be a versioned dotted ID'),
    );
  }
  if (contractStatus === 'active' && transport.availability !== 'active') {
    diagnostics.push(
      error(
        CODES.invalidStatus,
        `${field}.availability`,
        `an active import cannot depend on a ${transport.availability} transport`,
      ),
    );
  }
  return diagnostics;
}

function validateConsistency(consistency: Consistency, field: string): ArchitectureDiagnostic[] {
  const diagnostics: ArchitectureDiagnostic[] = [];
  const bound = parseDuration(consistency.maxStaleness ?? '');
  if (bound === undefined || bound <= 0) {
    diagnostics.push(
      error(
        CODES.invalidConsistency,
        `${field}.maxStaleness`,
        'max staleness must be one positive Go duration such as 5m',
      ),
    );
  }
  if (!FAILURE_MODES.has(consistency.onMissing) || consistency.onMissing === 'use-stale') {
    diagnostics.push(
      error(
        CODES.invalidConsistency,
        `${field}.onMissing`,
        `unknown or impossible missing-data behavior "${consistency.onMissing}"`,
      ),
    );
  }
  if (!FAILURE_MODES.has(consistency.onStale) || consistency.onStale === 'unavailable') {
    diagnostics.push(
      error(CODES.invalidConsistency, `${field}.onStale`, `unknown stale-data behavior "${consistency.onStale}"`),
    );
  }
  if (!ORDERING_STRATEGIES.has(consistency.ordering)) {
    diagnostics.push(
      error(CODES.invalidConsistency, `${field}.ordering`, `unknown ordering strategy "${consistency.ordering}"`),
    );
  }
  if (!validFactName(consistency.sourceVersion ?? '')) {
    diagnostics.push(
      error(CODES.invalidConsistency, `${field}.sourceVersion`, 'source version must name one source field'),
    );
  }
  if (consistency.ordering !== 'none' && !validFactName(consistency.idempotencyKey ?? '')) {
    diagnostics.push(
      error(CODES.invalidConsistency, `${field}.idempotencyKey`, 'ordered updates require an idempotency key field'),
    );
  }
  if (!LATE_EVENT_STRATEGIES.has(consistency.lateEvents)) {
    diagnostics.push(
      error(CODES.invalidConsistency, `${field}.lateEvents`, `unknown late-event strategy "${consistency.lateEvents}"`),
    );
  }
  return diagnostics;
}

function validateDeletion(deletion: Deletion, field: string): ArchitectureDiagnostic[] {
  if (!DELETION_STRATEGIES.has(deletion.strategy)) {
    return [error(CODES.invalidDeletion, `${field}.strategy`, `unknown deletion strategy "${deletion.strategy}"`)];
  }
  if (deletion.strategy === 'tombstone' && !validFactName(deletion.tombstoneField ?? '')) {
    return [error(CODES.invalidDeletion, `${field}.tombstoneField`, 'tombstone deletion requires one tombstone field')];
  }
  if (deletion.strategy !== 'tombstone' && (deletion.tombstoneField ?? '') !== '') {
    return [error(CODES.invalidDeletion, `${field}.tombstoneField`, 'only tombstone deletion names a tombstone field')];
  }
  return [];
}

function validateLocalModel(model: LocalModel, field: string): ArchitectureDiagnostic[] {
  const diagnostics: ArchitectureDiagnostic[] = [];
  if (!validSemanticId(model.name)) {
    diagnostics.push(error(CODES.invalidProjection, `${field}.name`, 'local model name must be a dotted semantic ID'));
  }
  if (!LOCAL_MODEL_KINDS.has(model.kind)) {
    diagnostics.push(error(CODES.invalidProjection, `${field}.kind`, `unknown local model kind "${model.kind}"`));
  }
  if (!validFactName(model.sourceIdentity ?? '')) {
    diagnostics.push(
      error(CODES.invalidProjection, `${field}.sourceIdentity`, 'source identity must name one projected field'),
    );
  }
  const projectedFields = model.projectedFields ?? [];
  const localFields = model.localFields ?? [];
  if (projectedFields.length === 0) {
    diagnostics.push(
      error(CODES.invalidProjection, `${field}.projectedFields`, 'local model must name its copied fields'),
    );
  }
  diagnostics.push(...validateFactNames(projectedFields, `${field}.projectedFields`));
  diagnostics.push(...validateFactNames(localFields, `${field}.localFields`));
  const projected = new Set(projectedFields);
  localFields.forEach((local, index) => {
    if (projected.has(local)) {
      diagnostics.push(
        error(
          CODES.invalidProjection,
          `${field}.localFields[${index}]`,
          `field "${local}" cannot be both projected and locally authoritative`,
        ),
      );
    }
  });
  for (const [member, value] of [
    ['provenanceField', model.provenanceField],
    ['observedAtField', model.observedAtField],
    ['freshnessField', model.freshnessField],
  ] as const) {
    if (!validFactName(value ?? '')) {
      diagnostics.push(
        error(CODES.invalidProjection, `${field}.${member}`, `${member} must name one local metadata field`),
      );
    }
  }
  if (!validSemanticId(model.writer)) {
    diagnostics.push(
      error(CODES.invalidProjection, `${field}.writer`, 'writer must identify the sole projector or ingester'),
    );
  }
  if (!REBUILD_STRATEGIES.has(model.rebuild)) {
    diagnostics.push(error(CODES.invalidProjection, `${field}.rebuild`, `unknown rebuild strategy "${model.rebuild}"`));
  }
  if (model.rebuildable && model.rebuild === 'not-applicable') {
    diagnostics.push(
      error(CODES.invalidProjection, `${field}.rebuild`, 'a rebuildable model needs a concrete rebuild strategy'),
    );
  }
  return diagnostics;
}

/** The extra requirements a projection's local model carries beyond a local model's own. */
function validateProjection(contract: ArchitectureImport, model: LocalModel, field: string): ArchitectureDiagnostic[] {
  const diagnostics: ArchitectureDiagnostic[] = [];
  if (model.kind !== 'projection') {
    diagnostics.push(
      error(CODES.invalidProjection, `${field}.localModel.kind`, 'projection access requires a projection local model'),
    );
  }
  if (!model.rebuildable || model.rebuild === 'not-applicable') {
    diagnostics.push(
      error(CODES.invalidProjection, `${field}.localModel.rebuildable`, 'a projection must be rebuildable'),
    );
  }
  if (contract.consistency) {
    if (contract.consistency.ordering === 'none') {
      diagnostics.push(
        error(CODES.invalidProjection, `${field}.consistency.ordering`, 'a projection requires monotone ordering'),
      );
    }
    if ((contract.consistency.idempotencyKey ?? '') === '') {
      diagnostics.push(
        error(
          CODES.invalidProjection,
          `${field}.consistency.idempotencyKey`,
          'a projection requires an idempotency key',
        ),
      );
    }
  }
  const projectedFields = model.projectedFields ?? [];
  if (!sameStringSet(contract.facts ?? [], projectedFields)) {
    diagnostics.push(
      error(
        CODES.invalidProjection,
        `${field}.localModel.projectedFields`,
        'projected fields must exactly match the minimized imported facts',
      ),
    );
  }
  if (!new Set(projectedFields).has(model.sourceIdentity)) {
    diagnostics.push(
      error(
        CODES.invalidProjection,
        `${field}.localModel.sourceIdentity`,
        'source identity must be one of the projected fields',
      ),
    );
  }
  return diagnostics;
}

function validateFactNames(values: readonly string[], field: string): ArchitectureDiagnostic[] {
  const diagnostics: ArchitectureDiagnostic[] = [];
  const seen = new Map<string, number>();
  values.forEach((value, index) => {
    const itemField = `${field}[${index}]`;
    if (!validFactName(value)) {
      diagnostics.push(
        error(CODES.invalidFact, itemField, 'fact name must be lower-case snake_case, optionally dot-separated'),
      );
    }
    const previous = seen.get(value);
    if (previous === undefined) {
      seen.set(value, index);
    } else {
      diagnostics.push(error(CODES.invalidFact, itemField, `fact "${value}" is repeated (also ${field}[${previous}])`));
    }
  });
  return diagnostics;
}

function sameStringSet(left: readonly string[], right: readonly string[]): boolean {
  if (left.length !== right.length) return false;
  const leftSet = new Set(left);
  return right.every((value) => leftSet.has(value));
}
