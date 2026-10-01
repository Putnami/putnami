package api

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	clientcontract "go.putnami.dev/protocol/clientcontract"
)

// ProjectClientResult reports what [GenerateProjectClients] produced.
type ProjectClientResult struct {
	// Generated is true when a Go client was emitted (the go target is enabled and
	// the spec exposed at least one service).
	Generated bool
	// OutputDir is the project-relative directory the client targets (e.g.
	// "clients/go"), set whenever a go target is configured even if nothing was
	// emitted this run.
	OutputDir string
	// Files lists the project-relative paths written this run.
	Files []string
	// Omitted lists, in contract order, the operations go.omitOperations left
	// out of the emitted client. The client's doc comment and its manifest name
	// them too.
	Omitted []string
}

// GenerateProjectClients reads the client-generation contract and OpenAPI spec a
// build already produced under projectRoot and emits the Go client directly into
// projectRoot/<go.output> (default clients/go). It is the standalone, app-less
// counterpart of the [ClientsPlugin] describer: the workspace `clientgen` command
// runs it — in its own Go toolchain — to emit a Go client for a provider written
// in ANOTHER language (e.g. a TypeScript service), reading that provider's OWN
// spec. Cross-language emission therefore needs no running app and no
// cross-project reads; it is mediated entirely through the shared spec artifact.
//
// client.gen.go is rewritten every run; go.mod and putnami.json are scaffolded
// only when absent, so committed or hand-edited module/project metadata is
// preserved — mirroring the same-language mirror step's scaffold-once rule. The
// result has Generated=false (and a nil error) when the project has no clientgen
// contract, the go target is disabled, or the spec exposes no services, so callers
// can treat "nothing to do" as success.
func GenerateProjectClients(projectRoot string) (ProjectClientResult, error) {
	cfgPath := filepath.Join(projectRoot, ".gen", ClientGenConfigPath)
	raw, err := readTrustedFile(cfgPath)
	if err != nil {
		if os.IsNotExist(err) {
			return ProjectClientResult{}, nil // no client generator configured for this project
		}
		return ProjectClientResult{}, fmt.Errorf("read clientgen config: %w", err)
	}
	var cfg clientGenConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return ProjectClientResult{}, fmt.Errorf("parse clientgen config %s: %w", cfgPath, err)
	}
	output, err := cleanClientOutputRel(cfg.Go.Output)
	if err != nil {
		return ProjectClientResult{}, err
	}
	result := ProjectClientResult{OutputDir: output}
	if !cfg.targetEnabled("go") {
		return result, nil
	}

	specJSON, specPath, err := readProjectSpec(projectRoot)
	if err != nil {
		return result, err
	}
	spec, err := ReadOpenAPISpec(specJSON)
	if err != nil {
		return result, fmt.Errorf("read openapi spec %s: %w", specPath, err)
	}
	if !cfg.ThirdParty && spec.Contract == nil {
		return result, fmt.Errorf(
			"clientgen: provider client generation requires x-putnami-client in %s; thirdParty is only for external contracts", specPath)
	}
	if len(spec.Services) == 0 {
		// Remove only files whose exact hashes are owned by a prior generated
		// manifest. A modified file is never silently deleted.
		if err := removeOwnedGeneratedTarget(filepath.Join(projectRoot, filepath.FromSlash(output))); err != nil {
			return result, err
		}
		return result, nil
	}

	// GenerateClientFromIR returns gofmt-clean source, so the committed client is
	// byte-stable and a build's lint --fix never reformats it out from under the
	// clientsync guard.
	source, err := GenerateClientFromIR(spec, ClientGenOptions{
		PackageName:    orDefault(cfg.Go.PackageName, "client"),
		ClientName:     orDefault(cfg.Go.ClientName, "Client"),
		Design:         clientDesignOptions(cfg.Design),
		OmitOperations: cfg.Go.OmitOperations,
	})
	if err != nil {
		return result, err
	}
	_, omitted, err := omitOperations(spec, cfg.Go.OmitOperations)
	if err != nil {
		return result, err
	}
	result.Omitted = omittedOperationIDs(omitted)

	targetDir := filepath.Join(projectRoot, filepath.FromSlash(output))
	if err := removeObsoleteGeneratedTargets(projectRoot, targetDir); err != nil {
		return result, err
	}
	if err := os.MkdirAll(targetDir, 0o750); err != nil {
		return result, err
	}
	if err := os.WriteFile(filepath.Join(targetDir, "client.gen.go"), []byte(source), 0o600); err != nil {
		return result, err
	}
	result.Files = append(result.Files, output+"/client.gen.go")

	if spec.Contract != nil {
		manifest, manifestErr := generatedGoManifest(projectRoot, output, cfg, specJSON, []byte(source), spec)
		if manifestErr != nil {
			return result, manifestErr
		}
		manifestBytes, marshalErr := marshalGeneratedGoManifest(manifest)
		if marshalErr != nil {
			return result, marshalErr
		}
		manifestPath := filepath.Join(targetDir, clientcontract.GeneratedManifestFile)
		if err := os.WriteFile(manifestPath, manifestBytes, 0o600); err != nil {
			return result, err
		}
		result.Files = append(result.Files, output+"/"+clientcontract.GeneratedManifestFile)
	}

	// go.mod makes clients/go a standalone module (required when the provider is in
	// another language, so the client cannot live as a package inside the provider
	// module). Scaffold it once and never overwrite, so a committed/edited module
	// file stays stable and the workspace go.work never churns.
	if cfg.Go.ModulePath != "" {
		gomodPath := filepath.Join(targetDir, "go.mod")
		if _, statErr := os.Stat(gomodPath); errIsNotExist(statErr) {
			gomod := renderGoMod(cfg.Go.ModulePath, goModVersion())
			if err := os.WriteFile(gomodPath, []byte(gomod), 0o600); err != nil {
				return result, err
			}
			result.Files = append(result.Files, output+"/go.mod")
		}
		projectPath := filepath.Join(targetDir, "putnami.json")
		if _, statErr := os.Stat(projectPath); errIsNotExist(statErr) {
			project := renderGeneratedGoProject(cfg.Go.ModulePath)
			if err := os.WriteFile(projectPath, []byte(project), 0o600); err != nil {
				return result, err
			}
			result.Files = append(result.Files, output+"/putnami.json")
		}
	}

	result.Generated = true
	return result, nil
}

