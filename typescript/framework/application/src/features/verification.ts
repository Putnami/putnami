import { Buffer } from 'node:buffer';
import type { FeatureDiagnostic } from './evidence';
import type {
  FeatureVerificationObservation,
  FeatureVerificationReport,
  ObservationMeasurement,
} from './verification.types';

export const FEATURE_VERIFICATION_SCHEMA_URL = 'https://putnami.dev/schemas/putnami-feature-verification.json';
export const FEATURE_VERIFICATION_PROTOCOL_VERSION = 1 as const;

/**
 * Reserved declared-artifact identity a task publishes one project's report
 * under. The report is retained with the session; it is never written into the
 * source tree as authored evidence.
 */
export const FEATURE_VERIFICATION_ARTIFACT_ID = 'putnami-feature-verification';

/** Bound on one run-scoped report, kept in step with the Go reader. */
export const FEATURE_VERIFICATION_MAX_OBSERVATIONS = 10_000;

const FEATURE_ID = /^[a-z0-9]+(?:-[a-z0-9]+)*(?:\/[a-z0-9]+(?:-[a-z0-9]+)*)+$/;
const SEGMENT_ID = /^[a-z0-9]+(?:-[a-z0-9]+)*$/;
const SEMANTIC_CODE = /^[a-z][a-z0-9]*(?:[.-][a-z0-9]+)*$/;
const SCHEME = /^[A-Za-z][A-Za-z0-9+.-]*:/;
const RFC3339 = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$/;
const STATUSES = new Set(['passed', 'failed', 'skipped']);
const AGGREGATIONS = new Set(['value', 'count', 'sum', 'avg', 'min', 'max', 'p50', 'p95', 'p99', 'ratio']);
const CHECK_ID_MAX_LENGTH = 128;
const SEMANTIC_CODE_MAX_LENGTH = 128;

// The report wire is closed at every level. Go refuses an unknown field in its
// strict decoder, which has no JavaScript equivalent, so the key sets are
// checked here instead. Without them a producer could add `issuer`, `project`,
// `source`, or `verdict` and have TypeScript accept a payload Go rejects.
const REPORT_KEYS = new Set(['$schema', 'protocolVersion', 'observations']);
const OBSERVATION_KEYS = new Set([
  'feature',
  'requirement',
  'check',
  'status',
  'measurement',
  'window',
  'environment',
  'provenance',
]);
const MEASUREMENT_KEYS = new Set(['name', 'aggregation', 'value', 'unit']);
const WINDOW_KEYS = new Set(['start', 'end']);
const PROVENANCE_KEYS = new Set(['path', 'symbol']);

function cmp(left: string, right: string): number {
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'));
}

function finding(code: string, field: string, message: string): FeatureDiagnostic {
  return { severity: 'error', code, field, message };
}

/**
 * Reject any key the wire does not declare. The path is fully qualified, which
 * Go's decoder cannot produce, so a reader always learns which object carried
 * the foreign field rather than only its name.
 */
function closedKeys(field: string, value: unknown, allowed: Set<string>, diagnostics: FeatureDiagnostic[]): void {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return;
  for (const key of Object.keys(value)) {
    if (allowed.has(key)) continue;
    diagnostics.push(
      finding(
        'features.unknown_field',
        field ? `${field}.${key}` : key,
        'unknown field; the verification report declares no issuer, project, source binding, threshold, or verdict',
      ),
    );
  }
}

function semanticCode(value: unknown, maximum = SEMANTIC_CODE_MAX_LENGTH): value is string {
  return typeof value === 'string' && Buffer.byteLength(value, 'utf8') <= maximum && SEMANTIC_CODE.test(value);
}

function boundedText(value: unknown, maximum: number): value is string {
  if (typeof value !== 'string' || value.trim().length === 0 || Buffer.byteLength(value, 'utf8') > maximum) {
    return false;
  }
  for (const character of value) {
    const code = character.codePointAt(0) ?? 0;
    if (code < 0x20 || code === 0x7f) return false;
  }
  return true;
}

