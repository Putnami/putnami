package workspaceclient

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	"go.putnami.dev/sdk/extension/putnamihome"
)

// RenderExpected creates an ephemeral mirror and runs the real language
// generators there. The returned cleanup must be called by the caller.
func RenderExpected(workspaceRoot string) (string, func(), error) {
	providers, findings := discover(workspaceRoot)
	if len(findings) > 0 {
		return "", func() {}, fmt.Errorf("cannot render invalid workspace contracts: %s", findings[0].Message)
	}
	tempRoot, err := os.MkdirTemp("", "putnami-clientgen-check-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(tempRoot) }
	if err := writeMirrorRoot(workspaceRoot, tempRoot); err != nil {
		cleanup()
		return "", func() {}, err
	}
	for _, provider := range providers {
		if provider.config == nil || provider.classification != ClassificationFirstParty {
			continue
		}
		mirrorProject := filepath.Join(tempRoot, filepath.FromSlash(provider.rel))
		if err := copyProviderInputs(workspaceRoot, tempRoot, provider); err != nil {
			cleanup()
			return "", func() {}, err
		}
		if err := runConfiguredGenerators(workspaceRoot, tempRoot, mirrorProject, provider.config); err != nil {
			cleanup()
			return "", func() {}, err
		}
	}
	return tempRoot, cleanup, nil
}

// Synchronize runs the real configured generators against provider projects.
func Synchronize(workspaceRoot string) error {
	providers, findings := discover(workspaceRoot)
	if len(findings) > 0 {
		return fmt.Errorf("cannot synchronize invalid workspace contracts: %s", findings[0].Message)
	}
	for _, provider := range providers {
		if provider.config == nil || provider.classification != ClassificationFirstParty {
			continue
		}
		if err := runConfiguredGenerators(workspaceRoot, workspaceRoot, provider.root, provider.config); err != nil {
			return err
		}
	}
	return nil
}

// SynchronizeTarget regenerates one configured language target and returns the
// project-relative output directory when it exists.
//
// A missing .gen/clientgen/config.json means "nothing to generate" only for a
// provider that commits no client in that language. When the provider commits
// one, the contract is missing, not empty: a writer of .gen may be mid-rewrite,
// and reporting no output would let the engine empty the committed target
// while the task succeeds. That case fails and leaves the target alone.
func SynchronizeTarget(workspaceRoot, projectRoot string, language clientcontract.GeneratedLanguage) (string, bool, error) {
	data, err := os.ReadFile(filepath.Join(projectRoot, ".gen", "clientgen", "config.json")) //nolint:gosec
	if err != nil {
		if os.IsNotExist(err) {
			return "", false, requireNoCommittedTarget(workspaceRoot, projectRoot, language)
		}
		return "", false, err
	}
	var config clientGenConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return "", false, err
	}
	enabled := false
	for _, target := range config.Targets {
		if target == string(language) {
			enabled = true
			break
		}
	}
	if !enabled {
		return "", false, nil
	}
	filtered := config
	filtered.Targets = []string{string(language)}
	if err := runConfiguredGenerators(workspaceRoot, workspaceRoot, projectRoot, &filtered); err != nil {
		return "", false, err
	}
	output := defaultOutput(config.Go.Output, "clients/go")
	if language == clientcontract.GeneratedLanguageTypeScript {
		output = defaultOutput(config.TS.Output, "clients/ts")
	}
	if _, err := os.Stat(filepath.Join(projectRoot, filepath.FromSlash(output))); err != nil {
		if os.IsNotExist(err) {
			return "", false, nil
		}
		return "", false, err
	}
	return output, true, nil
}

// requireNoCommittedTarget fails when the provider commits a generated client
// in language, or a manifest nobody can read as one, while its generation
// contract is missing. It reads the committed manifests through the same
// decoder discovery uses for a cold tree.
func requireNoCommittedTarget(workspaceRoot, projectRoot string, language clientcontract.GeneratedLanguage) error {
	committed, findings := configFromCommittedManifests(workspaceRoot, projectRoot)
	if len(findings) > 0 {
		return fmt.Errorf("%s has no .gen/clientgen/config.json and %s: %s", projectRoot, findings[0].Path, findings[0].Message)
	}
	if committed == nil || !hasConfiguredTarget(committed.Targets, string(language)) {
		return nil
	}
	output := committed.Go.Output
	if language == clientcontract.GeneratedLanguageTypeScript {
		output = committed.TS.Output
	}
	return fmt.Errorf("%s commits a %s client at %s but has no .gen/clientgen/config.json, so the committed "+
		"client is left untouched: run the provider's build, or delete the committed client if the provider "+
		"no longer generates one", projectRoot, language, output)
}

