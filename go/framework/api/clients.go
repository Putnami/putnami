package api

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"

	"go.putnami.dev/app"
	"go.putnami.dev/logger"
	clientcontract "go.putnami.dev/protocol/clientcontract"
	protofeatures "go.putnami.dev/protocol/features"
)

// describerNameClients is the describe target name for the client generator.
const describerNameClients = "clients"

// ClientGenConfigPath is the project-relative path of the client-generation
// contract the describer emits, mirroring the TypeScript generator
// (.gen/clientgen/config.json). It is the single source of truth the build
// runner reads to learn the targets, output dirs, and module/package naming.
const ClientGenConfigPath = "clientgen/config.json"

// ClientStageDir is the OutputDir-relative directory the describer stages
// generated client code into (e.g. .gen/clientgen/go/client.gen.go). The build
// runner mirrors it into the configured output directory in the project tree —
// the describer honors the "write only under OutputDir" contract, so the staging
// copy lives under .gen and the runner is the sole writer of the project tree.
const ClientStageDir = "clientgen"

// SpecSource is implemented by a plugin that can render the OpenAPI specification
// in-memory. The client describer collects it from the module tree (the openapi
// plugin satisfies it) and reads the spec directly, rather than depending on the
// openapi describer having already written .gen/schema/openapi.json. This makes
// the openapi → clients dependency explicit (a typed reference resolved via
// [app.Collect]) and removes the intra-describe file-ordering hazard.
type SpecSource interface {
	// OpenAPISpecJSON renders and returns the current OpenAPI spec as JSON.
	OpenAPISpecJSON() ([]byte, error)
}

// GoClientOptions configures the generated Go client target.
type GoClientOptions struct {
	// Output is the output directory, relative to the project root. Default "clients/go".
	Output string
	// ModulePath is the Go module path of the generated client module. When set,
	// the describer scaffolds a go.mod and a putnami.json so clients/go is a
	// standalone module and a workspace project of its own; when empty, only
	// client.gen.go is emitted (a package inside the parent module).
	ModulePath string
	// PackageName is the Go package name for the generated client. Default "client".
	PackageName string
	// ClientName is the generated client struct name. Default "Client".
	ClientName string
	// BaseURLDefault, when set, emits a DefaultBaseURL constant in the client.
	BaseURLDefault string
	// OmitOperations names operations, by operationId, that the Go target leaves
	// out instead of failing the whole client: the escape for an operation the
	// Go emitter cannot represent yet. The build log, the generated client's doc
	// comment and its client.putnami.json manifest name every operation left
	// out, and workspace coverage counts it as uncovered. A name the contract
	// does not declare fails generation. Empty by default: an operation the Go
	// emitter cannot represent fails the build.
	OmitOperations []string
}

// TSClientOptions configures the TypeScript client target in the emitted
// contract. The Go describer does not generate TypeScript itself; these values
// are recorded in config.json so the contract stays identical across languages.
type TSClientOptions struct {
	// Output is the output directory, relative to the project root. Default "clients/ts".
	Output string
	// PackageName is the npm package name for the generated TS client.
	PackageName string
}

// ClientsOptions configures the client generator describer.
type ClientsOptions struct {
	// Targets lists the languages to generate. Default ["go"].
	Targets []string
	// ThirdParty permits generation from an external OpenAPI document that has
	// no x-putnami-client contract. Provider-owned clients are strict by default.
	ThirdParty bool
	// Go configures the Go client target.
	Go GoClientOptions
	// TS configures the TypeScript client target recorded in the contract.
	TS TSClientOptions
}

// ClientsPlugin is the client-generator describer. During describe mode it emits
// the client-generation contract (.gen/clientgen/config.json) and, for every
// enabled target, generates a typed client from the OpenAPI spec.
//
//	apiPlugin := api.New(httpServer)
//	openapiPlugin := openapi.NewPlugin(openapi.PluginOptions{Title: "Items"}).From(apiPlugin)
//	clients := api.Clients(api.ClientsOptions{
//	    Go: api.GoClientOptions{ModulePath: "example.com/svc/clients/go"},
//	}).From(apiPlugin)
//	app.New("svc").Use(httpServer).Use(apiPlugin).Use(openapiPlugin).Use(clients)
type ClientsPlugin struct {
	opts         ClientsOptions
	apiPlugin    *Plugin
	module       *app.Module
	designSpec   *SpecIR
	designConfig clientGenConfig
}

