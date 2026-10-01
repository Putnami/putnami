import { existsSync, readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { buildJsonRecord } from '../logger/json.sink';
import type { LogEntry } from '../logger/logger.type';

/**
 * The TypeScript half of the cross-runtime logging conformance harness: it makes
 * the canonical corpus in `protocols/logging/conformance` EXECUTABLE against the
 * records the framework really emits.
 *
 * A boundary test loads the corpus cases for its boundary, drives REAL boundary
 * code (middleware chain / dispatch / pool) with a {@link MemoryLogger}, and
 * asserts each captured entry against its case:
 *
 * ```typescript
 * const cases = loadCases('http');
 * const logger = new MemoryLogger();
 * setRootLogger(logger);
 * // … drive real code …
 * const want = findCase(cases, 'http.terminal.success');
 * assertRecord(findRecord(logger.entries, want), want);
 * ```
 *
 * Rendering goes through `buildJsonRecord` — the exact function the production
 * `JsonSink` prints — so the sink's reserved-key protection, single-data-object
 * flattening, and trace-key choice are under test too, not just the call site's
 * intent.
 *
 * The comparison semantics (tokens, `$open` objects, sorted-key 2-space
 * re-serialization, byte compare) are normative in
 * `protocols/logging/conformance/README.md` and are implemented identically by the
 * Go half (`go.putnami.dev/logger/logtest`).
 */

/** Corpus tokens: a leaf pinned by TYPE because its value legitimately varies. */
export const TOKEN_TIMESTAMP = '<iso8601-millis-utc>';
export const TOKEN_NUMBER = '<number>';
export const TOKEN_STRING = '<string>';

/** Marks an expected object that tolerates runtime-specific extra keys. */
const OPEN_MARKER = '$open';

/** The corpus's `<iso8601-millis-utc>` contract: UTC, exactly three fractional digits. */
const TIMESTAMP_PATTERN = /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}\.\d{3}Z$/;

/** Relative location of the canonical corpus from the repository root. */
const MANIFEST_RELATIVE = join('protocols', 'logging', 'conformance', 'manifest.json');

/** One corpus case: the expected record plus what a failure message needs. */
export interface ConformanceCase {
  id: string;
  level: 'required' | 'recommended' | string;
  boundary: string;
  summary?: string;
  record: Record<string, unknown>;
}

interface ManifestFile {
  protocol: string;
  suite: string;
  protocolVersion: number;
  tokens: Record<string, string>;
  cases: ConformanceCase[];
}

/**
 * Resolve the canonical corpus by walking up from this module until the
 * repository's `protocols/logging/conformance/manifest.json` is found. Walking up
 * (rather than a fixed `../../../../..`) keeps the harness working from a
 * workspace symlink and from any consumer package's depth.
 */
export function manifestPath(): string {
  let dir = import.meta.dir;
  for (let i = 0; i < 12; i++) {
    const candidate = join(dir, MANIFEST_RELATIVE);
    if (existsSync(candidate)) {
      return candidate;
    }
    const parent = dirname(dir);
    if (parent === dir) {
      break;
    }
    dir = parent;
  }
  throw new Error(
    `logging conformance corpus not found: no ${MANIFEST_RELATIVE} above ${import.meta.dir}. ` +
      'The harness reads the corpus from the repository, so it must run inside the workspace.',
  );
}

/**
 * Load the corpus cases for one boundary (`http` | `event` | `database` |
 * `migration`). Throws when the boundary has no case: a silently empty suite
 * would make a boundary test pass while asserting nothing.
 */
export function loadCases(boundary: string): ConformanceCase[] {
  const path = manifestPath();
  const manifest = JSON.parse(readFileSync(path, 'utf8')) as ManifestFile;
  if (manifest.protocol !== 'putnami.logging.v1') {
    throw new Error(`${path} declares protocol "${manifest.protocol}", want putnami.logging.v1`);
  }
  const cases = manifest.cases.filter((c) => c.boundary === boundary);
  if (cases.length === 0) {
    throw new Error(`logging conformance manifest ${path} has no case for boundary "${boundary}"`);
  }
  return cases;
}

/** The case with the given id, or a throw naming what the boundary does have. */
export function findCase(cases: ConformanceCase[], id: string): ConformanceCase {
  const found = cases.find((c) => c.id === id);
  if (!found) {
    throw new Error(
      `logging conformance case "${id}" not found (have: ${cases.map((c) => c.id).join(', ')}). ` +
        'A renamed or deleted corpus case must break the boundary test, not silently disable it.',
    );
  }
  return found;
}

