package app

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"

	"go.putnami.dev/logger"
	"go.putnami.dev/migration"
	protocaps "go.putnami.dev/protocol/capabilities"
	protofeatures "go.putnami.dev/protocol/features"
)

const describerNameDesign = "design"

type designSource struct {
	path   string
	line   int
	symbol string
}

func callerDesignSource(skip int) designSource {
	pc, file, line, ok := runtime.Caller(skip + 1)
	if !ok {
		return designSource{}
	}
	// A caller path that cannot be relativized carries no provenance.
	cwd, err := os.Getwd()
	if err != nil {
		return designSource{}
	}
	path := normalizeCallerDesignPath(file, cwd)
	if path == "" {
		return designSource{}
	}
	symbol := ""
	if fn := runtime.FuncForPC(pc); fn != nil {
		symbol = fn.Name()
	}
	return designSource{path: path, line: line, symbol: symbol}
}

// normalizeCallerDesignPath makes runtime.Caller output byte-stable across
// regular and -trimpath builds. Absolute paths must live below the project
// root. A trimpath module path is reduced to the longest suffix that names an
// existing project file (for example example.com/service/internal/a.go becomes
// internal/a.go). Unresolvable paths are dropped rather than captured.
func normalizeCallerDesignPath(file, projectRoot string) string {
	if file == "" || projectRoot == "" {
		return ""
	}
	clean := filepath.Clean(file)
	if filepath.IsAbs(clean) {
		relative, err := filepath.Rel(projectRoot, clean)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return ""
		}
		return filepath.ToSlash(relative)
	}
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	parts := strings.Split(filepath.ToSlash(clean), "/")
	for index := range parts {
		candidate := filepath.FromSlash(strings.Join(parts[index:], "/"))
		info, err := os.Stat(filepath.Join(projectRoot, candidate))
		if err == nil && !info.IsDir() {
			return filepath.ToSlash(candidate)
		}
	}
	return ""
}

func (source designSource) protocol() *protofeatures.DesignProvenance {
	if source.path == "" {
		return nil
	}
	return &protofeatures.DesignProvenance{Path: source.path, Line: source.line, Symbol: source.symbol}
}

type designGraphBuilder struct {
	graph      protofeatures.DesignGraph
	nodes      map[string]protofeatures.DesignNode
	edges      map[string]protofeatures.DesignEdge
	moduleIDs  map[*Module]string
	featureIDs map[*Module]string
}

// DesignBuilder is passed to native framework plugins during describe. It is
// already scoped to one owning module and its nearest declared feature.
type DesignBuilder struct {
	state     *designGraphBuilder
	moduleID  string
	featureID string
}

// ModuleID returns the exact owning module node ID.
func (builder *DesignBuilder) ModuleID() string { return builder.moduleID }

// FeatureID returns the nearest feature node ID, or an empty string when the
// plugin is outside a feature scope.
func (builder *DesignBuilder) FeatureID() string { return builder.featureID }

// AddNode records one technical surface. Repeated identical nodes are folded,
// which lets several operations refer to the same schema or event topic.
func (builder *DesignBuilder) AddNode(node protofeatures.DesignNode) error {
	return builder.state.addNode(node)
}

// AddEdge records one exact or explicitly inferred relationship.
func (builder *DesignBuilder) AddEdge(edge protofeatures.DesignEdge) error {
	return builder.state.addEdge(edge)
}

// RelateFromModule is the common native-contributor operation: it links the
// scoped module to a technical surface without repeating the module ID.
func (builder *DesignBuilder) RelateFromModule(to string, kind protofeatures.DesignEdgeKind, authority protofeatures.DesignAuthority) error {
	return builder.AddEdge(protofeatures.DesignEdge{From: builder.moduleID, To: to, Kind: kind, Authority: authority})
}

func newDesignGraphBuilder(project string) *designGraphBuilder {
	return &designGraphBuilder{
		graph: protofeatures.DesignGraph{
			Compatibility: protofeatures.DesignGraphCompatibility,
			Project:       project,
			Nodes:         []protofeatures.DesignNode{},
			Edges:         []protofeatures.DesignEdge{},
		},
		nodes:      map[string]protofeatures.DesignNode{},
		edges:      map[string]protofeatures.DesignEdge{},
		moduleIDs:  map[*Module]string{},
		featureIDs: map[*Module]string{},
	}
}

