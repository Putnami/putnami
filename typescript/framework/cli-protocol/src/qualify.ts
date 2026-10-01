/**
 * TypeScript twin of `go.putnami.dev/protocol/qualify`: the smoke contract the
 * CLI derives from a workload's route inventory, and the verdict of running it
 * against a target.
 *
 * The verdict fails closed: `passed` is the only pass, and a `passed` verdict
 * without its proof is refused. `validateQualifyVerdict` and
 * `validateQualifyContract` apply the Go rules and emit the Go codes;
 * `test/qualify-conformance.test.ts` runs both against the corpus under
 * `protocols/qualify/fixtures`.
 */
import type { Violation } from './result-v2';

/** Document version of the contract and the verdict. A reader refuses any other value. */
export const QUALIFY_PROTOCOL_VERSION = 1;

/** `$id` of `protocols/qualify/schemas/contract.json`. */
export const QUALIFY_CONTRACT_SCHEMA_ID = 'https://putnami.dev/schemas/putnami-qualify-contract.json';

/** `$id` of `protocols/qualify/schemas/verdict.json`. */
export const QUALIFY_VERDICT_SCHEMA_ID = 'https://putnami.dev/schemas/putnami-qualify-verdict.json';

/** The closed outcome vocabulary, in the Go declaration order. Only `passed` is a pass. */
export const QUALIFY_STATES = [
  'passed',
  'failed',
  'unsupported',
  'not_run',
  'timed_out',
  'canceled',
  'target_unreachable',
  'digest_mismatch',
  'composition_failed',
] as const;

/** One verdict, phase, or request outcome. */
export type QualifyState = (typeof QUALIFY_STATES)[number];

/** Phase names, in execution order. A verdict records every one of them. */
export const QUALIFY_PHASE_NAMES = ['resolve-target', 'readiness', 'version-binding', 'smoke', 'teardown'] as const;

/** One phase name. */
export type QualifyPhaseName = (typeof QUALIFY_PHASE_NAMES)[number];

/** Validation codes, identical to the Go `ErrorCode*` constants. */
export const QUALIFY_VIOLATION_CODE = {
  parseError: 'qualify.parse_error',
  unknownField: 'qualify.unknown_field',
  unsupportedProtocolVersion: 'qualify.unsupported_protocol_version',
  required: 'qualify.required',
  invalidState: 'qualify.invalid_state',
  invalidTarget: 'qualify.invalid_target',
  invalidBinding: 'qualify.invalid_binding',
  invalidPhase: 'qualify.invalid_phase',
  invalidRequest: 'qualify.invalid_request',
  invalidSource: 'qualify.invalid_source',
  invalidDigest: 'qualify.invalid_digest',
  invalidCleanup: 'qualify.invalid_cleanup',
  invalidTimestamp: 'qualify.invalid_timestamp',
  unprovenPass: 'qualify.unproven_pass',
} as const;

/** One read-only smoke request. */
export interface QualifyRequest {
  /** `"<method> <path>"`, unique within a contract. */
  id: string;
  /** `GET` or `HEAD`. */
  method: string;
  /** Route path, relative to the target base URL. */
  path: string;
  /** Highest response status the request accepts. */
  maxStatus: number;
  /** Source kind of the route fact the request was derived from. */
  provenance: string;
}

/** One document a contract was derived from. */
export interface QualifySource {
  /** Source document kind; version 1 knows `http-routes` only. */
  kind: string;
  /** Workspace-relative, slash-separated path of the document read. */
  path: string;
  /** Digest the source document declares for itself. */
  digest: string;
}

/** The derived smoke contract of one workload. */
export interface QualifyContract {
  /** Document version. */
  protocolVersion: number;
  /** Canonical ID of the workload. */
  project: string;
  /** Documents the requests were derived from. */
  derivedFrom: QualifySource[];
  /** Smoke requests, in execution order. */
  requests: QualifyRequest[];
  /** `sha256:` plus the hex SHA-256 of the canonical JSON of `requests`. */
  digest: string;
}

/** What a verdict ran against. */
export interface QualifyTarget {
  /** `local` or `url`. */
  kind: string;
  /** Base URL; required for a url target, never carries credentials. */
  url?: string;
  /** Identifier of the local composition that served the target. */
  compositionId?: string;
}

