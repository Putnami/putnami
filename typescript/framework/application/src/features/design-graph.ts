import { randomUUID } from 'node:crypto';
import { mkdir, rm } from 'node:fs/promises';
import { isMigrationContributor } from '@putnami/migration';
import { isConfigContributor, tokenName, useLogger } from '@putnami/runtime';
import { robustRemove, robustRename } from '@putnami/runtime/robustio';
import { getCurrentProject, getDirectoryName, getProjectRoot, joinPath, relativePath } from '@putnami/utils';
import type { Module } from '../application/module';
import type { Plugin } from '../application/module.types';
import type { ContributionKind, InfraKind } from '../capabilities/manifest.types';
import { isLifecycleContributor } from '../capabilities/lifecycle';

export const DESIGN_GRAPH_COMPATIBILITY = 'provisional' as const;
export const DESIGN_GRAPH_ARTIFACT = 'design/graph.json' as const;

export type DesignNodeKind =
  | 'feature'
  | 'module'
  | 'api.operation'
  | 'api.schema'
  | 'service'
  | 'data.schema'
  | 'data.table'
  | 'data.migration'
  | 'event.topic'
  | 'event.outbox'
  | 'event.handler'
  | 'client.generated'
  // A hand-written client that declares which producer it calls. Kept distinct
  // from 'client.generated' so a hand-written call is never read as
  // generated-client authority. Go owns the authoring API today; this runtime
  // only has to agree on what is valid on the wire.
  | 'client.typed'
  // Workspace-level kinds minted by tooling from native workspace declarations
  // (putnami.json), never by this runtime: a framework producer only knows its
  // own project. They stay distinct kinds so a project or command can never be
  // promoted to a product feature by naming convention; this runtime only has
  // to agree on what is valid on the wire.
  | 'project'
  | 'command'
  // Native composition facts. These are technical nodes contained by the
  // feature's module; none is inferred into product intent by name.
  | 'config'
  | 'infra'
  | 'lifecycle'
  | 'test';

export type DesignEdgeKind =
  | 'implementedBy'
  | 'contains'
  | 'exposes'
  | 'injects'
  | 'accepts'
  | 'returns'
  | 'reads'
  | 'writes'
  | 'enqueues'
  | 'publishes'
  | 'subscribes'
  | 'generatedFrom'
  | 'calls'
  // One direct project-to-project workspace dependency, minted by tooling.
  // Never transitive: an indirect dependency is absent rather than invented.
  | 'dependsOn';

/**
 * Authority levels, strongest first. `currently-unmodeled` is the weakest and is
 * not a guess: the relationship is natively declared but its producer lineage
 * cannot be proven exactly, and the framework refuses to invent it.
 */
export type DesignAuthority = 'exact' | 'derived' | 'heuristic' | 'currently-unmodeled';

export interface DesignProvenance {
  path: string;
  line?: number;
  symbol?: string;
}

export interface DesignNode {
  id: string;
  kind: DesignNodeKind;
  name: string;
  properties?: Record<string, string>;
  provenance?: DesignProvenance;
}

export interface DesignEdge {
  from: string;
  to: string;
  kind: DesignEdgeKind;
  authority: DesignAuthority;
  properties?: Record<string, string>;
  provenance?: DesignProvenance;
}

export interface DesignGraph {
  compatibility: typeof DESIGN_GRAPH_COMPATIBILITY;
  project: string;
  nodes: DesignNode[];
  edges: DesignEdge[];
}

/**
 * One contribution of the project being built. It deliberately carries no owner
 * project: the describe producer owns exactly one project and stamps it, so a
 * workload cannot claim a contribution belonging to one of its dependencies.
 */
export interface FeatureContributionRef {
  readonly kind: ContributionKind;
  readonly subkind?: string;
  readonly key: string;
}

/**
 * One authored maturity requirement of the declaring feature, paired with the
 * exact contribution that supports it.
 *
 * This is the whole authoring surface for generated technical evidence. The
 * stage comes from the authored requirement in `putnami.features.json`, and the
 * evidence ID, issuer, source binding, provenance, and subject are computed by
 * the build — so a declaration states which fact proves which requirement, and
 * nothing else.
 */
export interface FeatureProof {
  readonly requirement: string;
  readonly contribution: FeatureContributionRef;
}

export interface FeatureDefinition {
  id: string;
  name: string;
  outcome: string;
  owner: string;
  /**
   * Associations that let a build emit Feature Evidence instead of leaving a
   * technical fact unclassified. Inert observational metadata: read only during
   * describe, and never consulted by composition or lifecycle.
   */
  readonly proves?: readonly FeatureProof[];
}

export interface DeclaredFeature extends FeatureDefinition {
  provenance?: DesignProvenance;
}

/**
 * Design-time selection of native owners that one feature declaration composes
 * without owning them in the module tree.
 *
 * A feature is normally implemented by its declaring module and everything below
 * it. Real compositions are wider than one subtree: a library composer mounts
 * sibling modules next to each other, and a workload's root `api()` plugin owns
 * the file routes that call into them. Selecting those owners keeps a single
 * full declaration instead of copied bindings, wrapper modules, or a broad
 * ancestor claim that would attribute unrelated behavior to the feature.
 *
 * The selection is read at build time from the already-composed module tree. It
 * never mounts, re-mounts, or re-orders anything: DI registration, plugin
 * collection order, and lifecycle are untouched.
 */
export interface FeatureComposition {
  /**
   * Modules composed elsewhere in the same application that this feature also
   * implements. Each module is selected by identity, so an unselected sibling
   * stays outside the feature.
   */
  readonly modules?: readonly Module[];

  /**
   * Project-relative source paths — a file, or a directory holding several —
   * whose native declarations belong to this feature. Used to select scanned
   * file routes owned by a workload root plugin that sits outside every feature
   * scope. The longest matching selection wins, as in `relateFromSource`.
   *
   * Paths resolve against the building project's root, and only declarations
   * from an owner outside every feature scope are associated: a surface already
   * inside one is attributed to the feature that owns it, not to the selection.
   */
  readonly sources?: readonly string[];
}

