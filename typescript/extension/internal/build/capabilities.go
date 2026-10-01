package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	capabilities "go.putnami.dev/protocol/capabilities"
	diagnostic "go.putnami.dev/protocol/diagnostic"
)

var routeLoaderNames = map[string]bool{
	"api-loader":    true,
	"react-loader":  true,
	"static-loader": true,
}

// ReconcileCapabilityManifest merges activation metadata emitted by hooks that
// ran after Application.build() into the validated capability manifest. The
// merged hook export set is authoritative for generated server modules; the
// manifest remains authoritative for bundled activation after reconciliation.
func ReconcileCapabilityManifest(projectPath, manifestPath string, exports map[string]string) (string, error) {
	finalPath, err := resolveCapabilityManifestPath(projectPath, manifestPath)
	if err != nil {
		return "", err
	}
	published := false
	defer func() {
		if !published {
			_ = os.Remove(finalPath)
		}
	}()

	document, err := readValidatedCapabilityDocument(finalPath)
	if err != nil {
		return "", err
	}

	keys := make([]string, 0, len(exports))
	for key := range exports {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		modulePath := exports[key]
		if !IsImportableServerLoader(key, modulePath) {
			continue
		}
		relativeModule, err := resolveGeneratedModulePath(projectPath, modulePath)
		if err != nil {
			return "", fmt.Errorf("capability manifest loader %q: %w", key, err)
		}
		if document.V2 != nil {
			if err := reconcileLoaderCapabilityV2(document.V2, key, relativeModule); err != nil {
				return "", err
			}
		} else if err := reconcileLoaderCapabilityV1(document.V1, key, relativeModule); err != nil {
			return "", err
		}
	}

	var data []byte
	if document.V2 != nil {
		if diags := capabilities.ValidateManifestV2(document.V2); diagnostic.HasErrors(diags) {
			return "", capabilityDiagnosticsError(diags)
		}
		data, err = capabilities.MarshalManifestV2(document.V2)
	} else {
		canonicalizeCapabilityManifest(document.V1)
		if diags := capabilities.ValidateManifest(document.V1); diagnostic.HasErrors(diags) {
			return "", capabilityDiagnosticsError(diags)
		}
		data, err = json.MarshalIndent(document.V1, "", "  ")
	}
	if err != nil {
		return "", fmt.Errorf("serializing reconciled capability manifest: %w", err)
	}
	if document.V1 != nil {
		data = append(data, '\n')
	}
	if err := writeFileAtomic(finalPath, data); err != nil {
		return "", fmt.Errorf("publishing reconciled capability manifest: %w", err)
	}
	published = true
	return finalPath, nil
}

// CapabilityActivationManifest is the version-neutral, runtime-relevant view.
// Feature evidence and v2 declaration metadata are intentionally absent.
type CapabilityActivationManifest struct {
	ProtocolVersion   int
	Project           string
	ConfigDefinitions []capabilityActivationConfig
	Schemas           []capabilityActivationSchema
	Discoverers       []capabilityActivationDiscoverer
}

type capabilityActivationConfig struct{ Path string }
type capabilityActivationSchema struct {
	Name, Kind, Path, SourceKind string
}
type capabilityActivationDiscoverer struct {
	Name, Kind, Path, SourceKind string
}

// readValidatedCapabilityManifest strictly parses either supported wire
// version and projects only the fields that have always driven activation.
func readValidatedCapabilityManifest(projectPath, manifestPath string) (*CapabilityActivationManifest, error) {
	finalPath, err := resolveCapabilityManifestPath(projectPath, manifestPath)
	if err != nil {
		return nil, err
	}
	document, err := readValidatedCapabilityDocument(finalPath)
	if err != nil {
		return nil, err
	}
	return capabilityActivationManifest(document)
}

func readValidatedCapabilityDocument(path string) (*capabilities.ManifestDocument, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading capability manifest: %w", err)
	}
	document, diags := capabilities.ParseAndValidateManifestDocument(data)
	if diagnostic.HasErrors(diags) {
		return nil, capabilityDiagnosticsError(diags)
	}
	return document, nil
}

