/**
 * TypeScript port of the canonical platform-endpoints protocol defined
 * in protocols/platform/. Kept in sync with the Go reference
 * (go.putnami.dev/protocol/platform) — every constant, status string,
 * and validation rule below must match the contract there.
 *
 * The contract fixes the wire surface every Putnami runtime presents
 * for operational endpoints: paths, response envelope, HTTP status
 * mapping, probe behaviour, and pprof posture.
 *
 * Runtimes import these constants instead of hardcoding strings, and
 * exercise the validators in their conformance tests so wire-shape
 * drift fails CI rather than slipping into production.
 */

/** Current protocol version. Bump for backwards-incompatible changes. */
export const PROTOCOL_VERSION = 1 as const;

/**
 * Canonical endpoint paths, relative to the configured prefix. Every
 * compliant runtime mounts these at `<prefix><path>`.
 */
export const PATH_LIVEZ = '/livez' as const;
export const PATH_HEALTHZ = '/healthz' as const;
export const PATH_READYZ = '/readyz' as const;
export const PATH_VERSION = '/version' as const;
export const PATH_PPROF_PREFIX = '/debug/pprof' as const;

/** Canonical ordered set of mandatory operational paths (pprof excluded — opt-in). */
export const CANONICAL_PATHS = [PATH_LIVEZ, PATH_HEALTHZ, PATH_READYZ, PATH_VERSION] as const;

/** Envelope status values. */
export type Status = 'ok' | 'unavailable' | 'degraded';
export const STATUS_OK: Status = 'ok';
export const STATUS_UNAVAILABLE: Status = 'unavailable';
export const STATUS_DEGRADED: Status = 'degraded';

/** HTTP status codes the protocol uses. Anything else is a violation. */
export const HTTP_STATUS_OK = 200 as const;
export const HTTP_STATUS_UNAVAILABLE = 503 as const;

/** Canonical envelope returned by /livez, /healthz, /readyz. */
export interface Envelope {
  status: Status;
  /**
   * Per-probe report. Present on /healthz and /readyz when probes run;
   * omitted on /livez and on /healthz / /readyz when the runtime
   * short-circuits with status="unavailable".
   */
  checks?: Record<string, string>;
}

/** Canonical /version response shape. Empty fields are omitted. */
export interface VersionInfo {
  name?: string;
  version?: string;
  sha?: string;
  branch?: string;
  buildTime?: string;
}

/** Default per-probe timeout (5 seconds). */
export const DEFAULT_PROBE_TIMEOUT_MS = 5000;

/**
 * Canonical platform error codes. Tooling and compliance suites key
 * off these — keep in sync with the Go side's ErrorCode* constants.
 */
export const ERROR_CODE_INVALID_PROBE_NAME = 'platform.invalid_probe_name' as const;
export const ERROR_CODE_DUPLICATE_PROBE = 'platform.duplicate_probe' as const;
export const ERROR_CODE_INVALID_STATUS = 'platform.invalid_status' as const;
export const ERROR_CODE_INVALID_ENDPOINT = 'platform.invalid_endpoint' as const;
export const ERROR_CODE_INVALID_ENVELOPE = 'platform.invalid_envelope' as const;
export const ERROR_CODE_INVALID_CAPABILITY = 'platform.invalid_capability' as const;
export const ERROR_CODE_INVALID_PREFIX = 'platform.invalid_prefix' as const;
export const ERROR_CODE_PROBE_TIMEOUT = 'platform.probe_timeout' as const;
export const ERROR_CODE_PROBE_CONTRACT_BROKEN = 'platform.probe_contract_broken' as const;

/**
 * Taxonomy label for the required-readiness behavior: a probe name a
 * workload marks as required that was never registered or discovered.
 * Unlike the codes above it is NOT a strict-validation diagnostic — no
 * validator emits it and it changes no wire shape. The runtime expresses
 * a missing required probe through the EXISTING `degraded` envelope (a
 * synthesized failing `checks` entry keyed by the missing name), so it
 * needs no protocol bump. Kept in sync with the Go side's
 * `ErrorCodeMissingProbe`; exists as a symbol so tooling references the
 * label instead of hardcoding the string.
 */
export const ERROR_CODE_MISSING_PROBE = 'platform.missing_probe' as const;

export interface Diagnostic {
  code: string;
  field: string;
  message: string;
}

const VALID_STATUSES: ReadonlySet<Status> = new Set<Status>([STATUS_OK, STATUS_UNAVAILABLE, STATUS_DEGRADED]);

/**
 * Canonical probe name pattern. Probe names surface as JSON keys,
 * log fields, and metric labels — restrict to a sane alphabet:
 * starts with [a-z0-9], then 0–63 of [a-z0-9_./-].
 */