export interface DesignContributor {
  contributeDesign(builder: DesignBuilder): void | Promise<void>;
}

/** One infrastructure fact exposed through the bounded native seam. */
export interface DesignInfraRequirement {
  readonly name: string;
  readonly kind: InfraKind;
  readonly provenance?: DesignProvenance;
  /**
   * Native declaration sources that may attribute a root-owned resource to a
   * feature. Several declarations can require the same logical resource, so
   * these locations belong on ownership edges rather than in node identity.
   */
  readonly sources?: readonly DesignProvenance[];
}

/**
 * Native infrastructure contributor. Unlike DesignContributor this can only
 * report the logical resources an ordinary framework registry already owns.
 */
export interface InfraContributor {
  designInfraRequirements(): readonly DesignInfraRequirement[];
}

export function isInfraContributor(value: unknown): value is InfraContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'designInfraRequirements' in value &&
    typeof (value as InfraContributor).designInfraRequirements === 'function'
  );
}

/** Closed classifications for explicitly registered tests. */
export type DesignTestKind = 'unit' | 'integration' | 'conformance' | 'e2e';

/**
 * Binds one declared test to an authored executable-spec check: the feature,
 * the feature-local requirement, and the exact check ID from the manifest's
 * verification criterion. Discoverability only — the graph shows
 * which declared test is EXPECTED to protect which check, but a declared
 * binding can never itself prove a passing test: only the test adapter's
 * observed verdict does.
 */
export interface DesignTestProof {
  readonly feature: string;
  readonly requirement: string;
  readonly check: string;
}

export interface DesignTest {
  readonly name: string;
  readonly kind: DesignTestKind;
  readonly proves?: readonly DesignTestProof[];
  readonly provenance?: DesignProvenance;
}

/** Bounded test registration seam; it never scans or infers test files. */
export interface TestContributor {
  designTests(): readonly DesignTest[];
}

export function isTestContributor(value: unknown): value is TestContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'designTests' in value &&
    typeof (value as TestContributor).designTests === 'function'
  );
}

/**
 * Projects a test's declared spec-check bindings onto one deterministic
 * scalar node property: sorted, de-duplicated `feature#requirement#check`
 * triplets joined by a single space. A partial binding is an authoring
 * error, not a guess; an empty list is simply absent. The encoding is shared
 * verbatim with the Go design producer
 * (go/framework/app/design_graph.go: designTestProvesProperty) so the
 * native-design equivalence golden pins both.
 */
function designTestProvesProperty(testName: string, proofs?: readonly DesignTestProof[]): string {
  if (!proofs || proofs.length === 0) {
    return '';
  }
  const entries = new Set<string>();
  for (const proof of proofs) {
    const feature = proof.feature.trim();
    const requirement = proof.requirement.trim();
    const check = proof.check.trim();
    if (!feature || !requirement || !check) {
      throw new Error(`design test ${testName}: a test proof must name feature, requirement, and check`);
    }
    entries.add(`${feature}#${requirement}#${check}`);
  }
  return [...entries].sort().join(' ');
}

/**
 * The design identity of one injectable dependency, keyed by its DI token label
 * ({@link tokenName}).
 *
 * Every producer that observes the same token must mint a byte-identical node:
 * {@link DesignBuilder.addNode} rejects two differing declarations of one id, and
 * reconciling them is the point — an endpoint that injects a repository token,
 * the module registration that provides it, and the database plugin that knows
 * which table it targets all describe the same service. Keeping the shape in one
 * function is what stops those three producers from drifting into a conflict
 * that fails a build.
 */
export function designServiceNode(token: string): DesignNode {
  return { id: `service:${token}`, kind: 'service', name: token };
}

/**
 * The design identity of one datasource namespace. A migration source declares
 * the schema it writes into while a `Table()` declares the schema it lives in;
 * both name the same namespace, so both mint this node. `engine` is part of the
 * shape because omitting it in one producer would make the two declarations
 * conflict on the first project that has a migration and a table in one schema.
 */
export function designDataSchemaNode(declaration: { datasource: string; engine: string; schema?: string }): DesignNode {
  const schema = declaration.schema ?? '';
  return {
    id: `data.schema:${declaration.datasource}${schema ? `:${schema}` : ''}`,
    kind: 'data.schema',
    name: schema || declaration.datasource,
    properties: {
      datasource: declaration.datasource,
      engine: declaration.engine,
      ...(schema ? { schema } : {}),
    },
  };
}

export function isDesignContributor(value: unknown): value is DesignContributor {
  return (
    typeof value === 'object' &&
    value !== null &&
    'contributeDesign' in value &&
    typeof (value as DesignContributor).contributeDesign === 'function'
  );
}

/**
 * A plugin that composes other plugins privately instead of mounting them on
 * the module tree. Only plugins reachable from `collectPlugins()` receive a
 * builder, so a controller that owns an inner `events()`/`sql()` plugin would
 * otherwise drop its native contributions — the gap that forces graph-only
 * wrapper plugins to restate facts the inner plugin already owns.
 *
 * Delegates contribute under the SAME owning module, and the seam is read only
 * while the disposable design graph is built: it never changes plugin
 * lifecycle, hook ordering, or dependency injection.
 */
export interface DesignDelegate {
  designDelegates(): readonly Plugin[];
}

export function isDesignDelegate(value: unknown): value is DesignDelegate {
  return (
    typeof value === 'object' &&
    value !== null &&
    'designDelegates' in value &&
    typeof (value as DesignDelegate).designDelegates === 'function'
  );
}

