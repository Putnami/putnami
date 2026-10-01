/**
 * Public support-status catalog: reader and page renderer.
 *
 * Source of truth is `putnami.support.json` at the WORKSPACE root — the single
 * reviewed authority defined by `protocols/support`. This site keeps no second
 * inventory: the published support page and every status badge are rendered
 * from that file, so a status can only change by changing the reviewed catalog.
 *
 * Reading is strict in the same ways `protocols/support` is strict, and for the
 * same reason: a status is a public promise, so a catalog this site cannot read
 * exactly must fail the build rather than be published approximately. That
 * rules out `JSON.parse`, which silently keeps the last of a set of duplicate
 * keys and collapses `1.0` and `1e0` into the same number as the exact integer
 * token `1`. The strict reader in `strict-json.ts` preserves what those checks
 * need, and the shared fixture corpus under `protocols/support/fixtures` is
 * asserted against this parser so the two implementations cannot drift.
 *
 * Client-safe: pure parsing and string rendering, no filesystem access. The
 * generate-time reader lives in `catalog.server.ts`.
 */
import { StrictJsonError, parseStrictJsonObject } from './strict-json';

/** The closed v1 subject-kind vocabulary. */
export type SupportKind = 'protocol' | 'package' | 'feature';

/** The closed v1 status vocabulary. `beta` and `evolving` are not wire values. */
export type SupportStatus = 'stable' | 'preview' | 'experimental';

export const SUPPORT_KINDS: readonly SupportKind[] = ['protocol', 'package', 'feature'];
export const SUPPORT_STATUSES: readonly SupportStatus[] = ['stable', 'preview', 'experimental'];

/** The exact `protocolVersion` token v1 readers accept. */
export const SUPPORT_PROTOCOL_VERSION = 1;

export interface SupportEntry {
  id: string;
  kind: SupportKind;
  status: SupportStatus;
  /** Independent statement about the default experience; absent makes no claim. */
  default?: boolean;
  /** v1 accepts only `unsupported`; absent makes no claim. */
  parity?: 'unsupported';
}

export interface SupportCatalog {
  protocolVersion: number;
  entries: SupportEntry[];
}

/** Identity of a classified subject: kind and id are both required. */
export interface SupportSubject {
  kind: SupportKind;
  id: string;
}

function fail(message: string): never {
  throw new Error(`putnami.support.json: ${message}`);
}

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** Fields the v1 `Catalog` struct declares. Anything else is a wire change. */
const CATALOG_FIELDS = new Set(['$schema', 'protocolVersion', 'entries']);

/**
 * Fields the v1 `Entry` struct declares. `maturity` is the one most likely to
 * be added by mistake: it belongs to the feature-evidence ladder, and a support
 * entry that carried one would be asserting a second, conflicting vocabulary.
 */
const ENTRY_FIELDS = new Set(['id', 'kind', 'status', 'default', 'parity']);

function rejectUnknownFields(raw: Record<string, unknown>, allowed: Set<string>, where: string): void {
  for (const key of Object.keys(raw)) {
    if (!allowed.has(key)) fail(`${where} has unknown field ${JSON.stringify(key)}`);
  }
}

/**
 * Canonical bounded subject id, mirroring `validSubjectID` in
 * `protocols/support/strict.go`: a lower-case identifier that cannot contain an
 * empty, `.`, or `..` path segment.
 */
const SUBJECT_ID_PATTERN = /^[a-z0-9@][a-z0-9@._/+:-]{0,255}$/;

function validSubjectID(value: string): boolean {
  if (!SUBJECT_ID_PATTERN.test(value)) return false;
  return value.split('/').every((segment) => segment !== '' && segment !== '.' && segment !== '..');
}

/** Identity and status: the three fields every entry must carry. */
function parseRequiredFields(raw: Record<string, unknown>, index: number): SupportEntry {
  const id = raw['id'];
  const kind = raw['kind'];
  const status = raw['status'];
  if (typeof id !== 'string' || id === '') fail(`entries[${index}].id must be a non-empty string`);
  if (!validSubjectID(id)) fail(`entries[${index}].id ${JSON.stringify(id)} is not a canonical bounded id`);
  if (typeof kind !== 'string' || !SUPPORT_KINDS.includes(kind as SupportKind)) {
    fail(`entries[${index}].kind ${JSON.stringify(kind)} is outside the closed subject-kind vocabulary`);
  }
  if (typeof status !== 'string' || !SUPPORT_STATUSES.includes(status as SupportStatus)) {
    fail(`entries[${index}].status ${JSON.stringify(status)} is outside the closed status vocabulary`);
  }
  return { id, kind: kind as SupportKind, status: status as SupportStatus };
}