func capabilityActivationManifest(document *capabilities.ManifestDocument) (*CapabilityActivationManifest, error) {
	activation := &CapabilityActivationManifest{ProtocolVersion: document.ProtocolVersion}
	if document.V1 != nil {
		activation.Project = document.V1.Project
		for _, definition := range document.V1.ConfigDefinitions {
			activation.ConfigDefinitions = append(activation.ConfigDefinitions, capabilityActivationConfig{Path: definition.Path})
		}
		for _, schema := range document.V1.Schemas {
			activation.Schemas = append(activation.Schemas, capabilityActivationSchema{
				Name: schema.Name, Kind: string(schema.Kind), Path: schema.Path, SourceKind: string(schema.Provenance.SourceKind),
			})
		}
		for _, discoverer := range document.V1.Discoverers {
			activation.Discoverers = append(activation.Discoverers, capabilityActivationDiscoverer{
				Name: discoverer.Name, Kind: string(discoverer.Kind), Path: discoverer.Provenance.EvidencePath,
				SourceKind: string(discoverer.Provenance.SourceKind),
			})
		}
		return activation, nil
	}

	manifest := capabilities.CanonicalManifestV2(document.V2)
	activation.Project = manifest.Project
	for _, definition := range manifest.ConfigDefinitions {
		activation.ConfigDefinitions = append(activation.ConfigDefinitions, capabilityActivationConfig{Path: definition.Path})
	}
	for _, schema := range manifest.Schemas {
		activation.Schemas = append(activation.Schemas, capabilityActivationSchema{
			Name: schema.Name, Kind: string(schema.Kind), Path: schema.Path, SourceKind: string(schema.Provenance.SourceKind),
		})
	}
	for _, discoverer := range manifest.Discoverers {
		path := ""
		if discoverer.Kind == capabilities.DiscovererKindSource &&
			discoverer.Provenance.SourceKind == capabilities.SourceKindGenerated &&
			strings.HasSuffix(discoverer.Name, "-loader") && !strings.HasSuffix(discoverer.Name, "client-loader") {
			var err error
			path, err = activationArtifactPathV2(discoverer.Name, discoverer.Provenance)
			if err != nil {
				return nil, err
			}
		}
		activation.Discoverers = append(activation.Discoverers, capabilityActivationDiscoverer{
			Name: discoverer.Name, Kind: string(discoverer.Kind), Path: path,
			SourceKind: string(discoverer.Provenance.SourceKind),
		})
	}
	return activation, nil
}

func activationArtifactPathV2(name string, provenance capabilities.ProvenanceV2) (string, error) {
	var candidates []string
	for _, artifact := range provenance.Artifacts {
		if artifact.Root == capabilities.LocationRootProject && IsImportableServerLoader("generated-loader", artifact.Path) {
			candidates = append(candidates, artifact.Path)
		}
	}
	sort.Strings(candidates)
	generated := candidates[:0]
	for _, candidate := range candidates {
		if strings.HasPrefix(candidate, ".gen/") {
			generated = append(generated, candidate)
		}
	}
	if len(generated) == 1 {
		return generated[0], nil
	}
	if len(generated) > 1 || len(candidates) != 1 {
		return "", fmt.Errorf("capability manifest loader %q must declare exactly one generated project-root module artifact", name)
	}
	return candidates[0], nil
}

func capabilityDiagnosticsError(diags []diagnostic.Diagnostic) error {
	lines := make([]string, 0, len(diags))
	for _, diag := range diags {
		if diag.Severity != diagnostic.Error {
			continue
		}
		if diag.Field != "" {
			lines = append(lines, fmt.Sprintf("[%s] %s: %s", diag.Code, diag.Field, diag.Message))
		} else {
			lines = append(lines, fmt.Sprintf("[%s] %s", diag.Code, diag.Message))
		}
	}
	return fmt.Errorf("capability manifest validation failed:\n%s", strings.Join(lines, "\n"))
}