func renderGeneratedGoProject(modulePath string) string {
	project := struct {
		Schema           string              `json:"$schema"`
		Name             string              `json:"name"`
		Tags             []string            `json:"tags"`
		Extensions       []string            `json:"extensions"`
		Disable          map[string][]string `json:"disable"`
		Options          map[string]any      `json:"options"`
		FeatureAuthority map[string]string   `json:"featureAuthority"`
	}{
		Schema:     "https://putnami.dev/schemas/putnami-project.json",
		Name:       modulePath,
		Tags:       []string{"go", "generated-client"},
		Extensions: []string{"@putnami/go"},
		Disable: map[string][]string{
			"jobs": {"serve"},
		},
		Options: map[string]any{
			"test": map[string]any{
				"coverage":           false,
				"coverage-threshold": 0,
			},
		},
		FeatureAuthority: map[string]string{
			"none": "Generated client code mirrors provider-owned operations and does not own a separate user outcome.",
		},
	}
	body, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		panic(err)
	}
	return string(body) + "\n"
}

// readProjectSpec resolves the OpenAPI spec a build produced for projectRoot,
// trying the fresh .gen JSON first, then its gzipped companion, and only then
// the committed fallback. A build is the provider source of truth for a sync;
// a stale tracked schema must never win over the contract just produced.
func readProjectSpec(projectRoot string) ([]byte, string, error) {
	for _, p := range []string{
		filepath.Join(projectRoot, ".gen", "schema", "openapi.json"),
	} {
		if body, err := readTrustedFile(p); err == nil {
			return body, p, nil
		}
	}
	gz := filepath.Join(projectRoot, ".gen", "schema", "openapi.json.gz")
	if body, err := readTrustedFile(gz); err == nil {
		r, gzErr := gzip.NewReader(bytes.NewReader(body))
		if gzErr != nil {
			return nil, gz, fmt.Errorf("open gzip spec %s: %w", gz, gzErr)
		}
		decoded, readErr := io.ReadAll(r)
		closeErr := r.Close()
		if readErr != nil {
			return nil, gz, fmt.Errorf("decompress spec %s: %w", gz, readErr)
		}
		if closeErr != nil {
			return nil, gz, fmt.Errorf("close gzip spec %s: %w", gz, closeErr)
		}
		return decoded, gz, nil
	}
	committed := filepath.Join(projectRoot, "schema", "openapi.json")
	if body, err := readTrustedFile(committed); err == nil {
		return body, committed, nil
	}
	return nil, "", fmt.Errorf(
		"clientgen: no OpenAPI spec under %s (looked for schema/openapi.json, .gen/schema/openapi.json[.gz]); run `putnami build` first",
		projectRoot)
}