// mirrorRootInputs are the workspace-root files whose presence or bytes decide
// what the TypeScript emitter writes; the Go emitter reads none. The mirror
// holds each one byte for byte, so a render there writes the bytes a render in
// the workspace writes:
//
//   - putnami.workspace.json marks the root the emitter resolves;
//   - package.json decides how a TypeScript client depends on
//     @putnami/client: `catalog:` when a root catalog lists it, the pinned
//     version when the root dependencies pin it, `workspace:*` otherwise;
//   - tsconfig.base.json is the file the TypeScript client's tsconfig extends;
//     the emitter writes its path, not its bytes.
//
// The formatter configuration is not a root input of the mirror: the
// TypeScript emitter resolves it from the real provider (--format-project).
var mirrorRootInputs = []string{"putnami.workspace.json", "package.json", "tsconfig.base.json"}

func writeMirrorRoot(workspaceRoot, tempRoot string) error {
	for _, name := range mirrorRootInputs {
		if err := copyOptional(filepath.Join(workspaceRoot, name), filepath.Join(tempRoot, name)); err != nil {
			return err
		}
	}
	if _, err := os.Stat(filepath.Join(tempRoot, "putnami.workspace.json")); os.IsNotExist(err) {
		if writeErr := os.WriteFile(filepath.Join(tempRoot, "putnami.workspace.json"), []byte("{\"name\":\"clientgen-check\"}\n"), 0o600); writeErr != nil {
			return writeErr
		}
	}
	return nil
}

func copyProviderInputs(workspaceRoot, tempRoot string, provider provider) error {
	destinationRoot := filepath.Join(tempRoot, filepath.FromSlash(provider.rel))
	// The formatter configuration deliberately does NOT travel into the mirror:
	// the TypeScript emitter is invoked with --format-project naming the REAL
	// provider root, so Biome's package, configuration, overrides and
	// EditorConfig are resolved against the provider's own identity. Copying
	// them here would create a second, silently divergent copy of the same
	// inputs. FormatterInputs derives that same resolution for the cache key.
	inputs := []string{
		joinRel(provider.rel, ".gen/clientgen/config.json"),
		provider.specPath,
		joinRel(provider.rel, "package.json"),
		joinRel(provider.rel, "go.mod"),
	}
	for _, rel := range inputs {
		if rel == "" {
			continue
		}
		if err := copyOptional(filepath.Join(workspaceRoot, filepath.FromSlash(rel)),
			filepath.Join(tempRoot, filepath.FromSlash(rel))); err != nil {
			return err
		}
	}
	if provider.config != nil && hasConfiguredTarget(provider.config.Targets, "go") {
		output, err := safeWorkspacePath(defaultOutput(provider.config.Go.Output, "clients/go"))
		if err != nil {
			return fmt.Errorf("copy Go client target metadata: %w", err)
		}
		for _, name := range []string{"go.mod", "putnami.json"} {
			rel := joinRel(provider.rel, output, name)
			if err := copyOptional(filepath.Join(workspaceRoot, filepath.FromSlash(rel)),
				filepath.Join(tempRoot, filepath.FromSlash(rel))); err != nil {
				return err
			}
		}
	}
	return os.MkdirAll(destinationRoot, 0o750)
}

func hasConfiguredTarget(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func copyOptional(source, destination string) error {
	input, err := os.Open(source) //nolint:gosec // paths are workspace-derived
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer input.Close()
	if err := os.MkdirAll(filepath.Dir(destination), 0o750); err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600) //nolint:gosec
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(output, input)
	closeErr := output.Close()
	if copyErr != nil {
		return copyErr
	}
	return closeErr
}

func runConfiguredGenerators(workspaceRoot, commandRoot, projectRoot string, config *clientGenConfig) error {
	targets := map[string]bool{}
	for _, target := range config.Targets {
		targets[target] = true
	}
	var goExecutable string
	var typescriptExecutable string
	var sourceProjectRoot string
	if targets["go"] {
		var err error
		goExecutable, err = goGeneratorExecutable()
		if err != nil {
			return err
		}
	}
	if targets["ts"] {
		var err error
		var resolveErr error
		sourceProjectRoot, resolveErr = sourceProjectForCommandRoot(workspaceRoot, commandRoot, projectRoot)
		if resolveErr != nil {
			return resolveErr
		}
		typescriptExecutable, err = typescriptGeneratorExecutable(workspaceRoot, sourceProjectRoot, config)
		if err != nil {
			return err
		}
	}
	return runResolvedGenerators(commandRoot, projectRoot, sourceProjectRoot, goExecutable, typescriptExecutable)
}

