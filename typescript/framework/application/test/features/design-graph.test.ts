import { describe, expect, it } from 'bun:test';
import type { MigrationContributor } from '@putnami/migration';
import { Config, type ConfigContributor, named, provide } from '@putnami/runtime';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import {
  buildDesignGraph,
  type DesignBuilder,
  type DesignContributor,
  type DesignDelegate,
  type DesignGraph,
  type InfraContributor,
  designServiceNode,
  isDesignDelegate,
  isSourceScopedDesignContributor,
  module,
  type Plugin,
  serializeDesignGraph,
  type SourceScopedDesignContributor,
  type TestContributor,
  validateDesignGraph,
} from '../../src';

const SHARED_DESIGN_FIXTURE = join(
  import.meta.dir,
  '../../../../../protocols/features/fixtures/equivalence/go-typescript-design.golden.json',
);

const NativeDesignConfig = Config('tasks', { enabled: Boolean });

const nativeCompositionPlugin: Plugin & ConfigContributor & InfraContributor & TestContributor = {
  configDefinitions: () => [NativeDesignConfig],
  designInfraRequirements: () => [
    { name: 'tasks', kind: 'database', provenance: { path: 'test/native.ts', symbol: 'TasksDatabase' } },
  ],
  designTests: () => [
    {
      name: 'tasks/create',
      kind: 'integration',
      proves: [{ feature: 'tasks/manage', requirement: 'creation', check: 'task-create-flow' }],
      provenance: { path: 'test/native.test.ts', symbol: 'createsTask' },
    },
  ],
  warmup: () => {},
  start: () => {},
  stop: () => {},
};

class TaskStoreContributor implements DesignContributor {
  contributeDesign(builder: DesignBuilder): void {
    builder.addNode({ id: 'service:task-store', kind: 'service', name: 'task-store' });
    builder.relateFromModule('service:task-store', 'injects');
  }
}

class ConflictingUnscopedContributor implements DesignContributor {
  contributeDesign(builder: DesignBuilder): void {
    builder.addNode({ id: 'service:task-store', kind: 'service', name: 'wrong-store' });
  }
}

/**
 * Stands in for the SQL plugin: registered once on the application root for the
 * whole workload, and locating each declaration by its own source rather than by
 * the module it is attached to.
 */
class WorkloadWideDataContributor implements SourceScopedDesignContributor {
  readonly designAttribution = 'source' as const;

  contributeDesign(builder: DesignBuilder): void {
    builder.addNode({ id: 'data.table:default:tasks', kind: 'data.table', name: 'tasks' });
    builder.relateFromModule('data.table:default:tasks', 'contains');
  }
}

/**
 * Stands in for the client generator, which derives `generatedFrom` edges from
 * the OpenAPI IR while the api plugin owns the operation nodes. When the two
 * disagree the edge has no endpoint.
 */
class DanglingClientContributor implements DesignContributor {
  contributeDesign(builder: DesignBuilder): void {
    builder.addNode({ id: 'client.generated:ts:@example/items/ItemsClient', kind: 'client.generated', name: 'Items' });
    builder.addEdge({
      from: 'client.generated:ts:@example/items/ItemsClient',
      to: 'api.operation:GET:/never-registered',
      kind: 'generatedFrom',
      authority: 'exact',
    });
  }
}

