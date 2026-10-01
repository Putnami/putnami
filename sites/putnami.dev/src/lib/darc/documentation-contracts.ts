/**
 * The `public-docs` domain's declared cross-domain access, as code.
 *
 * The site publishes documentation five other domains own. Until now that was a
 * build-time file copy with no contract: the TypeScript extension logs
 * `generate asset source not found, skipping` for a source that disappeared, the
 * section vanishes, and the build stays green. The declarations below are the
 * same five copies stated as Domain Access & Replication Contracts, plus the
 * reference to the TypeScript framework runtime the site itself consumes.
 *
 * Every contract here must stay identical to the committed
 * `putnami.architecture.json`. It is not a copy that may drift: a row emitted
 * from a contract the manifest does not declare fails
 * `putnami architecture validate` with `architecture.evidence_without_declaration`,
 * and that failure is the point. `bindings` are deliberately absent — they are
 * maintained by `putnami architecture sync` against the resolved project graph,
 * and restating them here would be a second, unreviewable copy. This mirrors the
 * Go precedent in `sites/telemetry.putnami.dev/darc.go`.
 */
import type { ArchitectureImport, Consistency, Transport } from '@putnami/application';
import { DOCUMENTATION_TREE_CONTRACT } from './documentation-tree';

/**
 * The carrier the five documentation snapshots share: the markdown tree one
 * `generate.assets` entry copies out of the producing domain's repository root.
 */
const DOCUMENTATION_TRANSPORT: Transport = {
  kind: 'file',
  contract: DOCUMENTATION_TREE_CONTRACT,
  availability: 'active',
};

/**
 * The freshness, ordering and failure promise the five snapshots share.
 *
 * `onMissing: fail-closed` is the whole contract: a source that is not there
 * fails the build instead of publishing an empty section. `onStale: use-stale`
 * with a 30-day bound is the deployment half — an old container keeps serving
 * the documentation it baked and says how old the copy is, because refusing to
 * serve documentation is worse than serving documentation from last month.
 */
const DOCUMENTATION_CONSISTENCY: Consistency = {
  maxStaleness: '720h',
  onMissing: 'fail-closed',
  onStale: 'use-stale',
  ordering: 'source-version',
  sourceVersion: 'tree_digest',
  idempotencyKey: 'tree_digest',
  lateEvents: 'ignore-older',
};

/** One published section: the declared import, the source root, and where it lands. */
export interface DocumentationSourceContract {
  /** The declared import this section is governed by. */
  readonly contract: ArchitectureImport;
  /** The workspace-relative source root, exactly as `generate.assets.from` names it. */
  readonly sourceRoot: string;
  /** The published target, exactly as `generate.assets.to` names it. */
  readonly section: string;
}

function documentationImport(input: {
  id: string;
  as: string;
  domain: string;
  exportId: string;
  fact: string;
  justification: string;
}): ArchitectureImport {
  return {
    id: input.id,
    version: 1,
    from: { domain: input.domain, export: input.exportId },
    as: input.as,
    mode: 'snapshot',
    status: 'active',
    facts: [input.fact],
    transport: DOCUMENTATION_TRANSPORT,
    consistency: DOCUMENTATION_CONSISTENCY,
    justification: input.justification,
  };
}

/**
 * The five repository-owned documentation sources, in the order
 * `sites/putnami.dev/putnami.json` declares their `generate.assets` copies.
 *
 * The site's own `/sites/putnami.dev/doc` is absent on purpose: it is this
 * domain's own documentation, and a domain does not import from itself.
 * `/tooling/cli/scripts/install.sh`, `/tooling/cli/scripts/install-commands.txt`,
 * `/tooling/cli/scripts/install.ps1` and `/LICENSE.md` are absent for the same
 * kind of reason — release artifacts and a workspace-root legal file are not
 * documentation facts any domain owns.
 */
