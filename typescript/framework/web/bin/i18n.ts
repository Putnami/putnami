#!/usr/bin/env bun
/**
 * `putnami i18n sync`: bring a localized project's translation catalogs in
 * step with its messages.
 *
 * Adds an entry for each new message, removes the entries of deleted ones, and
 * marks stale every translation whose default text changed, keeping its text
 * for the translator. Writes `locales/<locale>.json` for each non-default
 * locale whose file differs, and nothing else; the build only reads them.
 *
 * Usage: bun run bin/i18n.ts sync   (from the project root)
 */

import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, relative } from 'node:path';
import { readProjectConfigFile } from '@putnami/utils';
import { formatDiagnostic, resolveI18nBuild } from '../src/ssr/i18n/build';
import { serializeCatalog } from '../src/ssr/i18n/catalog';

export async function sync(projectRoot: string, log: (line: string) => void): Promise<number> {
  const options = readProjectConfigFile(projectRoot)?.options?.['@putnami/web:generate'] as
    | { i18n?: unknown }
    | undefined;
  if (options?.i18n === undefined) {
    log('No i18n block in options["@putnami/web:generate"]; nothing to sync');
    return 0;
  }
  const build = await resolveI18nBuild(projectRoot, options.i18n, false);
  for (const catalog of build.catalogs) {
    const file = relative(projectRoot, catalog.file);
    const { added, removed, staled } = catalog.changes;
    if (catalog.outOfSync) {
      mkdirSync(dirname(catalog.file), { recursive: true });
      writeFileSync(catalog.file, serializeCatalog(catalog.synced));
    }
    log(
      `${file}: ${catalog.outOfSync ? 'written' : 'up to date'} (${added.length} added, ${removed.length} removed, ${staled.length} stale)`,
    );
    for (const key of staled) log(`  stale ${key}: the default text changed; review the translation`);
  }
  const errors = build.diagnostics.filter((d) => d.level === 'error' && d.code !== 'missing');
  for (const error of errors) log(`error ${formatDiagnostic(error)}`);
  const missing = build.diagnostics.filter((d) => d.code === 'missing').length;
  if (missing > 0) log(`${missing} message(s) have no translation yet`);
  return errors.length > 0 ? 1 : 0;
}

if (import.meta.main) {
  const [verb] = process.argv.slice(2);
  if (verb !== 'sync') {
    // biome-ignore lint/suspicious/noConsole: CLI usage output
    console.error('Usage: putnami i18n sync');
    process.exit(2);
  }
  // biome-ignore lint/suspicious/noConsole: CLI report output
  process.exit(await sync(process.cwd(), (line) => console.log(line)));
}