/**
 * A contributor whose declarations each carry their own source location, so the
 * feature that owns one is decided by containment rather than by the module the
 * plugin happens to be registered on.
 *
 * An application-wide plugin is registered once on the application root — the
 * SQL plugin owns one runner and one primary datasource for the whole workload —
 * and that root normally declares no feature. Scoping such a contributor to its
 * owner would silently drop every declaration it reports for the documented
 * composition. Opting in runs it with an unscoped builder instead:
 * `relateFromModule` no-ops, while `relateFromSource` still attributes each
 * declaration to the feature directory that contains it. A declaration outside
 * every feature directory stays an unreachable node, which no feature
 * projection selects.
 */
export interface SourceScopedDesignContributor extends DesignContributor {
  readonly designAttribution: 'source';
}

export function isSourceScopedDesignContributor(value: unknown): value is SourceScopedDesignContributor {
  return isDesignContributor(value) && hasSourceScopedDesignAttribution(value);
}

/** Source-attributed native infrastructure without arbitrary graph mutation. */
export interface SourceScopedInfraContributor extends InfraContributor {
  readonly designAttribution: 'source';
}

/** Every bounded native contributor that can attribute root-owned facts by source. */
export type SourceScopedContributor = SourceScopedDesignContributor | SourceScopedInfraContributor;

export function isSourceScopedContributor(value: unknown): value is SourceScopedContributor {
  return hasSourceScopedDesignAttribution(value) && (isDesignContributor(value) || isInfraContributor(value));
}

function hasSourceScopedDesignAttribution(value: unknown): boolean {
  return (
    typeof value === 'object' &&
    value !== null &&
    'designAttribution' in value &&
    (value as { designAttribution?: unknown }).designAttribution === 'source'
  );
}

interface FeatureScope {
  directory: string;
  moduleId: string;
  featureId: string;
}

/** One source path a feature declaration selected, resolved to its owner. */
interface SelectedSource {
  path: string;
  moduleId: string;
  feature: string;
}

/** One module a feature declaration selected, kept with its declaring module. */
interface ModuleClaim {
  feature: DeclaredFeature;
  declaredBy: Module;
}

interface GraphState {
  readonly projectRoot: string;
  readonly project: string;
  readonly nodes: Map<string, DesignNode>;
  readonly edges: Map<string, DesignEdge>;
  readonly moduleIds: Map<Module, string>;
  readonly featureIds: Map<Module, string>;
  readonly scopes: FeatureScope[];
  readonly moduleClaims: Map<Module, ModuleClaim>;
  readonly claimedModules: Set<Module>;
  readonly selectedSources: SelectedSource[];
  readonly matchedSources: Set<string>;
}

/** A native contributor receives this builder already scoped to its owner. */
export class DesignBuilder {
  constructor(
    protected readonly state: GraphState,
    readonly moduleId = '',
    readonly featureId = '',
  ) {}

  addNode(node: DesignNode): void {
    const normalized = normalizeNode(node, this.state.projectRoot);
    const previous = this.state.nodes.get(normalized.id);
    if (previous && stableStringify(previous) !== stableStringify(normalized)) {
      throw new Error(`design node '${normalized.id}' has conflicting native declarations`);
    }
    this.state.nodes.set(normalized.id, normalized);
  }

  addEdge(edge: DesignEdge): void {
    const normalized = normalizeEdge(edge, this.state.projectRoot);
    const key = `${normalized.from}\0${normalized.to}\0${normalized.kind}`;
    const previous = this.state.edges.get(key);
    if (previous && stableStringify(previous) !== stableStringify(normalized)) {
      const { provenance: previousProvenance, ...previousFact } = previous;
      const { provenance: candidateProvenance, ...candidateFact } = normalized;
      if (stableStringify(previousFact) === stableStringify(candidateFact)) {
        if (
          candidateProvenance &&
          (!previousProvenance || compareDesignProvenance(candidateProvenance, previousProvenance) < 0)
        ) {
          this.state.edges.set(key, normalized);
        }
        return;
      }
      throw new Error(
        `design edge '${normalized.from}' -> '${normalized.to}' (${normalized.kind}) has conflicting native declarations`,
      );
    }
    this.state.edges.set(key, normalized);
  }

  relateFromModule(to: string, kind: DesignEdgeKind, authority: DesignAuthority = 'exact'): void {
    if (!this.moduleId) return;
    this.addEdge({ from: this.moduleId, to, kind, authority });
  }

  /**
   * Associate a native declaration with the closest feature source directory.
   * The technical fact remains exact; only source-containment attribution is
   * marked derived and is therefore never presented as stronger evidence.
   */
  relateFromSource(sourcePath: string, to: string, kind: DesignEdgeKind): void {
    const source = normalizePath(sourcePath, this.state.projectRoot);
    const scope = [...this.state.scopes]
      .filter(({ directory }) => source === directory || source.startsWith(`${directory}/`))
      .sort((left, right) => right.directory.length - left.directory.length)[0];
    if (scope) {
      this.addEdge({
        from: scope.moduleId,
        to,
        kind,
        authority: 'derived',
        provenance: { path: source },
      });
      return;
    }
    const selection = matchSelectedSource(this.state, source);
    if (!selection) return;
    this.state.matchedSources.add(selection.path);
    this.addEdge({
      from: selection.moduleId,
      to,
      kind,
      authority: 'derived',
      properties: { association: 'selected' },
      provenance: { path: source },
    });
  }
}

/**
 * Builder handed to a contributor whose owner module sits outside every feature
 * scope — typically a workload root `api()` plugin that scans file routes for
 * modules it does not own.
 *
 * Such a contributor must not be able to claim the graph, so nothing it emits is
 * written directly. Declarations are buffered, and only the subgraph seeded by a
 * node whose declaration source a feature explicitly selected is committed,
 * followed forward so a selected operation keeps its own schemas and injected
 * services. An unselected route, and any contributor no feature selected from,
 * is discarded exactly as before.
 */