/** The two optional, independent claims. Omitting one makes no claim. */
function applyOptionalClaims(entry: SupportEntry, raw: Record<string, unknown>, index: number): void {
  if ('default' in raw) {
    if (typeof raw['default'] !== 'boolean') fail(`entries[${index}].default must be a boolean`);
    entry.default = raw['default'];
  }
  if ('parity' in raw) {
    if (raw['parity'] !== 'unsupported') fail(`entries[${index}].parity accepts only "unsupported"`);
    entry.parity = 'unsupported';
  }
}

function parseEntry(raw: unknown, index: number): SupportEntry {
  if (!isRecord(raw)) fail(`entries[${index}] must be an object`);
  rejectUnknownFields(raw, ENTRY_FIELDS, `entries[${index}]`);
  const entry = parseRequiredFields(raw, index);
  applyOptionalClaims(entry, raw, index);
  if (entry.status === 'experimental' && entry.default === true) {
    fail(`${entry.kind} ${JSON.stringify(entry.id)} is experimental and cannot also be default-on`);
  }
  return entry;
}

/**
 * Parse the reviewed catalog with the strictness `protocols/support` applies.
 *
 * Rejected: malformed JSON, duplicate object fields, explicit nulls, trailing
 * values, unknown fields on the catalog or an entry, any `protocolVersion`
 * token other than the exact integer `1`, an empty entry list, a non-canonical
 * subject id, an unknown kind, status, or parity, a duplicate `(kind, id)`
 * identity, and an `experimental` entry that still claims to be default-on.
 *
 * This is deliberately strict rather than lenient. The site publishes a public
 * commitment; a catalog it cannot fully understand must fail the build, not
 * render a partial table that reads as complete.
 */
export function parseSupportCatalog(source: string): SupportCatalog {
  let document: Record<string, unknown>;
  let members: ReturnType<typeof parseStrictJsonObject>['members'];
  try {
    ({ value: document, members } = parseStrictJsonObject(source));
  } catch (error) {
    if (error instanceof StrictJsonError) fail(error.message);
    throw error;
  }

  rejectUnknownFields(document, CATALOG_FIELDS, 'catalog');

  // The exact TOKEN, not the parsed number: `1.0` and `1e0` both parse to the
  // JavaScript number 1, and the wire contract pins the integer spelling.
  const version = members.get('protocolVersion');
  if (version === undefined) fail('protocolVersion is required');
  if (version.raw !== String(SUPPORT_PROTOCOL_VERSION)) {
    fail(`protocolVersion token ${version.raw} is not supported (want exact integer token 1)`);
  }

  const rawEntries = document['entries'];
  if (!Array.isArray(rawEntries)) fail('entries must be an array');
  if (rawEntries.length === 0) fail('entries must contain at least one reviewed support declaration');

  const seen = new Set<string>();
  const entries = rawEntries.map((raw, index) => {
    const entry = parseEntry(raw, index);
    // Identity is (kind, id) together: the same id under two kinds classifies
    // two different subjects and is not a duplicate.
    const identity = `${entry.kind} ${entry.id}`;
    if (seen.has(identity)) fail(`duplicate ${entry.kind} entry ${JSON.stringify(entry.id)}`);
    seen.add(identity);
    return entry;
  });

  return { protocolVersion: SUPPORT_PROTOCOL_VERSION, entries };
}

/**
 * The reviewed status of one subject, or `undefined` when the catalog does not
 * classify it. Absence is meaningful: it means "no reviewed public status", not
 * "assume stable".
 */
export function statusOf(catalog: SupportCatalog, subject: SupportSubject): SupportStatus | undefined {
  return catalog.entries.find((entry) => entry.kind === subject.kind && entry.id === subject.id)?.status;
}

