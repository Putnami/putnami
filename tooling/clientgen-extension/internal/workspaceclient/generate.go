package workspaceclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"go.putnami.dev/sdk/extension/putnamihome"
)

// GoGeneration is one run of the Go emitter: the contract it reads, the
// directory it writes client.gen.go into, and the options that decide the
// emitted bytes besides the contract.
type GoGeneration struct {
	// Contract is the absolute path of the provider's committed OpenAPI
	// document.
	Contract string
	// Output is the absolute directory that receives client.gen.go.
	Output string
	// PackageName and ClientName are the Go package and client type names. An
	// empty value leaves the emitter's default.
	PackageName string
	ClientName  string
	// OmitOperations are the operation IDs the client leaves out.
	OmitOperations []string
}

// TypeScriptGeneration is one run of the TypeScript emitter.
type TypeScriptGeneration struct {
	// Contract is the absolute path of the provider's committed OpenAPI
	// document.
	Contract string
	// Project is the absolute directory of the client project. The emitter
	// replaces its src tree, and formats it with the project's own Biome and
	// EditorConfig files.
	Project string
	// PackageName is the npm name of the client project, from its own
	// package.json.
	PackageName string
}

// GenerateGo runs the packaged Go emitter, putnami-client-generate-go, which
// prepare builds beside this runtime. It writes only <Output>/client.gen.go.
func GenerateGo(workspaceRoot string, generation GoGeneration) error {
	executable, err := goGeneratorExecutable()
	if err != nil {
		return err
	}
	command := exec.Command(executable, goGeneratorArguments(generation)...)
	command.Dir = generation.Output
	command.Env = replaceEnvironmentValue(os.Environ(), "PUTNAMI_WORKSPACE_ROOT", workspaceRoot)
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("go client generation into %s failed: %w\n%s", generation.Output, err, output)
	}
	return nil
}

// goGeneratorArguments is the Go emitter's command line. The omitted
// operations are sorted and deduplicated, so their order in putnami.json does
// not change the command.
func goGeneratorArguments(generation GoGeneration) []string {
	arguments := []string{"--contract", generation.Contract, "--output", generation.Output}
	if generation.PackageName != "" {
		arguments = append(arguments, "--package", generation.PackageName)
	}
	if generation.ClientName != "" {
		arguments = append(arguments, "--client", generation.ClientName)
	}
	omitted := slices.Clone(generation.OmitOperations)
	slices.Sort(omitted)
	for _, operationID := range slices.Compact(omitted) {
		arguments = append(arguments, "--omit", operationID)
	}
	return arguments
}

// GenerateTypeScript runs the @putnami/client emitter the workspace installed.
// It writes only the project's src tree.
func GenerateTypeScript(workspaceRoot string, generation TypeScriptGeneration) error {
	shim, err := typescriptGeneratorExecutable(workspaceRoot, generation.Project)
	if err != nil {
		return err
	}
	command, err := typescriptGeneratorCommand(shim, generation)
	if err != nil {
		return err
	}
	command.Dir = generation.Project
	command.Env = typescriptEmitterEnvironment(replaceEnvironmentValue(os.Environ(), "PUTNAMI_WORKSPACE_ROOT", workspaceRoot))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("typescript client generation for %s failed: %w\n%s", generation.Project, err, output)
	}
	return nil
}

// PackageName reads the npm name a client project declares in its own
// package.json.
func PackageName(projectRoot string) (string, error) {
	data, err := os.ReadFile(filepath.Join(projectRoot, "package.json"))
	if err != nil {
		return "", fmt.Errorf("read the client's package.json: %w", err)
	}
	var document struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("parse the client's package.json: %w", err)
	}
	if strings.TrimSpace(document.Name) == "" {
		return "", errors.New("the client's package.json declares no name; the generated client is published under it")
	}
	return document.Name, nil
}

// ModuleTarget is the in-module Go client a provider commits in clientgen.json:
// a Go package of the provider's own module, generated from the provider's own
// committed contract.
type ModuleTarget struct {
	// Output is the project-relative directory that receives client.gen.go.
	Output         string
	PackageName    string
	ClientName     string
	OmitOperations []string
}

// ReadModuleTarget reads the in-module Go target from the committed
// clientgen.json at the project root. It fails when the file names no Go
// target, names another language, or places the client outside the project.
func ReadModuleTarget(projectRoot string) (ModuleTarget, error) {
	data, err := os.ReadFile(filepath.Join(projectRoot, committedConfigFile))
	if err != nil {
		return ModuleTarget{}, fmt.Errorf("read %s: %w", committedConfigFile, err)
	}
	var config clientGenConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return ModuleTarget{}, fmt.Errorf("parse %s: %w", committedConfigFile, err)
	}
	if !slices.Equal(config.Targets, []string{"go"}) {
		return ModuleTarget{}, fmt.Errorf("%s targets %q; a provider generates only an in-module Go client, and every "+
			"other client is a project of its own that names its provider in generatedFrom", committedConfigFile, config.Targets)
	}
	if config.Go.ModulePath != "" {
		return ModuleTarget{}, fmt.Errorf("%s sets go.modulePath %q; an in-module client is a package of the provider's "+
			"own module, and a client module of its own is a project that names its provider in generatedFrom",
			committedConfigFile, config.Go.ModulePath)
	}
	output, err := safeWorkspacePath(config.Go.Output)
	if err != nil {
		return ModuleTarget{}, fmt.Errorf("%s go.output %q must name a directory inside the project", committedConfigFile, config.Go.Output)
	}
	return ModuleTarget{
		Output:         output,
		PackageName:    config.Go.PackageName,
		ClientName:     config.Go.ClientName,
		OmitOperations: config.Go.OmitOperations,
	}, nil
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

// typescriptGeneratorCommand runs the emitter shim. When the task resolved the
// runtime the workspace lock pins, the shim runs under that runtime; a
// declared but empty runtime is a resolution that failed, never a fallback to
// whatever the shim's own interpreter line names.
func typescriptGeneratorCommand(shim string, generation TypeScriptGeneration) (*exec.Cmd, error) {
	arguments := []string{shim,
		"--contract", generation.Contract,
		"--output", generation.Project,
		"--package-name", generation.PackageName,
		"--format-project", generation.Project,
	}
	const runtimeEnv = "PUTNAMI_CLIENTGEN_TYPESCRIPT_RUNTIME"
	if executable, declared := os.LookupEnv(runtimeEnv); declared {
		if strings.TrimSpace(executable) == "" {
			return nil, fmt.Errorf("typescript client generation requires the exact runtime pinned by the workspace lock")
		}
		return exec.Command(executable, arguments...), nil
	}
	return exec.Command(shim, arguments[1:]...), nil
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

// typescriptGeneratorExecutable finds the @putnami/client emitter shim the
// workspace installed: in the client project's own install first, then in
// the workspace root's.
func typescriptGeneratorExecutable(workspaceRoot, projectRoot string) (string, error) {
	name := "putnami-client-generate"
	if runtime.GOOS == "windows" {
		name += ".cmd"
	}
	candidates := []string{
		filepath.Join(projectRoot, "node_modules", ".bin", name),
		filepath.Join(workspaceRoot, "node_modules", ".bin", name),
	}
	for _, path := range candidates {
		if requireExecutable(path) == nil {
			return path, nil
		}
	}
	return "", fmt.Errorf("resolve @putnami/client generator package shim from the client project or the workspace; run `putnami deps install`")
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