/** The pinned logger name of a case's record. */
export function caseLogger(want: ConformanceCase): string {
  return typeof want.record['logger'] === 'string' ? (want.record['logger'] as string) : '';
}

/** The pinned stable message of a case's record. */
export function caseMessage(want: ConformanceCase): string {
  return typeof want.record['message'] === 'string' ? (want.record['message'] as string) : '';
}

/** Render captured entries as `[level] logger message` lines for failures. */
export function dumpRecords(entries: readonly LogEntry[]): string {
  if (entries.length === 0) {
    return '\n  (no records captured)';
  }
  return entries.map((e) => `\n  [${e.level}] ${e.logger ?? ''} ${e.message}`).join('');
}

/**
 * The single captured entry matching a case's pinned logger and message. Exactly
 * one is required: the contract's terminal-record policy is "exactly one terminal
 * record per boundary", so a duplicate is a contract violation.
 */
export function findRecord(entries: readonly LogEntry[], want: ConformanceCase): LogEntry {
  const logger = caseLogger(want);
  const message = caseMessage(want);
  const found = entries.filter((e) => (e.logger ?? '') === logger && e.message === message);
  if (found.length !== 1) {
    throw new Error(
      `logging conformance case "${want.id}": expected exactly 1 "${message}" record from logger ` +
        `"${logger}", got ${found.length}.${dumpRecords(entries)}`,
    );
  }
  return found[0] as LogEntry;
}

/**
 * Assert that a captured entry matches its corpus case after canonicalization.
 * Throws (failing the calling `bun:test` case) with both canonical forms and the
 * three places a contract change must touch together.
 */
export function assertRecord(entry: LogEntry, want: ConformanceCase): void {
  const { ok, detail } = compareRecord(entry, want);
  if (!ok) {
    throw new Error(detail);
  }
}

/**
 * Compare one captured entry against its case, returning the failure detail
 * rather than throwing. The pure core of {@link assertRecord}, exported so the
 * harness's own semantics are unit-testable.
 */
export function compareRecord(entry: LogEntry, want: ConformanceCase): { ok: boolean; detail: string } {
  // JSON round-trip: this is EXACTLY what the JsonSink prints (its extra
  // circular-safety fallbacks only matter for hostile values), so the comparison
  // sees the aggregator's view — undefined fields dropped, Dates as ISO strings.
  const got = JSON.parse(JSON.stringify(buildJsonRecord(entry))) as unknown;

  const [wantCanon, gotCanon] = canonicalize(want.record, got);
  const wantText = stableStringify(wantCanon);
  const gotText = stableStringify(gotCanon);
  if (wantText === gotText) {
    return { ok: true, detail: '' };
  }
  return { ok: false, detail: failureDetail(want, gotText, wantText) };
}

/**
 * The shared failure text of both runtimes' harnesses: it names the corpus AND the
 * twin runtime's test, because a divergence here means either the change forgot
 * the other runtime or it forgot the corpus.
 */
function failureDetail(want: ConformanceCase, gotText: string, wantText: string): string {
  return [
    `logging conformance case "${want.id}" (boundary "${want.boundary}") does not match the corpus.`,
    'got (canonical):',
    gotText,
    'want (canonical):',
    wantText,
    'This record is a public contract (dashboards, alerts, log-based metrics). Either the',
    'emitter drifted, or the change is intentional — in which case update the fixture AND',
    "the other runtime's test in the same commit:",
    '  protocols/logging/conformance/manifest.json   (the normative corpus)',
    `  ${tsTwin(want.boundary)}`,
    `  ${goTwin(want.boundary)}`,
  ].join('\n');
}

function tsTwin(boundary: string): string {
  if (boundary === 'http') return 'typescript/framework/application/test/http/logging-cross-language.test.ts';
  if (boundary === 'event') return 'typescript/framework/events/test/logging-cross-language.test.ts';
  return 'typescript/framework/database/test/logging-cross-language.test.ts';
}

function goTwin(boundary: string): string {
  if (boundary === 'http') return 'go/framework/http/logging_cross_language_test.go';
  if (boundary === 'event') return 'go/framework/events/logging_cross_language_test.go';
  // database + migration records are both emitted by go.putnami.dev/database.
  return 'go/framework/database/logging_cross_language_test.go';
}

/**
 * Walk the expected and actual trees together and return the two canonical forms
 * to byte-compare:
 *
 * - a token leaf is type/pattern checked and, when it matches, replaced by the
 *   token text on BOTH sides;
 * - an expected object carrying `"$open": true` keeps only the keys it declares on
 *   the actual side (runtime-specific extras are legitimate there) and drops the
 *   marker;
 * - every other object is closed: an unexpected key stays on the actual side and
 *   therefore fails the compare, which is how the corpus catches drift by ADDITION
 *   as well as by omission.
 */