class AssociationBuilder extends DesignBuilder {
  private readonly buffered = new Map<string, DesignNode>();
  private readonly relations: DesignEdge[] = [];
  private readonly moduleRelations: Array<{ to: string; kind: DesignEdgeKind }> = [];
  private readonly sourceRelations: Array<{ source: string; to: string; kind: DesignEdgeKind }> = [];

  override addNode(node: DesignNode): void {
    const normalized = normalizeNode(node, this.state.projectRoot);
    const previous = this.buffered.get(normalized.id);
    if (previous && stableStringify(previous) !== stableStringify(normalized)) {
      throw new Error(`design node '${normalized.id}' has conflicting native declarations`);
    }
    this.buffered.set(normalized.id, normalized);
  }

  override addEdge(edge: DesignEdge): void {
    this.relations.push(normalizeEdge(edge, this.state.projectRoot));
  }

  override relateFromModule(to: string, kind: DesignEdgeKind): void {
    // The owner module is outside every feature scope, so it has no graph
    // identity to relate from. The relationship is resolved at commit time
    // against whichever feature selected the target's declaration source.
    this.moduleRelations.push({ to, kind });
  }

  override relateFromSource(sourcePath: string, to: string, kind: DesignEdgeKind): void {
    this.sourceRelations.push({ source: normalizePath(sourcePath, this.state.projectRoot), to, kind });
  }

  /** Write the selected subgraph into the shared state. */
  commit(): void {
    const claims = new Map<string, SelectedSource[]>();
    const addClaim = (id: string, selection: SelectedSource): void => {
      const existing = claims.get(id) ?? [];
      if (!existing.some(({ moduleId }) => moduleId === selection.moduleId)) existing.push(selection);
      claims.set(id, existing);
      this.state.matchedSources.add(selection.path);
    };
    for (const node of this.buffered.values()) {
      const selection = matchSelectedSource(this.state, node.provenance?.path);
      if (!selection) continue;
      addClaim(node.id, selection);
    }
    for (const relation of this.sourceRelations) {
      const selection = matchSelectedSource(this.state, relation.source);
      if (selection) addClaim(relation.to, selection);
    }
    if (claims.size === 0) return;

    const reachable = new Set(claims.keys());
    for (let grown = true; grown; ) {
      grown = false;
      for (const relation of this.relations) {
        if (!reachable.has(relation.from) || reachable.has(relation.to) || !this.buffered.has(relation.to)) continue;
        reachable.add(relation.to);
        grown = true;
      }
    }

    const target = new DesignBuilder(this.state);
    for (const id of reachable) target.addNode(this.buffered.get(id) as DesignNode);
    for (const relation of this.relations) {
      if (reachable.has(relation.from) && reachable.has(relation.to)) target.addEdge(relation);
    }
    for (const { to, kind } of this.moduleRelations) {
      const selections = claims.get(to) ?? [];
      // The author selected a source path, not this declaration: the framework
      // still has to resolve which declarations live there. That inference is
      // the same class of evidence as source-directory containment, so it is
      // recorded as derived and carries the matched source as provenance.
      for (const selection of selections) {
        target.addEdge({
          from: selection.moduleId,
          to,
          kind,
          authority: 'derived',
          properties: { association: 'selected' },
          provenance: { path: (this.buffered.get(to) as DesignNode).provenance?.path ?? selection.path },
        });
      }
    }
    for (const { source, to, kind } of this.sourceRelations) {
      if (!reachable.has(to)) continue;
      const selection = matchSelectedSource(this.state, source);
      if (!selection) continue;
      target.addEdge({
        from: selection.moduleId,
        to,
        kind,
        authority: 'derived',
        properties: { association: 'selected' },
        provenance: { path: source },
      });
    }
  }
}

/** Longest explicit source selection containing `path`, if any. */
function matchSelectedSource(state: GraphState, path: string | undefined): SelectedSource | undefined {
  if (!path) return undefined;
  return [...state.selectedSources]
    .filter((selection) => path === selection.path || path.startsWith(`${selection.path}/`))
    .sort((left, right) => right.path.length - left.path.length)[0];
}

export async function emitDesignGraph(application: Module): Promise<void> {
  const projectRoot = getProjectRoot();
  const output = joinPath(projectRoot, '.gen', DESIGN_GRAPH_ARTIFACT);
  if (!application.declaresAnyFeature()) {
    await robustRemove(output);
    return;
  }
  const project = getCurrentProject().name;
  const graph = await buildDesignGraph(application, project, projectRoot);
  if (!graph) {
    await robustRemove(output);
    return;
  }

  validateDesignGraph(graph);
  const directory = joinPath(projectRoot, '.gen', 'design');
  const temporary = `${output}.${randomUUID()}.tmp`;
  await mkdir(directory, { recursive: true });
  await Bun.write(temporary, serializeDesignGraph(graph));
  try {
    await robustRename(temporary, output);
  } catch (error) {
    await rm(temporary, { force: true });
    throw error;
  }
}

