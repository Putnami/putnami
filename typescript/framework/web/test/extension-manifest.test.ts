import { describe, expect } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { extname, join } from 'node:path';
import { specTest } from '@putnami/runtime/spectest';

const feature = 'typescript/web-localization';
const projectRoot = join(import.meta.dir, '..');
const placeholder = '{extensionRoot}/';

type ManifestCommand = { command?: string; args?: string[] };
type Manifest = { tasks?: Record<string, ManifestCommand>; hooks?: Record<string, ManifestCommand> };

/** Every path a task or hook of the manifest names under the extension root, with its owner. */
function namedEntries(manifest: Manifest): { owner: string; entry: string }[] {
  const owners: [string, ManifestCommand][] = [
    ...Object.entries(manifest.tasks ?? {}).map(([name, task]): [string, ManifestCommand] => [`task ${name}`, task]),
    ...Object.entries(manifest.hooks ?? {}).map(([name, hook]): [string, ManifestCommand] => [`hook ${name}`, hook]),
  ];
  return owners.flatMap(([owner, { command, args }]) =>
    [command ?? '', ...(args ?? [])]
      .filter((token) => token.includes(placeholder))
      .map((token) => ({ owner, entry: token.slice(token.indexOf(placeholder) + placeholder.length) })),
  );
}

describe('the @putnami/web extension manifest', () => {
  specTest(
    'names each entry point without its extension, as a TypeScript source of the project',
    {
      feature,
      requirement: 'installable-catalog-commands',
      check: 'each-file-the-manifest-names-is-an-entry-point-of-the-project',
    },
    () => {
      const manifest = JSON.parse(readFileSync(join(projectRoot, 'putnami.extension.json'), 'utf8')) as Manifest;
      const entries = namedEntries(manifest);
      // The i18n commands run their tasks through this name, so it must be here.
      expect(entries.map(({ entry }) => entry)).toContain('bin/i18n');
      for (const { owner, entry } of entries) {
        // A name with its extension resolves in the source only: the archive
        // holds the bundle under .js (typescript/extension ADR 0012).
        expect({ owner, extension: extname(entry) }).toEqual({ owner, extension: '' });
        expect({ owner, source: existsSync(join(projectRoot, `${entry}.ts`)) }).toEqual({ owner, source: true });
      }
    },
  );
});