func (builder *designGraphBuilder) addNode(node protofeatures.DesignNode) error {
	if existing, ok := builder.nodes[node.ID]; ok {
		if !reflect.DeepEqual(existing, node) {
			return fmt.Errorf("design node %q has conflicting native declarations", node.ID)
		}
		return nil
	}
	builder.nodes[node.ID] = node
	return nil
}

func (builder *designGraphBuilder) addEdge(edge protofeatures.DesignEdge) error {
	key := edge.From + "\x00" + edge.To + "\x00" + string(edge.Kind)
	if existing, ok := builder.edges[key]; ok {
		if reflect.DeepEqual(existing, edge) {
			return nil
		}
		// Several native declarations can establish the same relationship. Keep
		// one deterministic exact source on the edge while preserving conflicts
		// in authority or properties.
		existingWithoutSource, candidateWithoutSource := existing, edge
		existingWithoutSource.Provenance = nil
		candidateWithoutSource.Provenance = nil
		if reflect.DeepEqual(existingWithoutSource, candidateWithoutSource) {
			if existing.Provenance == nil && edge.Provenance != nil {
				builder.edges[key] = edge
			} else if existing.Provenance != nil && edge.Provenance != nil && compareDesignProvenance(edge.Provenance, existing.Provenance) < 0 {
				builder.edges[key] = edge
			}
			return nil
		}
		return fmt.Errorf("design edge %q -> %q (%s) has conflicting native declarations", edge.From, edge.To, edge.Kind)
	}
	builder.edges[key] = edge
	return nil
}

func compareDesignProvenance(left, right *protofeatures.DesignProvenance) int {
	if value := strings.Compare(left.Path, right.Path); value != 0 {
		return value
	}
	if left.Line < right.Line {
		return -1
	}
	if left.Line > right.Line {
		return 1
	}
	return strings.Compare(left.Symbol, right.Symbol)
}

func (builder *designGraphBuilder) finish() (*protofeatures.DesignGraph, []string) {
	for _, node := range builder.nodes {
		builder.graph.Nodes = append(builder.graph.Nodes, node)
	}
	// Contributors derive their relationships independently: the clients plugin
	// synthesizes generatedFrom edges from the OpenAPI IR while the api plugin
	// creates the operation nodes, and only contributors inside a feature scope
	// run at all. Any drift between the two leaves an edge with no endpoint. The
	// graph is a provisional, disposable projection, so drop those edges and
	// report them rather than failing the whole build on a derived artifact.
	var dropped []string
	for _, edge := range builder.edges {
		_, hasFrom := builder.nodes[edge.From]
		_, hasTo := builder.nodes[edge.To]
		if !hasFrom || !hasTo {
			dropped = append(dropped, fmt.Sprintf("%s -%s-> %s", edge.From, edge.Kind, edge.To))
			continue
		}
		builder.graph.Edges = append(builder.graph.Edges, edge)
	}
	sort.Strings(dropped)
	return &builder.graph, dropped
}

func (a *Application) buildDesignGraph() (*protofeatures.DesignGraph, error) {
	builder := newDesignGraphBuilder(a.name)
	if err := builder.collectModule(a.Module, nil, ""); err != nil {
		return nil, err
	}
	if len(builder.featureIDs) == 0 {
		return nil, nil
	}

	for _, owned := range a.CollectPlugins() {
		moduleID := builder.moduleIDs[owned.Owner]
		featureID := builder.featureIDs[owned.Owner]
		if moduleID == "" || featureID == "" {
			continue
		}
		scoped := &DesignBuilder{state: builder, moduleID: moduleID, featureID: featureID}
		if contributor, ok := owned.Plugin.(DesignContributor); ok {
			if err := contributor.ContributeDesign(scoped); err != nil {
				return nil, fmt.Errorf("design contribution from %s: %w", contributor.Name(), err)
			}
		}
		if err := contributeNativePluginDesign(scoped, owned.Plugin); err != nil {
			return nil, fmt.Errorf("native design contribution from %s: %w", owned.Plugin.Name(), err)
		}
	}
	// Typed clients are projected from module composition, not from a scoped
	// contributor callback: one client instance composed into several modules
	// must yield one `calls` edge per consuming module.
	if err := builder.contributeTypedClientDesign(a.Module); err != nil {
		return nil, err
	}
	// Migration sources are already collected once by Describe for runtime and
	// capability publication. Project the same frozen declarations into the
	// design graph instead of asking authors for parallel feature metadata or
	// invoking MigrationSources a second time.
	for _, association := range a.capabilityMigrationAssociations {
		moduleID := builder.moduleIDs[association.owner]
		featureID := builder.featureIDs[association.owner]
		if moduleID == "" || featureID == "" {
			continue
		}
		if err := contributeMigrationDesign(&DesignBuilder{state: builder, moduleID: moduleID, featureID: featureID}, association.source); err != nil {
			return nil, fmt.Errorf("migration design contribution from %s: %w", association.contributor.Name(), err)
		}
	}
	graph, dropped := builder.finish()
	if len(dropped) > 0 {
		logger.Default().Named(describerNameDesign).Warn(
			"dropped design relationships with no matching node",
			slog.Int("count", len(dropped)),
			slog.String("edges", strings.Join(dropped, ", ")),
		)
	}
	return graph, nil
}