export async function buildDesignGraph(
  application: Module,
  project: string,
  projectRoot: string,
): Promise<DesignGraph | undefined> {
  const state: GraphState = {
    projectRoot,
    project,
    nodes: new Map(),
    edges: new Map(),
    moduleIds: new Map(),
    featureIds: new Map(),
    scopes: [],
    moduleClaims: new Map(),
    claimedModules: new Set(),
    selectedSources: [],
    matchedSources: new Set(),
  };
  // A selected sibling can be composed before the module that selects it, so
  // claims are collected over the whole tree before identities are assigned.
  collectFeatureClaims(state, application);
  collectModules(state, application, undefined, '', project, [], true);
  if (state.featureIds.size === 0) return undefined;

  for (const { plugin, owner } of application.collectPlugins()) {
    const moduleId = state.moduleIds.get(owner);
    const featureId = state.featureIds.get(owner);
    // A contributor is native to the feature scope of its owner. Running one
    // with an empty builder would leak orphan nodes from an unrelated module
    // and could make an otherwise valid scoped declaration conflict. Two opt-in
    // escapes exist for a contributor whose owner is outside every feature
    // scope: a source-attributed contributor is registered once for the whole
    // workload and locates every declaration itself via `relateFromSource`, so
    // scoping it to its owner would drop all of them; an association builder
    // instead commits only the subgraph a feature's explicit source selection
    // reaches, for a contributor that has no attribution opinion of its own.
    if (!moduleId || !featureId) {
      for (const contributor of expandDesignDelegates(plugin)) {
        if (isSourceScopedContributor(contributor)) {
          const sourceScoped = new DesignBuilder(state);
          if (isDesignContributor(contributor)) await contributor.contributeDesign(sourceScoped);
          contributeNativePluginDesign(sourceScoped, contributor);
        } else if (
          state.selectedSources.length > 0 &&
          (isDesignContributor(contributor) || isInfraContributor(contributor))
        ) {
          const association = new AssociationBuilder(state);
          if (isDesignContributor(contributor)) await contributor.contributeDesign(association);
          contributeNativePluginDesign(association, contributor);
          association.commit();
        }
      }
      continue;
    }
    const builder = new DesignBuilder(state, moduleId, featureId);
    for (const contributor of expandDesignDelegates(plugin)) {
      if (isDesignContributor(contributor)) {
        await contributor.contributeDesign(builder);
      }
      contributeNativePluginDesign(builder, contributor);
      contributeMigrations(builder, contributor);
    }
  }
  reportUnmatchedSelections(state);

  // Contributors derive their relationships independently — the client generator
  // synthesises generatedFrom edges from the OpenAPI IR while the api plugin
  // creates the operation nodes — so drift between them leaves an edge with no
  // endpoint. This graph is a provisional, disposable projection: drop those
  // edges and warn rather than failing a build on a derived artifact.
  const nodes = [...state.nodes.values()].sort((left, right) => compare(left.id, right.id));
  const known = new Set(nodes.map(({ id }) => id));
  const edges: DesignEdge[] = [];
  const dropped: string[] = [];
  for (const edge of [...state.edges.values()].sort(compareEdges)) {
    if (known.has(edge.from) && known.has(edge.to)) edges.push(edge);
    else dropped.push(`${edge.from} -${edge.kind}-> ${edge.to}`);
  }
  if (dropped.length > 0) {
    useLogger('design').warn(
      `dropped ${dropped.length} design relationship(s) with no matching node: ${dropped.join(', ')}`,
    );
  }

  const graph: DesignGraph = { compatibility: DESIGN_GRAPH_COMPATIBILITY, project, nodes, edges };
  validateDesignGraph(graph);
  return graph;
}

/**
 * Record every module one feature declaration selects. Selection is by module
 * identity rather than name or path, so it cannot silently widen, and two
 * features claiming the same module is a contradiction rather than a race.
 */
function collectFeatureClaims(state: GraphState, current: Module): void {
  const declared = current.getFeature();
  const composition = typeof current.getFeatureComposition === 'function' ? current.getFeatureComposition() : undefined;
  if (declared && composition) {
    for (const selected of composition.modules ?? []) {
      // Selecting a module selects its subtree, so selecting an ancestor of the
      // declaring module — or that module itself — silently re-creates the broad
      // common-ancestor claim this contract exists to replace: every unrelated
      // descendant would inherit the feature. Only the narrow claim is allowed.
      if (selected === current) {
        throw new Error(`design feature '${declared.id}' selects its own declaring module '${current.name}'`);
      }
      if (typeof selected.collectModules === 'function' && selected.collectModules().includes(current)) {
        throw new Error(
          `design feature '${declared.id}' selects module '${selected.name}', which contains the module '${current.name}' that declares it — a selection cannot claim an ancestor`,
        );
      }
      const previous = state.moduleClaims.get(selected);
      if (previous && previous.feature.id !== declared.id) {
        throw new Error(
          `design module '${selected.name}' is selected by features '${previous.feature.id}' and '${declared.id}'`,
        );
      }
      state.moduleClaims.set(selected, { feature: declared, declaredBy: current });
    }
  }
  for (const child of current.getModules()) {
    collectFeatureClaims(state, child);
  }
}

function collectModules(
  state: GraphState,
  current: Module,
  inherited: DeclaredFeature | undefined,
  parentId: string,
  project: string,
  parentPath: string[],
  root: boolean,
): void {
  const declared = current.getFeature();
  const claim = state.moduleClaims.get(current);
  if (claim) {
    state.claimedModules.add(current);
    // One module implements one outcome. Letting a selection sit on top of a
    // declared or inherited feature would attribute the same module to two
    // features, so the contradiction fails the declaration instead.
    const conflicting = declared ?? inherited;
    if (conflicting) {
      throw new Error(
        `design module '${current.name}' already implements feature '${conflicting.id}' and is selected by feature '${claim.feature.id}' declared on module '${claim.declaredBy.name}'`,
      );
    }
  }
  const feature = declared ?? claim?.feature ?? inherited;
  let path = parentPath;
  let moduleId = '';
  if (feature) {
    // `app` is only the conventional application root. A nested module named
    // `app` is a real path segment and must not collapse onto its parent.
    path = parentId !== '' || !(root && current.name === 'app') ? [...parentPath, current.name] : parentPath;
    moduleId = `module:${[project, ...path].join('/')}`;
    state.moduleIds.set(current, moduleId);
    const featureId = `feature:${feature.id}`;
    state.featureIds.set(current, featureId);
    addNode(state, { id: moduleId, kind: 'module', name: current.name });
    if (declared) {
      addNode(state, {
        id: featureId,
        kind: 'feature',
        name: feature.name,
        properties: { outcome: feature.outcome, owner: feature.owner },
        provenance: feature.provenance,
      });
      addEdge(state, { from: featureId, to: moduleId, kind: 'implementedBy', authority: 'exact' });
      if (feature.provenance?.path) {
        state.scopes.push({
          directory: getDirectoryName(normalizePath(feature.provenance.path, state.projectRoot)).replaceAll('\\', '/'),
          moduleId,
          featureId,
        });
      }
      selectSources(state, current, moduleId, feature.id);
    } else if (claim) {
      // The author named this module object in the feature declaration, so the
      // containment is an explicit assertion with no inference step between the
      // declaration and the module — exact, like a declaration on the module
      // itself. The property keeps the two distinguishable for readers.
      addEdge(state, {
        from: featureId,
        to: moduleId,
        kind: 'implementedBy',
        authority: 'exact',
        properties: { association: 'selected' },
      });
    } else if (parentId) {
      addEdge(state, { from: parentId, to: moduleId, kind: 'contains', authority: 'exact' });
    }
  }
  contributeRegistrations(state, current, moduleId);
  if (moduleId && current.hasShutdownHooks()) {
    contributeLifecycleDesign(new DesignBuilder(state, moduleId, state.featureIds.get(current)), ['stop']);
  }
  for (const child of current.getModules()) {
    collectModules(state, child, feature, moduleId, project, path, false);
  }
}

