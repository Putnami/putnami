import { describe, expect, it } from 'bun:test';
import { readFileSync } from 'node:fs';
import { type ArchitectureImport, DarcError, DarcPlugin } from '@putnami/application';
import { specTest } from '@putnami/spectest';
import { fileExists, joinPath } from '@putnami/utils';
import {
  APPLICATION_RUNTIME_IMPORT,
  DOCUMENTATION_SOURCES,
  PUBLIC_DOCS_CONTRACTS,
} from '../src/lib/darc/documentation-contracts';
import {
  contentDigest,
  type DocumentationFile,
  documentationTreeVersion,
  type ObservedDocumentationTree,
  observeDocumentationTree,
  readDocumentationTree,
} from '../src/lib/darc/documentation-tree';
import { PublicDocumentationAccess } from '../src/lib/darc/public-docs';
import { prerenderDocs, prerenderSourcesFor } from '../src/plugins/docs-prerender.plugin';

/**
 * The `public-docs` domain's runtime enforcement of its declared imports.
 *
 * A `generate.assets` entry whose source was deleted used to publish nothing and
 * keep the build green. Each of the five repository-owned sources is now a
 * `Snapshot` contract, and the docs pipeline reads the file inventory it
 * publishes out of `Snapshot.latest()` — so the declared `onMissing: fail-closed`
 * is what refuses, not an `if` in the plugin.
 *
 * Every clock here is injected. A freshness verdict measured against the wall
 * clock would make these outcomes depend on when the suite ran.
 */

const PROJECT_ROOT = joinPath(import.meta.dir, '..');
const WORKSPACE_ROOT = joinPath(PROJECT_ROOT, '..', '..');

/** A fixed observation instant, so `observedAt` never comes from the wall clock. */
const OBSERVED_AT = new Date('2026-01-01T00:00:00.000Z');
const HOUR = 3_600_000;

const GO_DOCS = '/go/doc/framework';
const TOOLING_DOCS = '/tooling/doc';

function file(path: string, content: string): DocumentationFile {
  return { path, digest: contentDigest(content) };
}

/** A synthetic source tree, so no test depends on the exact bytes of real documentation. */
function tree(root: string, ...files: DocumentationFile[]): ObservedDocumentationTree {
  return observeDocumentationTree(root, files);
}

/** Every declared source resolves, except the roots named absent. */
function accessWith(absent: readonly string[] = [], clock: () => Date = () => OBSERVED_AT) {
  return new PublicDocumentationAccess({
    clock,
    readTree: (root) => (absent.includes(root) ? undefined : tree(root, file('index.md', `# ${root}\n`))),
  });
}

