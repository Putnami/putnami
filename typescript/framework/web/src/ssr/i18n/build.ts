import { Glob } from 'bun';
import { existsSync, readFileSync } from 'node:fs';
import { join, sep } from 'node:path';
import { parseIcu } from '../../shared/i18n/icu';
import {
  isLocalizedPath,
  type LocaleSettings,
  localePrefix,
  parseLocaleSettings,
  splitLocalePath,
} from '../../shared/i18n/locale';
import { definitionText, type LocaleMessages, type Messages } from '../../shared/i18n/messages';
import {
  type Catalog,
  type CatalogDiagnostic,
  checkCatalog,
  type DeclaredMessages,
  parseCatalogFile,
  runtimeMessages,
  type SyncChanges,
  serializeCatalogFile,
  syncCatalog,
} from './catalog';

/** Where a project keeps its catalogs, relative to its root. */
export const CATALOG_DIR = 'locales';
/** Where a project declares its messages, relative to its root. */
export const MESSAGES_GLOB = 'src/**/*.messages.{ts,tsx}';

export interface LocaleCatalogState {
  locale: string;
  /** The locale's catalog directory, `locales/<locale>`, absolute. */
  dir: string;
  synced: Catalog;
  changes: SyncChanges;
  /** The namespace files whose bytes on disk differ from the synced catalog, absolute. */
  writes: { file: string; content: string }[];
  /** The files of namespaces no declaration holds, absolute. */
  deletions: string[];
  /** Whether the directory on disk differs from the synced catalog. */
  outOfSync: boolean;
}

/** The file of one namespace's catalog: `docs/sidebar` is `locales/fr/docs/sidebar.json`. */
export function catalogFile(locale: string, namespace: string): string {
  return `${CATALOG_DIR}/${locale}/${namespace}.json`;
}

export interface I18nBuild {
  settings: LocaleSettings;
  declared: DeclaredMessages;
  /** Declaration files, relative to the project root, by namespace. */
  sources: ReadonlyMap<string, string>;
  catalogs: LocaleCatalogState[];
  /** What the runtime serves, by non-default locale. */
  messages: Record<string, LocaleMessages>;
  diagnostics: CatalogDiagnostic[];
}

function isMessages(value: unknown): value is Messages {
  return (
    value !== null &&
    typeof value === 'object' &&
    typeof (value as Messages).namespace === 'string' &&
    typeof (value as Messages).definitions === 'object'
  );
}

/**
 * Import every `*.messages.ts` file of a project and collect its declarations.
 * A namespace declared twice is an error: its catalog keys would collide.
 */
export async function collectMessages(
  projectRoot: string,
): Promise<{ declared: Map<string, Record<string, string>>; sources: Map<string, string> }> {
  const declared = new Map<string, Record<string, string>>();
  const sources = new Map<string, string>();
  const files = [...new Glob(MESSAGES_GLOB).scanSync({ cwd: projectRoot })].sort();
  for (const file of files) {
    const module = (await import(join(projectRoot, file))) as Record<string, unknown>;
    for (const value of Object.values(module)) {
      if (!isMessages(value)) continue;
      const previous = sources.get(value.namespace);
      if (previous && previous !== file) {
        throw new Error(`messages namespace '${value.namespace}' is declared in both ${previous} and ${file}`);
      }
      if (previous) continue;
      const texts: Record<string, string> = {};
      for (const [key, definition] of Object.entries(value.definitions)) {
        texts[key] = definitionText(definition);
      }
      declared.set(value.namespace, texts);
      sources.set(value.namespace, file);
    }
  }
  return { declared, sources };
}

/** Read a locale's directory: one file per namespace, named after it. */
function readCatalog(projectRoot: string, locale: string): { catalog: Catalog; raw: Map<string, string> } {
  const dir = join(projectRoot, CATALOG_DIR, locale);
  const catalog: Catalog = {};
  const raw = new Map<string, string>();
  if (!existsSync(dir)) return { catalog, raw };
  for (const path of [...new Glob('**/*.json').scanSync({ cwd: dir })].sort()) {
    const namespace = path.slice(0, -'.json'.length).split(sep).join('/');
    const label = catalogFile(locale, namespace);
    const text = readFileSync(join(dir, path), 'utf8');
    let value: unknown;
    try {
      value = JSON.parse(text);
    } catch (error) {
      throw new Error(`${label}: not valid JSON: ${(error as Error).message}`);
    }
    catalog[namespace] = parseCatalogFile(label, value);
    raw.set(namespace, text);
  }
  return { catalog, raw };
}

