#!/usr/bin/env bun
/**
 * Standalone CLI for building the search index.
 *
 * Usage:
 *   bun run scripts/build-search-index.ts [--docs-root <path>] [--output-dir <path>]
 *
 * Defaults:
 *   --docs-root    doc/              (source markdown files)
 *   --output-dir   .gen/public/search/    (output directory for index.json)
 *
 * The default stays out of the source `public/` folder: the staticFiles plugin
 * would otherwise declare `/search/` routes from it on the next build.
 */
import { mkdirSync, statSync, writeFileSync } from 'node:fs';
import { joinPath } from '@putnami/utils';
import { buildSearchIndex } from '../src/lib/search/build-index';

const args = process.argv.slice(2);
function getArg(name: string, fallback: string): string {
  const idx = args.indexOf(name);
  if (idx !== -1 && idx + 1 < args.length) {
    return args[idx + 1];
  }
  return fallback;
}

const projectRoot = joinPath(import.meta.dir, '..');
const docsRoot = joinPath(projectRoot, getArg('--docs-root', 'doc'));
const outputDir = joinPath(projectRoot, getArg('--output-dir', '.gen/public/search'));

const index = buildSearchIndex(docsRoot);

mkdirSync(outputDir, { recursive: true });
const outputPath = joinPath(outputDir, 'index.json');
writeFileSync(outputPath, JSON.stringify(index));

const sizeBytes = statSync(outputPath).size;
const _sizeKB = (sizeBytes / 1024).toFixed(1);