describe('documentation sources are version-addressed snapshots', () => {
  specTest(
    'refuses to publish a section whose source is gone',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'documentation-sources-are-version-addressed-snapshots',
      check: 'missing-documentation-source-fails-closed',
    },
    () => {
      // The source is simulated absent through the injected reader; no
      // repository file is touched.
      const access = accessWith([GO_DOCS]);
      access.observeAll();

      const entry = { from: GO_DOCS, to: 'public/docs/09-frameworks/02-go' };
      let refusal: unknown;
      try {
        prerenderSourcesFor(access, WORKSPACE_ROOT, entry);
      } catch (error) {
        refusal = error;
      }

      // The refusal comes from the declared contract, and names the clause.
      expect(refusal).toBeInstanceOf(DarcError);
      expect((refusal as DarcError).code).toBe('missing');
      expect((refusal as DarcError).message).toContain('public-docs.go-framework-documentation.v1');

      // Every other declared section still resolves, so one deleted source
      // fails the build rather than degrading the whole docs tree.
      for (const source of DOCUMENTATION_SOURCES) {
        if (source.sourceRoot === GO_DOCS) continue;
        const files = prerenderSourcesFor(access, WORKSPACE_ROOT, {
          from: source.sourceRoot,
          to: source.section,
        });
        expect(files.map((entry) => entry.relative)).toEqual(['index.md']);
      }
    },
  );

  specTest(
    'fails the generate pass, before it writes or renders anything',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'documentation-sources-are-version-addressed-snapshots',
      check: 'missing-documentation-source-fails-the-generate-pass',
    },
    async () => {
      // The whole plugin, not just the resolver: the refusal has to reach the
      // build. It is raised before the markdown renderer is loaded and before
      // any `.gen` file is written, so a deleted source can never leave a
      // half-published docs tree behind.
      const plugin = prerenderDocs(accessWith([GO_DOCS]));
      let refusal: unknown;
      try {
        await plugin.generate();
      } catch (error) {
        refusal = error;
      }
      expect(refusal).toBeInstanceOf(DarcError);
      expect((refusal as DarcError).code).toBe('missing');
    },
  );

  specTest(
    'addresses a documentation tree by the digest of its contents',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'documentation-sources-are-version-addressed-snapshots',
      check: 'documentation-tree-version-is-content-addressed',
    },
    () => {
      const files = [file('b/second.md', 'second\n'), file('a/first.md', 'first\n')];

      // Same pairs in, same version out — whatever order they arrive in.
      expect(documentationTreeVersion(files)).toBe(documentationTreeVersion([...files].reverse()));
      expect(documentationTreeVersion(files)).toMatch(/^sha256:[0-9a-f]{64}$/);

      // One changed byte is a different producer state.
      const edited = [file('b/second.md', 'second!\n'), file('a/first.md', 'first\n')];
      expect(documentationTreeVersion(edited)).not.toBe(documentationTreeVersion(files));

      // A renamed file is a different state too: the path is part of the pair.
      const renamed = [file('b/renamed.md', 'second\n'), file('a/first.md', 'first\n')];
      expect(documentationTreeVersion(renamed)).not.toBe(documentationTreeVersion(files));

      // The same real tree read twice is the same version: a cold build and a
      // warm build publish the same producer state.
      const first = readDocumentationTree(WORKSPACE_ROOT, TOOLING_DOCS);
      const second = readDocumentationTree(WORKSPACE_ROOT, TOOLING_DOCS);
      expect(first?.version).toBeDefined();
      expect(first?.version).toBe(second?.version as string);
      expect(first?.tree.files.length).toBeGreaterThan(0);

      // An absent root is absent, not an empty observation.
      expect(readDocumentationTree(WORKSPACE_ROOT, '/tooling/doc-that-was-deleted')).toBeUndefined();
    },
  );

  it('uses the tree observed by each full scan, even when its digest sorts lower', () => {
    let goTree = tree(GO_DOCS, file('index.md', 'first revision\n'));
    const access = new PublicDocumentationAccess({
      clock: () => OBSERVED_AT,
      readTree: (root) => (root === GO_DOCS ? goTree : tree(root, file('index.md', `# ${root}\n`))),
    });

    access.observeAll();
    const firstVersion = access.resolve(GO_DOCS).sourceVersion;

    let nextTree: ObservedDocumentationTree | undefined;
    for (let attempt = 1; attempt <= 100; attempt += 1) {
      const candidate = tree(GO_DOCS, file('index.md', `changed revision ${attempt}\n`));
      if (candidate.version < firstVersion) {
        nextTree = candidate;
        break;
      }
    }
    if (!nextTree) throw new Error('expected a deterministic SHA-256 tree digest below the initial digest');

    goTree = nextTree;
    access.observeAll();

    expect(goTree.version < firstVersion).toBe(true);
    expect(access.resolve(GO_DOCS).sourceVersion).toBe(goTree.version);
  });

  specTest(
    'refuses to re-attach a version with different content',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'documentation-sources-are-version-addressed-snapshots',
      check: 'documentation-snapshot-version-is-immutable',
    },
    () => {
      let observed = tree(GO_DOCS, file('index.md', 'original\n'));
      const access = new PublicDocumentationAccess({ clock: () => OBSERVED_AT, readTree: () => observed });

      access.observe(GO_DOCS);
      // Re-observing the identical state is a transport redelivering, not an error.
      access.observe(GO_DOCS);
      expect(access.resolve(GO_DOCS).sourceVersion).toBe(observed.version);

      // The same version carrying different content would make every reader
      // that already resolved it wrong.
      observed = { version: observed.version, tree: { root: GO_DOCS, files: [file('index.md', 'rewritten\n')] } };
      let refusal: unknown;
      try {
        access.observe(GO_DOCS);
      } catch (error) {
        refusal = error;
      }
      expect(refusal).toBeInstanceOf(DarcError);
      expect((refusal as DarcError).code).toBe('immutable');
    },
  );

  specTest(
    'serves a stale copy and says how old it is',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'documentation-sources-are-version-addressed-snapshots',
      check: 'stale-documentation-copy-is-served-and-labelled',
    },
    () => {
      // Observed once, read much later: a deployed container keeps serving the
      // documentation it baked, because the contract declares use-stale.
      let now = OBSERVED_AT;
      const access = accessWith([], () => now);
      access.observeAll();

      const fresh = access.resolve(TOOLING_DOCS);
      expect(fresh.freshness).toBe('fresh');
      expect(fresh.observedAt.toISOString()).toBe(OBSERVED_AT.toISOString());
      // Provenance is the producer export the import names — carried by the
      // record, not by a parallel channel this site invented.
      expect(fresh.provenance).toBe('cli.workspace-documentation.v1');

      now = new Date(OBSERVED_AT.getTime() + 719 * HOUR);
      expect(access.resolve(TOOLING_DOCS).freshness).toBe('fresh');

      now = new Date(OBSERVED_AT.getTime() + 721 * HOUR);
      const stale = access.resolve(TOOLING_DOCS);
      expect(stale.freshness).toBe('stale');
      expect(stale.value.files.map((entry) => entry.path)).toEqual(['index.md']);
    },
  );

  specTest(
    'reports exactly the six contracts it enforces, deterministically',
    {
      feature: 'putnami-dev/public-documentation',
      requirement: 'documentation-sources-are-version-addressed-snapshots',
      check: 'documentation-access-evidence-is-complete-and-deterministic',
    },
    () => {
      const plugin = new DarcPlugin('public-docs-contracts', ...accessWith().components());
      const rows = plugin.domainAccessContracts();

      expect(rows.map((row) => [row.import, row.mode])).toEqual([
        ['public-docs.application-runtime.v1', 'reference'],
        ['public-docs.go-framework-documentation.v1', 'snapshot'],
        ['public-docs.method-documentation.v1', 'snapshot'],
        ['public-docs.python-surface-documentation.v1', 'snapshot'],
        ['public-docs.typescript-framework-documentation.v1', 'snapshot'],
        ['public-docs.workspace-documentation.v1', 'snapshot'],
      ]);

      // The domain opts into evidence with all six rows or none: a partial set
      // makes `architecture validate` report declared_without_evidence for the
      // rest of the domain.
      expect(rows).toHaveLength(PUBLIC_DOCS_CONTRACTS.length);
      for (const row of rows) expect(row.status).toBe('active');

      // Every snapshot row carries the declared carrier and consistency block
      // verbatim; the reference row carries neither, because it enforces neither.
      for (const row of rows.filter((candidate) => candidate.mode === 'snapshot')) {
        expect(row.transports).toEqual([
          { role: 'transport', kind: 'file', contract: 'putnami.documentation-tree.v1', availability: 'active' },
        ]);
        expect(row.enforced).toEqual({
          maxStaleness: '720h',
          onMissing: 'fail-closed',
          onStale: 'use-stale',
          ordering: 'source-version',
          lateEvents: 'ignore-older',
        });
      }
      const reference = rows.find((row) => row.mode === 'reference');
      expect(reference?.transports).toBeUndefined();
      expect(reference?.enforced).toBeUndefined();

      // Deterministic: the committed sidecar these rows land in must not move
      // between two runs of the producing job.
      const again = new DarcPlugin('public-docs-contracts', ...accessWith().components()).domainAccessContracts();
      expect(JSON.stringify(again)).toBe(JSON.stringify(rows));
    },
  );
});