/** Which build a verdict proves. */
export interface QualifyBinding {
  /** `tree` or `artifact`. */
  kind: string;
  /** Content digest of the worktree a tree binding served. */
  fingerprint?: string;
  /** Whether that worktree differed from HEAD. */
  dirty?: boolean;
  /** Commit the worktree sat on. */
  headSHA?: string;
  /** Sha an artifact binding requires the target to report. */
  expectedSHA?: string;
  /** Sha the target reported on `/version`. */
  observedSHA?: string;
  /** Version the target reported on `/version`. */
  version?: string;
}

/** A structured finding on a phase, shaped like `go.putnami.dev/protocol/diagnostic`. */
export interface QualifyDiagnostic {
  /** `error`, `warning` or `info`. */
  severity: string;
  /** Stable `qualify.*` code. */
  code: string;
  /** Human-readable explanation. */
  message: string;
  /** Dotted member path the finding is about. */
  field?: string;
}

/** One step of an execution. */
export interface QualifyPhase {
  /** One of {@link QUALIFY_PHASE_NAMES}. */
  name: string;
  /** Phase outcome. */
  state: QualifyState;
  /** Wall-clock duration in milliseconds. */
  durationMs: number;
  /** Findings explaining a non-pass state. */
  diagnostics?: QualifyDiagnostic[];
}

/** The outcome of one contract request. */
export interface QualifyRequestResult {
  /** Contract request ID. */
  id: string;
  /** HTTP status the target answered, when it answered. */
  status?: number;
  /** Wall-clock duration in milliseconds. */
  durationMs: number;
  /** Request outcome. */
  state: QualifyState;
  /** Explanation of a non-pass state. */
  reason?: string;
}

/** Identifies the executed contract. */
export interface QualifyContractRef {
  /** Digest of the executed contract. */
  digest: string;
  /** Number of requests the contract holds. */
  requests: number;
  /** Documents the executed contract was derived from. */
  derivedFrom: QualifySource[];
}

/** Teardown report of a local target. */
export interface QualifyCleanup {
  /** `clean` or `partial`. */
  state: string;
  /** Resources a partial teardown left behind. */
  leftovers: string[];
}

/** The single outcome of qualifying one workload against one target. */
export interface QualifyVerdict {
  /** Document version. */
  protocolVersion: number;
  /** Canonical ID of the qualified workload. */
  project: string;
  /** What the contract ran against. */
  target: QualifyTarget;
  /** Which build the verdict proves. */
  binding: QualifyBinding;
  /** The executed contract. */
  contract: QualifyContractRef;
  /** Every phase, in order. */
  phases: QualifyPhase[];
  /** Every contract request, in contract order. */
  requests: QualifyRequestResult[];
  /** The verdict. */
  state: QualifyState;
  /** RFC 3339 UTC start time. */
  startedAt: string;
  /** RFC 3339 UTC finish time. */
  finishedAt: string;
  /** Teardown report of a local target; absent for a url target. */
  cleanup?: QualifyCleanup;
}

/** Reports whether `state` is the single passing state. */
export function isQualifyPass(state: unknown): state is 'passed' {
  return state === 'passed';
}

type Shape = 'string' | 'integer' | 'boolean' | { object: Record<string, Shape> } | { array: Shape };

const SOURCE: Shape = { object: { kind: 'string', path: 'string', digest: 'string' } };
const REQUEST: Shape = {
  object: { id: 'string', method: 'string', path: 'string', maxStatus: 'integer', provenance: 'string' },
};
const CONTRACT: Shape = {
  object: {
    protocolVersion: 'integer',
    project: 'string',
    derivedFrom: { array: SOURCE },
    requests: { array: REQUEST },
    digest: 'string',
  },
};
const VERDICT: Shape = {
  object: {
    protocolVersion: 'integer',
    project: 'string',
    target: { object: { kind: 'string', url: 'string', compositionId: 'string' } },
    binding: {
      object: {
        kind: 'string',
        fingerprint: 'string',
        dirty: 'boolean',
        headSHA: 'string',
        expectedSHA: 'string',
        observedSHA: 'string',
        version: 'string',
      },
    },
    contract: { object: { digest: 'string', requests: 'integer', derivedFrom: { array: SOURCE } } },
    phases: {
      array: {
        object: {
          name: 'string',
          state: 'string',
          durationMs: 'integer',
          diagnostics: {
            array: { object: { severity: 'string', code: 'string', message: 'string', field: 'string' } },
          },
        },
      },
    },
    requests: {
      array: {
        object: { id: 'string', status: 'integer', durationMs: 'integer', state: 'string', reason: 'string' },
      },
    },
    state: 'string',
    startedAt: 'string',
    finishedAt: 'string',
    cleanup: { object: { state: 'string', leftovers: { array: 'string' } } },
  },
};