func generatedGoManifest(projectRoot, output string, cfg clientGenConfig, contractBytes, source []byte, spec SpecIR) (*clientcontract.GeneratedClientManifestV1, error) {
	importPath, err := generatedGoImportPath(projectRoot, output, cfg.Go.ModulePath)
	if err != nil {
		return nil, err
	}
	return generatedGoManifestForImportPath(importPath, cfg, contractBytes, source, spec)
}

// generatedGoManifestForImportPath is the single manifest projection shared by
// the in-app api.Clients describer and the workspace clientgen command. Keeping
// the projection here prevents the two generation paths from drifting.
func generatedGoManifestForImportPath(importPath string, cfg clientGenConfig, contractBytes, source []byte, spec SpecIR) (*clientcontract.GeneratedClientManifestV1, error) {
	if spec.Contract == nil {
		return nil, fmt.Errorf("generated manifest requires a first-party contract")
	}
	// The manifest inventories the operations the emitted client has; the ones
	// go.omitOperations leaves out are named apart, so the workspace guard
	// accounts for every contract operation from committed bytes alone.
	spec, omitted, err := omitOperations(spec, cfg.Go.OmitOperations)
	if err != nil {
		return nil, err
	}
	// The emitter resolves symbols over this same post-omission contract, so
	// the manifest names every operation exactly as client.gen.go does.
	symbols, err := strictMethodSymbols(spec.Services)
	if err != nil {
		return nil, err
	}
	clientName := orDefault(cfg.Go.ClientName, "Client")
	contractHash := sha256.Sum256(contractBytes)
	sourceHash := sha256.Sum256(source)
	manifest := &clientcontract.GeneratedClientManifestV1{
		ProtocolVersion: clientcontract.ProtocolVersion,
		GeneratedBy:     clientcontract.GeneratedBy,
		Language:        clientcontract.GeneratedLanguageGo,
		Service:         spec.Contract.Service,
		Binding: clientcontract.GeneratedBinding{
			ImportPath: importPath,
			Clients: []clientcontract.GeneratedBindingClient{{
				Service:       spec.Contract.Service.ID,
				ClientSymbol:  clientName,
				BindingSymbol: "Register" + clientName,
			}},
		},
		ContractSHA256:    fmt.Sprintf("%x", contractHash),
		OmittedOperations: omittedOperationIDs(omitted),
		Files:             []clientcontract.GeneratedFile{{Path: "client.gen.go", SHA256: fmt.Sprintf("%x", sourceHash)}},
	}
	for _, service := range spec.Services {
		for _, method := range service.Methods {
			if method.Client == nil {
				return nil, fmt.Errorf("operation %s has no first-party client contract", method.OperationID)
			}
			operation := clientcontract.GeneratedOperation{
				OperationID:  method.OperationID,
				Service:      spec.Contract.Service.ID,
				MethodSymbol: symbols[method.OperationID],
				Stream:       method.Client.Stream,
				Transports:   append([]clientcontract.Transport(nil), method.Client.Transports...),
			}
			if method.Client.Resilience != nil {
				operation.Cache = cloneCachePolicy(method.Client.Resilience.Cache)
			}
			manifest.Operations = append(manifest.Operations, operation)
		}
	}
	manifest.RuntimeCapabilities = clientcontract.RequiredRuntimeCapabilities(manifest.Operations)
	clientcontract.SortGeneratedManifest(manifest)
	if diagnostics := clientcontract.ValidateGeneratedManifest(manifest); len(diagnostics) > 0 {
		return nil, fmt.Errorf("validate generated client manifest: %s", diagnostics[0].String())
	}
	return manifest, nil
}

func marshalGeneratedGoManifest(manifest *clientcontract.GeneratedClientManifestV1) ([]byte, error) {
	body, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode generated client manifest: %w", err)
	}
	return append(body, '\n'), nil
}

// generatedGoImportPath names the package consumers import. It is part of the
// ownership manifest, so it is resolved strictly: either the contract configures
// a standalone module path, or the provider project is itself a Go module and
// the client is a package inside it. Neither available means no consumer could
// import the emitted package, so generation fails with both remedies named
// rather than emitting a client no manifest can own.
func generatedGoImportPath(projectRoot, output, configured string) (string, error) {
	if configured != "" {
		return configured, nil
	}
	moduleBytes, err := readTrustedFile(filepath.Join(projectRoot, "go.mod"))
	if os.IsNotExist(err) {
		return "", fmt.Errorf(
			"resolve generated client import path: %s is not a Go module; set go.modulePath in the clientgen contract to emit a standalone client module, or run the generator inside the provider's Go module",
			projectRoot)
	}
	if err != nil {
		return "", fmt.Errorf("resolve generated client import path: %w", err)
	}
	for _, line := range strings.Split(string(moduleBytes), "\n") {
		if modulePath, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok && strings.TrimSpace(modulePath) != "" {
			return strings.TrimRight(strings.TrimSpace(modulePath), "/") + "/" + strings.Trim(output, "/"), nil
		}
	}
	return "", fmt.Errorf("resolve generated client import path: go.mod has no module directive")
}

