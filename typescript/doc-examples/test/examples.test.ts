import { describe, expect, test } from 'bun:test';
import { existsSync, readFileSync } from 'node:fs';
import { dirname, join, posix, relative, sep } from 'node:path';
import { check, type Example, LANGUAGES } from '../src/check';
import { documents, readManifest } from '../src/manifest';
import { blocks, isChecked } from '../src/markdown';

const project = join(import.meta.dir, '..');

function workspaceRoot(): string {
  let dir = project;
  while (!existsSync(join(dir, 'putnami.workspace.json'))) {
    const parent = dirname(dir);
    if (parent === dir) throw new Error('no putnami.workspace.json above the project');
    dir = parent;
  }
  return dir;
}

describe('documentation examples', () => {
  test(
    'every TypeScript block of the documents examples.json names type-checks',
    () => {
      const root = workspaceRoot();
      const docs = documents(readManifest(join(project, 'examples.json')), root);
      const examples: Example[] = [];
      for (const doc of docs) {
        for (const block of blocks(readFileSync(join(root, doc), 'utf8'))) {
          if (LANGUAGES.has(block.lang) && isChecked(block)) examples.push({ doc, block });
        }
      }
      expect(examples.length).toBeGreaterThan(0);
      const findings = check(examples, project).map((f) => `${f.doc}:${f.line}: ${f.message}`);
      expect(findings).toEqual([]);
    },
    { timeout: 600_000 },
  );
});

describe('test inputs', () => {
  test('every checked document is a declared test input of putnami.json', () => {
    const root = workspaceRoot();
    const docs = documents(readManifest(join(project, 'examples.json')), root);
    const manifest = JSON.parse(readFileSync(join(project, 'putnami.json'), 'utf8')) as {
      options: { test: { filePatterns: string[] } };
    };
    const projectPath = relative(root, project).split(sep).join('/');
    const inputs = manifest.options.test.filePatterns.map(
      (pattern) => new Bun.Glob(posix.normalize(posix.join(projectPath, pattern))),
    );
    const uncovered = docs.filter((doc) => !inputs.some((input) => input.match(doc)));
    // The test's declared inputs decide when it runs again and what its cache
    // key holds: add a filePatterns entry for each document listed here.
    expect(uncovered).toEqual([]);
  });
});