describe('the application-runtime reference minimizes', () => {
  it('resolves the one fact it declares and refuses any other', () => {
    const access = accessWith();
    expect(access.runtimeProvenance()).toBe('typescript-framework.application-runtime.v1');
    expect(() => access.runtime.fact('go_application_runtime_api')).toThrow(DarcError);
    expect(access.runtime.facts()).toEqual(['typescript_application_runtime_api']);
  });
});

/**
 * The capability manifest the build emits, and where it lands.
 *
 * `putnami architecture validate` reads the COMMITTED
 * `schema/capabilities.json`; the build writes `.gen/schema/capabilities.json`.
 * Prefer the committed copy and fall back to the emitted one so this suite
 * reports on whichever exists.
 */
function capabilityManifest(): { path: string; document: Record<string, unknown> } | undefined {
  for (const relative of ['schema/capabilities.json', '.gen/schema/capabilities.json']) {
    const path = joinPath(PROJECT_ROOT, relative);
    if (!fileExists(path)) continue;
    return { path: relative, document: JSON.parse(readFileSync(path, 'utf8')) };
  }
  return undefined;
}

interface ManifestDomainAccess {
  identity?: { ownerProject?: string };
  import?: string;
  mode?: string;
}

describe('emitted domain-access evidence', () => {
  it('attributes every row to this project, not to a dependency', () => {
    const manifest = capabilityManifest();
    if (!manifest) {
      return;
    }

    const rows = (manifest.document['domainAccess'] ?? []) as ManifestDomainAccess[];
    expect(rows.map((row) => [row.import, row.mode])).toEqual([
      ['public-docs.application-runtime.v1', 'reference'],
      ['public-docs.go-framework-documentation.v1', 'snapshot'],
      ['public-docs.method-documentation.v1', 'snapshot'],
      ['public-docs.python-surface-documentation.v1', 'snapshot'],
      ['public-docs.typescript-framework-documentation.v1', 'snapshot'],
      ['public-docs.workspace-documentation.v1', 'snapshot'],
    ]);

    // The owner is this workspace project — `putnami.dev`, the name
    // `/sites/putnami.dev` declares — and never one of the framework packages
    // whose code carries the row.
    const project = manifest.document['project'];
    expect(project).toBe('putnami.dev');
    for (const row of rows) expect(row.identity?.ownerProject).toBe('putnami.dev');
  });
});