var validDesignTestKinds = map[DesignTestKind]bool{
	DesignTestUnit: true, DesignTestIntegration: true,
	DesignTestConformance: true, DesignTestE2E: true,
}

// contributeNativePluginDesign projects bounded facts from native registration
// seams. Unlike DesignContributor, none of these interfaces can create an
// arbitrary node or relationship.
func contributeNativePluginDesign(builder *DesignBuilder, plugin Plugin) error {
	if contributor, ok := plugin.(ConfigContributor); ok {
		provenance := designMethodProvenance(plugin, "ConfigDefinitions")
		for _, definition := range contributor.ConfigDefinitions() {
			name := strings.TrimSpace(definition.Path)
			if name == "" {
				return fmt.Errorf("config definition path is required")
			}
			id := "config:" + name
			if err := builder.AddNode(protofeatures.DesignNode{
				ID: id, Kind: protofeatures.DesignNodeConfig, Name: name,
				Properties: map[string]string{"path": name},
			}); err != nil {
				return err
			}
			if err := relateNativeFact(builder, id, provenance); err != nil {
				return err
			}
		}
	}

	if contributor, ok := plugin.(InfraContributor); ok {
		for _, requirement := range contributor.DesignInfraRequirements() {
			name := strings.TrimSpace(requirement.Name)
			if name == "" || !protocaps.ValidInfraKinds[requirement.Kind] {
				return fmt.Errorf("infra requirement must have a name and supported kind (got %q, %q)", requirement.Name, requirement.Kind)
			}
			id := "infra:" + string(requirement.Kind) + ":" + name
			if err := builder.AddNode(protofeatures.DesignNode{
				ID: id, Kind: protofeatures.DesignNodeInfra, Name: name,
				Properties: map[string]string{"kind": string(requirement.Kind)},
			}); err != nil {
				return err
			}
			if err := relateNativeFact(builder, id, requirement.Provenance); err != nil {
				return err
			}
		}
	}

	if contributor, ok := plugin.(TestContributor); ok {
		for _, test := range contributor.DesignTests() {
			name := strings.TrimSpace(test.Name)
			if name == "" || !validDesignTestKinds[test.Kind] {
				return fmt.Errorf("test declaration must have a name and supported kind (got %q, %q)", test.Name, test.Kind)
			}
			id := "test:" + string(test.Kind) + ":" + name
			properties := map[string]string{"kind": string(test.Kind)}
			proves, err := designTestProvesProperty(test.Proves)
			if err != nil {
				return fmt.Errorf("test %q: %w", name, err)
			}
			if proves != "" {
				properties["proves"] = proves
			}
			if err := builder.AddNode(protofeatures.DesignNode{
				ID: id, Kind: protofeatures.DesignNodeTest, Name: name,
				Properties: properties,
			}); err != nil {
				return err
			}
			if err := relateNativeFact(builder, id, test.Provenance); err != nil {
				return err
			}
		}
	}

	phases := make(map[string]bool)
	if _, ok := plugin.(Configurer); ok {
		phases["configure"] = true
	}
	if _, ok := plugin.(Starter); ok {
		phases["start"] = true
	}
	if _, ok := plugin.(Stopper); ok {
		phases["stop"] = true
	}
	return contributeLifecycleDesign(builder, phases)
}