func resolveCapabilityManifestPath(projectPath, manifestPath string) (string, error) {
	if manifestPath == "" {
		return "", fmt.Errorf("capability manifest path is empty")
	}
	if !filepath.IsAbs(manifestPath) {
		manifestPath = filepath.Join(projectPath, filepath.FromSlash(manifestPath))
	}
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		return "", fmt.Errorf("resolving project path: %w", err)
	}
	absManifest, err := filepath.Abs(manifestPath)
	if err != nil {
		return "", fmt.Errorf("resolving capability manifest path: %w", err)
	}
	rel, err := filepath.Rel(absProject, absManifest)
	if err != nil || filepath.Clean(rel) != filepath.Join(".gen", "schema", capabilities.ManifestFilename) {
		return "", fmt.Errorf("capability manifest must be %s", filepath.ToSlash(filepath.Join(".gen", "schema", capabilities.ManifestFilename)))
	}
	return absManifest, nil
}

func IsImportableServerLoader(name, path string) bool {
	if !strings.HasSuffix(name, "-loader") || strings.HasSuffix(name, "client-loader") {
		return false
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ts", ".tsx", ".js", ".jsx", ".mts", ".cts", ".mjs", ".cjs":
		return true
	default:
		return false
	}
}

func resolveGeneratedModulePath(projectPath, modulePath string) (string, error) {
	if modulePath == "" {
		return "", fmt.Errorf("activation path is empty")
	}
	if !filepath.IsAbs(modulePath) {
		modulePath = filepath.Join(projectPath, filepath.FromSlash(modulePath))
	}
	absProject, err := filepath.Abs(projectPath)
	if err != nil {
		return "", fmt.Errorf("resolving project path: %w", err)
	}
	absModule, err := filepath.Abs(modulePath)
	if err != nil {
		return "", fmt.Errorf("resolving activation module: %w", err)
	}
	rel, err := filepath.Rel(absProject, absModule)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("activation path escapes the project: %s", modulePath)
	}
	if !IsImportableServerLoader("generated-loader", absModule) {
		return "", fmt.Errorf("activation path is not an importable TypeScript/JavaScript module: %s", modulePath)
	}
	if !projectFileExists(absModule) {
		return "", fmt.Errorf("activation module not found: %s", filepath.ToSlash(rel))
	}
	return filepath.ToSlash(rel), nil
}

func reconcileLoaderCapabilityV1(manifest *capabilities.Manifest, key, modulePath string) error {
	expectedRoute := routeLoaderNames[key]
	found := false
	for _, schema := range manifest.Schemas {
		if schema.Name != key {
			continue
		}
		if !expectedRoute || schema.Kind != capabilities.SchemaKindRoute || schema.Path != modulePath ||
			schema.Provenance.Project != manifest.Project ||
			schema.Provenance.SourceKind != capabilities.SourceKindGenerated ||
			schema.Provenance.EvidencePath != modulePath {
			return fmt.Errorf("capability manifest loader %q conflicts with schema identity/path/provenance", key)
		}
		if found {
			return fmt.Errorf("capability manifest declares loader %q more than once", key)
		}
		found = true
	}
	for _, discoverer := range manifest.Discoverers {
		if discoverer.Name != key {
			continue
		}
		if expectedRoute || discoverer.Kind != capabilities.DiscovererKindSource ||
			discoverer.Provenance.Project != manifest.Project ||
			discoverer.Provenance.SourceKind != capabilities.SourceKindGenerated ||
			discoverer.Provenance.EvidencePath != modulePath {
			return fmt.Errorf("capability manifest loader %q conflicts with discoverer identity/path/provenance", key)
		}
		if found {
			return fmt.Errorf("capability manifest declares loader %q more than once", key)
		}
		found = true
	}
	if found {
		return nil
	}

	provenance := capabilities.Provenance{
		Project:      manifest.Project,
		SourceKind:   capabilities.SourceKindGenerated,
		EvidencePath: modulePath,
	}
	if expectedRoute {
		manifest.Schemas = append(manifest.Schemas, capabilities.SchemaContribution{
			Name: key, Kind: capabilities.SchemaKindRoute, Path: modulePath, Provenance: provenance,
		})
	} else {
		manifest.Discoverers = append(manifest.Discoverers, capabilities.Discoverer{
			Name: key, Kind: capabilities.DiscovererKindSource, Provenance: provenance,
		})
	}
	return nil
}

