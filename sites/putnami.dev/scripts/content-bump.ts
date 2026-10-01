#!/usr/bin/env bun
/**
 * `content bump` — advance the site-content lock (`content.lock.json`).
 *
 * The ONLY channel-resolution point of the content pipeline: resolves each
 * lock entry's `package@channelHint` against its registry, computes the blob's
 * sha256 digest from the fetched bytes, rewrites the lock, and primes the
 * digest-keyed blob cache so the next generate is offline-safe. Committing the
 * lock diff is what changes the site — generate itself only ever fetches by
 * digest (a determinism constraint).
 *
 * Usage:
 *   bun run scripts/content-bump.ts [--bundle <name>]...
 *
 * A `putnami content bump` CLI verb is deliberately deferred until a second
 * site consumes bundles.
 */
import { mkdirSync, renameSync, writeFileSync } from 'node:fs';
import { joinPath } from '@putnami/utils';
import { bumpContentLock } from '../src/lib/content/bump';
import { formatContentLock, readContentLock } from '../src/lib/content/lock';
import { resolveChannelFromRegistry } from '../src/lib/content/registry';

const args = process.argv.slice(2);
const only: string[] = [];
for (let i = 0; i < args.length; i++) {
  if (args[i] === '--bundle' && i + 1 < args.length) {
    only.push(args[i + 1]);
    i++;
  }
}

const projectRoot = joinPath(import.meta.dir, '..');
const workspaceRoot = joinPath(projectRoot, '..', '..');
const lockPath = joinPath(projectRoot, 'content.lock.json');
const cacheDir = joinPath(workspaceRoot, '.putnami', 'site-content', 'blobs');

// CLI progress goes through stdout directly: the repo lints with
// noConsole:error, and `lint --fix` strips console.* calls outright.
const report = (message: string) => process.stdout.write(`${message}\n`);

const lock = readContentLock(lockPath);
if (lock.bundles.length === 0) {
  report('content bump: content.lock.json pins no bundles — nothing to resolve.');
  process.exit(0);
}

const result = await bumpContentLock(
  lock,
  (bundle) => resolveChannelFromRegistry(bundle.package, bundle.channelHint, bundle.registry),
  only.length > 0 ? only : undefined,
);

if (result.changes.length === 0) {
  report('content bump: every bundle is already at its channel head.');
  process.exit(0);
}

// Prime the digest-keyed cache so the next generate needs no network.
mkdirSync(cacheDir, { recursive: true });
for (const change of result.changes) {
  const tmp = joinPath(cacheDir, `.${change.toDigest}.tmp-${process.pid}`);
  writeFileSync(tmp, change.blob);
  renameSync(tmp, joinPath(cacheDir, change.toDigest));
}

writeFileSync(lockPath, formatContentLock(result.lock));
for (const change of result.changes) {
  report(
    `content bump: ${change.name} ${change.fromVersion} (${change.fromDigest.slice(0, 12)}…) → ` +
      `${change.toVersion} (${change.toDigest.slice(0, 12)}…)`,
  );
}
report(`content bump: rewrote ${lockPath} — review and commit the diff.`);
