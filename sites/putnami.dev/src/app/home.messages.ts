import { messages } from '@putnami/web';

/**
 * The home page copy. A paragraph that carries inline markup is split at the
 * markup, so each part is one message.
 */
export default messages('home', {
  metaTitle: 'Putnami — Declare the system once',
  description:
    'A workspace model, framework primitives for Go and TypeScript, and a layer of wire protocols underneath them. Your build, your CI, your infrastructure and your agents read the same contracts.',

  heroPill: 'Workspace · Protocols · Agent-operable',
  heroTitle: 'Declare the system once. Humans review it. Agents operate it.',
  heroLede:
    'Putnami is a workspace model, framework primitives for Go and TypeScript, and a layer of wire protocols underneath them. Your build, your CI, your infrastructure and your agents all read the same contracts — from your laptop to production.',
  getStarted: 'Get started',
  whyPutnami: 'Why Putnami →',

  problemLabel: 'Why this exists',
  problemTitle: 'Your system is real. Your description of it isn’t.',
  pain1Title: 'The system only exists at runtime',
  pain1Desc:
    'Config in five dialects, infrastructure in another repo, CI in provider YAML. Nothing in git states what the system is — only fragments of how to rebuild it, if you already know.',
  pain2Title: 'Every boundary is a convention',
  pain2Desc:
    'The CLI knows what the framework emits because the same person wrote both. Add a language, a second consumer, or an agent, and the convention lives only in someone’s head.',
  pain3Title: 'Drift isn’t unfixed. It’s invisible.',
  pain3Desc:
    'Local, CI and production diverge quietly, because nothing in the system compares them. You find out at the worst possible moment.',
  pain4Title: 'Agents can edit. They can’t operate.',
  pain4Desc:
    'An agent can write code against your repo. Operating your system — knowing the blast radius, reading the state, undoing a mistake — needs a contract, and you don’t have one to hand it.',

  chainLabel: 'How it works',
  chainTitle: 'One declaration. Everything downstream reads it.',
  chainLede:
    'You write intent next to the code it describes. Putnami derives the rest, deterministically, and publishes it as a contract anyone can parse — including the tools you didn’t write.',
  chainDeclare: 'You declare',
  chainDerives: 'Putnami derives',
  chainReads: 'Who reads it',
  chain1Derives: 'the workspace graph',
  chain1Reads: 'the CLI, every extension, every task',
  chain2Declare: 'an HTTP handler',
  chain2Derives: 'a route inventory',
  chain2Reads: 'your edge, as a generated default-deny allowlist',
  chain3Declare: 'an infra need, next to the code',
  chain3Derives: 'one merged manifest, with provenance',
  chain3Reads: 'your deploy target',
  chain4Derives: 'a validated job graph',
  chain4Reads: 'any execution plane — it reads the document, it never widens it',
  chain5Declare: 'a migration, a repository',
  chain5Derives: 'a bundle digest',
  chain5Reads: 'Go and TypeScript adapters, the test provisioner',
  chain6Declare: '— nothing. it’s derived',
  chain6Derives: 'the agent context',
  chain6Reads: 'your agent, through the CLI',
  branchLead: 'Your branch is already an environment.',
  branchBody:
    'Git carries the intent — what changed, by whom, against what — and every document above is derived from it, so one workspace structure, one set of commands and one set of contracts hold from your laptop through CI to whatever runs it. Managed per-branch previews and deploys are where this is going; nothing in the chain requires an account.',
  chainCaption:
    'Every row above is a wire protocol: a published schema, a corpus of valid and invalid fixtures, and a strict parser in each language that implements it.',
  readProtocols: 'Read the protocols →',

  operateLabel: 'Built to be operated',
  operateTitle: 'An agent doesn’t need to be taught your repository.',
  operateLede: 'It reads the same contracts your build reads.',
  contextPackBefore:
    'A deterministic per-project orientation document: identity and dependency graph, composition roots, capability and contract references, representative source ranges, tests, docs, provenance. Aggregated',
  contextPackStrong: 'by reference',
  contextPackAfter:
    '— paths, digests and ranges, never file contents — behind a fail-closed publish-safety gate. Printed read-only by the CLI.',
  impacted:
    'The blast radius of a change, before anything acts on it. An agent that knows what it is about to affect is an agent you can let run.',
  outputModes:
    'Exit codes, output modes and machine documents are a versioned contract, not an implementation detail that moves under a parser. The same corpus validates the Go and the TypeScript CLI.',
  doctor:
    'Production-readiness findings under a deployment profile, a frozen check-code taxonomy, and a remediation baked into every check. “What’s wrong and what do I do about it” is data, not prose.',
  operateCaption:
    'One JSONL event stream per job, so the agent watching a build and the human reading the log are parsing the same lines.',

  protocolsLabel: 'Why you can check any of this',
  protocolsTitle: 'One package per boundary. No shared assumptions.',
  protocolsLede:
    'The frameworks, the tooling and the platform don’t integrate through shared code. They integrate through protocols: a published schema, a corpus of valid and invalid fixtures, a strict parser in every language that implements it, and at least one real consumer.',
  protocolsQuote: 'A protocol without consumers is a spec, not a contract.',
  matrixBefore:
    'The repository publishes a conformance matrix — who implements or consumes each contract, and how it’s verified. It has three states, and the middle one is published on purpose:',
  matrixTested: 'tested',
  matrixAligned: 'aligned by hand',
  matrixAlignedNote: '(drift possible), and',
  matrixNotConsumer: 'not a consumer',
  matrixAfter: '. A matrix that only showed green would be marketing.',
  supportStatus: 'Support status →',

  principlesLabel: 'Principles',
  principlesTitle: 'Constraints, not aspirations. Violations are bugs.',
  principle1Title: 'Observable by construction',
  principle1Desc:
    'Every runtime emits structured signals by default. Local, preview and production expose the same shape.',
  principle2Title: 'Performance is a constraint',
  principle2Desc:
    'Defaults must survive production load. Regressions are detectable in CI. Abstractions expose their cost.',
  principle3Title: 'Deterministic and reviewable',
  principle3Desc: 'Git defines system intent. Deployments are pure functions of versioned state. Drift is detectable.',
  principle4Title: 'Security is foundational',
  principle4Desc:
    'Secure defaults are enforced. The system fails closed. Unsafe paths require explicit acknowledgment.',
  principle5Title: 'Data ownership is non-negotiable',
  principle5Desc: 'Data lives in stores you control. Schemas are versioned, portable, and part of the contract.',
  principle6Title: 'Automation is a first-class user',
  readPrinciples: 'Read the full principles →',
  principle6Desc:
    'The CLI is the primary interface. Output is deterministic and machine-readable, so humans and machines follow the same rules.',

  surfacesLabel: 'The shape of the product',
  surfacesTitle: 'Three surfaces. One declaration.',
  surfaceDev:
    'Structural precision. The workspace model, the framework primitives, the CLI, and the protocol layer underneath them. Complete locally, no account required. FSL-1.1-MIT.',
  surfaceCloud:
    'Operational depth on the same documents: release provenance, runtime correlation, incident context, rollout protection. It reads your route inventory to build an allowlist, your infra manifest to provision, your CI document to execute. It gets no private dialect.',
  surfaceIntelligence:
    'What ships today: agent orientation through the CLI, a versioned workspace index, and spec-driven development — features, specs and architecture recorded next to the code. The review-and-audit loop, with freshness and receipts on every finding, is where this is going. It stands alone: useful with neither Putnami frameworks nor Putnami Cloud.',
  surfacesCaption:
    'Adopting putnami.dev sharpens what Intelligence knows about your structure. Adopting Cloud deepens what it knows about your operations. Neither is a prerequisite — and Cloud integrates through the same published contracts as any third-party adapter. Its advantage is zero-configuration correlation, not a private backchannel.',

  docsLabel: 'One docs model, every surface',
  docsTitle: 'Tooling first. Language depth when you need it.',
  pageCount: '{count, plural, one {# page} other {# pages}}',
  openDocs: 'Open the documentation hub →',

  ctaTitle: 'Start with a workspace.',
  ctaLede:
    'Everything above runs on your machine, without an account. The platform is there when operating it stops being your job.',
  viewOnGithub: 'View on GitHub',
});
