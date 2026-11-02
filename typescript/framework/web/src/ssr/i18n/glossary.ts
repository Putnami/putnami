import { existsSync, readFileSync } from 'node:fs';
import { join } from 'node:path';
import { CatalogFormatError } from './catalog';

/** The schema a glossary file names, published by putnami.dev from `protocols/i18n`. */
export const GLOSSARY_SCHEMA = 'https://putnami.dev/schemas/i18n/glossary.json';

export interface GlossaryTerm {
  /** The term in the locale; `null` keeps it as written, like a product name. */
  translation: string | null;
  /** What a translator needs to know: the meaning, the register, a form to avoid. */
  note?: string;
}

/** One locale's terms, by default-locale term. */
export type Glossary = Record<string, GlossaryTerm>;

/** A locale's glossary, next to its catalog directory: `locales/fr.glossary.json`. */
export function glossaryFile(locale: string): string {
  return `locales/${locale}.glossary.json`;
}

/** Parse a glossary file, refusing any shape its schema refuses. */
export function parseGlossaryFile(file: string, value: unknown): Glossary {
  const isObject = (v: unknown): v is Record<string, unknown> =>
    v !== null && typeof v === 'object' && !Array.isArray(v);
  if (!isObject(value)) throw new CatalogFormatError(file, 'a glossary is a JSON object of terms');
  const glossary: Glossary = {};
  for (const [term, raw] of Object.entries(value)) {
    if (term === '$schema') continue;
    if (!isObject(raw)) throw new CatalogFormatError(file, `${term} must be an object`);
    const { translation, note, ...rest } = raw;
    const unknownKey = Object.keys(rest)[0];
    if (unknownKey) throw new CatalogFormatError(file, `${term} has an unknown field '${unknownKey}'`);
    if (translation !== null && (typeof translation !== 'string' || translation === '')) {
      throw new CatalogFormatError(file, `${term}.translation must be a non-empty string, or null to keep the term`);
    }
    if (note !== undefined && (typeof note !== 'string' || note === '')) {
      throw new CatalogFormatError(file, `${term}.note must be a non-empty string`);
    }
    glossary[term] = { translation, ...(note !== undefined ? { note } : {}) };
  }
  return glossary;
}

/** Read a locale's glossary; a project without one has no terms. */
export function readGlossary(projectRoot: string, locale: string): Glossary {
  const label = glossaryFile(locale);
  const path = join(projectRoot, label);
  if (!existsSync(path)) return {};
  let value: unknown;
  try {
    value = JSON.parse(readFileSync(path, 'utf8'));
  } catch (error) {
    throw new CatalogFormatError(label, `not valid JSON: ${(error as Error).message}`);
  }
  return parseGlossaryFile(label, value);
}