const INFRA_KINDS = new Set<InfraKind>(['database', 'events', 'storage', 'secret', 'scheduledJob']);
const TEST_KINDS = new Set<DesignTestKind>(['unit', 'integration', 'conformance', 'e2e']);

/** Project bounded native composition facts without arbitrary graph mutation. */
function contributeNativePluginDesign(builder: DesignBuilder, plugin: Plugin): void {
  if (builder.moduleId && isConfigContributor(plugin)) {
    for (const definition of plugin.configDefinitions()) {
      const name = definition.path.trim();
      if (!name) throw new Error('design config definition path is required');
      const id = `config:${name}`;
      builder.addNode({ id, kind: 'config', name, properties: { path: name } });
      builder.relateFromModule(id, 'contains');
    }
  }

  if (isInfraContributor(plugin)) {
    for (const requirement of plugin.designInfraRequirements()) {
      const name = requirement.name.trim();
      if (!name || !INFRA_KINDS.has(requirement.kind)) {
        throw new Error(`design infra requirement must have a name and supported kind`);
      }
      if (!builder.moduleId && (requirement.sources?.length ?? 0) === 0) continue;
      const id = `infra:${requirement.kind}:${name}`;
      builder.addNode({
        id,
        kind: 'infra',
        name,
        properties: { kind: requirement.kind },
      });
      if (builder.moduleId) {
        builder.addEdge({
          from: builder.moduleId,
          to: id,
          kind: 'contains',
          authority: 'exact',
          ...(requirement.provenance ? { provenance: requirement.provenance } : {}),
        });
      } else {
        for (const source of requirement.sources ?? []) {
          if (source.path) builder.relateFromSource(source.path, id, 'contains');
        }
      }
    }
  }

  if (builder.moduleId && isTestContributor(plugin)) {
    for (const test of plugin.designTests()) {
      const name = test.name.trim();
      if (!name || !TEST_KINDS.has(test.kind)) {
        throw new Error(`design test declaration must have a name and supported kind`);
      }
      const id = `test:${test.kind}:${name}`;
      const properties: Record<string, string> = { kind: test.kind };
      const proves = designTestProvesProperty(name, test.proves);
      if (proves) {
        properties['proves'] = proves;
      }
      builder.addNode({
        id,
        kind: 'test',
        name,
        properties,
      });
      builder.addEdge({
        from: builder.moduleId,
        to: id,
        kind: 'contains',
        authority: 'exact',
        ...(test.provenance ? { provenance: test.provenance } : {}),
      });
    }
  }

  if (!builder.moduleId) return;
  const phases = new Set<string>();
  if (typeof plugin.generate === 'function' || typeof plugin.postGenerate === 'function') phases.add('generate');
  if (typeof plugin.warmup === 'function') phases.add('configure');
  if (typeof plugin.migrate === 'function') phases.add('migrate');
  if (typeof plugin.start === 'function') phases.add('start');
  if (typeof plugin.stop === 'function') phases.add('stop');
  if (isLifecycleContributor(plugin)) {
    for (const contribution of plugin.lifecycleContributions()) {
      phases.add(contribution.phase === 'starter' ? 'start' : 'stop');
    }
  }
  contributeLifecycleDesign(builder, [...phases]);
}

/**
 * Module+phase is the stable lifecycle identity both runtimes can promise.
 * Multiple callbacks fold into one node; registration indexes and JavaScript
 * constructor names never become protocol identity.
 */
function contributeLifecycleDesign(builder: DesignBuilder, phases: readonly string[]): void {
  for (const phase of [...new Set(phases)].sort(compare)) {
    const id = `lifecycle:${phase}:${builder.moduleId}`;
    builder.addNode({ id, kind: 'lifecycle', name: phase, properties: { phase } });
    builder.relateFromModule(id, 'contains');
  }
}

/**
 * Expand one collected plugin into itself followed by its transitive design
 * delegates, in declaration order. The visited set is per collected plugin, so
 * behavior is unchanged for a plugin mounted on two modules (it still
 * contributes to both owners) while a delegate cycle or a delegate reachable
 * through two paths of the same owner is visited exactly once.
 */
function expandDesignDelegates(plugin: Plugin): Plugin[] {
  const ordered: Plugin[] = [];
  const visited = new Set<Plugin>();
  const walk = (current: Plugin): void => {
    if (visited.has(current)) return;
    visited.add(current);
    ordered.push(current);
    if (!isDesignDelegate(current)) return;
    for (const delegate of current.designDelegates()) walk(delegate);
  };
  walk(plugin);
  return ordered;
}