/**
 * Resolve a project's localization: settings, declared messages, each
 * non-default locale's catalog synced in memory, and the check of all of them.
 * Writes nothing; `putnami i18n sync` writes the synced catalogs.
 */
export async function resolveI18nBuild(projectRoot: string, rawSettings: unknown, strict: boolean): Promise<I18nBuild> {
  const settings = parseLocaleSettings(rawSettings);
  const { declared, sources } = await collectMessages(projectRoot);
  const diagnostics: CatalogDiagnostic[] = [];
  const catalogs: LocaleCatalogState[] = [];
  const messages: Record<string, LocaleMessages> = {};

  for (const locale of settings.locales) {
    if (locale === settings.defaultLocale) continue;
    const dir = join(projectRoot, CATALOG_DIR, locale);
    const { catalog, raw } = readCatalog(projectRoot, locale);
    const { catalog: synced, changes } = syncCatalog(declared, catalog);
    const writes: LocaleCatalogState['writes'] = [];
    for (const [namespace, entries] of Object.entries(synced)) {
      const content = serializeCatalogFile(entries);
      if (raw.get(namespace) !== content) writes.push({ file: join(dir, `${namespace}.json`), content });
    }
    const deletions = [...raw.keys()].filter((ns) => !Object.hasOwn(synced, ns)).map((ns) => join(dir, `${ns}.json`));
    catalogs.push({
      locale,
      dir,
      synced,
      changes,
      writes,
      deletions,
      outOfSync: writes.length + deletions.length > 0,
    });
    diagnostics.push(...checkCatalog(declared, locale, synced, changes, strict));
    messages[locale] = runtimeMessages(synced);
  }

  // A default text the parser refuses would fail every locale the same way;
  // report it once, even for a project with a single locale.
  if (catalogs.length === 0) {
    for (const [namespace, defaults] of declared) {
      for (const [key, text] of Object.entries(defaults)) {
        try {
          parseIcu(text);
        } catch (error) {
          diagnostics.push({
            level: 'error',
            code: 'invalid-icu',
            locale: 'default',
            key: `${namespace}.${key}`,
            message: `default text is not valid ICU MessageFormat: ${(error as Error).message}`,
          });
        }
      }
    }
  }

  return { settings, declared, sources, catalogs, messages, diagnostics: dedupe(diagnostics) };
}

function dedupe(diagnostics: CatalogDiagnostic[]): CatalogDiagnostic[] {
  const seen = new Set<string>();
  return diagnostics.filter((d) => {
    const id = `${d.level}:${d.code}:${d.locale}:${d.key}`;
    if (seen.has(id)) return false;
    seen.add(id);
    return true;
  });
}

export function formatDiagnostic(d: CatalogDiagnostic): string {
  if (d.locale === 'default') return `${d.key}: ${d.message}`;
  // A key is an identifier, so the last dot ends the namespace.
  const dot = d.key.lastIndexOf('.');
  return `${catalogFile(d.locale, d.key.slice(0, dot))} ${d.key.slice(dot + 1)}: ${d.message}`;
}

/**
 * Add one row per other locale to each localized route row: the manifest and
 * the HTTP route inventory list what is served, and `/fr/pricing` is. A
 * catch-all's expanded paths go to the row of their own locale.
 */
export function localizeRows<T extends { route: string; expandedPaths?: string[] }>(
  settings: LocaleSettings | undefined,
  rows: T[],
): T[] {
  if (!settings) return rows;
  const ofLocale = (paths: string[] | undefined, locale: string) =>
    paths?.filter((p) => splitLocalePath(settings, p).locale === locale);
  return rows.flatMap((row) => {
    if (!isLocalizedPath(settings, row.route)) return [row];
    return settings.locales.map((locale) => {
      const prefix = localePrefix(settings, locale);
      const route = prefix ? (row.route === '/' ? prefix : `${prefix}${row.route}`) : row.route;
      const expandedPaths = ofLocale(row.expandedPaths, locale);
      return { ...row, route, ...(expandedPaths ? { expandedPaths } : {}) };
    });
  });
}
