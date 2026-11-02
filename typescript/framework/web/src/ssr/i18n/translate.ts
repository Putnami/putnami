import { join } from 'node:path';
import { IcuSyntaxError } from '../../shared/i18n/icu';
import type { I18nBuild } from './build';
import { type CatalogFile, messageArguments, serializeCatalogFile } from './catalog';
import type { Glossary } from './glossary';

/** Where `putnami i18n translate` writes its work file, relative to the project root. */
export const WORK_FILE = '.putnami/i18n/translate.json';

/** One entry to translate. The agent replaces `text: null` with its translation. */
export interface WorkEntry {
  /** The default text to translate. */
  source: string;
  text: string | null;
  /** A stale entry's old default text and the translation made from it. */
  previous?: { source: string; text: string };
}

export interface WorkNamespace {
  /** The file that declares the namespace's messages, relative to the project root. */
  file: string;
  entries: Record<string, WorkEntry>;
  /** The namespace's other translated messages, to keep their words and tone. */
  context: Record<string, { source: string; text: string }>;
  /** The locale's glossary terms that the entries' default texts contain. */
  glossary: Glossary;
}

/** The work file: for each locale, the namespaces that have entries to translate. */
export interface TranslationWork {
  protocolVersion: 1;
  locales: Record<string, Record<string, WorkNamespace>>;
}

/** Whether a default text contains a glossary term, as a whole word, ignoring case. */
function containsTerm(text: string, term: string): boolean {
  const escaped = term.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
  return new RegExp(`(^|[^\\p{L}\\p{N}])${escaped}($|[^\\p{L}\\p{N}])`, 'iu').test(text);
}

/**
 * Collect every missing and stale entry of each published locale, with what a
 * translator needs: the default text, a stale entry's previous source and
 * text, the namespace's other translations and the glossary terms in play.
 * Validated and draft entries are not asked for.
 */
export function translationWork(build: I18nBuild, glossaries: Readonly<Record<string, Glossary>>): TranslationWork {
  const locales: TranslationWork['locales'] = {};
  for (const catalog of build.catalogs) {
    const glossary = glossaries[catalog.locale] ?? {};
    const namespaces: Record<string, WorkNamespace> = {};
    for (const [namespace, defaults] of build.declared) {
      const synced = catalog.synced[namespace] ?? {};
      const entries: Record<string, WorkEntry> = {};
      const context: WorkNamespace['context'] = {};
      for (const [key, source] of Object.entries(defaults)) {
        const entry = synced[key];
        if (!entry || entry.state === 'missing') {
          entries[key] = { source, text: null };
        } else if (entry.state === 'stale') {
          entries[key] = { source, text: null, previous: { source: entry.source ?? '', text: entry.text } };
        } else {
          context[key] = { source, text: entry.text };
        }
      }
      if (Object.keys(entries).length === 0) continue;
      const sources = Object.values(entries).map((e) => e.source);
      const terms: Glossary = {};
      for (const [term, definition] of Object.entries(glossary)) {
        if (sources.some((text) => containsTerm(text, term))) terms[term] = definition;
      }
      namespaces[namespace] = { file: build.sources.get(namespace) ?? '', entries, context, glossary: terms };
    }
    if (Object.keys(namespaces).length > 0) locales[catalog.locale] = namespaces;
  }
  return { protocolVersion: 1, locales };
}

/** How many entries a work file asks for. */
export function countWork(work: TranslationWork): number {
  let count = 0;
  for (const namespaces of Object.values(work.locales)) {
    for (const ns of Object.values(namespaces)) count += Object.keys(ns.entries).length;
  }
  return count;
}

/**
 * Keep the answers of a previous work file in a new one: an entry whose default
 * text is unchanged keeps its non-empty text, so rewriting the work file after
 * one default text changed costs only that entry. Returns how many it kept.
 */
export function carryOverAnswers(work: TranslationWork, previous: unknown): number {
  if (!isObject(previous) || previous['protocolVersion'] !== 1 || !isObject(previous['locales'])) return 0;
  let kept = 0;
  for (const [locale, namespaces] of Object.entries(work.locales)) {
    const before = previous['locales'][locale];
    if (!isObject(before)) continue;
    for (const [namespace, group] of Object.entries(namespaces)) {
      const old = before[namespace];
      const entries = isObject(old) && isObject(old['entries']) ? old['entries'] : {};
      for (const [key, entry] of Object.entries(group.entries)) {
        const answer = entries[key];
        if (!isObject(answer) || answer['source'] !== entry.source) continue;
        const text = answer['text'];
        if (typeof text === 'string' && text.trim() !== '') {
          entry.text = text;
          kept++;
        }
      }
    }
  }
  return kept;
}