/** Exposed for the drift test that holds these shapes to the published schemas. */
export const QUALIFY_SHAPES = { contract: CONTRACT, verdict: VERDICT } as const;

const DIGEST_PATTERN = /^sha256:[0-9a-f]{64}$/;
const TIMESTAMP_PATTERN = /^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}(\.[0-9]+)?(Z|[+-][0-9]{2}:[0-9]{2})$/;

type JsonObject = Record<string, unknown>;

function isObject(value: unknown): value is JsonObject {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function join(parent: string, key: string): string {
  return parent === '' ? key : `${parent}.${key}`;
}

/** First undeclared member, visiting object members in sorted order (the Go walk's order). */
function firstUnknownMember(value: unknown, shape: Shape, path: string): string | undefined {
  if (typeof shape === 'string') return undefined;
  if ('object' in shape && isObject(value)) {
    for (const key of Object.keys(value).sort()) {
      const child = join(path, key);
      const memberShape = shape.object[key];
      if (memberShape === undefined) return child;
      const found = firstUnknownMember(value[key], memberShape, child);
      if (found !== undefined) return found;
    }
  }
  if ('array' in shape && Array.isArray(value)) {
    for (const [index, item] of value.entries()) {
      const found = firstUnknownMember(item, shape.array, `${path}[${index}]`);
      if (found !== undefined) return found;
    }
  }
  return undefined;
}

/** Whether value decodes into shape the way encoding/json would; null reads as absent. */
function decodes(value: unknown, shape: Shape): boolean {
  if (value === null || value === undefined) return true;
  if (shape === 'string') return typeof value === 'string';
  if (shape === 'boolean') return typeof value === 'boolean';
  if (shape === 'integer') return typeof value === 'number' && Number.isInteger(value);
  if ('array' in shape) return Array.isArray(value) && value.every((item) => decodes(item, shape.array));
  if (!isObject(value)) return false;
  return Object.entries(value).every(([key, member]) => {
    const memberShape = shape.object[key];
    return memberShape !== undefined && decodes(member, memberShape);
  });
}

function strictDecode(value: unknown, shape: Shape): Violation[] | undefined {
  if (!isObject(value)) {
    return [{ code: QUALIFY_VIOLATION_CODE.parseError, path: '' }];
  }
  const unknownMember = firstUnknownMember(value, shape, '');
  if (unknownMember !== undefined) {
    return [{ code: QUALIFY_VIOLATION_CODE.unknownField, path: unknownMember }];
  }
  if (!decodes(value, shape)) {
    return [{ code: QUALIFY_VIOLATION_CODE.parseError, path: '' }];
  }
  return undefined;
}

function str(value: unknown): string {
  return typeof value === 'string' ? value : '';
}

function num(value: unknown): number {
  return typeof value === 'number' ? value : 0;
}

function list(value: unknown): unknown[] {
  return Array.isArray(value) ? value : [];
}

function obj(value: unknown): JsonObject {
  return isObject(value) ? value : {};
}

function isState(value: string): boolean {
  return (QUALIFY_STATES as readonly string[]).includes(value);
}

function blank(value: string): boolean {
  return value.trim() === '';
}

function sortViolations(violations: Violation[]): Violation[] {
  return violations.sort((a, b) => {
    if (a.path !== b.path) return a.path < b.path ? -1 : 1;
    if (a.code !== b.code) return a.code < b.code ? -1 : 1;
    return 0;
  });
}

function validateSources(field: string, sources: unknown): Violation[] {
  const out: Violation[] = [];
  for (const [index, raw] of list(sources).entries()) {
    const source = obj(raw);
    const at = `${field}[${index}]`;
    if (str(source['kind']) !== 'http-routes') {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidSource, path: `${at}.kind` });
    }
    if (blank(str(source['path'])) || blank(str(source['digest']))) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidSource, path: at });
    }
  }
  return out;
}

