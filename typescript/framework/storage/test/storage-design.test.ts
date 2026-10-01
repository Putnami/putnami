import { afterEach, beforeEach, describe, it } from 'bun:test';
import { buildDesignGraph, module } from '@putnami/application';
import { bucketRegistry } from '../src/bucket/bucket.registry';
import { BUCKET_MARKER } from '../src/bucket/bucket.types';
import { storagePlugin } from '../src/storage.plugin';

beforeEach(() => {
  bucketRegistry.clear();
});

afterEach(() => {
  bucketRegistry.clear();
});

describe('storage native design projection', () => {
  it('attributes a root plugin bucket from its declared source without generic graph mutation', async () => {
    // This test owns the registry input and the graph projection assertion. Do
    // not route setup through Bucket(): Bun's mock.module() is process-global,
    // and a suite loaded earlier may have cached Bucket with a test-local
    // getProjectRoot. Registering the native declaration shape directly keeps
    // file order from changing whether this source-scoped resource exists.
    bucketRegistry.register({
      __bucket: BUCKET_MARKER,
      bucketName: 'task-attachments',
      options: {},
      __source: { path: 'src/tasks/bucket.ts' },
    });
    const plugin = storagePlugin();
    const application = module('app')
      .use(plugin)
      .use(
        module('tasks').feature(
          {
            id: 'tasks/manage',
            name: 'Task management',
            outcome: 'Users can attach files to tasks',
            owner: 'samples',
          },
          { sources: ['src/tasks'] },
        ),
      );

    const graph = await buildDesignGraph(application, '@example/tasks', '/workspace');
    const infraNode = graph?.nodes.find(({ id }) => id === 'infra:storage:task-attachments');
    const expectedInfraNode = {
      id: 'infra:storage:task-attachments',
      kind: 'infra',
      name: 'task-attachments',
      properties: { kind: 'storage' },
    };
    if (JSON.stringify(infraNode) !== JSON.stringify(expectedInfraNode)) {
      throw new Error(`root storage infra node = ${JSON.stringify(infraNode)}`);
    }
    const infraEdge = graph?.edges.find(
      ({ from, to, kind }) =>
        from === 'module:@example/tasks/tasks' && to === 'infra:storage:task-attachments' && kind === 'contains',
    );
    const expectedInfraEdge = {
      from: 'module:@example/tasks/tasks',
      to: 'infra:storage:task-attachments',
      kind: 'contains',
      authority: 'derived',
      properties: { association: 'selected' },
      provenance: { path: 'src/tasks/bucket.ts' },
    };
    if (JSON.stringify(infraEdge) !== JSON.stringify(expectedInfraEdge)) {
      throw new Error(
        `root storage infra edge = ${JSON.stringify(infraEdge)} requirements=${JSON.stringify(plugin.designInfraRequirements())} in ${JSON.stringify(graph?.edges)}`,
      );
    }
  });
});