function canonicalize(want: unknown, got: unknown): [unknown, unknown] {
  if (typeof want === 'string' && isToken(want)) {
    // Keep the raw actual value on a mismatch so the diff shows what arrived.
    return tokenMatches(want, got) ? [want, want] : [want, got];
  }
  if (Array.isArray(want)) {
    return canonicalizeArray(want, got);
  }
  if (isPlainObject(want)) {
    return canonicalizeObject(want, got);
  }
  return [want, got];
}

function canonicalizeObject(want: Record<string, unknown>, got: unknown): [unknown, unknown] {
  if (!isPlainObject(got)) {
    // Shape mismatch (e.g. a group rendered as a string): report it with the raw
    // actual value.
    return [stripMarkers(want), got];
  }
  const open = want[OPEN_MARKER] === true;
  const wantOut: Record<string, unknown> = {};
  const gotOut: Record<string, unknown> = {};

  for (const [key, wantValue] of Object.entries(want)) {
    if (key === OPEN_MARKER) continue;
    if (!(key in got)) {
      // Missing on the actual side: only the expected key is emitted, so the diff
      // shows it as absent.
      wantOut[key] = stripMarkers(wantValue);
      continue;
    }
    const [wc, gc] = canonicalize(wantValue, got[key]);
    wantOut[key] = wc;
    gotOut[key] = gc;
  }
  for (const [key, gotValue] of Object.entries(got)) {
    if (key in want) continue;
    if (open) continue; // runtime-specific extra: stripped by contract
    gotOut[key] = gotValue; // unexpected key: kept so the byte compare fails
  }
  return [wantOut, gotOut];
}

function canonicalizeArray(want: unknown[], got: unknown): [unknown, unknown] {
  if (!Array.isArray(got)) {
    return [stripMarkers(want), got];
  }
  const wantOut: unknown[] = [];
  const gotOut: unknown[] = [];
  for (let i = 0; i < want.length; i++) {
    if (i >= got.length) {
      wantOut.push(stripMarkers(want[i]));
      continue;
    }
    const [wc, gc] = canonicalize(want[i], got[i]);
    wantOut.push(wc);
    gotOut.push(gc);
  }
  // Extra actual elements are kept so the compare reports the length difference.
  for (let i = want.length; i < got.length; i++) {
    gotOut.push(got[i]);
  }
  return [wantOut, gotOut];
}

/** Remove `$open` markers from an expected subtree with no actual counterpart. */
function stripMarkers(want: unknown): unknown {
  if (Array.isArray(want)) {
    return want.map(stripMarkers);
  }
  if (isPlainObject(want)) {
    const out: Record<string, unknown> = {};
    for (const [key, value] of Object.entries(want)) {
      if (key === OPEN_MARKER) continue;
      out[key] = stripMarkers(value);
    }
    return out;
  }
  return want;
}

function isToken(value: string): boolean {
  return value === TOKEN_TIMESTAMP || value === TOKEN_NUMBER || value === TOKEN_STRING;
}

function tokenMatches(token: string, got: unknown): boolean {
  if (token === TOKEN_TIMESTAMP) {
    return typeof got === 'string' && TIMESTAMP_PATTERN.test(got);
  }
  if (token === TOKEN_NUMBER) {
    return typeof got === 'number' && Number.isFinite(got);
  }
  return typeof got === 'string' && got.length > 0;
}

/** True for objects with the default (or null) prototype — not arrays/instances. */
function isPlainObject(value: unknown): value is Record<string, unknown> {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) {
    return false;
  }
  const proto = Object.getPrototypeOf(value);
  return proto === Object.prototype || proto === null;
}

/**
 * Serialize with sorted keys and a 2-space indent — the corpus's canonical form.
 * Go's `encoding/json` sorts map keys natively; this sorts explicitly so both
 * runtimes produce the identical text and insertion order is never compared.
 */
function stableStringify(value: unknown): string {
  return JSON.stringify(sortDeep(value), null, 2);
}

function sortDeep(value: unknown): unknown {
  if (Array.isArray(value)) {
    return value.map(sortDeep);
  }
  if (isPlainObject(value)) {
    const out: Record<string, unknown> = {};
    for (const key of Object.keys(value).sort()) {
      out[key] = sortDeep(value[key]);
    }
    return out;
  }
  return value;
}