/**
 * The canonical digest of a request list: `sha256:` plus the hex SHA-256 of an
 * array of objects with members sorted by name, no whitespace, no HTML
 * escaping, and U+2028/U+2029 escaped the way Go's encoder escapes them.
 */
export async function qualifyContractDigest(requests: readonly QualifyRequest[]): Promise<string> {
  const canonical = JSON.stringify(
    requests.map((request) => ({
      id: request.id,
      maxStatus: request.maxStatus,
      method: request.method,
      path: request.path,
      provenance: request.provenance,
    })),
  )
    .split(String.fromCharCode(0x20_28))
    .join('\\u2028')
    .split(String.fromCharCode(0x20_29))
    .join('\\u2029');
  const hash = new Uint8Array(await crypto.subtle.digest('SHA-256', new TextEncoder().encode(canonical)));
  return `sha256:${Array.from(hash, (byte) => byte.toString(16).padStart(2, '0')).join('')}`;
}

/** Validates a parsed contract document with the Go `ParseAndValidateContract` rules. */
export async function validateQualifyContract(value: unknown): Promise<Violation[]> {
  const decodeViolations = strictDecode(value, CONTRACT);
  if (decodeViolations) return decodeViolations;
  const contract = value as JsonObject;
  const out: Violation[] = [];
  if (num(contract['protocolVersion']) !== QUALIFY_PROTOCOL_VERSION) {
    out.push({ code: QUALIFY_VIOLATION_CODE.unsupportedProtocolVersion, path: 'protocolVersion' });
  }
  if (blank(str(contract['project']))) {
    out.push({ code: QUALIFY_VIOLATION_CODE.required, path: 'project' });
  }
  out.push(...validateSources('derivedFrom', contract['derivedFrom']));
  const requests: QualifyRequest[] = [];
  const seen = new Set<string>();
  for (const [index, raw] of list(contract['requests']).entries()) {
    const request = obj(raw);
    const field = `requests[${index}]`;
    const typed: QualifyRequest = {
      id: str(request['id']),
      method: str(request['method']),
      path: str(request['path']),
      maxStatus: num(request['maxStatus']),
      provenance: str(request['provenance']),
    };
    requests.push(typed);
    if (typed.method !== 'GET' && typed.method !== 'HEAD') {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: `${field}.method` });
    }
    if (!typed.path.startsWith('/')) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: `${field}.path` });
    }
    if (typed.id !== `${typed.method} ${typed.path}`) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: `${field}.id` });
    }
    if (seen.has(typed.id)) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: `${field}.id` });
    }
    seen.add(typed.id);
    if (typed.maxStatus < 100 || typed.maxStatus > 599) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: `${field}.maxStatus` });
    }
    if (blank(typed.provenance)) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: `${field}.provenance` });
    }
  }
  const digest = str(contract['digest']);
  if (!DIGEST_PATTERN.test(digest)) {
    out.push({ code: QUALIFY_VIOLATION_CODE.invalidDigest, path: 'digest' });
  } else if (digest !== (await qualifyContractDigest(requests))) {
    out.push({ code: QUALIFY_VIOLATION_CODE.invalidDigest, path: 'digest' });
  }
  return out;
}