func removeOwnedGeneratedTarget(targetDir string) error {
	manifestPath := filepath.Join(targetDir, clientcontract.GeneratedManifestFile)
	raw, err := readTrustedFile(manifestPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read prior generated client manifest: %w", err)
	}
	manifest, diagnostics := clientcontract.ParseAndValidateGeneratedManifest(raw)
	if len(diagnostics) > 0 || manifest.Language != clientcontract.GeneratedLanguageGo {
		return fmt.Errorf("refuse to remove generated target with an invalid ownership manifest")
	}
	for _, file := range manifest.Files {
		path := filepath.Join(targetDir, filepath.FromSlash(file.Path))
		body, readErr := readTrustedFile(path)
		if readErr != nil {
			return fmt.Errorf("verify owned generated file %s: %w", file.Path, readErr)
		}
		hash := sha256.Sum256(body)
		if fmt.Sprintf("%x", hash) != file.SHA256 {
			return fmt.Errorf("refuse to remove modified generated file %s", file.Path)
		}
	}
	for _, file := range manifest.Files {
		if err := os.Remove(filepath.Join(targetDir, filepath.FromSlash(file.Path))); err != nil {
			return fmt.Errorf("remove owned generated file %s: %w", file.Path, err)
		}
	}
	if err := os.Remove(manifestPath); err != nil {
		return fmt.Errorf("remove generated client manifest: %w", err)
	}
	return nil
}

// restoreStagingMarker is part of the name of every tree a cache restore
// stages under a unique name and then renames or reclaims.
const restoreStagingMarker = ".tmp-materialize-"

// removeObsoleteGeneratedTargets removes the owned files of every Go generated
// target under projectRoot except selectedTarget.
//
// Other tasks of the same project write under projectRoot while the walk runs.
// A cache restore of another output replaces its destination whole and stages
// the replacement under a unique name that vanishes again. So an entry below
// projectRoot that is gone by the time the walk reaches it is skipped, and so
// is a manifest that is gone by the time it is read. A directory whose name
// carries restoreStagingMarker is never entered: it is a restore in flight,
// not a target this project owns. projectRoot itself must exist.
func removeObsoleteGeneratedTargets(projectRoot, selectedTarget string) error {
	selectedTarget = filepath.Clean(selectedTarget)
	return filepath.WalkDir(projectRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			if path == projectRoot || !errors.Is(walkErr, fs.ErrNotExist) {
				return walkErr
			}
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			if path == projectRoot {
				return nil
			}
			switch name := entry.Name(); name {
			case ".git", ".gen", ".putnami", "node_modules":
				return filepath.SkipDir
			default:
				if strings.Contains(name, restoreStagingMarker) {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if entry.Name() != clientcontract.GeneratedManifestFile || filepath.Clean(filepath.Dir(path)) == selectedTarget {
			return nil
		}
		raw, err := readTrustedFile(path)
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		manifest, diagnostics := clientcontract.ParseAndValidateGeneratedManifest(raw)
		if len(diagnostics) != 0 || manifest.Language != clientcontract.GeneratedLanguageGo {
			return nil
		}
		return removeOwnedGeneratedTarget(filepath.Dir(path))
	})
}

// cleanClientOutputRel validates and normalizes the configured go.output, applying
// the default and rejecting any path that escapes the project root.
func cleanClientOutputRel(output string) (string, error) {
	if output == "" {
		output = "clients/go"
	}
	clean := filepath.Clean(filepath.FromSlash(output))
	if clean == "." || clean == ".." || filepath.IsAbs(clean) ||
		strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("clientgen go.output %q escapes project root", output)
	}
	return filepath.ToSlash(clean), nil
}

// readTrustedFile reads a file whose path is derived from the trusted project
// root joined with a fixed, code-controlled relative name (the clientgen contract
// or the OpenAPI spec) — never user-supplied input — so gosec's G304 file-
// inclusion warning does not apply.
func readTrustedFile(path string) ([]byte, error) {
	return os.ReadFile(path) //nolint:gosec // G304: project-root-derived path, fixed filename
}

// errIsNotExist reports whether err indicates a missing file. A nil error (the
// file exists) and any other error (e.g. a permission problem) both mean "do not
// scaffold", so go.mod is only ever written when the path is confirmed absent.
func errIsNotExist(err error) bool {
	return err != nil && os.IsNotExist(err)
}