// Clients creates a client-generator describer.
func Clients(opts ClientsOptions) *ClientsPlugin {
	return &ClientsPlugin{opts: opts}
}

// From wires the api.Plugin whose routes back the generated client. The spec
// itself is read from the openapi plugin (discovered via [SpecSource]); the api
// plugin reference gates generation on whether any routes exist.
func (p *ClientsPlugin) From(apiPlugin *Plugin) *ClientsPlugin {
	p.apiPlugin = apiPlugin
	return p
}

// Name returns the plugin name. Implements app.Plugin.
func (p *ClientsPlugin) Name() string { return describerNameClients }

// AdditionalDescribeTargets makes the design graph imply client discovery.
// A design-only pass prepares native client nodes and relationships without
// emitting the client-generation contract or staged source code.
func (p *ClientsPlugin) AdditionalDescribeTargets() []string { return []string{"design"} }

// Configure captures the owning module so Describe can discover the [SpecSource].
// Implements app.Configurer.
func (p *ClientsPlugin) Configure(_ context.Context, owner *app.Module) error {
	p.module = owner
	return nil
}

// Describe emits the client-generation contract and generates the configured
// clients. Implements app.Describer.
func (p *ClientsPlugin) Describe(ctx *app.DescribeContext) error {
	p.designSpec = nil
	p.designConfig = clientGenConfig{}
	wantsClients := ctx.Wants(p.Name())
	wantsDesign := ctx.Wants("design")
	if !wantsClients && !wantsDesign {
		return nil
	}

	cfg := p.resolveConfig()
	if operations := p.operationProducers(); len(operations) > 0 {
		cfg.Design = &clientGenConfigDesign{Operations: operations}
	}

	// The clients target owns the contract. A design-only pass discovers the
	// same OpenAPI surface in memory but must not publish unrelated artifacts.
	if wantsClients {
		if err := writeClientGenConfig(ctx.OutputDir, cfg); err != nil {
			return err
		}
	}

	generateGo := wantsClients && cfg.targetEnabled("go")
	discoverDesign := cfg.Design != nil && (cfg.targetEnabled("go") || cfg.targetEnabled("ts"))
	if !generateGo && !discoverDesign {
		return nil
	}

	// Nothing to generate when the api plugin discovered no routes — the contract
	// is written, but there's no client surface yet.
	if p.apiPlugin != nil && len(p.apiPlugin.DiscoveredRoutes()) == 0 {
		return nil
	}

	specJSON, err := p.specJSON()
	if err != nil {
		if !generateGo && cfg.ThirdParty {
			logger.Default().Named(describerNameClients).Warn(
				"skipped TypeScript client design discovery",
				slog.String("error", err.Error()),
			)
			return nil
		}
		return err
	}

	spec, err := ReadOpenAPISpec(specJSON)
	if err != nil {
		if !generateGo && cfg.ThirdParty {
			logger.Default().Named(describerNameClients).Warn(
				"skipped TypeScript client design discovery",
				slog.String("error", err.Error()),
			)
			return nil
		}
		return err
	}
	if !cfg.ThirdParty && spec.Contract == nil {
		return fmt.Errorf("api: provider client generation requires x-putnami-client; set ClientsOptions.ThirdParty only for external contracts")
	}
	if len(spec.Services) == 0 {
		return nil
	}
	p.designSpec = &spec
	p.designConfig = cfg
	if !generateGo {
		return nil
	}

	source, err := GenerateClientFromIR(spec, ClientGenOptions{
		PackageName:    cfg.Go.PackageName,
		ClientName:     cfg.Go.ClientName,
		BaseURLDefault: p.opts.Go.BaseURLDefault,
		Design:         clientDesignOptions(cfg.Design),
		OmitOperations: cfg.Go.OmitOperations,
	})
	if err != nil {
		return err
	}
	if _, omitted, omitErr := omitOperations(spec, cfg.Go.OmitOperations); omitErr == nil {
		for _, method := range omitted {
			logger.Default().Named(describerNameClients).Warn(
				"left an operation out of the Go client",
				slog.String("operationId", method.OperationID),
				slog.String("route", strings.ToUpper(method.HTTPMethod)+" "+method.Path),
				slog.String("option", "go.omitOperations"),
			)
		}
	}

	return p.stageGoClient(ctx.OutputDir, cfg, source, specJSON, spec)
}