/**
 * Project the module's DI registrations. A registration is a native runtime
 * declaration: `provide(Service, { deps: [...] })` names the injectable identity
 * the container resolves and the exact tokens it resolves for it, so both the
 * node and its `injects` edges are read from the composition rather than
 * inferred from source text. `deps` may be incomplete for a factory provider
 * that opted out of `depsComplete` — that omits edges, it never makes a declared
 * one wrong, so every emitted edge stays exact.
 *
 * Identities and dependency edges are emitted for every module, scoped or not,
 * because one application owns one container tree: a service registered on the
 * unscoped application root is still the service a feature-scoped operation
 * injects, and minting the same identity from both ends is what connects an
 * injected repository to the table it targets. Ownership is the only
 * feature-scoped part, so `contains` is emitted just for a module inside a
 * feature scope; a provider no feature reaches stays an unreachable node that no
 * feature projection selects.
 */
function contributeRegistrations(state: GraphState, current: Module, moduleId: string): void {
  for (const { provider } of current.getRegistrations()) {
    const service = designServiceNode(tokenName(provider.token));
    addNode(state, service);
    if (moduleId) {
      addEdge(state, { from: moduleId, to: service.id, kind: 'contains', authority: 'exact' });
    }
    for (const dependency of provider.deps) {
      const injected = designServiceNode(tokenName(dependency));
      addNode(state, injected);
      // Two tokens can share a label — a class and a `named()` token of the same
      // text — and a provider may legally list itself as its own dependency in a
      // dynamic/refresh setup. Neither is a self-relationship worth drawing.
      if (injected.id === service.id) continue;
      addEdge(state, { from: service.id, to: injected.id, kind: 'injects', authority: 'exact' });
    }
  }
}

/** Resolve the source paths a declaring module selected to its graph identity. */
function selectSources(state: GraphState, current: Module, moduleId: string, feature: string): void {
  const composition = typeof current.getFeatureComposition === 'function' ? current.getFeatureComposition() : undefined;
  for (const source of composition?.sources ?? []) {
    const path = normalizePath(source, state.projectRoot).replace(/\/+$/, '');
    if (!path) {
      throw new Error(`design feature '${feature}' selects an empty source path`);
    }
    const previous = state.selectedSources.find((selection) => selection.path === path);
    if (previous && previous.feature !== feature) {
      throw new Error(`design source '${path}' is selected by features '${previous.feature}' and '${feature}'`);
    }
    if (!previous) state.selectedSources.push({ path, moduleId, feature });
  }
}

/**
 * A selection that matched nothing is reported rather than failing the build:
 * scanned-route discovery is itself best-effort, so an unimported loader must
 * not turn a disposable projection into a build failure.
 */
function reportUnmatchedSelections(state: GraphState): void {
  const unmatched: string[] = [];
  for (const [selected, claim] of state.moduleClaims) {
    if (!state.claimedModules.has(selected)) {
      unmatched.push(`module '${selected.name}' selected by '${claim.feature.id}'`);
    }
  }
  for (const selection of state.selectedSources) {
    if (!state.matchedSources.has(selection.path)) {
      unmatched.push(`source '${selection.path}' selected by '${selection.feature}'`);
    }
  }
  if (unmatched.length === 0) return;
  useLogger('design').warn(
    `dropped ${unmatched.length} feature selection(s) matching no native declaration: ${unmatched.sort(compare).join(', ')}`,
  );
}

function contributeMigrations(builder: DesignBuilder, plugin: Plugin): void {
  if (!builder.moduleId || !isMigrationContributor(plugin)) return;
  for (const source of plugin.migrationSources()) {
    const raw = source as unknown as { definitions?: Array<{ name?: string; source?: string }> };
    const definitions = raw.definitions ?? [];
    const migrationIds: string[] = [];
    if (definitions.length === 0) {
      const id = `data.migration:${source.kind}:${source.namespace}`;
      builder.addNode({
        id,
        kind: 'data.migration',
        name: source.namespace,
        properties: { kind: source.kind, namespace: source.namespace },
      });
      builder.relateFromModule(id, 'contains');
      migrationIds.push(id);
    }
    for (const definition of definitions) {
      const name = definition.name ?? `${source.namespace}/migration`;
      const id = `data.migration:${source.kind}:${name}`;
      builder.addNode({
        id,
        kind: 'data.migration',
        name,
        properties: { kind: source.kind, namespace: source.namespace },
        ...(definition.source && !definition.source.includes(':') ? { provenance: { path: definition.source } } : {}),
      });
      builder.relateFromModule(id, 'contains');
      migrationIds.push(id);
    }

    const database = source.infraDatabase?.();
    if (!database) continue;
    const schemas = database.schemas && database.schemas.length > 0 ? database.schemas : [''];
    for (const schema of schemas) {
      const node = designDataSchemaNode({ datasource: database.name, engine: database.engine, schema });
      const id = node.id;
      builder.addNode(node);
      for (const migrationId of migrationIds) {
        builder.addEdge({ from: migrationId, to: id, kind: 'writes', authority: 'exact' });
      }
    }
  }
}

function addNode(state: GraphState, node: DesignNode): void {
  new DesignBuilder(state).addNode(node);
}

function addEdge(state: GraphState, edge: DesignEdge): void {
  new DesignBuilder(state).addEdge(edge);
}

function normalizeNode(node: DesignNode, root: string): DesignNode {
  return {
    id: node.id,
    kind: node.kind,
    name: node.name,
    ...(node.properties ? { properties: sortRecord(node.properties) } : {}),
    ...(node.provenance ? { provenance: normalizeProvenance(node.provenance, root) } : {}),
  };
}