export const DOCUMENTATION_SOURCES: readonly DocumentationSourceContract[] = [
  {
    contract: documentationImport({
      id: 'public-docs.workspace-documentation.v1',
      as: 'public-docs.workspace-documentation',
      domain: 'cli',
      exportId: 'cli.workspace-documentation.v1',
      fact: 'workspace_documentation',
      justification:
        'The site publishes the workspace and CLI reference at public/docs/08-tooling-&-workspace by copying the /tooling/doc tree verbatim at build time. The cli domain decides what that reference says; the site owns only its section number and its position in the navigation.',
    }),
    sourceRoot: '/tooling/doc',
    section: 'public/docs/08-tooling-&-workspace',
  },
  {
    contract: documentationImport({
      id: 'public-docs.method-documentation.v1',
      as: 'public-docs.method-documentation',
      domain: 'sdd',
      exportId: 'sdd.method-documentation.v1',
      fact: 'spec_driven_development_documentation',
      justification:
        "The site publishes the spec-driven development method at public/docs/07-spec-driven-development by copying the /tooling/sdd-extension/doc tree verbatim at build time. The sdd domain owns the method's wording; the site owns only its placement in the published information architecture.",
    }),
    sourceRoot: '/tooling/sdd-extension/doc',
    section: 'public/docs/07-spec-driven-development',
  },
  {
    contract: documentationImport({
      id: 'public-docs.typescript-framework-documentation.v1',
      as: 'public-docs.typescript-framework-documentation',
      domain: 'typescript-framework',
      exportId: 'typescript-framework.api-documentation.v1',
      fact: 'typescript_framework_documentation',
      justification:
        "The site publishes the TypeScript framework reference at public/docs/09-frameworks/01-typescript by copying the /typescript/doc/framework tree verbatim at build time. The typescript-framework domain decides what that reference says; the site decides only where it appears. This is a separate contract from the site's runtime dependency on the same domain: one is a copied document, the other is a linked API.",
    }),
    sourceRoot: '/typescript/doc/framework',
    section: 'public/docs/09-frameworks/01-typescript',
  },
  {
    contract: documentationImport({
      id: 'public-docs.go-framework-documentation.v1',
      as: 'public-docs.go-framework-documentation',
      domain: 'go-framework',
      exportId: 'go-framework.api-documentation.v1',
      fact: 'go_framework_documentation',
      justification:
        'The site publishes the Go framework reference at public/docs/09-frameworks/02-go by copying the /go/doc/framework tree verbatim at build time. The go-framework domain decides what that reference says; the site decides only where it appears and in what order. A missing tree fails the build rather than publishing a gap, and a tree older than the freshness bound is still published because stale documentation beats no documentation.',
    }),
    sourceRoot: '/go/doc/framework',
    section: 'public/docs/09-frameworks/02-go',
  },
  {
    contract: documentationImport({
      id: 'public-docs.python-surface-documentation.v1',
      as: 'public-docs.python-surface-documentation',
      domain: 'extension-providers',
      exportId: 'extension-providers.python-surface-documentation.v1',
      fact: 'python_surface_documentation',
      justification:
        'The site publishes the Python surface reference at public/docs/09-frameworks/03-python by copying the /python/doc/framework tree verbatim at build time. The Python surface is the extension and its templates only, and the extension-providers domain decides what that reference claims; the site never promotes it to framework parity by where it places it.',
    }),
    sourceRoot: '/python/doc/framework',
    section: 'public/docs/09-frameworks/03-python',
  },
];

/**
 * The TypeScript framework runtime this site is built on.
 *
 * Nothing is copied, so the only promise a reference makes is MINIMIZATION: the
 * import names the exact fact it consumes, and reaching past it is a boundary
 * crossing nobody reviewed. Declaring no transport is deliberate — the framework
 * is linked into this workload, and naming a carrier for an in-process package
 * dependency would describe a delivery that does not happen.
 */
export const APPLICATION_RUNTIME_IMPORT: ArchitectureImport = {
  id: 'public-docs.application-runtime.v1',
  version: 1,
  from: { domain: 'typescript-framework', export: 'typescript-framework.application-runtime.v1' },
  as: 'public-docs.application-runtime',
  mode: 'reference',
  status: 'active',
  facts: ['typescript_application_runtime_api'],
  justification:
    'putnami.dev is an ordinary Putnami TypeScript application: it boots on the application container, renders its pages through the web runtime, draws them with the shared UI components, and measures its own audience with the analytics plugin into a database it owns. The site consumes the public framework API the same way a tenant application does, and reaches into no framework internal.',
};

/** The fact the application-runtime reference names, and the only one it may resolve. */
export const APPLICATION_RUNTIME_FACT = 'typescript_application_runtime_api';

/** Every contract this domain declares, snapshots first, in declaration order. */
export const PUBLIC_DOCS_CONTRACTS: readonly ArchitectureImport[] = [
  ...DOCUMENTATION_SOURCES.map((source) => source.contract),
  APPLICATION_RUNTIME_IMPORT,
];