// operationProducers derives, for every endpoint in the application, the feature
// that produced it. Ownership is the module that owns the api.Plugin the
// endpoint was registered on — never this generator's module, so an operation
// whose owner declares no feature stays unattributed instead of borrowing the
// generator's lineage.
//
// The walk is the module tree in depth-first pre-order (see
// [app.Module.CollectPlugins]) and the result is sorted, so the emitted contract
// is byte-stable across runs. Two api plugins cannot legitimately serve the same
// method and path — the transport would reject the second binding — so the
// first owner in tree order wins and later duplicates are dropped rather than
// producing an ambiguous second attribution.
func (p *ClientsPlugin) operationProducers() []ClientOperationProducer {
	if p.module == nil {
		return nil
	}
	project := p.module.Root().Name()
	seen := make(map[string]bool)
	var operations []ClientOperationProducer
	for _, owned := range p.module.Root().CollectPlugins() {
		apiPlugin, ok := owned.Plugin.(*Plugin)
		if !ok {
			continue
		}
		feature := owned.Owner.EffectiveFeature()
		if feature == nil {
			continue
		}
		for _, route := range apiPlugin.DiscoveredRoutes() {
			method := strings.ToUpper(route.Method)
			key := method + " " + route.Path
			if seen[key] {
				continue
			}
			seen[key] = true
			operations = append(operations, ClientOperationProducer{
				Method:          method,
				Path:            route.Path,
				ProducerProject: project,
				ProducerFeature: feature.ID,
			})
		}
	}
	slices.SortFunc(operations, func(left, right ClientOperationProducer) int {
		if left.Method != right.Method {
			return strings.Compare(left.Method, right.Method)
		}
		return strings.Compare(left.Path, right.Path)
	})
	return operations
}

// ContributeDesign links every configured generated client to the exact
// operations in the producer's OpenAPI IR, carrying the producing feature on
// each generatedFrom edge. Attribution is per operation: the client node itself
// claims no feature, because one client legitimately spans several.
func (p *ClientsPlugin) ContributeDesign(builder *app.DesignBuilder) error {
	if p.designSpec == nil || p.designConfig.Design == nil {
		return nil
	}
	cfg := p.designConfig
	if cfg.targetEnabled("go") {
		identity := cfg.Go.ModulePath
		if identity == "" {
			identity = cfg.Go.PackageName
		}
		nodeID := "client.generated:go:" + identity + "/" + cfg.Go.ClientName
		if err := p.addDesignClient(builder, nodeID, cfg.Go.ClientName, "go", identity); err != nil {
			return err
		}
		// The Go client is generated from what it actually has: an operation
		// go.omitOperations leaves out has no generatedFrom edge from it.
		goSpec, _, err := omitOperations(*p.designSpec, cfg.Go.OmitOperations)
		if err != nil {
			return err
		}
		for _, service := range goSpec.Services {
			if err := p.relateClientOperations(builder, nodeID, service.Methods); err != nil {
				return err
			}
		}
	}
	if cfg.targetEnabled("ts") {
		for _, service := range p.designSpec.Services {
			nodeID := "client.generated:ts:" + cfg.TS.PackageName + "/" + service.ClassName
			if err := p.addDesignClient(builder, nodeID, service.ClassName, "ts", cfg.TS.PackageName); err != nil {
				return err
			}
			if err := p.relateClientOperations(builder, nodeID, service.Methods); err != nil {
				return err
			}
		}
	}
	return nil
}

// addDesignClient records the generated client itself. It deliberately carries
// no `feature` property: the client is emitted by one project but its operations
// may be produced by several features, and a client-wide feature would attribute
// every one of them to whichever feature happened to own the generator.
func (p *ClientsPlugin) addDesignClient(builder *app.DesignBuilder, nodeID, name, language, packageName string) error {
	properties := map[string]string{
		"language": language,
		"package":  packageName,
	}
	if p.module != nil {
		properties["producer"] = p.module.Root().Name()
	}
	if p.designSpec.SpecHash != "" {
		properties["specHash"] = p.designSpec.SpecHash
	}
	if err := builder.AddNode(protofeatures.DesignNode{
		ID: nodeID, Kind: protofeatures.DesignNodeClient, Name: name, Properties: properties,
	}); err != nil {
		return err
	}
	return builder.RelateFromModule(nodeID, protofeatures.DesignEdgeContains, protofeatures.DesignAuthorityExact)
}

