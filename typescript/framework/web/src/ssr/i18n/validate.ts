import { join } from 'node:path';
import type { I18nBuild } from './build';
import { type CatalogFile, messageArguments, serializeCatalogFile } from './catalog';

/**
 * The variables an agent host sets for the commands its agent runs: Claude
 * Code and Codex. Validation is a person's statement, so it refuses to run
 * under any of them.
 */
export const AGENT_SESSION_VARIABLES = ['CLAUDECODE', 'CLAUDE_CODE_SESSION_ID', 'CODEX_THREAD_ID'] as const;

/** One translation a person can validate. */
export interface ValidationCandidate {
  locale: string;
  namespace: string;
  key: string;
  state: 'draft' | 'stale';
  /** The current default text. */
  source: string;
  /** The default text the translation was made from, when it changed since. */
  previousSource?: string;
  text: string;
  /** Why it cannot be validated as written; the person corrects it first. */
  defect?: string;
}

/** Every draft and stale translation of the selected locales, in catalog order. */
export function validationCandidates(build: I18nBuild, locale?: string): ValidationCandidate[] {
  const candidates: ValidationCandidate[] = [];
  for (const catalog of build.catalogs) {
    if (locale !== undefined && catalog.locale !== locale) continue;
    for (const [namespace, entries] of Object.entries(catalog.synced)) {
      const defaults = build.declared.get(namespace) ?? {};
      for (const [key, entry] of Object.entries(entries)) {
        if (entry.state !== 'draft' && entry.state !== 'stale') continue;
        const source = defaults[key];
        if (source === undefined || entry.text === '') continue;
        const candidate: ValidationCandidate = {
          locale: catalog.locale,
          namespace,
          key,
          state: entry.state,
          source,
          text: entry.text,
          ...(entry.state === 'stale' && entry.source !== undefined ? { previousSource: entry.source } : {}),
        };
        try {
          const actual = messageArguments(entry.text).join(', ');
          const expected = messageArguments(source).join(', ');
          if (actual !== expected) {
            candidate.defect = `uses ${actual || 'no argument'} where the default text uses ${expected || 'no argument'}`;
          }
        } catch (error) {
          candidate.defect = `not valid ICU MessageFormat: ${(error as Error).message}`;
        }
        candidates.push(candidate);
      }
    }
  }
  return candidates;
}

/**
 * Lock the accepted candidates: each becomes `validated`, made from the
 * current default text, with who validated it and when. Returns the catalog
 * files to write.
 */
export function validateEntries(
  build: I18nBuild,
  accepted: readonly ValidationCandidate[],
  validatedBy: string,
  validatedAt: string,
): { file: string; content: string }[] {
  const changed = new Map<string, { file: string; entries: CatalogFile }>();
  for (const candidate of accepted) {
    const catalog = build.catalogs.find((c) => c.locale === candidate.locale);
    if (!catalog) continue;
    const label = `${candidate.locale}/${candidate.namespace}`;
    let target = changed.get(label);
    if (!target) {
      target = {
        file: join(catalog.dir, `${candidate.namespace}.json`),
        entries: { ...(catalog.synced[candidate.namespace] ?? {}) },
      };
      changed.set(label, target);
    }
    target.entries[candidate.key] = {
      text: candidate.text,
      source: candidate.source,
      state: 'validated',
      validatedBy,
      validatedAt,
    };
  }
  return [...changed.values()].map(({ file, entries }) => ({ file, content: serializeCatalogFile(entries) }));
}