function validatePath(field: string, value: unknown, diagnostics: FeatureDiagnostic[]): void {
  if (
    typeof value !== 'string' ||
    value.length === 0 ||
    value.includes('\\') ||
    value.includes('\0') ||
    value.startsWith('/') ||
    SCHEME.test(value)
  ) {
    diagnostics.push(finding('features.invalid_path', field, 'path must be a non-empty contained relative slash path'));
    return;
  }
  const segments = value.split('/');
  if (segments.includes('..')) {
    diagnostics.push(finding('features.path_escape', field, 'path must remain within its declared source root'));
  } else if (segments.some((segment) => segment === '' || segment === '.')) {
    diagnostics.push(finding('features.invalid_path', field, 'path must be its slash-separated lexical clean form'));
  }
}

function validateMeasured(
  field: string,
  observation: FeatureVerificationObservation,
  diagnostics: FeatureDiagnostic[],
): void {
  const measurement = observation.measurement as ObservationMeasurement;
  if (!semanticCode(measurement?.name)) {
    diagnostics.push(
      finding(
        'features.invalid_observation',
        `${field}.measurement.name`,
        'measurement name must be a bounded lower-case semantic code',
      ),
    );
  }
  if (!AGGREGATIONS.has(measurement?.aggregation)) {
    diagnostics.push(
      finding(
        'features.invalid_observation',
        `${field}.measurement.aggregation`,
        'measurement aggregation is not supported',
      ),
    );
  }
  if (typeof measurement?.value !== 'number' || !Number.isFinite(measurement.value)) {
    diagnostics.push(
      finding(
        'features.invalid_observation',
        `${field}.measurement.value`,
        'measurement value must be a finite number',
      ),
    );
  }
  if (!semanticCode(measurement?.unit)) {
    diagnostics.push(
      finding(
        'features.invalid_observation',
        `${field}.measurement.unit`,
        'measurement unit must be a bounded lower-case semantic code',
      ),
    );
  }
  if (observation.environment !== undefined && !semanticCode(observation.environment)) {
    diagnostics.push(
      finding(
        'features.invalid_observation',
        `${field}.environment`,
        'environment must be a bounded lower-case semantic code',
      ),
    );
  }
  const window = observation.window;
  if (!window || !RFC3339.test(window.start ?? '') || !RFC3339.test(window.end ?? '')) {
    diagnostics.push(
      finding(
        'features.invalid_observation',
        `${field}.window`,
        'observation window bounds must be valid RFC 3339 timestamps',
      ),
    );
    return;
  }
  if (Date.parse(window.end) < Date.parse(window.start)) {
    diagnostics.push(
      finding('features.invalid_observation', `${field}.window`, 'observation window must not end before it starts'),
    );
  }
}

function validateObservation(
  field: string,
  observation: FeatureVerificationObservation,
  diagnostics: FeatureDiagnostic[],
): void {
  closedKeys(field, observation, OBSERVATION_KEYS, diagnostics);
  closedKeys(`${field}.measurement`, observation?.measurement, MEASUREMENT_KEYS, diagnostics);
  closedKeys(`${field}.window`, observation?.window, WINDOW_KEYS, diagnostics);
  closedKeys(`${field}.provenance`, observation?.provenance, PROVENANCE_KEYS, diagnostics);
  if (!FEATURE_ID.test(observation?.feature ?? '')) {
    diagnostics.push(finding('features.unknown_feature', `${field}.feature`, 'feature reference is not canonical'));
  }
  if (!SEGMENT_ID.test(observation?.requirement ?? '')) {
    diagnostics.push(
      finding('features.unknown_requirement', `${field}.requirement`, 'requirement reference is not canonical'),
    );
  }
  if (
    typeof observation?.check !== 'string' ||
    observation.check.length > CHECK_ID_MAX_LENGTH ||
    !SEGMENT_ID.test(observation.check)
  ) {
    diagnostics.push(
      finding(
        'features.invalid_observation',
        `${field}.check`,
        'check ID must be one bounded lower-case ASCII ID segment',
      ),
    );
  }
  validatePath(`${field}.provenance.path`, observation?.provenance?.path, diagnostics);
  if (observation?.provenance?.symbol !== undefined && !boundedText(observation.provenance.symbol, 512)) {
    diagnostics.push(
      finding('features.invalid_observation', `${field}.provenance.symbol`, 'provenance symbol must be bounded text'),
    );
  }
  // An observation states either an acceptance verdict or a measurement. A
  // producer that stated both could describe a numeric objective and then
  // declare its own result for it.
  if ((observation?.status === undefined) === (observation?.measurement === undefined)) {
    diagnostics.push(
      finding('features.invalid_observation', field, 'an observation must carry exactly one of status or measurement'),
    );
    return;
  }
  if (observation.status !== undefined) {
    if (!STATUSES.has(observation.status)) {
      diagnostics.push(
        finding('features.invalid_observation', `${field}.status`, 'observation status is not supported'),
      );
    }
    if (observation.window !== undefined || observation.environment !== undefined) {
      diagnostics.push(
        finding('features.invalid_observation', field, 'an acceptance observation must omit window and environment'),
      );
    }
    return;
  }
  validateMeasured(field, observation, diagnostics);
}