function normalizeEdge(edge: DesignEdge, root: string): DesignEdge {
  return {
    from: edge.from,
    to: edge.to,
    kind: edge.kind,
    authority: edge.authority,
    ...(edge.properties ? { properties: sortRecord(edge.properties) } : {}),
    ...(edge.provenance ? { provenance: normalizeProvenance(edge.provenance, root) } : {}),
  };
}

function normalizeProvenance(provenance: DesignProvenance, root: string): DesignProvenance {
  return {
    path: normalizePath(provenance.path, root),
    ...(provenance.line ? { line: provenance.line } : {}),
    ...(provenance.symbol ? { symbol: provenance.symbol } : {}),
  };
}

function normalizePath(path: string, root: string): string {
  const relative = path.startsWith(root) ? relativePath(root, path) : path;
  return relative.replaceAll('\\', '/');
}

function sortRecord(record: Record<string, string>): Record<string, string> {
  return Object.fromEntries(Object.entries(record).sort(([left], [right]) => compare(left, right)));
}

function compare(left: string, right: string): number {
  return Buffer.compare(Buffer.from(left, 'utf8'), Buffer.from(right, 'utf8'));
}

function compareDesignProvenance(left: DesignProvenance, right: DesignProvenance): number {
  const path = compare(left.path, right.path);
  if (path !== 0) return path;
  const line = (left.line ?? 0) - (right.line ?? 0);
  if (line !== 0) return line;
  return compare(left.symbol ?? '', right.symbol ?? '');
}

function compareEdges(left: DesignEdge, right: DesignEdge): number {
  return (
    compare(left.from, right.from) ||
    compare(left.to, right.to) ||
    compare(left.kind, right.kind) ||
    compare(left.authority, right.authority)
  );
}

function stableStringify(value: unknown): string {
  return JSON.stringify(value);
}

export function serializeDesignGraph(graph: DesignGraph): string {
  // Paths were already made project-relative when the node or edge was added, so
  // re-running normalizePath here would resolve them a second time — and against
  // the process CWD, since an empty root makes `startsWith` vacuously true. Only
  // re-order keys and re-sort; leave the paths alone.
  const canonical: DesignGraph = {
    compatibility: DESIGN_GRAPH_COMPATIBILITY,
    project: graph.project,
    nodes: graph.nodes.map(canonicalNode).sort((left, right) => compare(left.id, right.id)),
    edges: graph.edges.map(canonicalEdge).sort(compareEdges),
  };
  return `${JSON.stringify(canonical, null, 2)}\n`;
}

function canonicalNode(node: DesignNode): DesignNode {
  return {
    id: node.id,
    kind: node.kind,
    name: node.name,
    ...(node.properties ? { properties: sortRecord(node.properties) } : {}),
    ...(node.provenance ? { provenance: canonicalProvenance(node.provenance) } : {}),
  };
}

function canonicalEdge(edge: DesignEdge): DesignEdge {
  return {
    from: edge.from,
    to: edge.to,
    kind: edge.kind,
    authority: edge.authority,
    ...(edge.properties ? { properties: sortRecord(edge.properties) } : {}),
    ...(edge.provenance ? { provenance: canonicalProvenance(edge.provenance) } : {}),
  };
}

function canonicalProvenance(provenance: DesignProvenance): DesignProvenance {
  return {
    path: provenance.path,
    ...(provenance.line ? { line: provenance.line } : {}),
    ...(provenance.symbol ? { symbol: provenance.symbol } : {}),
  };
}

const DESIGN_NODE_KINDS = new Set<string>([
  'feature',
  'module',
  'api.operation',
  'api.schema',
  'service',
  'data.schema',
  'data.table',
  'data.migration',
  'event.topic',
  'event.outbox',
  'event.handler',
  'client.generated',
  'client.typed',
  'project',
  'command',
  'config',
  'infra',
  'lifecycle',
  'test',
]);

const DESIGN_EDGE_KINDS = new Set<string>([
  'implementedBy',
  'contains',
  'exposes',
  'injects',
  'accepts',
  'returns',
  'reads',
  'writes',
  'enqueues',
  'publishes',
  'subscribes',
  'generatedFrom',
  'calls',
  'dependsOn',
]);

const DESIGN_AUTHORITIES = new Set<string>(['exact', 'derived', 'heuristic', 'currently-unmodeled']);

export function validateDesignGraph(graph: DesignGraph): void {
  if (graph.compatibility !== DESIGN_GRAPH_COMPATIBILITY) throw new Error('unsupported design graph compatibility');
  if (!graph.project.trim()) throw new Error('design graph project is required');
  const nodes = new Set<string>();
  for (const node of graph.nodes) {
    if (!node.id || !node.name) throw new Error('design graph node id and name are required');
    if (nodes.has(node.id)) throw new Error(`duplicate design node '${node.id}'`);
    // The Go CLI parses this artifact strictly. Reject an unsupported kind at the
    // producer instead of letting `features inspect` fail on the written file.
    if (!DESIGN_NODE_KINDS.has(node.kind))
      throw new Error(`design node '${node.id}' kind '${node.kind}' is unsupported`);
    nodes.add(node.id);
  }
  const edges = new Set<string>();
  for (const edge of graph.edges) {
    if (!nodes.has(edge.from) || !nodes.has(edge.to)) {
      throw new Error(`design edge '${edge.from}' -> '${edge.to}' is dangling`);
    }
    if (!DESIGN_EDGE_KINDS.has(edge.kind)) {
      throw new Error(`design edge '${edge.from}' -> '${edge.to}' kind '${edge.kind}' is unsupported`);
    }
    if (!DESIGN_AUTHORITIES.has(edge.authority)) {
      throw new Error(`design edge '${edge.from}' -> '${edge.to}' authority '${edge.authority}' is unsupported`);
    }
    const key = `${edge.from}\0${edge.to}\0${edge.kind}`;
    if (edges.has(key)) throw new Error(`duplicate design edge '${edge.from}' -> '${edge.to}' (${edge.kind})`);
    edges.add(key);
  }
}