const PROBE_NAME_PATTERN = /^[a-z0-9][a-z0-9_./-]{0,63}$/;

/**
 * What `normalizePrefix` drops at either end: ASCII whitespace and `/`. The
 * Go protocol trims exactly this set.
 */
const PREFIX_TRIM_SET = ' \t\n\v\f\r/';

/**
 * Collapse `raw` to the canonical prefix form: empty or `/segment[/...]`.
 * ASCII whitespace and slashes at either end are dropped together, then one
 * leading slash is added: `''`, `/`, `//` and `'/ /'` normalise to `''`;
 * `v1`, `' v1'`, `//v1`, `'/ v1'` and `'/v1/ '` normalise to `/v1`.
 * Interior characters, a doubled slash included, are kept. Runtimes apply
 * this before mounting so two workloads with equivalent prefixes mount on
 * identical paths.
 */
export function normalizePrefix(raw: string): string {
  let start = 0;
  let end = raw.length;
  while (start < end && PREFIX_TRIM_SET.includes(raw[start])) start++;
  while (end > start && PREFIX_TRIM_SET.includes(raw[end - 1])) end--;
  return start === end ? '' : `/${raw.slice(start, end)}`;
}

/** Apply the canonical prefix to a relative endpoint path. */
export function joinPrefix(prefix: string, endpointPath: string): string {
  return normalizePrefix(prefix) + endpointPath;
}

/** Map an envelope status to its canonical HTTP status code. */
export function httpStatusFor(s: Status): number | undefined {
  switch (s) {
    case STATUS_OK:
      return HTTP_STATUS_OK;
    case STATUS_UNAVAILABLE:
    case STATUS_DEGRADED:
      return HTTP_STATUS_UNAVAILABLE;
    default:
      return undefined;
  }
}

/** Validate a probe name against the canonical pattern. */
export function validateProbeName(name: string): Diagnostic[] {
  if (name === '') {
    return [{ code: ERROR_CODE_INVALID_PROBE_NAME, field: 'name', message: 'probe name is required' }];
  }
  if (!PROBE_NAME_PATTERN.test(name)) {
    return [
      {
        code: ERROR_CODE_INVALID_PROBE_NAME,
        field: 'name',
        message: `probe name ${JSON.stringify(name)} does not match canonical pattern ${PROBE_NAME_PATTERN.toString()}`,
      },
    ];
  }
  return [];
}

/** Validate a status value. */
export function validateStatus(s: string): Diagnostic[] {
  if (!VALID_STATUSES.has(s as Status)) {
    return [
      {
        code: ERROR_CODE_INVALID_STATUS,
        field: 'status',
        message: `status ${JSON.stringify(s)} is not a canonical platform status`,
      },
    ];
  }
  return [];
}

/**
 * Validate a canonical envelope.
 *
 * Rules (must mirror the Go validator exactly):
 * - `status` is required and must be canonical.
 * - `unavailable` envelopes must NOT include `checks` — the runtime
 *   short-circuited before running probes.
 * - `ok` envelopes may include `checks`; every entry value must be
 *   exactly "ok".
 * - `degraded` envelopes must include at least one failing entry
 *   (anything other than "ok" or empty).
 */
export function validateEnvelope(envelope: Envelope): Diagnostic[] {
  const diagnostics: Diagnostic[] = [...validateStatus(envelope.status)];
  const checks = envelope.checks ?? {};

  switch (envelope.status) {
    case STATUS_UNAVAILABLE: {
      if (Object.keys(checks).length > 0) {
        diagnostics.push({
          code: ERROR_CODE_INVALID_ENVELOPE,
          field: 'checks',
          message: `envelope with status "${STATUS_UNAVAILABLE}" must not include checks`,
        });
      }
      break;
    }
    case STATUS_DEGRADED: {
      let hasFailure = false;
      for (const [name, entry] of Object.entries(checks)) {
        diagnostics.push(...validateProbeName(name));
        if (entry !== 'ok' && entry !== '') {
          hasFailure = true;
        }
      }
      if (!hasFailure) {
        diagnostics.push({
          code: ERROR_CODE_INVALID_ENVELOPE,
          field: 'checks',
          message: `envelope with status "${STATUS_DEGRADED}" must include at least one failing probe`,
        });
      }
      break;
    }
    case STATUS_OK: {
      for (const [name, entry] of Object.entries(checks)) {
        diagnostics.push(...validateProbeName(name));
        if (entry !== 'ok') {
          diagnostics.push({
            code: ERROR_CODE_INVALID_ENVELOPE,
            field: `checks[${name}]`,
            message: `envelope with status "${STATUS_OK}" must report "ok" for every check; got ${JSON.stringify(entry)}`,
          });
        }
      }
      break;
    }
  }

  return diagnostics;
}