func sourceProjectForCommandRoot(workspaceRoot, commandRoot, projectRoot string) (string, error) {
	rel, err := filepath.Rel(commandRoot, projectRoot)
	if err != nil {
		return "", fmt.Errorf("resolve provider project relative to generator command root: %w", err)
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("provider project %s is outside generator command root %s", projectRoot, commandRoot)
	}
	return filepath.Join(workspaceRoot, rel), nil
}

func runResolvedGenerators(workspaceRoot, projectRoot, formatProjectRoot, goExecutable, typescriptExecutable string) error {
	if goExecutable != "" {
		command := exec.Command(goExecutable, "--project", projectRoot) //nolint:gosec // executable is the extension-packaged framework emitter
		command.Dir = projectRoot
		command.Env = replaceEnvironmentValue(os.Environ(), "PUTNAMI_WORKSPACE_ROOT", workspaceRoot)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("go client generation for %s failed: %w\n%s", projectRoot, err, output)
		}
	}
	if typescriptExecutable != "" {
		command, err := typescriptGeneratorCommand(typescriptExecutable, projectRoot, formatProjectRoot)
		if err != nil {
			return err
		}
		command.Dir = projectRoot
		command.Env = typescriptEmitterEnvironment(replaceEnvironmentValue(os.Environ(), "PUTNAMI_WORKSPACE_ROOT", workspaceRoot))
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("typescript client generation for %s failed: %w\n%s", projectRoot, err, output)
		}
	}
	return nil
}

// typescriptEmitterEnvironment keeps what the emitter's Bun writes under its
// install when Putnami installed that Bun.
//
// The CLI names the install of the resolved runtime in BUN_INSTALL, and Bun
// keeps its package cache under it. Bun does not derive its transpiler cache
// from BUN_INSTALL: without BUN_RUNTIME_TRANSPILER_CACHE_PATH it writes that
// cache under the user's home directory. For an install under the Putnami
// home, toolchains/bun/bun-<version>, the cache is install/cache/@t@ in it. A
// Bun the host holds keeps its own locations, and an explicit setting is left
// alone.
func typescriptEmitterEnvironment(environment []string) []string {
	const installEnv, cacheEnv = "BUN_INSTALL", "BUN_RUNTIME_TRANSPILER_CACHE_PATH"
	install := strings.TrimSpace(environmentValue(environment, installEnv))
	if !putnamihome.IsToolchainInstall(install, "bun") || strings.TrimSpace(environmentValue(environment, cacheEnv)) != "" {
		return environment
	}
	return replaceEnvironmentValue(environment, cacheEnv, filepath.Join(install, "install", "cache", "@t@"))
}

// environmentValue returns the value the last entry of environment gives key,
// which is the one a started program sees.
func environmentValue(environment []string, key string) string {
	prefix := key + "="
	for index := len(environment) - 1; index >= 0; index-- {
		if value, found := strings.CutPrefix(environment[index], prefix); found {
			return value
		}
	}
	return ""
}

func replaceEnvironmentValue(environment []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(environment)+1)
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func typescriptGeneratorCommand(shim, projectRoot, formatProjectRoot string) (*exec.Cmd, error) {
	arguments := []string{shim, "--project", projectRoot, "--format-project", formatProjectRoot}
	const runtimeEnv = "PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME"
	if executable, declared := os.LookupEnv(runtimeEnv); declared {
		if strings.TrimSpace(executable) == "" {
			return nil, fmt.Errorf("typescript client generation requires the exact runtime pinned by the workspace lock")
		}
		return exec.Command(executable, arguments...), nil //nolint:gosec // both executable paths are resolved from typed runtime/package declarations
	}
	return exec.Command(shim, arguments[1:]...), nil //nolint:gosec // compatibility for direct low-level/test invocation
}

func goGeneratorExecutable() (string, error) {
	runtimeExecutable, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve clientgen extension runtime: %w", err)
	}
	name := "putnami-client-generate-go"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(filepath.Dir(runtimeExecutable), name)
	if err := requireExecutable(path); err != nil {
		return "", fmt.Errorf("resolve packaged Go client emitter: %w", err)
	}
	return path, nil
}

func typescriptGeneratorExecutable(workspaceRoot, projectRoot string, config *clientGenConfig) (string, error) {
	name := "putnami-client-generate"
	if runtime.GOOS == "windows" {
		name += ".cmd"
	}
	output := defaultOutput(config.TS.Output, "clients/ts")
	candidates := []string{
		filepath.Join(projectRoot, filepath.FromSlash(output), "node_modules", ".bin", name),
		filepath.Join(projectRoot, "node_modules", ".bin", name),
		filepath.Join(workspaceRoot, "node_modules", ".bin", name),
	}
	for _, path := range candidates {
		if requireExecutable(path) == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("resolve @putnami/client generator package shim from generated target, provider, or workspace; run `putnami deps install`")
}

func requireExecutable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable regular file", path)
	}
	return nil
}
