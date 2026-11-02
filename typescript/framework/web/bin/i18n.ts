#!/usr/bin/env bun
/**
 * `putnami i18n sync`: bring a localized project's translation catalogs in
 * step with its messages.
 *
 * Adds an entry for each new message, removes the entries of deleted ones, and
 * marks stale every translation whose default text changed, keeping its text
 * for the translator. Each namespace has its own file,
 * `locales/<locale>/<namespace>.json`: sync writes the files that differ,
 * deletes the files of namespaces no declaration holds, and touches nothing
 * else; the build only reads them.
 *
 * Usage: bun run bin/i18n.ts sync   (from the project root)
 */

import { mkdirSync, readdirSync, rmdirSync, rmSync, writeFileSync } from 'node:fs';
import { dirname, relative } from 'node:path';
import { readProjectConfigFile } from '@putnami/utils';
import { formatDiagnostic, resolveI18nBuild } from '../src/ssr/i18n/build';

/** Remove the directories a deletion emptied, up to the locale's directory. */
function pruneEmpty(dir: string, stop: string): void {
  for (let current = dir; current !== stop && current.startsWith(stop); current = dirname(current)) {
    if (readdirSync(current).length > 0) return;
    rmdirSync(current);
  }
}

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
    const { added, removed, staled } = catalog.changes;
    log(
      `${relative(projectRoot, catalog.dir)}: ${catalog.outOfSync ? 'synced' : 'up to date'} (${added.length} added, ${removed.length} removed, ${staled.length} stale)`,
    );
    for (const { file, content } of catalog.writes) {
      mkdirSync(dirname(file), { recursive: true });
      writeFileSync(file, content);
      log(`  wrote ${relative(projectRoot, file)}`);
    }
    for (const file of catalog.deletions) {
      rmSync(file);
      pruneEmpty(dirname(file), catalog.dir);
      log(`  deleted ${relative(projectRoot, file)}: no messages declaration holds this namespace`);
    }
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