// designTestProvesProperty projects a test's declared spec-check bindings
// onto one deterministic scalar node property: sorted, de-duplicated
// `feature#requirement#check` triplets joined by a single space. A partial
// binding is an authoring error, not a guess; an empty list is simply absent.
// The encoding is shared verbatim with the TypeScript design producer
// (typescript/framework/application/src/features/design-graph.ts) so the
// native-design equivalence golden pins both.
func designTestProvesProperty(proofs []DesignTestProof) (string, error) {
	if len(proofs) == 0 {
		return "", nil
	}
	seen := make(map[string]bool, len(proofs))
	entries := make([]string, 0, len(proofs))
	for _, proof := range proofs {
		feature := strings.TrimSpace(proof.Feature)
		requirement := strings.TrimSpace(proof.Requirement)
		check := strings.TrimSpace(proof.Check)
		if feature == "" || requirement == "" || check == "" {
			return "", fmt.Errorf("a test proof must name feature, requirement, and check (got %q, %q, %q)",
				proof.Feature, proof.Requirement, proof.Check)
		}
		entry := feature + "#" + requirement + "#" + check
		if seen[entry] {
			continue
		}
		seen[entry] = true
		entries = append(entries, entry)
	}
	sort.Strings(entries)
	return strings.Join(entries, " "), nil
}

func relateNativeFact(builder *DesignBuilder, to string, provenance *protofeatures.DesignProvenance) error {
	return builder.AddEdge(protofeatures.DesignEdge{
		From: builder.ModuleID(), To: to, Kind: protofeatures.DesignEdgeContains,
		Authority: protofeatures.DesignAuthorityExact, Provenance: provenance,
	})
}

// contributeLifecycleDesign records module+phase, the strongest stable
// identity both runtimes can promise. Several callbacks in the same module and
// phase intentionally fold into one node; neither registration order nor a JS
// constructor name becomes protocol identity.
func contributeLifecycleDesign(builder *DesignBuilder, phases map[string]bool) error {
	ordered := make([]string, 0, len(phases))
	for phase := range phases {
		ordered = append(ordered, phase)
	}
	sort.Strings(ordered)
	for _, phase := range ordered {
		id := "lifecycle:" + phase + ":" + builder.ModuleID()
		if err := builder.AddNode(protofeatures.DesignNode{
			ID: id, Kind: protofeatures.DesignNodeLifecycle, Name: phase,
			Properties: map[string]string{"phase": phase},
		}); err != nil {
			return err
		}
		if err := builder.RelateFromModule(id, protofeatures.DesignEdgeContains, protofeatures.DesignAuthorityExact); err != nil {
			return err
		}
	}
	return nil
}

func designMethodProvenance(value any, method string) *protofeatures.DesignProvenance {
	file, symbol, err := contributionMethodSource(value, method)
	if err != nil {
		return nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil
	}
	path := normalizeCallerDesignPath(file, cwd)
	if path == "" {
		return nil
	}
	return &protofeatures.DesignProvenance{Path: path, Symbol: symbol}
}

