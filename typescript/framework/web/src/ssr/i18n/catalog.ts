import { createHash } from 'node:crypto';
import { icuArguments, IcuSyntaxError, parseIcu } from '../../shared/i18n/icu';
import type { LocaleMessages } from '../../shared/i18n/messages';

/**
 * Where a translation stands:
 * - `missing`: nobody translated the key yet; its text is empty.
 * - `draft`: an agent or a contributor wrote it; nobody validated it.
 * - `validated`: a human locked it; agents never rewrite it.
 * - `stale`: its default text changed after it was written; the old text is
 *   still served until someone updates it.
 */
export type CatalogState = 'missing' | 'draft' | 'validated' | 'stale';

export interface CatalogEntry {
  text: string;
  /** Hash of the default text this translation was made from. */
  source?: string;
  state: CatalogState;
  validatedBy?: string;
  validatedAt?: string;
}

/** One locale's catalog file: namespace, then key, then entry. */
export type Catalog = Record<string, Record<string, CatalogEntry>>;

/** The default texts the code declares: namespace, then key, then ICU text. */
export type DeclaredMessages = ReadonlyMap<string, Readonly<Record<string, string>>>;

const STATES = new Set<CatalogState>(['missing', 'draft', 'validated', 'stale']);

export function sourceHash(text: string): string {
  return createHash('sha256').update(text, 'utf8').digest('hex').slice(0, 16);
}

export class CatalogFormatError extends Error {
  constructor(file: string, detail: string) {
    super(`${file}: ${detail}`);
    this.name = 'CatalogFormatError';
  }
}

/** Parse a catalog file's JSON value, refusing any shape the sync would not write. */
export function parseCatalog(file: string, value: unknown): Catalog {
  const isObject = (v: unknown): v is Record<string, unknown> =>
    v !== null && typeof v === 'object' && !Array.isArray(v);
  if (!isObject(value)) throw new CatalogFormatError(file, 'a catalog is a JSON object of namespaces');
  const catalog: Catalog = {};
  for (const [namespace, keys] of Object.entries(value)) {
    if (namespace === '$schema') continue;
    if (!isObject(keys)) throw new CatalogFormatError(file, `namespace '${namespace}' must be an object of keys`);
    const entries: Record<string, CatalogEntry> = {};
    for (const [key, raw] of Object.entries(keys)) {
      const where = `${namespace}.${key}`;
      if (!isObject(raw)) throw new CatalogFormatError(file, `${where} must be an object`);
      const { text, source, state, validatedBy, validatedAt, ...rest } = raw;
      const unknownKey = Object.keys(rest)[0];
      if (unknownKey) throw new CatalogFormatError(file, `${where} has an unknown field '${unknownKey}'`);
      if (typeof text !== 'string') throw new CatalogFormatError(file, `${where}.text must be a string`);
      if (source !== undefined && typeof source !== 'string') {
        throw new CatalogFormatError(file, `${where}.source must be a string`);
      }
      if (typeof state !== 'string' || !STATES.has(state as CatalogState)) {
        throw new CatalogFormatError(file, `${where}.state must be one of ${[...STATES].join(', ')}`);
      }
      for (const [name, field] of [
        ['validatedBy', validatedBy],
        ['validatedAt', validatedAt],
      ] as const) {
        if (field !== undefined && typeof field !== 'string') {
          throw new CatalogFormatError(file, `${where}.${name} must be a string`);
        }
      }
      entries[key] = {
        text,
        ...(source !== undefined ? { source: source as string } : {}),
        state: state as CatalogState,
        ...(validatedBy !== undefined ? { validatedBy: validatedBy as string } : {}),
        ...(validatedAt !== undefined ? { validatedAt: validatedAt as string } : {}),
      };
    }
    catalog[namespace] = entries;
  }
  return catalog;
}

export interface SyncChanges {
  added: string[];
  removed: string[];
  staled: string[];
}

/**
 * Align a catalog with the declared messages. Adds a `missing` entry for each
 * new key, drops keys no declaration holds, fills an absent source hash, and
 * marks a translation `stale` when its default text changed; it never edits a
 * translation's text. The result is sorted, so the same inputs write the same
 * bytes.
 */