// relateClientOperations emits one generatedFrom edge per operation, keyed by the
// canonical operation id the generated descriptor and the runtime trace use, and
// carrying that operation's producer. An operation the attribution table does
// not name keeps the edge (the client really was generated from it) without
// producer properties.
func (p *ClientsPlugin) relateClientOperations(builder *app.DesignBuilder, clientID string, methods []MethodIR) error {
	for _, method := range methods {
		httpMethod := strings.ToUpper(method.HTTPMethod)
		properties := map[string]string{"operationId": method.OperationID}
		if producer := p.designConfig.Design.producerOf(httpMethod, method.Path); producer.ProducerFeature != "" {
			properties["producerProject"] = producer.ProducerProject
			properties["producerFeature"] = producer.ProducerFeature
		}
		if err := builder.AddEdge(protofeatures.DesignEdge{
			From:       clientID,
			To:         "api.operation:" + httpMethod + ":" + method.Path,
			Kind:       protofeatures.DesignEdgeGeneratedFrom,
			Authority:  protofeatures.DesignAuthorityExact,
			Properties: properties,
		}); err != nil {
			return err
		}
	}
	return nil
}

// specJSON resolves the OpenAPI spec from the discovered SpecSource (the openapi
// plugin). The dependency is explicit: if no SpecSource is registered the build
// fails with an actionable message rather than silently emitting an empty client.
func (p *ClientsPlugin) specJSON() ([]byte, error) {
	if p.module != nil {
		for _, src := range app.Collect[SpecSource](p.module) {
			return src.OpenAPISpecJSON()
		}
	}
	return nil, fmt.Errorf(
		"api: client generator found no OpenAPI source; register openapi.NewPlugin(...).From(apiPlugin) so it can read the spec")
}