describe('native feature design graph', () => {
  it('preserves the public source-scoped design contributor contract', () => {
    const contributor: SourceScopedDesignContributor = new WorkloadWideDataContributor();

    expect(typeof contributor.contributeDesign).toBe('function');
    expect(isSourceScopedDesignContributor(contributor)).toBe(true);
  });

  it('matches Go semantics for config, infra, lifecycle, and test native composition', async () => {
    const tasks = module('tasks')
      .feature({
        id: 'tasks/manage',
        name: 'Task management',
        outcome: 'Users can manage tasks',
        owner: 'samples',
      })
      .use(nativeCompositionPlugin);
    const application = module('app').use(tasks);

    const graph = await buildDesignGraph(application, 'tasks', '/workspace');
    const semanticGraph: DesignGraph = {
      ...(graph as DesignGraph),
      nodes: (graph?.nodes ?? []).map(({ provenance: _, ...node }) => node),
      edges: (graph?.edges ?? []).map(({ provenance: _, ...edge }) => edge),
    };
    expect(serializeDesignGraph(semanticGraph)).toBe(readFileSync(SHARED_DESIGN_FIXTURE, 'utf8'));
    expect(graph?.nodes.find(({ id }) => id === 'infra:database:tasks')?.provenance).toBeUndefined();
    expect(
      graph?.edges.find(
        ({ from, to, kind }) => from === 'module:tasks/tasks' && to === 'infra:database:tasks' && kind === 'contains',
      )?.provenance,
    ).toEqual({
      path: 'test/native.ts',
      symbol: 'TasksDatabase',
    });
  });

  it('refuses a partial spec-check binding on a declared test', async () => {
    const broken: Plugin & TestContributor = {
      name: () => 'broken-proof',
      designTests: () => [
        {
          name: 'tasks/create',
          kind: 'integration',
          // A proof must name feature, requirement, AND check — a partial
          // binding is an authoring error, never a guess.
          proves: [{ feature: 'tasks/manage', requirement: '', check: 'task-create-flow' }],
        },
      ],
    };
    const tasks = module('tasks')
      .feature({ id: 'tasks/manage', name: 'Task management', outcome: 'Users can manage tasks', owner: 'samples' })
      .use(broken);
    const application = module('app').use(tasks);
    await expect(buildDesignGraph(application, 'tasks', '/workspace')).rejects.toThrow(
      'must name feature, requirement, and check',
    );
  });

  it('keeps shared infra nodes semantic and canonicalizes duplicate ownership provenance', async () => {
    const requirement = (path: string, line?: number): Plugin & InfraContributor => ({
      designInfraRequirements: () => [
        {
          name: 'tasks',
          kind: 'database',
          provenance: { path, ...(line ? { line } : {}), symbol: 'TasksDatabase' },
        },
      ],
    });
    const application = module('app').use(
      module('tasks')
        .feature({
          id: 'tasks/manage',
          name: 'Task management',
          outcome: 'Users can manage tasks',
          owner: 'samples',
        })
        .use({ designInfraRequirements: () => [{ name: 'tasks', kind: 'database' as const }] })
        .use(requirement('src/tasks/z.database.ts'))
        .use(requirement('src/tasks/a.database.ts', 20))
        .use(requirement('src/tasks/a.database.ts', 3)),
    );

    const graph = await buildDesignGraph(application, 'tasks', '/workspace');
    expect(graph?.nodes.find(({ id }) => id === 'infra:database:tasks')?.provenance).toBeUndefined();
    const provenance = graph?.edges.find(
      ({ from, to, kind }) => from === 'module:tasks/tasks' && to === 'infra:database:tasks' && kind === 'contains',
    )?.provenance;
    if (
      JSON.stringify(provenance) !==
      JSON.stringify({ path: 'src/tasks/a.database.ts', line: 3, symbol: 'TasksDatabase' })
    ) {
      throw new Error(`canonical infra provenance = ${JSON.stringify(provenance)}`);
    }
  });

  it('derives deterministic technical relationships from one feature declaration', async () => {
    const tasks = module('tasks')
      .feature({
        id: 'tasks/manage',
        name: 'Task management',
        outcome: 'Users can manage tasks',
        owner: 'samples',
      })
      .use(new TaskStoreContributor());
    const application = module('app').use(tasks);

    const graph = await buildDesignGraph(application, '@example/tasks', '/workspace');

    expect(graph?.project).toBe('@example/tasks');
    expect(graph?.nodes.map(({ id }) => id)).toEqual([
      'feature:tasks/manage',
      'module:@example/tasks/tasks',
      'service:task-store',
    ]);
    expect(graph?.edges).toEqual([
      {
        from: 'feature:tasks/manage',
        to: 'module:@example/tasks/tasks',
        kind: 'implementedBy',
        authority: 'exact',
      },
      {
        from: 'module:@example/tasks/tasks',
        to: 'service:task-store',
        kind: 'injects',
        authority: 'exact',
      },
    ]);
    expect(serializeDesignGraph(graph!)).toEndWith('\n');
  });

  it('does not run contributors owned by modules outside a feature scope', async () => {
    const tasks = module('tasks')
      .feature({
        id: 'tasks/manage',
        name: 'Task management',
        outcome: 'Users can manage tasks',
        owner: 'samples',
      })
      .use(new TaskStoreContributor());
    const application = module('app').use(new ConflictingUnscopedContributor()).use(tasks);

    const graph = await buildDesignGraph(application, '@example/tasks', '/workspace');

    expect(graph?.nodes.find(({ id }) => id === 'service:task-store')?.name).toBe('task-store');
  });

  it('runs a source-attributed contributor owned by the unscoped application root', async () => {
    const tasks = module('tasks')
      .feature({
        id: 'tasks/manage',
        name: 'Task management',
        outcome: 'Users can manage tasks',
        owner: 'samples',
      })
      .use(new TaskStoreContributor());
    // The shape every application-wide plugin has: registered once on the root,
    // which declares no feature. Scoping it to its owner would drop everything
    // it reports, which is the whole reason the opt-in exists.
    const application = module('app').use(new WorkloadWideDataContributor()).use(tasks);

    const graph = await buildDesignGraph(application, '@example/tasks', '/workspace');

    expect(graph?.nodes.map(({ id }) => id)).toContain('data.table:default:tasks');
    // Its owner has no module id, so `relateFromModule` no-ops rather than
    // inventing containment from a root that owns no feature.
    expect(graph?.edges.some(({ to }) => to === 'data.table:default:tasks')).toBe(false);
  });

  it('keeps a nested app module distinct from a feature-scoped app root', async () => {
    const application = module('app')
      .feature({
        id: 'tasks/manage',
        name: 'Task management',
        outcome: 'Users can manage tasks',
        owner: 'samples',
      })
      .use(module('app'));

    const graph = await buildDesignGraph(application, '@example/tasks', '/workspace');

    expect(graph?.nodes.filter(({ kind }) => kind === 'module').map(({ id }) => id)).toEqual([
      'module:@example/tasks',
      'module:@example/tasks/app',
    ]);
    expect(graph?.edges).toContainEqual({
      from: 'module:@example/tasks',
      to: 'module:@example/tasks/app',
      kind: 'contains',
      authority: 'exact',
    });
    expect(graph?.edges.some(({ from, to }) => from === to)).toBe(false);
  });

  it('projects a migration source onto its datasource schema without colliding with a same-named table', async () => {
    // The sql plugin mints one node per declared table; a migration source
    // mints one per datasource schema. A table named after a schema on the same
    // datasource used to produce the same id under one shared kind, which
    // addNode rejected as a conflicting native declaration.
    const tables: DesignContributor = {
      contributeDesign(builder: DesignBuilder): void {
        builder.addNode({
          id: 'data.table:default:audit',
          kind: 'data.table',
          name: 'audit',
          properties: { columns: 'id,message', datasource: 'default' },
        });
        builder.relateFromModule('data.table:default:audit', 'contains');
      },
    };
    const migrations: Plugin & MigrationContributor = {
      migrationSources: () => [
        {
          kind: 'sql',
          namespace: 'audit',
          infraDatabase: () => ({ name: 'default', engine: 'postgres', schemas: ['audit'] }),
        },
      ],
    };
    const application = module('app').use(
      module('audit')
        .feature({
          id: 'audit/trail',
          name: 'Audit trail',
          outcome: 'Operators can read an audit trail',
          owner: 'samples',
        })
        .use(tables)
        .use(migrations),
    );

    const graph = await buildDesignGraph(application, '@example/audit', '/workspace');

    expect(graph?.nodes.filter(({ id }) => id.startsWith('data.'))).toEqual([
      {
        id: 'data.migration:sql:audit',
        kind: 'data.migration',
        name: 'audit',
        properties: { kind: 'sql', namespace: 'audit' },
      },
      {
        id: 'data.schema:default:audit',
        kind: 'data.schema',
        name: 'audit',
        properties: { datasource: 'default', engine: 'postgres', schema: 'audit' },
      },
      {
        id: 'data.table:default:audit',
        kind: 'data.table',
        name: 'audit',
        properties: { columns: 'id,message', datasource: 'default' },
      },
    ]);
    expect(graph?.edges).toContainEqual({
      from: 'data.migration:sql:audit',
      to: 'data.schema:default:audit',
      kind: 'writes',
      authority: 'exact',
    });
  });

  it('omits data.schema when a migration source declares no infra database', async () => {
    const migrations: Plugin & MigrationContributor = {
      migrationSources: () => [{ kind: 'sql', namespace: 'audit' }],
    };
    const application = module('app').use(
      module('audit')
        .feature({
          id: 'audit/trail',
          name: 'Audit trail',
          outcome: 'Operators can read an audit trail',
          owner: 'samples',
        })
        .use(migrations),
    );

    const graph = await buildDesignGraph(application, '@example/audit', '/workspace');

    expect(graph?.nodes.some(({ id }) => id === 'data.migration:sql:audit')).toBe(true);
    expect(graph?.nodes.some(({ kind }) => kind === 'data.schema')).toBe(false);
  });

  it('derives service identity and dependencies from module registrations', async () => {
    // `provide(Class, { deps: [...] })` is the runtime declaration: the
    // container resolves exactly these tokens for this provider, so both the
    // identity and the relationship are read rather than inferred.
    const repository = named<{ rows: number }>('putnami.repository:default.tasks');
    class TaskService {}
    const tasks = module('tasks')
      .feature({
        id: 'tasks/manage',
        name: 'Task management',
        outcome: 'Users can manage tasks',
        owner: 'samples',
      })
      .register(provide(repository, () => ({ rows: 0 })))
      .provide(TaskService, { deps: [repository] });

    const graph = await buildDesignGraph(module('app').use(tasks), '@example/tasks', '/workspace');

    const repositoryId = "service:named('putnami.repository:default.tasks')";
    expect(graph?.nodes.filter(({ kind }) => kind === 'service')).toEqual([
      { id: 'service:TaskService', kind: 'service', name: 'TaskService' },
      { id: repositoryId, kind: 'service', name: "named('putnami.repository:default.tasks')" },
    ]);
    expect(graph?.edges).toContainEqual({
      from: 'module:@example/tasks/tasks',
      to: repositoryId,
      kind: 'contains',
      authority: 'exact',
    });
    expect(graph?.edges).toContainEqual({
      from: 'service:TaskService',
      to: repositoryId,
      kind: 'injects',
      authority: 'exact',
    });
  });

  it('mints one identity for a token that is both registered and injected', async () => {
    // The api plugin mints a service node from an endpoint's injected token
    // while the registration walk mints one from the provider. A conflicting
    // declaration of the same id fails the whole build, so the two producers
    // must agree byte-for-byte.
    const repository = named<{ rows: number }>('putnami.repository:default.tasks');
    const endpoint: Plugin & DesignContributor = {
      contributeDesign(builder: DesignBuilder): void {
        builder.addNode({ id: 'api.operation:GET:/tasks', kind: 'api.operation', name: 'GET /tasks' });
        builder.relateFromModule('api.operation:GET:/tasks', 'exposes');
        const service = designServiceNode("named('putnami.repository:default.tasks')");
        builder.addNode(service);
        builder.addEdge({
          from: 'api.operation:GET:/tasks',
          to: service.id,
          kind: 'injects',
          authority: 'exact',
        });
      },
    };
    const tasks = module('tasks')
      .feature({
        id: 'tasks/manage',
        name: 'Task management',
        outcome: 'Users can manage tasks',
        owner: 'samples',
      })
      .register(provide(repository, () => ({ rows: 0 })))
      .use(endpoint);

    const graph = await buildDesignGraph(module('app').use(tasks), '@example/tasks', '/workspace');

    expect(graph?.nodes.filter(({ kind }) => kind === 'service').map(({ id }) => id)).toEqual([
      "service:named('putnami.repository:default.tasks')",
    ]);
  });

  it('records a provider declared outside a feature scope without giving it an owner', async () => {
    // One application owns one container tree, so a root-level provider is
    // still the service a feature-scoped operation injects — but no feature
    // owns it, and an unreferenced one stays unreachable from every feature.
    class RootService {}
    class RootDependency {}
    const application = module('app')
      .provide(RootDependency)
      .provide(RootService, { deps: [RootDependency] })
      .use(
        module('tasks').feature({
          id: 'tasks/manage',
          name: 'Task management',
          outcome: 'Users can manage tasks',
          owner: 'samples',
        }),
      );

    const graph = await buildDesignGraph(application, '@example/tasks', '/workspace');

    expect(graph?.nodes.filter(({ kind }) => kind === 'service').map(({ id }) => id)).toEqual([
      'service:RootDependency',
      'service:RootService',
    ]);
    expect(graph?.edges).toContainEqual({
      from: 'service:RootService',
      to: 'service:RootDependency',
      kind: 'injects',
      authority: 'exact',
    });
    expect(graph?.edges.some(({ kind, to }) => kind === 'contains' && to.startsWith('service:'))).toBe(false);
  });

  it('drops relationships with no matching node instead of failing the build', async () => {
    const tasks = module('tasks')
      .feature({
        id: 'tasks/manage',
        name: 'Task management',
        outcome: 'Users can manage tasks',
        owner: 'samples',
      })
      .use(new DanglingClientContributor());

    const graph = await buildDesignGraph(module('app').use(tasks), '@example/tasks', '/workspace');

    expect(graph?.nodes.map(({ id }) => id)).toContain('client.generated:ts:@example/items/ItemsClient');
    expect(graph?.edges.map(({ kind }) => kind)).not.toContain('generatedFrom');
  });

  // A controller that owns an inner plugin privately is invisible to
  // collectModules/collectPlugins, so its native contributions used to vanish
  // and had to be restated by a graph-only wrapper.
  it('forwards native contributions from a plugin that owns another plugin privately', async () => {
    const inner: Plugin & DesignContributor & MigrationContributor = {
      contributeDesign(builder: DesignBuilder): void {
        builder.addNode({ id: 'event.outbox:identity', kind: 'event.outbox', name: 'identity' });
        builder.relateFromModule('event.outbox:identity', 'enqueues');
      },
      migrationSources: () => [{ kind: 'sql', namespace: 'identity' }],
    };
    // The relay wraps `inner` and delegates, rather than restating its facts.
    const relay: Plugin & DesignDelegate = { designDelegates: () => [inner] };
    const application = module('app').use(
      module('identity')
        .feature({
          id: 'identity/sessions',
          name: 'Sessions',
          outcome: 'Operators can revoke sessions',
          owner: 'identity',
        })
        .use(relay),
    );

    const graph = await buildDesignGraph(application, '@example/identity', '/workspace');

    expect(isDesignDelegate(relay)).toBe(true);
    expect(isDesignDelegate(inner)).toBe(false);
    expect(graph?.nodes.map(({ id }) => id)).toContain('event.outbox:identity');
    expect(graph?.nodes.map(({ id }) => id)).toContain('data.migration:sql:identity');
    expect(graph?.edges).toContainEqual({
      from: 'module:@example/identity/identity',
      to: 'event.outbox:identity',
      kind: 'enqueues',
      authority: 'exact',
    });
  });

  // A delegate graph is authored by plugin owners: two plugins may reference
  // each other. Expansion must terminate and must not double-run a delegate.
  it('visits a delegate once and terminates on a delegate cycle', async () => {
    let contributions = 0;
    const left: Plugin & DesignDelegate & DesignContributor = {
      designDelegates: () => [right, shared],
      contributeDesign(): void {
        contributions += 1;
      },
    };
    const right: Plugin & DesignDelegate = { designDelegates: () => [left, shared] };
    const shared: Plugin & DesignContributor = {
      contributeDesign(builder: DesignBuilder): void {
        contributions += 1;
        builder.addNode({ id: 'service:shared', kind: 'service', name: 'shared' });
        builder.relateFromModule('service:shared', 'injects');
      },
    };
    const application = module('app').use(
      module('identity')
        .feature({
          id: 'identity/sessions',
          name: 'Sessions',
          outcome: 'Operators can revoke sessions',
          owner: 'identity',
        })
        .use(left),
    );

    const graph = await buildDesignGraph(application, '@example/identity', '/workspace');

    expect(contributions).toBe(2);
    expect(graph?.nodes.filter(({ id }) => id === 'service:shared')).toHaveLength(1);
  });

  // The Go producer publishes hand-written typed clients and the
  // currently-unmodeled authority. Both runtimes read each other's artifacts, so
  // this runtime must accept that vocabulary — and only that vocabulary.
  it('agrees with the Go runtime on typed clients and the currently-unmodeled authority', () => {
    const graph: DesignGraph = {
      compatibility: 'provisional',
      project: '@example/distribution',
      nodes: [
        { id: 'module:@example/distribution', kind: 'module', name: 'distribution' },
        {
          id: 'client.typed:cloud/identity:introspectauth',
          kind: 'client.typed',
          name: 'introspectauth',
          properties: { producer: 'cloud/identity', language: 'go', operations: 'POST /v1/introspect' },
        },
      ],
      edges: [
        {
          from: 'module:@example/distribution',
          to: 'client.typed:cloud/identity:introspectauth',
          kind: 'calls',
          authority: 'currently-unmodeled',
        },
      ],
    };

    expect(() => validateDesignGraph(graph)).not.toThrow();

    const unknownKind = { ...graph, nodes: [graph.nodes[0], { ...graph.nodes[1], kind: 'client.handwritten' }] };
    expect(() => validateDesignGraph(unknownKind as DesignGraph)).toThrow(/unsupported/);
    const unknownAuthority = { ...graph, edges: [{ ...graph.edges[0], authority: 'probably' }] };
    expect(() => validateDesignGraph(unknownAuthority as DesignGraph)).toThrow(/unsupported/);
  });

  it('agrees with the Go runtime on tooling-minted project and command nodes', () => {
    const graph: DesignGraph = {
      compatibility: 'provisional',
      project: 'samples/tasks',
      nodes: [
        { id: 'project:samples/tasks', kind: 'project', name: 'samples/tasks', provenance: { path: 'putnami.json' } },
        { id: 'project:@putnami/utils', kind: 'project', name: '@putnami/utils' },
        { id: 'command:tasks-admin', kind: 'command', name: 'tasks-admin', properties: { entry: 'src/bin/admin.ts' } },
        { id: 'module:tasks', kind: 'module', name: 'tasks' },
      ],
      edges: [
        { from: 'project:samples/tasks', to: 'module:tasks', kind: 'contains', authority: 'exact' },
        { from: 'project:samples/tasks', to: 'command:tasks-admin', kind: 'exposes', authority: 'exact' },
        { from: 'project:samples/tasks', to: 'project:@putnami/utils', kind: 'dependsOn', authority: 'exact' },
      ],
    };

    expect(() => validateDesignGraph(graph)).not.toThrow();

    const transitiveGuess = {
      ...graph,
      edges: [
        ...graph.edges,
        { from: 'project:samples/tasks', to: 'project:@putnami/utils', kind: 'provides', authority: 'exact' },
      ],
    };
    expect(() => validateDesignGraph(transitiveGuess as DesignGraph)).toThrow(/unsupported/);
  });

  it('fails when two feature declarations select the same module', async () => {
    const shared = module('token-store');
    const tokens = module('tokens').feature(
      { id: 'auth/opaque-tokens', name: 'Opaque tokens', outcome: 'Tokens are revocable', owner: 'auth' },
      { modules: [shared] },
    );
    const sessions = module('sessions').feature(
      { id: 'auth/sessions', name: 'Sessions', outcome: 'Sessions expire', owner: 'auth' },
      { modules: [shared] },
    );
    const application = module('app').use(tokens).use(sessions).use(shared);

    expect(buildDesignGraph(application, '@example/auth', '/workspace')).rejects.toThrow(
      "design module 'token-store' is selected by features 'auth/opaque-tokens' and 'auth/sessions'",
    );
  });

  it('fails when a selected module already implements a feature of its own', async () => {
    const google = module('google-oauth').feature({
      id: 'auth/google',
      name: 'Google sign-in',
      outcome: 'Users sign in with Google',
      owner: 'auth',
    });
    const tokens = module('tokens').feature(
      { id: 'auth/opaque-tokens', name: 'Opaque tokens', outcome: 'Tokens are revocable', owner: 'auth' },
      { modules: [google] },
    );
    const application = module('app').use(tokens).use(google);

    expect(buildDesignGraph(application, '@example/auth', '/workspace')).rejects.toThrow(
      "design module 'google-oauth' already implements feature 'auth/google' and is selected by feature 'auth/opaque-tokens' declared on module 'tokens'",
    );
  });

  it('fails when a feature selects its own declaring module', async () => {
    const tokens = module('tokens');
    tokens.feature(
      { id: 'auth/opaque-tokens', name: 'Opaque tokens', outcome: 'Tokens are revocable', owner: 'auth' },
      { modules: [tokens] },
    );

    expect(buildDesignGraph(module('app').use(tokens), '@example/auth', '/workspace')).rejects.toThrow(
      "design feature 'auth/opaque-tokens' selects its own declaring module 'tokens'",
    );
  });

  // Selecting a module selects its subtree. An ancestor's subtree contains the
  // declaring module and every unrelated sibling beside it, so accepting this
  // would silently rebuild the broad common-ancestor claim the contract replaces.
  it('fails when a feature selects an ancestor of its declaring module', async () => {
    const tokens = module('tokens');
    const unrelated = module('github-oauth').use(new TaskStoreContributor());
    const auth = module('auth').use(tokens).use(unrelated);
    tokens.feature(
      { id: 'auth/opaque-tokens', name: 'Opaque tokens', outcome: 'Tokens are revocable', owner: 'auth' },
      { modules: [auth] },
    );

    expect(buildDesignGraph(module('app').use(auth), '@example/auth', '/workspace')).rejects.toThrow(
      "design feature 'auth/opaque-tokens' selects module 'auth', which contains the module 'tokens' that declares it — a selection cannot claim an ancestor",
    );
  });

  // A selection that matches nothing leaves the graph incomplete, not wrong.
  // Scanned-route discovery is itself best-effort, so a loader that could not be
  // imported must not turn a disposable projection into a build failure.
  it('keeps building when a selection matches no native declaration', async () => {
    const unmounted = module('token-store');
    const tokens = module('tokens').feature(
      { id: 'auth/opaque-tokens', name: 'Opaque tokens', outcome: 'Tokens are revocable', owner: 'auth' },
      { modules: [unmounted], sources: ['src/api/auth/token'] },
    );

    const graph = await buildDesignGraph(module('app').use(tokens), '@example/auth', '/workspace');

    expect(graph?.nodes.map(({ id }) => id)).toEqual(['feature:auth/opaque-tokens', 'module:@example/auth/tokens']);
  });

  // Selection widens which contributors run, so the guard on unscoped
  // contributors has to hold on the association path too: only a declaration
  // whose source the feature selected can enter the graph.
  it('commits nothing from an unscoped contributor the selection does not reach', async () => {
    const tokens = module('tokens').feature(
      { id: 'auth/opaque-tokens', name: 'Opaque tokens', outcome: 'Tokens are revocable', owner: 'auth' },
      { sources: ['src/api/auth/token'] },
    );
    const selected: DesignContributor = {
      contributeDesign(builder: DesignBuilder): void {
        builder.addNode({
          id: 'api.operation:POST:/auth/token',
          kind: 'api.operation',
          name: 'POST /auth/token',
          provenance: { path: 'src/api/auth/token/post.ts' },
        });
        builder.relateFromModule('api.operation:POST:/auth/token', 'exposes');
        builder.addNode({
          id: 'api.operation:GET:/tasks',
          kind: 'api.operation',
          name: 'GET /tasks',
          provenance: { path: 'src/api/tasks/get.ts' },
        });
        builder.relateFromModule('api.operation:GET:/tasks', 'exposes');
      },
    };
    const application = module('app').use(tokens).use(selected).use(new ConflictingUnscopedContributor());

    const graph = await buildDesignGraph(application, '@example/auth', '/workspace');

    expect(graph?.nodes.map(({ id }) => id)).toEqual([
      'api.operation:POST:/auth/token',
      'feature:auth/opaque-tokens',
      'module:@example/auth/tokens',
    ]);
    expect(graph?.edges).toContainEqual({
      from: 'module:@example/auth/tokens',
      to: 'api.operation:POST:/auth/token',
      kind: 'exposes',
      authority: 'derived',
      properties: { association: 'selected' },
      provenance: { path: 'src/api/auth/token/post.ts' },
    });
  });

  // Paths are made project-relative when a node is added. Re-normalizing at
  // serialization time resolved them a second time against the process cwd,
  // which rewrote any path outside the project root into a cwd-dependent '../'
  // chain — a machine-dependent byte in a captured .gen artifact.
  it('serializes provenance paths verbatim rather than re-resolving them', () => {
    const graph: DesignGraph = {
      compatibility: 'provisional',
      project: '@example/tasks',
      nodes: [
        {
          id: 'data.schema:default:tasks',
          kind: 'data.schema',
          name: 'tasks',
          provenance: { path: '/elsewhere/vendor/tasks.entity.ts', line: 7 },
        },
        {
          id: 'module:@example/tasks',
          kind: 'module',
          name: 'tasks',
          provenance: { path: 'src/tasks/tasks.module.ts', line: 4 },
        },
      ],
      edges: [],
    };

    const paths = JSON.parse(serializeDesignGraph(graph)).nodes.map(
      (node: { provenance: { path: string } }) => node.provenance.path,
    );
    expect(paths).toEqual(['/elsewhere/vendor/tasks.entity.ts', 'src/tasks/tasks.module.ts']);
  });
});
