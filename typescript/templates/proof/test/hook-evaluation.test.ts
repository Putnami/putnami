import { describe, expect, test } from 'bun:test';
import { mkdirSync, mkdtempSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { disposeProject, type RenderedProject, runPreBuildHooks } from './harness';

describe('preBuild proof route evaluation', () => {
  for (const [name, output, expectedExit] of [
    ['complete', { type: 'summary', data: { httpRoutes: { v: 1, state: 'complete' } } }, 0],
    ['legacy', { type: 'summary', data: {} }, 1],
    [
      'unavailable',
      { type: 'summary', data: { httpRoutes: { v: 1, state: 'unavailable', reason: 'not evaluated' } } },
      1,
    ],
    ['unknown version', { type: 'summary', data: { httpRoutes: { v: 2, state: 'complete' } } }, 1],
    ['extra field', { type: 'summary', data: { httpRoutes: { v: 1, state: 'complete', reason: 'unexpected' } } }, 1],
    ['null', { type: 'summary', data: { httpRoutes: null } }, 1],
    ['no summary', { type: 'progress', data: {} }, 1],
  ] as const) {
    test(`a real ${name} hook has exit code ${expectedExit}`, async () => {
      const workspace = mkdtempSync(join(tmpdir(), 'putnami-hook-evaluation-'));
      const project = join(workspace, 'consumer');
      const producer = join(project, 'node_modules', 'fixture-hook');
      const rendered: RenderedProject = {
        workspace,
        project,
        template: { name: 'fixture', dir: workspace, manifest: { name: 'fixture' }, features: [] },
        vars: {
          projectName: 'consumer',
          projectPath: 'consumer',
          projectModule: 'consumer',
          putnamiVersion: '0.0.0',
          workspaceRelativePath: '..',
        },
      };
      try {
        mkdirSync(producer, { recursive: true });
        writeFileSync(join(project, 'package.json'), JSON.stringify({ dependencies: { 'fixture-hook': '1.0.0' } }));
        writeFileSync(
          join(producer, 'putnami.extension.json'),
          JSON.stringify({ hooks: { preBuild: { command: 'bun', args: ['{extensionRoot}/hook.ts'] } } }),
        );
        writeFileSync(join(producer, 'hook.ts'), `console.log(${JSON.stringify(JSON.stringify(output))});\n`);

        const result = await runPreBuildHooks(rendered, 'test');

        expect(result.exitCode).toBe(expectedExit);
        if (expectedExit !== 0) {
          expect(result.output).toContain('hook fixture-hook emitted no complete route evaluation');
        }
      } finally {
        disposeProject(rendered);
      }
    });
  }
});