// stageGoClient writes the generated client (and, when a module path is set,
// the go.mod and putnami.json scaffold) under <OutputDir>/clientgen/go. The
// build runner mirrors this staging tree into the configured project output
// directory, rewriting client.gen.go and the manifest every run and copying
// the scaffold files only when the target has none — the same scaffold-once
// pair the cross-language [GenerateProjectClients] emits, so a Go-emitted
// client module is a workspace project whichever language its provider is
// written in.
func (p *ClientsPlugin) stageGoClient(outputDir string, cfg clientGenConfig, source string, contractBytes []byte, spec SpecIR) error {
	dir := filepath.Join(outputDir, ClientStageDir, "go")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "client.gen.go"), []byte(source), 0o600); err != nil {
		return err
	}
	if spec.Contract != nil {
		importPath := cfg.Go.ModulePath
		if importPath == "" {
			modulePath, err := executableGoModulePath()
			if err != nil {
				return err
			}
			importPath = strings.TrimRight(modulePath, "/") + "/" + strings.Trim(cfg.Go.Output, "/")
		}
		manifest, err := generatedGoManifestForImportPath(importPath, cfg, contractBytes, []byte(source), spec)
		if err != nil {
			return err
		}
		manifestBytes, err := marshalGeneratedGoManifest(manifest)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, clientcontract.GeneratedManifestFile), manifestBytes, 0o600); err != nil {
			return err
		}
	}
	if cfg.Go.ModulePath != "" {
		gomod := renderGoMod(cfg.Go.ModulePath, goModVersion())
		if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte(gomod), 0o600); err != nil {
			return err
		}
		project := renderGeneratedGoProject(cfg.Go.ModulePath)
		if err := os.WriteFile(filepath.Join(dir, "putnami.json"), []byte(project), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// executableGoModulePath returns the module identity embedded by the Go
// toolchain in the describe executable. The application's logical name is not
// an import path and must never be used to construct a generated binding.
func executableGoModulePath() (string, error) {
	info, ok := debug.ReadBuildInfo()
	if !ok || strings.TrimSpace(info.Main.Path) == "" || info.Main.Path == "command-line-arguments" {
		return "", fmt.Errorf("api: resolve generated client import path: describe executable has no Go module identity")
	}
	return strings.TrimSpace(info.Main.Path), nil
}

// --- Config contract (mirrors typescript/.../config.type.ts) ---

type clientGenConfig struct {
	Targets    []string               `json:"targets"`
	ThirdParty bool                   `json:"thirdParty,omitempty"`
	TS         clientGenConfigTS      `json:"ts"`
	Go         clientGenConfigGo      `json:"go"`
	Design     *clientGenConfigDesign `json:"design,omitempty"`
}

// clientGenConfigDesign is the producer-attribution half of the contract: the
// operations the application declares a feature for, keyed by method and path.
// The canonical operation id is NOT repeated here — it belongs to the OpenAPI
// contract, and duplicating it would create a second identity to keep in sync.
type clientGenConfigDesign struct {
	// Operations reuses the emitter's public attribution entry so the contract and
	// the generator cannot disagree on either the field set or the JSON key order.
	Operations []ClientOperationProducer `json:"operations"`
}

func (design *clientGenConfigDesign) producerOf(httpMethod, path string) ClientOperationProducer {
	if design == nil {
		return ClientOperationProducer{}
	}
	for _, operation := range design.Operations {
		if strings.EqualFold(operation.Method, httpMethod) && operation.Path == path {
			return operation
		}
	}
	return ClientOperationProducer{}
}

func clientDesignOptions(design *clientGenConfigDesign) *ClientDesignOptions {
	if design == nil {
		return nil
	}
	return &ClientDesignOptions{Operations: design.Operations}
}

type clientGenConfigTS struct {
	Output      string `json:"output"`
	PackageName string `json:"packageName"`
}

type clientGenConfigGo struct {
	Output      string `json:"output"`
	ModulePath  string `json:"modulePath"`
	PackageName string `json:"packageName"`
	ClientName  string `json:"clientName"`
	// OmitOperations is GoClientOptions.OmitOperations. It is absent from the
	// emitted contract when empty, as the TypeScript resolver leaves it out.
	OmitOperations []string `json:"omitOperations,omitempty"`
}

func (c clientGenConfig) targetEnabled(target string) bool {
	return slices.Contains(c.Targets, target)
}

// resolveConfig applies the documented defaults, mirroring the TS resolver so the
// two languages agree on output dirs and naming.
func (p *ClientsPlugin) resolveConfig() clientGenConfig {
	targets := p.opts.Targets
	if len(targets) == 0 {
		targets = []string{"go"}
	}
	return clientGenConfig{
		Targets:    targets,
		ThirdParty: p.opts.ThirdParty,
		TS: clientGenConfigTS{
			Output:      orDefault(p.opts.TS.Output, "clients/ts"),
			PackageName: p.opts.TS.PackageName,
		},
		Go: clientGenConfigGo{
			Output:         orDefault(p.opts.Go.Output, "clients/go"),
			ModulePath:     p.opts.Go.ModulePath,
			PackageName:    orDefault(p.opts.Go.PackageName, "client"),
			ClientName:     orDefault(p.opts.Go.ClientName, "Client"),
			OmitOperations: slices.Clone(p.opts.Go.OmitOperations),
		},
	}
}

// writeClientGenConfig serializes the resolved contract to
// <OutputDir>/clientgen/config.json (2-space indent + trailing newline, matching
// the TS emitter byte-for-byte).
func writeClientGenConfig(outputDir string, cfg clientGenConfig) error {
	path := filepath.Join(outputDir, ClientGenConfigPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	return os.WriteFile(path, body, 0o600)
}

func renderGoMod(modulePath, goVersion string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "module %s\n\n", modulePath)
	fmt.Fprintf(&b, "go %s\n\n", goVersion)
	// Framework modules are resolved by the workspace while developing and may
	// be pinned by a published generated-client module.
	b.WriteString("require (\n")
	b.WriteString("\tgo.putnami.dev/app v0.0.1\n")
	b.WriteString("\tgo.putnami.dev/client v0.0.1\n")
	b.WriteString("\tgo.putnami.dev/inject v0.0.1\n")
	b.WriteString(")\n")
	return b.String()
}

// goModVersion returns the major.minor Go version for the generated go.mod's
// `go` directive, derived from the toolchain that built the describe binary.
func goModVersion() string {
	v := strings.TrimPrefix(runtime.Version(), "go")
	parts := strings.SplitN(v, ".", 3)
	if len(parts) >= 2 {
		return parts[0] + "." + parts[1]
	}
	return v
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

var _ app.DesignContributor = (*ClientsPlugin)(nil)
var _ app.AdditionalDescribeTargeter = (*ClientsPlugin)(nil)