/**
 * The declaration is authority; the code is what implements it.
 *
 * `bindings` are excluded on purpose: `putnami architecture sync` maintains them
 * against the resolved project graph, and restating them in code would be a
 * second, unreviewable copy. The Go precedent
 * (`sites/telemetry.putnami.dev/darc.go`) omits them for the same reason.
 */
function withoutBindings(contract: ArchitectureImport): Omit<ArchitectureImport, 'bindings'> {
  const { bindings: _bindings, ...rest } = contract;
  return rest;
}

describe('declared contracts match the reviewed manifest', () => {
  it('states in code exactly what putnami.architecture.json declares', () => {
    const path = joinPath(PROJECT_ROOT, 'putnami.architecture.json');
    if (!fileExists(path)) {
      return;
    }
    const manifest = JSON.parse(readFileSync(path, 'utf8')) as { imports?: ArchitectureImport[] };
    const declared = manifest.imports ?? [];
    if (declared.length === 0) {
      return;
    }

    expect(declared.map((contract) => contract.id).sort()).toEqual(
      PUBLIC_DOCS_CONTRACTS.map((contract) => contract.id).sort(),
    );
    for (const contract of PUBLIC_DOCS_CONTRACTS) {
      const match = declared.find((candidate) => candidate.id === contract.id);
      expect(match).toBeDefined();
      expect(withoutBindings(match as ArchitectureImport)).toEqual(withoutBindings(contract));
    }
  });

  it('keeps every declared source root and section aligned with generate.assets', () => {
    const config = JSON.parse(readFileSync(joinPath(PROJECT_ROOT, 'putnami.json'), 'utf8')) as {
      options?: { generate?: { assets?: { from: string; to: string }[] } };
    };
    const assets = config.options?.generate?.assets ?? [];
    for (const source of DOCUMENTATION_SOURCES) {
      expect(assets.filter((entry) => entry.from === source.sourceRoot)).toEqual([
        { from: source.sourceRoot, to: source.section },
      ]);
    }
    // The site's own documentation is copied but never imported: a domain does
    // not import from itself.
    const access = accessWith();
    expect(access.declares('/sites/putnami.dev/doc')).toBe(false);
    expect(APPLICATION_RUNTIME_IMPORT.mode).toBe('reference');
  });
});