export function syncCatalog(declared: DeclaredMessages, current: Catalog): { catalog: Catalog; changes: SyncChanges } {
  const changes: SyncChanges = { added: [], removed: [], staled: [] };
  const catalog: Catalog = {};
  for (const namespace of [...declared.keys()].sort()) {
    const defaults = declared.get(namespace) ?? {};
    const existing = current[namespace] ?? {};
    const entries: Record<string, CatalogEntry> = {};
    for (const key of Object.keys(defaults).sort()) {
      const hash = sourceHash(defaults[key] as string);
      const entry = existing[key];
      if (!entry) {
        entries[key] = { text: '', source: hash, state: 'missing' };
        changes.added.push(`${namespace}.${key}`);
        continue;
      }
      const next: CatalogEntry = { ...entry };
      if (next.text === '') {
        next.state = 'missing';
        next.source = hash;
      } else if (next.source === undefined) {
        next.source = hash;
        if (next.state === 'missing') next.state = 'draft';
      } else if (next.source !== hash && next.state !== 'stale') {
        next.state = 'stale';
        changes.staled.push(`${namespace}.${key}`);
      } else if (next.state === 'missing') {
        next.state = 'draft';
      }
      entries[key] = orderEntry(next);
    }
    for (const key of Object.keys(existing)) {
      if (!Object.hasOwn(defaults, key)) changes.removed.push(`${namespace}.${key}`);
    }
    catalog[namespace] = entries;
  }
  for (const namespace of Object.keys(current)) {
    if (!declared.has(namespace)) {
      for (const key of Object.keys(current[namespace] ?? {})) changes.removed.push(`${namespace}.${key}`);
    }
  }
  return { catalog, changes };
}

function orderEntry(entry: CatalogEntry): CatalogEntry {
  return {
    text: entry.text,
    ...(entry.source !== undefined ? { source: entry.source } : {}),
    state: entry.state,
    ...(entry.validatedBy !== undefined ? { validatedBy: entry.validatedBy } : {}),
    ...(entry.validatedAt !== undefined ? { validatedAt: entry.validatedAt } : {}),
  };
}

export function serializeCatalog(catalog: Catalog): string {
  return `${JSON.stringify(catalog, null, 2)}\n`;
}

export interface CatalogDiagnostic {
  level: 'error' | 'warning';
  code: 'missing' | 'invalid-icu' | 'argument-mismatch' | 'stale' | 'orphan';
  locale: string;
  key: string;
  message: string;
}

function argumentNames(text: string): string[] {
  return [...icuArguments(parseIcu(text)).keys()].sort();
}

/**
 * Check declared messages and one synced catalog. `strict` is the production
 * build: a missing translation is an error there and a warning while serving
 * or testing, where the default text stands in.
 */
export function checkCatalog(
  declared: DeclaredMessages,
  locale: string,
  synced: Catalog,
  changes: SyncChanges,
  strict: boolean,
): CatalogDiagnostic[] {
  const diagnostics: CatalogDiagnostic[] = [];
  for (const [namespace, defaults] of declared) {
    for (const [key, defaultText] of Object.entries(defaults)) {
      const id = `${namespace}.${key}`;
      let expected: string[];
      try {
        expected = argumentNames(defaultText);
      } catch (error) {
        diagnostics.push({
          level: 'error',
          code: 'invalid-icu',
          locale: 'default',
          key: id,
          message: `default text is not valid ICU MessageFormat: ${(error as Error).message}`,
        });
        continue;
      }
      const entry = synced[namespace]?.[key];
      if (!entry || entry.state === 'missing') {
        diagnostics.push({
          level: strict ? 'error' : 'warning',
          code: 'missing',
          locale,
          key: id,
          message: strict ? 'no translation' : 'no translation; the default text stands in',
        });
        continue;
      }
      let actual: string[];
      try {
        actual = argumentNames(entry.text);
      } catch (error) {
        const detail = error instanceof IcuSyntaxError ? error.message : String(error);
        diagnostics.push({
          level: 'error',
          code: 'invalid-icu',
          locale,
          key: id,
          message: `not valid ICU MessageFormat: ${detail}`,
        });
        continue;
      }
      if (actual.join(',') !== expected.join(',')) {
        diagnostics.push({
          level: 'error',
          code: 'argument-mismatch',
          locale,
          key: id,
          message: `uses {${actual.join('}, {')}} where the default text uses {${expected.join('}, {')}}`,
        });
      }
      if (entry.state === 'stale') {
        diagnostics.push({
          level: 'warning',
          code: 'stale',
          locale,
          key: id,
          message: 'the default text changed after this translation was written',
        });
      }
    }
  }
  for (const id of changes.removed) {
    diagnostics.push({
      level: 'warning',
      code: 'orphan',
      locale,
      key: id,
      message: 'no messages declaration holds this key; `putnami i18n sync` removes it',
    });
  }
  return diagnostics;
}

/** The text a runtime serves: every entry with a translation, by namespace. */
export function runtimeMessages(catalog: Catalog): LocaleMessages {
  const out: Record<string, Record<string, string>> = {};
  for (const [namespace, entries] of Object.entries(catalog)) {
    const texts: Record<string, string> = {};
    for (const [key, entry] of Object.entries(entries)) {
      if (entry.state !== 'missing' && entry.text !== '') texts[key] = entry.text;
    }
    if (Object.keys(texts).length > 0) out[namespace] = texts;
  }
  return out;
}