export interface AppliedWork {
  /** The catalog files to write, absolute, when no answer was refused. */
  writes: { file: string; content: string }[];
  /** Each refused answer, as `locales/fr/home.json title: reason`. */
  errors: string[];
  applied: number;
}

const isObject = (v: unknown): v is Record<string, unknown> => v !== null && typeof v === 'object' && !Array.isArray(v);

/**
 * Check a filled work file against the current messages and catalogs, and
 * turn its answers into draft entries. One refused answer refuses the file:
 * `writes` is then empty. An entry still `null` is skipped.
 */
export function applyTranslationWork(build: I18nBuild, work: unknown): AppliedWork {
  const errors: string[] = [];
  if (!isObject(work) || work['protocolVersion'] !== 1 || !isObject(work['locales'])) {
    return {
      writes: [],
      errors: [`${WORK_FILE}: not a version 1 work file; run \`putnami i18n translate\``],
      applied: 0,
    };
  }
  const changed = new Map<string, { file: string; entries: CatalogFile }>();
  let applied = 0;
  for (const [locale, namespaces] of Object.entries(work['locales'])) {
    const catalog = build.catalogs.find((c) => c.locale === locale);
    if (!catalog) {
      errors.push(`${locale}: not a published locale other than the default one`);
      continue;
    }
    if (!isObject(namespaces)) {
      errors.push(`${locale}: must be an object of namespaces`);
      continue;
    }
    for (const [namespace, group] of Object.entries(namespaces)) {
      const defaults = build.declared.get(namespace);
      const label = `locales/${locale}/${namespace}.json`;
      if (!defaults) {
        errors.push(`${label}: no messages declaration holds this namespace`);
        continue;
      }
      if (!isObject(group) || !isObject(group['entries'])) {
        errors.push(`${label}: must hold an object of entries`);
        continue;
      }
      for (const [key, answer] of Object.entries(group['entries'])) {
        const where = `${label} ${key}`;
        if (!isObject(answer)) {
          errors.push(`${where}: an answer is an object with source and text`);
          continue;
        }
        const text = answer['text'];
        if (text === null) continue;
        const source = defaults[key];
        if (source === undefined) {
          errors.push(`${where}: no message has this key`);
          continue;
        }
        const current = catalog.synced[namespace]?.[key];
        if (current?.state === 'validated') {
          errors.push(`${where}: a person validated this translation; an agent never rewrites it`);
          continue;
        }
        if (answer['source'] !== source) {
          errors.push(
            `${where}: the default text changed after the work file was written; run \`putnami i18n translate\` again: it keeps the other answers`,
          );
          continue;
        }
        if (typeof text !== 'string' || text.trim() === '') {
          errors.push(`${where}: the translation must be a non-empty string, or null to skip it`);
          continue;
        }
        let actual: string[];
        try {
          actual = messageArguments(text);
        } catch (error) {
          const detail = error instanceof IcuSyntaxError ? error.message : String(error);
          errors.push(`${where}: not valid ICU MessageFormat: ${detail}`);
          continue;
        }
        const expected = messageArguments(source);
        if (actual.join(',') !== expected.join(',')) {
          errors.push(
            `${where}: uses ${actual.join(', ') || 'no argument'} where the default text uses ${expected.join(', ') || 'no argument'}`,
          );
          continue;
        }
        let target = changed.get(label);
        if (!target) {
          target = { file: join(catalog.dir, `${namespace}.json`), entries: { ...(catalog.synced[namespace] ?? {}) } };
          changed.set(label, target);
        }
        target.entries[key] = { text, source, state: 'draft' };
        applied++;
      }
    }
  }
  if (errors.length > 0) return { writes: [], errors, applied: 0 };
  const writes = [...changed.values()].map(({ file, entries }) => ({ file, content: serializeCatalogFile(entries) }));
  return { writes, errors, applied };
}
