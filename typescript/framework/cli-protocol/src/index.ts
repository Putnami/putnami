/**
 * TypeScript binding of the Putnami CLI result-envelope contract.
 *
 * Current builds emit {@link ResultV2} (`protocolVersion: 2`). The types below
 * are the version-1 envelope, read for documents without `protocolVersion`.
 * The field-by-field mapping is in `protocols/cli/doc/02-result-v2.md` §
 * Migrating from version 1.
 *
 * The canonical source of the v1 shape is the JSON schema at
 * `protocols/cli/schemas/result.json` (`$id`
 * `https://putnami.dev/schemas/putnami-cli-result.json`), also implemented in Go
 * as `go.putnami.dev/protocol/cli` (`Result`/`ResultError`).
 *
 * These types are hand-authored and kept in lock-step with the schema by
 * `test/protocol-conformance.test.ts` — the TypeScript analog of the Go
 * `drift_test.go`.
 */

/**
 * The version 2 machine result contract: the versioned envelope, the typed task
 * identity, the canonical run summary, and `validateDocument`. It is what every
 * CLI surface emits; a document without `protocolVersion` is version 1.
 */
export * from './qualify';
export * from './result-v2';
export * from './session-reporting';
export * from './session-subscribers';

/** The `$id` of the version-1 result-envelope JSON schema. */
export const RESULT_SCHEMA_ID = 'https://putnami.dev/schemas/putnami-cli-result.json' as const;

/** `Result.status` values. */
export const RESULT_STATUS = {
  success: 'success',
  failure: 'failure',
} as const;

/** A command's success/failure status. */
export type ResultStatus = (typeof RESULT_STATUS)[keyof typeof RESULT_STATUS];

/**
 * `ResultError.code` values — the stable error class matching the exit-code
 * taxonomy. `signal` is the class of a run a SIGINT/SIGTERM cut short; it maps
 * to exit code 130 and is distinct from an unclassified `failure`.
 */
export const RESULT_ERROR_CODE = {
  usage: 'usage',
  auth: 'auth',
  api: 'api',
  signal: 'signal',
  failure: 'failure',
} as const;

/** The stable error class carried by a failed result. */
export type ResultErrorCode = (typeof RESULT_ERROR_CODE)[keyof typeof RESULT_ERROR_CODE];

/** The error member of a failed {@link Result}. */
export interface ResultError {
  /** Stable error class matching the exit-code taxonomy. */
  code: ResultErrorCode;
  /** Human-readable failure message. */
  message: string;
  /** Optional command or documentation link that helps recover from the failure. */
  next?: string;
}

/**
 * The version-1 envelope for a command's machine-readable output in
 * `--output=json` mode. Current builds emit {@link ResultV2}; this shape reads
 * documents without `protocolVersion`.
 */
export interface Result {
  /** Full command path, e.g. "build" or "cloud status". */
  command: string;
  /** "success" or "failure". */
  status: ResultStatus;
  /** Command-specific payload (a run summary, a list, ...). */
  data?: unknown;
  /** Present, describing the failure, when status is "failure". */
  error?: ResultError;
  /** Process exit code the command returns (see the exit-code taxonomy). */
  exitCode: number;
}

/** Reports whether value conforms to the {@link ResultError} shape. */
export function isResultError(value: unknown): value is ResultError {
  if (typeof value !== 'object' || value === null) {
    return false;
  }
  const obj = value as Record<string, unknown>;
  return (
    typeof obj['code'] === 'string' &&
    (Object.values(RESULT_ERROR_CODE) as string[]).includes(obj['code']) &&
    typeof obj['message'] === 'string' &&
    (obj['next'] === undefined || typeof obj['next'] === 'string')
  );
}

/** Reports whether value conforms to the {@link Result} shape. */
export function isResult(value: unknown): value is Result {
  if (typeof value !== 'object' || value === null) {
    return false;
  }
  const obj = value as Record<string, unknown>;
  if (typeof obj['command'] !== 'string') {
    return false;
  }
  if (obj['status'] !== RESULT_STATUS.success && obj['status'] !== RESULT_STATUS.failure) {
    return false;
  }
  if (typeof obj['exitCode'] !== 'number') {
    return false;
  }
  if (obj['error'] !== undefined && !isResultError(obj['error'])) {
    return false;
  }
  return true;
}