/** Entries of one kind, sorted by id, for stable rendering. */
export function entriesOfKind(catalog: SupportCatalog, kind: SupportKind): SupportEntry[] {
  return catalog.entries.filter((entry) => entry.kind === kind).sort((a, b) => a.id.localeCompare(b.id));
}

const STATUS_LABEL: Record<SupportStatus, string> = {
  stable: 'Stable',
  preview: 'Preview',
  experimental: 'Experimental',
};

/** Human label for a reviewed status. */
export function statusLabel(status: SupportStatus): string {
  return STATUS_LABEL[status];
}

const STATUS_MEANING: Record<SupportStatus, string> = {
  stable: 'Carries the normal public support commitment.',
  preview: 'Public and usable, but may change before becoming stable.',
  experimental: 'No compatibility or parity promise, and never default-on.',
};

function claims(entry: SupportEntry): string {
  const notes: string[] = [];
  if (entry.default === false) notes.push('not default');
  if (entry.parity === 'unsupported') notes.push('no cross-implementation parity promise');
  return notes.length > 0 ? notes.join('; ') : '—';
}

function table(entries: SupportEntry[], subjectHeading: string): string[] {
  const lines = [`| ${subjectHeading} | Status | Independent claims |`, '| --- | --- | --- |'];
  for (const entry of entries) {
    lines.push(`| \`${entry.id}\` | ${statusLabel(entry.status)} | ${claims(entry)} |`);
  }
  return lines;
}

/**
 * Render the published `/docs/support` page from the reviewed catalog.
 *
 * The output is a pure function of the catalog bytes: entries are sorted by id
 * within each kind, so the same catalog always produces the same page and the
 * generate step stays cacheable.
 */
export function renderSupportPage(catalog: SupportCatalog): string {
  const packages = entriesOfKind(catalog, 'package');
  const protocols = entriesOfKind(catalog, 'protocol');
  const features = entriesOfKind(catalog, 'feature');

  const lines: string[] = [
    '# Support status',
    '',
    'Support status is a public commitment about a package or a protocol: what',
    'you can depend on today, and how much it may still move. This page is',
    'generated from the reviewed catalog at the root of the Putnami repository,',
    'so it cannot disagree with it.',
    '',
    '> Support status and feature maturity are different concepts. Support status',
    '> is a product commitment about a subject. Feature maturity is an evidence',
    '> ladder about what a repository has actually proven — see',
    '> [Write a feature spec](/docs/how-to/write-a-feature-spec).',
    '',
    '## What each status means',
    '',
    '| Status | Meaning |',
    '| --- | --- |',
    ...SUPPORT_STATUSES.map((status) => `| ${statusLabel(status)} | ${STATUS_MEANING[status]} |`),
    '',
    'Two further claims are recorded independently of the status, and each is',
    'only present when it was reviewed:',
    '',
    '- **not default** — the subject is never part of the default experience.',
    '- **no cross-implementation parity promise** — nothing guarantees that this',
    '  subject behaves like its Go or TypeScript counterpart.',
    '',
    'An omitted claim makes no statement about that axis.',
    '',
    '## Packages',
    '',
    ...table(packages, 'Package'),
    '',
    '## Protocols',
    '',
    'Protocols are the versioned wire contracts every layer implements. Their',
    'identity is the owning Go module path.',
    '',
    ...table(protocols, 'Protocol'),
  ];

  if (features.length > 0) {
    lines.push('', '## Features', '', ...table(features, 'Feature'));
  }

  lines.push(
    '',
    '## What is not on this page',
    '',
    'A subject that does not appear above has **no reviewed public support',
    'status**. The catalog classifies subjects that were reviewed; it does not',
    'infer a status for everything else, and absence is never an implicit',
    'promise.',
    '',
    'Deployed websites are not classified. The support protocol classifies',
    'packages, protocols, and features — things you depend on — not services',
    'Putnami operates.',
    '',
    'Release planning is also separate. Which platforms are release candidates,',
    'how compatibility works before v1.0.0, and the intended license transition',
    'are recorded in',
    '[RELEASE.md](https://github.com/putnami/putnami/blob/main/RELEASE.md).',
    'The license in force is always the checked-in',
    '[LICENSE.md](https://github.com/putnami/putnami/blob/main/LICENSE.md).',
    '',
  );

  return `${lines.join('\n')}`;
}