func reconcileLoaderCapabilityV2(manifest *capabilities.ManifestV2, key, modulePath string) error {
	expectedRoute := routeLoaderNames[key]
	found := false
	wantKind := capabilities.ContributionKindDiscoverer
	wantSubkind := string(capabilities.DiscovererKindSource)
	if expectedRoute {
		wantKind = capabilities.ContributionKindSchema
		wantSubkind = string(capabilities.SchemaKindRoute)
	}
	wantIdentity := capabilities.ContributionIdentity{
		OwnerProject: manifest.Project, Kind: wantKind, Subkind: wantSubkind, Key: key,
	}
	for _, schema := range manifest.Schemas {
		if schema.Name != key {
			continue
		}
		artifact, err := activationArtifactPathV2(key, schema.Provenance)
		if !expectedRoute || schema.Identity != wantIdentity || schema.Kind != capabilities.SchemaKindRoute ||
			schema.Path != modulePath || schema.Provenance.Project != manifest.Project ||
			schema.Provenance.SourceKind != capabilities.SourceKindGenerated || err != nil || artifact != modulePath {
			return fmt.Errorf("capability manifest loader %q conflicts with schema identity/path/provenance", key)
		}
		if found {
			return fmt.Errorf("capability manifest declares loader %q more than once", key)
		}
		found = true
	}
	for _, discoverer := range manifest.Discoverers {
		if discoverer.Name != key {
			continue
		}
		artifact, err := activationArtifactPathV2(key, discoverer.Provenance)
		if expectedRoute || discoverer.Identity != wantIdentity || discoverer.Kind != capabilities.DiscovererKindSource ||
			discoverer.Provenance.Project != manifest.Project ||
			discoverer.Provenance.SourceKind != capabilities.SourceKindGenerated || err != nil || artifact != modulePath {
			return fmt.Errorf("capability manifest loader %q conflicts with discoverer identity/path/provenance", key)
		}
		if found {
			return fmt.Errorf("capability manifest declares loader %q more than once", key)
		}
		found = true
	}
	if found {
		return nil
	}

	provenance, err := generatedLoaderProvenanceV2(manifest, modulePath)
	if err != nil {
		return fmt.Errorf("capability manifest loader %q: %w", key, err)
	}
	if expectedRoute {
		manifest.Schemas = append(manifest.Schemas, capabilities.SchemaContributionV2{
			Identity: wantIdentity, Name: key, Kind: capabilities.SchemaKindRoute, Path: modulePath, Provenance: provenance,
		})
	} else {
		manifest.Discoverers = append(manifest.Discoverers, capabilities.DiscovererV2{
			Identity: wantIdentity, Name: key, Kind: capabilities.DiscovererKindSource, Provenance: provenance,
		})
	}
	return nil
}

func generatedLoaderProvenanceV2(manifest *capabilities.ManifestV2, modulePath string) (capabilities.ProvenanceV2, error) {
	for _, pkg := range manifest.Packages {
		if pkg.Identity.OwnerProject != manifest.Project || pkg.Package != manifest.Project ||
			pkg.Provenance.Project != manifest.Project {
			continue
		}
		return capabilities.ProvenanceV2{
			Project: manifest.Project, Package: pkg.Provenance.Package,
			SourceKind:  capabilities.SourceKindGenerated,
			Declaration: pkg.Provenance.Declaration,
			Artifacts:   []capabilities.ArtifactLocation{{Root: capabilities.LocationRootProject, Path: modulePath}},
		}, nil
	}
	// Historical v2 inputs can still reconcile before canonicalization migrates
	// packageVersions to the stable packages collection.
	for _, pkg := range manifest.PackageVersions {
		if pkg.Identity.OwnerProject != manifest.Project || pkg.Package != manifest.Project ||
			pkg.Provenance.Project != manifest.Project {
			continue
		}
		return capabilities.ProvenanceV2{
			Project: manifest.Project, Package: pkg.Provenance.Package,
			SourceKind:  capabilities.SourceKindGenerated,
			Declaration: pkg.Provenance.Declaration,
			Artifacts:   []capabilities.ArtifactLocation{{Root: capabilities.LocationRootProject, Path: modulePath}},
		}, nil
	}
	return capabilities.ProvenanceV2{}, fmt.Errorf("cannot add a generated loader without workload package metadata")
}