/**
 * Result of {@link parseEnvelope} / {@link parseAndValidateEnvelope}:
 * the decoded envelope (absent when parsing failed) plus any diagnostics.
 * Mirrors the Go reference's `(*Envelope, []Diagnostic)` return.
 */
export interface ParseResult {
  envelope?: Envelope;
  diagnostics: Diagnostic[];
}

/**
 * Strict-parse a canonical envelope from JSON text — the TS twin of the
 * Go reference's `ParseEnvelope`. The Go decoder runs with
 * `DisallowUnknownFields` against the `Envelope` struct, so unknown
 * top-level fields, a non-string `status`, or non-string `checks` values
 * are rejected here. Semantic rules (status/checks consistency) are NOT
 * applied — run {@link validateEnvelope} or {@link parseAndValidateEnvelope}
 * for those.
 */
export function parseEnvelope(data: string): ParseResult {
  let raw: unknown;
  try {
    raw = JSON.parse(data);
  } catch (error) {
    return {
      diagnostics: [
        {
          code: ERROR_CODE_INVALID_ENVELOPE,
          field: '',
          message: `invalid envelope JSON: ${error instanceof Error ? error.message : String(error)}`,
        },
      ],
    };
  }
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) {
    return {
      diagnostics: [{ code: ERROR_CODE_INVALID_ENVELOPE, field: '', message: 'envelope must be a JSON object' }],
    };
  }

  const record = raw as Record<string, unknown>;
  const diagnostics: Diagnostic[] = [];
  for (const key of Object.keys(record)) {
    if (key !== 'status' && key !== 'checks') {
      diagnostics.push({
        code: ERROR_CODE_INVALID_ENVELOPE,
        field: key,
        message: `unknown envelope field ${JSON.stringify(key)}`,
      });
    }
  }

  const rawStatus = record['status'];
  if (rawStatus !== undefined && typeof rawStatus !== 'string') {
    diagnostics.push({
      code: ERROR_CODE_INVALID_ENVELOPE,
      field: 'status',
      message: 'envelope status must be a string',
    });
  }

  const rawChecks = record['checks'];
  const checks: Record<string, string> = {};
  let hasChecks = false;
  if (rawChecks !== undefined) {
    if (typeof rawChecks !== 'object' || rawChecks === null || Array.isArray(rawChecks)) {
      diagnostics.push({
        code: ERROR_CODE_INVALID_ENVELOPE,
        field: 'checks',
        message: 'envelope checks must be a JSON object of string values',
      });
    } else {
      hasChecks = true;
      for (const [name, value] of Object.entries(rawChecks)) {
        if (typeof value !== 'string') {
          diagnostics.push({
            code: ERROR_CODE_INVALID_ENVELOPE,
            field: `checks[${name}]`,
            message: 'envelope check value must be a string',
          });
        } else {
          checks[name] = value;
        }
      }
    }
  }

  if (diagnostics.length > 0) {
    return { diagnostics };
  }

  const envelope: Envelope = { status: rawStatus as Status };
  if (hasChecks) envelope.checks = checks;
  return { envelope, diagnostics };
}

/**
 * Strict-parse then semantically validate an envelope — the TS twin of
 * the Go reference's `ParseAndValidateEnvelope`. Parse errors
 * short-circuit (no semantic validation on an unparseable payload).
 * Cross-language compliance suites feed the shared fixture corpus
 * (`protocols/platform/fixtures/envelope`) through this entry point so TS
 * and Go accept/reject exactly the same envelopes.
 */
export function parseAndValidateEnvelope(data: string): ParseResult {
  const parsed = parseEnvelope(data);
  if (parsed.diagnostics.length > 0 || !parsed.envelope) {
    return parsed;
  }
  return { envelope: parsed.envelope, diagnostics: validateEnvelope(parsed.envelope) };
}

/** Validate that a (raw) prefix can normalise to canonical form. */
export function validatePrefix(prefix: string): Diagnostic[] {
  const normalized = normalizePrefix(prefix);
  if (normalized !== '' && !normalized.startsWith('/')) {
    return [
      {
        code: ERROR_CODE_INVALID_PREFIX,
        field: 'prefix',
        message: `normalised prefix ${JSON.stringify(normalized)} must be empty or start with "/"`,
      },
    ];
  }
  if (normalized.endsWith('/') && normalized !== '') {
    return [
      {
        code: ERROR_CODE_INVALID_PREFIX,
        field: 'prefix',
        message: `normalised prefix ${JSON.stringify(normalized)} must not have a trailing slash`,
      },
    ];
  }
  return [];
}