/** Validates a parsed verdict document with the Go `ParseAndValidateVerdict` rules. */
export function validateQualifyVerdict(value: unknown): Violation[] {
  const decodeViolations = strictDecode(value, VERDICT);
  if (decodeViolations) return decodeViolations;
  const verdict = value as JsonObject;
  const out: Violation[] = [];
  if (num(verdict['protocolVersion']) !== QUALIFY_PROTOCOL_VERSION) {
    out.push({ code: QUALIFY_VIOLATION_CODE.unsupportedProtocolVersion, path: 'protocolVersion' });
  }
  if (blank(str(verdict['project']))) {
    out.push({ code: QUALIFY_VIOLATION_CODE.required, path: 'project' });
  }

  const target = obj(verdict['target']);
  const targetKind = str(target['kind']);
  if (targetKind === 'url') {
    if (blank(str(target['url']))) out.push({ code: QUALIFY_VIOLATION_CODE.invalidTarget, path: 'target.url' });
  } else if (targetKind !== 'local') {
    out.push({ code: QUALIFY_VIOLATION_CODE.invalidTarget, path: 'target.kind' });
  }

  const binding = obj(verdict['binding']);
  const bindingKind = str(binding['kind']);
  if (bindingKind === 'artifact') {
    if (blank(str(binding['expectedSHA']))) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidBinding, path: 'binding.expectedSHA' });
    }
  } else if (bindingKind === 'tree') {
    if (blank(str(binding['fingerprint'])) || blank(str(binding['headSHA']))) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidBinding, path: 'binding' });
    }
  } else {
    out.push({ code: QUALIFY_VIOLATION_CODE.invalidBinding, path: 'binding.kind' });
  }

  const contract = obj(verdict['contract']);
  if (!DIGEST_PATTERN.test(str(contract['digest']))) {
    out.push({ code: QUALIFY_VIOLATION_CODE.invalidDigest, path: 'contract.digest' });
  }
  if (num(contract['requests']) < 0) {
    out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: 'contract.requests' });
  }
  out.push(...validateSources('contract.derivedFrom', contract['derivedFrom']));

  const phases = list(verdict['phases']).map(obj);
  const seenPhases = new Set<string>();
  for (const [index, phase] of phases.entries()) {
    const field = `phases[${index}]`;
    const name = str(phase['name']);
    if (!(QUALIFY_PHASE_NAMES as readonly string[]).includes(name) || seenPhases.has(name)) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidPhase, path: `${field}.name` });
    }
    seenPhases.add(name);
    if (!isState(str(phase['state']))) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidState, path: `${field}.state` });
    }
    if (num(phase['durationMs']) < 0) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidPhase, path: `${field}.durationMs` });
    }
  }

  const requests = list(verdict['requests']).map(obj);
  for (const [index, request] of requests.entries()) {
    const field = `requests[${index}]`;
    if (blank(str(request['id']))) {
      out.push({ code: QUALIFY_VIOLATION_CODE.required, path: `${field}.id` });
    }
    if (!isState(str(request['state']))) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidState, path: `${field}.state` });
    }
    const status = num(request['status']);
    if (status !== 0 && (status < 100 || status > 599)) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: `${field}.status` });
    }
    if (num(request['durationMs']) < 0) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidRequest, path: `${field}.durationMs` });
    }
  }

  const state = str(verdict['state']);
  if (!isState(state)) {
    out.push({ code: QUALIFY_VIOLATION_CODE.invalidState, path: 'state' });
  }
  for (const field of ['startedAt', 'finishedAt']) {
    if (!TIMESTAMP_PATTERN.test(str(verdict[field]))) {
      out.push({ code: QUALIFY_VIOLATION_CODE.invalidTimestamp, path: field });
    }
  }

  const rawCleanup = verdict['cleanup'];
  const hasCleanup = isObject(rawCleanup);
  const cleanupState = hasCleanup ? str(rawCleanup['state']) : '';
  if (hasCleanup) {
    const leftovers = list(rawCleanup['leftovers']).length;
    const clean = cleanupState === 'clean' && leftovers === 0;
    const partial = cleanupState === 'partial' && leftovers > 0;
    if (!clean && !partial) out.push({ code: QUALIFY_VIOLATION_CODE.invalidCleanup, path: 'cleanup' });
  }

  if (isQualifyPass(state)) {
    // A passed verdict must carry its proof; each gap is refused, never read as vacuously true.
    const byName = new Map(phases.map((phase) => [str(phase['name']), str(phase['state'])]));
    for (const name of QUALIFY_PHASE_NAMES) {
      if (!isQualifyPass(byName.get(name))) out.push({ code: QUALIFY_VIOLATION_CODE.unprovenPass, path: 'phases' });
    }
    if (requests.length === 0) out.push({ code: QUALIFY_VIOLATION_CODE.unprovenPass, path: 'requests' });
    for (const [index, request] of requests.entries()) {
      if (!isQualifyPass(str(request['state']))) {
        out.push({ code: QUALIFY_VIOLATION_CODE.unprovenPass, path: `requests[${index}].state` });
      }
    }
    if (bindingKind === 'artifact' && !str(binding['observedSHA']).startsWith(str(binding['expectedSHA']))) {
      out.push({ code: QUALIFY_VIOLATION_CODE.unprovenPass, path: 'binding.observedSHA' });
    }
    if (hasCleanup && cleanupState !== 'clean') {
      out.push({ code: QUALIFY_VIOLATION_CODE.unprovenPass, path: 'cleanup' });
    }
  }
  return sortViolations(out);
}