func contributeMigrationDesign(builder *DesignBuilder, source migration.Source) error {
	kind := string(source.Kind())
	namespace := source.Namespace()
	migrationID := "data.migration:" + kind + ":" + namespace
	if err := builder.AddNode(protofeatures.DesignNode{
		ID: migrationID, Kind: protofeatures.DesignNodeDataMigration, Name: namespace,
		Properties: map[string]string{"kind": kind, "namespace": namespace},
	}); err != nil {
		return err
	}
	if err := builder.RelateFromModule(migrationID, protofeatures.DesignEdgeContains, protofeatures.DesignAuthorityExact); err != nil {
		return err
	}

	schemas, ok := source.(migration.SchemaContributor)
	if !ok {
		return nil
	}
	for _, database := range schemas.InfraDatabases() {
		infraID := "infra:" + string(protocaps.InfraKindDatabase) + ":" + database.Name
		if err := builder.AddNode(protofeatures.DesignNode{
			ID: infraID, Kind: protofeatures.DesignNodeInfra, Name: database.Name,
			Properties: map[string]string{"kind": string(protocaps.InfraKindDatabase)},
		}); err != nil {
			return err
		}
		if err := builder.RelateFromModule(infraID, protofeatures.DesignEdgeContains, protofeatures.DesignAuthorityExact); err != nil {
			return err
		}
		names := append([]string(nil), database.Schemas...)
		if len(names) == 0 {
			names = []string{""}
		}
		for _, schema := range names {
			schemaID := "data.schema:" + database.Name
			name := database.Name
			properties := map[string]string{"datasource": database.Name, "engine": string(database.Engine)}
			if schema != "" {
				schemaID += ":" + schema
				name = schema
				properties["schema"] = schema
			}
			if err := builder.AddNode(protofeatures.DesignNode{
				ID: schemaID, Kind: protofeatures.DesignNodeDataSchema, Name: name, Properties: properties,
			}); err != nil {
				return err
			}
			if err := builder.AddEdge(protofeatures.DesignEdge{
				From: migrationID, To: schemaID, Kind: protofeatures.DesignEdgeWrites, Authority: protofeatures.DesignAuthorityExact,
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (builder *designGraphBuilder) collectModule(module *Module, inherited *Feature, parentID string) error {
	feature := inherited
	if module.feature != nil {
		feature = module.feature
	}
	moduleID := ""
	if feature != nil {
		moduleID = designModuleID(module)
		builder.moduleIDs[module] = moduleID
		builder.featureIDs[module] = "feature:" + feature.ID
		if err := builder.addNode(protofeatures.DesignNode{ID: moduleID, Kind: protofeatures.DesignNodeModule, Name: module.name}); err != nil {
			return err
		}
		if module.feature != nil {
			featureID := "feature:" + feature.ID
			if err := builder.addNode(protofeatures.DesignNode{
				ID: featureID, Kind: protofeatures.DesignNodeFeature, Name: feature.Name,
				Properties: map[string]string{"outcome": feature.Outcome, "owner": feature.Owner},
				Provenance: feature.provenance.protocol(),
			}); err != nil {
				return err
			}
			if err := builder.addEdge(protofeatures.DesignEdge{From: featureID, To: moduleID, Kind: protofeatures.DesignEdgeImplementedBy, Authority: protofeatures.DesignAuthorityExact}); err != nil {
				return err
			}
		} else if parentID != "" {
			if err := builder.addEdge(protofeatures.DesignEdge{From: parentID, To: moduleID, Kind: protofeatures.DesignEdgeContains, Authority: protofeatures.DesignAuthorityExact}); err != nil {
				return err
			}
		}
		phases := make(map[string]bool)
		if len(module.preConfigureHooks) > 0 || len(module.postConfigureHooks) > 0 {
			phases["configure"] = true
		}
		if len(module.startHooks) > 0 {
			phases["start"] = true
		}
		if len(module.shutdownHooks) > 0 {
			phases["stop"] = true
		}
		if err := contributeLifecycleDesign(&DesignBuilder{state: builder, moduleID: moduleID, featureID: builder.featureIDs[module]}, phases); err != nil {
			return err
		}
	}
	for _, child := range module.modules {
		if err := builder.collectModule(child, feature, moduleID); err != nil {
			return err
		}
	}
	return nil
}

func designModuleID(module *Module) string {
	parts := []string{module.name}
	for parent := module.parent; parent != nil; parent = parent.parent {
		parts = append(parts, parent.name)
	}
	for left, right := 0, len(parts)-1; left < right; left, right = left+1, right-1 {
		parts[left], parts[right] = parts[right], parts[left]
	}
	return "module:" + strings.Join(parts, "/")
}

func writeDesignGraph(outputDir string, graph *protofeatures.DesignGraph) error {
	data, err := protofeatures.MarshalDesignGraph(graph)
	if err != nil {
		return err
	}
	path := filepath.Join(outputDir, filepath.FromSlash(protofeatures.DesignGraphArtifact))
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(path), ".graph-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer removeDesignTemporary(temporaryPath)
	if _, err := temporary.Write(data); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Chmod(0o640); err != nil {
		return errors.Join(err, temporary.Close())
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	// #nosec G703 -- OutputDir is the framework-owned describe root and the
	// artifact suffix is a compile-time protocol constant.
	return os.Rename(temporaryPath, path)
}

func removeDesignGraph(outputDir string) error {
	path := filepath.Join(outputDir, filepath.FromSlash(protofeatures.DesignGraphArtifact))
	// #nosec G703 -- OutputDir is the framework-owned describe root and the
	// artifact suffix is a compile-time protocol constant.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func removeDesignTemporary(path string) {
	// A successful rename already moved the staging file, so ErrNotExist is the
	// normal outcome. Anything else leaves a stray .graph-*.json in the describe
	// root, which is worth reporting rather than discarding silently.
	// #nosec G703 -- path is returned by os.CreateTemp in writeDesignGraph.
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Default().Named(describerNameDesign).Warn("could not remove design graph staging file",
			slog.String("path", path), slog.String("error", err.Error()))
	}
}