/** Validate one run-scoped feature verification report. */
export function validateFeatureVerificationReport(report: FeatureVerificationReport): FeatureDiagnostic[] {
  const diagnostics: FeatureDiagnostic[] = [];
  closedKeys('', report, REPORT_KEYS, diagnostics);
  if (report?.protocolVersion !== FEATURE_VERIFICATION_PROTOCOL_VERSION) {
    diagnostics.push(finding('features.invalid_protocol_version', 'protocolVersion', 'protocolVersion must equal 1'));
  }
  if (!Array.isArray(report?.observations)) {
    diagnostics.push(finding('features.parse_error', 'observations', 'observations is required and must be an array'));
    return diagnostics;
  }
  if (report.observations.length > FEATURE_VERIFICATION_MAX_OBSERVATIONS) {
    diagnostics.push(
      finding(
        'features.invalid_observation',
        'observations',
        `a verification report carries at most ${FEATURE_VERIFICATION_MAX_OBSERVATIONS} observations`,
      ),
    );
  }
  const seen = new Map<string, string>();
  for (const [index, observation] of canonicalFeatureVerificationReport(report).observations.entries()) {
    const field = `observations[${index}]`;
    validateObservation(field, observation, diagnostics);
    const key = `${observation?.feature} ${observation?.requirement} ${observation?.check}`;
    const first = seen.get(key);
    if (first) {
      diagnostics.push(
        finding(
          'features.duplicate_observation',
          field,
          `check is observed more than once; first observation is ${first}`,
        ),
      );
    } else {
      seen.set(key, field);
    }
  }
  return diagnostics.sort(
    (left, right) => cmp(left.code, right.code) || cmp(left.field, right.field) || cmp(left.message, right.message),
  );
}

/**
 * Return a canonical sorted copy without mutating caller-owned observations.
 * Observations sort by the join key they are read through, so two runs that
 * observe the same checks in a different order produce the same bytes.
 */
export function canonicalFeatureVerificationReport(report: FeatureVerificationReport): FeatureVerificationReport {
  return {
    $schema: report.$schema,
    protocolVersion: FEATURE_VERIFICATION_PROTOCOL_VERSION,
    observations: [...(report.observations ?? [])].sort(
      (left, right) =>
        cmp(left.feature, right.feature) || cmp(left.requirement, right.requirement) || cmp(left.check, right.check),
    ),
  };
}

function escapeLikeGo(json: string): string {
  return json.replace(/[<>&\u2028\u2029]/g, (character) => {
    if (character === '<') return '\\u003c';
    if (character === '>') return '\\u003e';
    if (character === '&') return '\\u0026';
    if (character === '\u2028') return '\\u2028';
    return '\\u2029';
  });
}

function observationOut(observation: FeatureVerificationObservation): Record<string, unknown> {
  const measurement = observation.measurement;
  return {
    feature: observation.feature,
    requirement: observation.requirement,
    check: observation.check,
    status: observation.status || undefined,
    measurement: measurement
      ? {
          name: measurement.name,
          aggregation: measurement.aggregation,
          value: measurement.value,
          unit: measurement.unit,
        }
      : undefined,
    window: observation.window ? { start: observation.window.start, end: observation.window.end } : undefined,
    environment: observation.environment || undefined,
    provenance: {
      path: observation.provenance.path,
      symbol: observation.provenance.symbol || undefined,
    },
  };
}

/** Canonical Go-compatible verification-report JSON bytes represented as a string. */
export function serializeFeatureVerificationReport(report: FeatureVerificationReport): string {
  const canonical = canonicalFeatureVerificationReport(report);
  return `${escapeLikeGo(
    JSON.stringify(
      {
        $schema: canonical.$schema || undefined,
        protocolVersion: canonical.protocolVersion,
        observations: canonical.observations.map(observationOut),
      },
      null,
      2,
    ),
  )}\n`;
}
