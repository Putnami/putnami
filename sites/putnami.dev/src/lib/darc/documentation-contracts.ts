/**
 * The `public-docs` domain's declared cross-domain access, as code.
 *
 * The site publishes documentation that other domains own. A build-time file
 * copy alone carries no contract: the TypeScript extension logs
 * `generate asset source not found, skipping` for a source that disappeared,
 * the section vanishes, and the build stays green. The declarations below are
 * those copies stated as Domain Access & Replication Contracts, plus the
 * reference to the TypeScript framework runtime the site itself consumes and
 * the reference to the content-bundle protocol the site implements.
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
 * The carrier every documentation snapshot shares: the markdown one
 * `generate.assets` entry copies out of the producing domain's repository root.
 */
const DOCUMENTATION_TRANSPORT: Transport = {
  kind: 'file',
  contract: DOCUMENTATION_TREE_CONTRACT,
  availability: 'active',
};

/**
 * The freshness, ordering and failure promise every documentation snapshot shares.
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

/** One `generate.assets` copy: where the documentation is read, and where it lands. */
export interface DocumentationCopy {
  /**
   * The workspace-relative source root, exactly as `generate.assets.from`
   * names it: a directory, or one markdown file.
   */
  readonly sourceRoot: string;
  /** The published target, exactly as `generate.assets.to` names it. */
  readonly section: string;
}

/**
 * One declared import and every copy it governs.
 *
 * Most imports govern one tree. An import may govern several copies when one
 * producer export covers them all — the reference of each stack extension, say
 * — and the snapshot then holds them as one set: a missing member is a missing
 * set, so the build fails exactly as it would for a missing tree.
 */
export interface DocumentationSourceContract {
  /** The declared import these copies are governed by. */
  readonly contract: ArchitectureImport;
  /** Every copy the import governs, in the order `putnami.json` declares them. */
  readonly copies: readonly DocumentationCopy[];
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

/** The reference page of each stack extension, published under the languages section. */
const STACK_REFERENCES = ['dart', 'dotnet', 'java', 'php', 'ruby', 'rust', 'swift'] as const;

/** The reference page of each monorepo tool integration, published under the monorepo tools section. */
const MONOREPO_TOOL_REFERENCES = ['bazel', 'moon', 'nx', 'pants', 'rush', 'turbo'] as const;

/**
 * The repository-owned documentation sources, in the order
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
    copies: [{ sourceRoot: '/tooling/doc', section: 'public/docs/08-tooling-&-workspace' }],
  },
  {
    contract: documentationImport({
      id: 'public-docs.observe-documentation.v1',
      as: 'public-docs.observe-documentation',
      domain: 'extension-providers',
      exportId: 'extension-providers.observe-documentation.v1',
      fact: 'observe_documentation',
      justification:
        'The site publishes the observe reference at public/docs/08-tooling-&-workspace/08-observe by copying the /tooling/observe/doc tree verbatim at build time. The extension-providers domain decides what that reference says about what observe reads; the site decides only where it appears.',
    }),
    copies: [{ sourceRoot: '/tooling/observe/doc', section: 'public/docs/08-tooling-&-workspace/08-observe' }],
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
    copies: [{ sourceRoot: '/tooling/sdd-extension/doc', section: 'public/docs/07-spec-driven-development' }],
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
    copies: [{ sourceRoot: '/typescript/doc/framework', section: 'public/docs/09-frameworks/01-typescript' }],
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
    copies: [{ sourceRoot: '/go/doc/framework', section: 'public/docs/09-frameworks/02-go' }],
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
    copies: [{ sourceRoot: '/python/doc/framework', section: 'public/docs/09-frameworks/03-python' }],
  },
  {
    contract: documentationImport({
      id: 'public-docs.stack-documentation.v1',
      as: 'public-docs.stack-documentation',
      domain: 'stacks',
      exportId: 'stacks.stack-documentation.v1',
      fact: 'stack_extension_documentation',
      justification:
        'The site publishes the reference page of each stack extension under public/docs/11-languages by copying /<language>/extension/doc/01-<language>.md verbatim at build time, one copy per extension, all governed by this one import. The stacks domain decides what each reference says; the site decides only that they appear together, under one section, next to the frameworks. A missing reference fails the build rather than publishing a section with a language missing.',
    }),
    copies: STACK_REFERENCES.map((language) => ({
      sourceRoot: `/${language}/extension/doc/01-${language}.md`,
      section: `public/docs/11-languages/${language}.md`,
    })),
  },
  {
    contract: documentationImport({
      id: 'public-docs.monorepo-tool-documentation.v1',
      as: 'public-docs.monorepo-tool-documentation',
      domain: 'stacks',
      exportId: 'stacks.monorepo-tool-documentation.v1',
      fact: 'monorepo_tool_documentation',
      justification:
        'The site publishes the reference page of each monorepo tool integration under public/docs/12-monorepo-tools by copying /integrations/<tool>/doc/01-<tool>.md verbatim at build time, one copy per integration, all governed by this one import. The stacks domain decides what each reference says about running a tool; the site decides only that they appear together, under one section. A missing reference fails the build rather than publishing a section with a tool missing.',
    }),
    copies: MONOREPO_TOOL_REFERENCES.map((tool) => ({
      sourceRoot: `/integrations/${tool}/doc/01-${tool}.md`,
      section: `public/docs/12-monorepo-tools/${tool}.md`,
    })),
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

/**
 * The wire contract this site implements: `site-content-bundle/v1`, owned by
 * `/protocols/sitecontent`.
 *
 * `src/lib/content/` verifies and mounts content bundles by that contract's
 * rules, and the site's tests prove the protocol's requirements. Like the
 * runtime reference it copies nothing, so it declares no transport and names
 * the one fact it consumes.
 */
export const PROTOCOL_CONTRACTS_IMPORT: ArchitectureImport = {
  id: 'public-docs.protocol-contracts.v1',
  version: 1,
  from: { domain: 'protocols', export: 'protocols.wire-contracts.v1' },
  as: 'public-docs.protocol-contracts',
  mode: 'reference',
  status: 'active',
  facts: ['wire_contract_definitions'],
  justification:
    "putnami.dev implements the site-content-bundle/v1 contract that /protocols/sitecontent owns: it verifies and mounts content bundles by that contract's rules, and its tests prove the protocol's requirements. The protocols domain owns what the contract means; the site holds a reference to it and owns none of its meaning.",
};

/** Every contract this domain declares, snapshots first, in declaration order. */
export const PUBLIC_DOCS_CONTRACTS: readonly ArchitectureImport[] = [
  ...DOCUMENTATION_SOURCES.map((source) => source.contract),
  APPLICATION_RUNTIME_IMPORT,
  PROTOCOL_CONTRACTS_IMPORT,
];
