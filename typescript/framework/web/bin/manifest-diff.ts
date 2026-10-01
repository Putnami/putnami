#!/usr/bin/env bun
/**
 * @putnami/web:manifest-diff CLI command
 *
 * Compare two web rendering manifests and report route-level changes
 * (mode, hydration, client-JS budget). Exits non-zero when anything changed,
 * so CI can gate on unreviewed rendering shifts.
 *
 * Usage:
 *   bunx putnami-web-manifest-diff <baseline.json> <current.json>
 */

import { file } from 'bun';
import { diffManifests, formatManifestDiff, hasManifestChanges, type WebManifest } from '../src/ssr/manifest';

async function readManifest(path: string): Promise<WebManifest> {
  return JSON.parse(await file(path).text()) as WebManifest;
}

async function main(): Promise<void> {
  const [prevPath, nextPath] = process.argv.slice(2);
  if (!prevPath || !nextPath) {
    // biome-ignore lint/suspicious/noConsole: CLI usage output
    console.error('Usage: putnami-web-manifest-diff <baseline.json> <current.json>');
    process.exit(2);
  }

  const [prev, next] = await Promise.all([readManifest(prevPath), readManifest(nextPath)]);
  const diff = diffManifests(prev, next);
  // biome-ignore lint/suspicious/noConsole: CLI report output
  console.log(formatManifestDiff(diff));
  process.exit(hasManifestChanges(diff) ? 1 : 0);
}

if (import.meta.main) {
  void main();
}