func canonicalizeCapabilityManifest(manifest *capabilities.Manifest) {
	provenancePackage := func(p capabilities.Provenance) string { return p.Package }
	sort.SliceStable(manifest.ConfigDefinitions, func(i, j int) bool {
		a, b := manifest.ConfigDefinitions[i], manifest.ConfigDefinitions[j]
		return a.Path < b.Path || (a.Path == b.Path && provenancePackage(a.Provenance) < provenancePackage(b.Provenance))
	})
	sort.SliceStable(manifest.Schemas, func(i, j int) bool {
		a, b := manifest.Schemas[i], manifest.Schemas[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return provenancePackage(a.Provenance) < provenancePackage(b.Provenance)
	})
	sort.SliceStable(manifest.Discoverers, func(i, j int) bool {
		a, b := manifest.Discoverers[i], manifest.Discoverers[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return provenancePackage(a.Provenance) < provenancePackage(b.Provenance)
	})
	sort.SliceStable(manifest.Migrations, func(i, j int) bool {
		a, b := manifest.Migrations[i], manifest.Migrations[j]
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Datasource != b.Datasource {
			return a.Datasource < b.Datasource
		}
		return provenancePackage(a.Provenance) < provenancePackage(b.Provenance)
	})
	sort.SliceStable(manifest.InfraRequirements, func(i, j int) bool {
		return manifest.InfraRequirements[i].Name < manifest.InfraRequirements[j].Name
	})
	sort.SliceStable(manifest.HealthContributors, func(i, j int) bool {
		a, b := manifest.HealthContributors[i], manifest.HealthContributors[j]
		if a.Probe != b.Probe {
			return a.Probe < b.Probe
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return provenancePackage(a.Provenance) < provenancePackage(b.Provenance)
	})
	sort.SliceStable(manifest.LifecycleHooks, func(i, j int) bool {
		a, b := manifest.LifecycleHooks[i], manifest.LifecycleHooks[j]
		if a.Phase != b.Phase {
			return a.Phase < b.Phase
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		return provenancePackage(a.Provenance) < provenancePackage(b.Provenance)
	})
	sort.SliceStable(manifest.PackageVersions, func(i, j int) bool {
		a, b := manifest.PackageVersions[i], manifest.PackageVersions[j]
		if a.Package != b.Package {
			return a.Package < b.Package
		}
		if a.Version != b.Version {
			return a.Version < b.Version
		}
		return provenancePackage(a.Provenance) < provenancePackage(b.Provenance)
	})
	sort.SliceStable(manifest.RequiredCapabilities, func(i, j int) bool {
		a, b := manifest.RequiredCapabilities[i], manifest.RequiredCapabilities[j]
		return a.Name < b.Name || (a.Name == b.Name && provenancePackage(a.Provenance) < provenancePackage(b.Provenance))
	})
	for i := range manifest.RequiredCapabilities {
		sort.Slice(manifest.RequiredCapabilities[i].Requires, func(a, b int) bool {
			return manifest.RequiredCapabilities[i].Requires[a] < manifest.RequiredCapabilities[i].Requires[b]
		})
	}
}

func writeFileAtomic(finalPath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(finalPath), 0755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(finalPath), ".capabilities-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpPath, finalPath)
}

func projectFileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
