// Package configmerge merges configuration files from a project's workspace
// dependencies into .gen/conf/.env.{env}.yaml, mirroring the TypeScript
// ConfigPlugin behavior.
//
// Each project's config-merge step runs after its dependencies' config-merge
// steps (via dependsOn: ["^config-merge"]), so transitive merging happens
// naturally through the pipeline DAG.
//
// The merged file is a DECLARED output of both config-merge tasks
// (go/extension/putnami.extension.json): build-generate cedes .gen/conf, so a
// config-merge cache hit restores the file and a generate restore leaves it
// alone. config-merge-exec names the file through the mergedConfig
// port because its environment suffix comes from the ambient APP_ENV; the
// port therefore carries a PROJECT-RELATIVE slash path, which is the only
// shape the CLI accepts for a declared output (an absolute path is dropped).
package configmerge

import (
	"encoding/json"
	"os"
	"path"
	"path/filepath"
	"time"

	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
	"gopkg.in/yaml.v3"
)

// MergedConfigPort is the result-data port through which a successful run
// reports the project-relative slash path of the merged config file. The
// manifest's config-merge-exec task declares its output with
// `pathFrom: "mergedConfig"`, so this name is a contract with the CLI, not a
// label. A SKIP reports nothing under it: the declared output is optionalEmpty
// and an absent port means "this run produced no such file".
const MergedConfigPort = "mergedConfig"

// mergedConfigRelPath returns the project-relative slash path of the merged
// config file for env, the path the mergedConfig port reports.
func mergedConfigRelPath(env string) string {
	return path.Join(".gen", "conf", ".env."+env+".yaml")
}

// Run executes the config-merge job.
func Run(ctx *pctx.Context, emit *jsonl.Emitter, args []string) (string, map[string]any, error) {
	emit.PhaseStart("config-merge")

	// Skip if CONFIG_DATA or K_SERVICE is set (cloud deployment — config is injected).
	if os.Getenv("CONFIG_DATA") != "" || os.Getenv("K_SERVICE") != "" {
		emit.PhaseEnd("config-merge", "skipped")
		return "SKIP", map[string]any{"reason": "runtime config injection active"}, nil
	}

	projectPath := ctx.Project.FullPath
	env := environment()

	// Discover dependencies from putnami.json.
	deps, err := readDependencies(projectPath)
	if err != nil {
		emit.Log("warn", "failed to read putnami.json dependencies: "+err.Error())
	}

	if len(deps) == 0 {
		// No dependencies — check if there's own config to generate.
		ownConfig := loadProjectConfig(projectPath, env)
		if ownConfig == nil {
			emit.PhaseEnd("config-merge", "skipped")
			return "SKIP", map[string]any{"reason": "no dependencies and no config files"}, nil
		}
	}

	// Merge configs: dependencies first (in order), then own project last.
	merged := make(map[string]any)
	sources := make(map[string]int64) // path → mtime for manifest

	for _, dep := range deps {
		depPath := filepath.Join(ctx.WorkspaceRoot, dep)

		// Prefer .gen/conf/ (already merged by dep's own config-merge step).
		genPath := filepath.Join(depPath, ".gen", "conf", ".env."+env+".yaml")
		if data, mtime := loadYAMLFile(genPath); data != nil {
			DeepMerge(merged, data)
			sources[genPath] = mtime
			continue
		}

		// Fall back to dep's source config files.
		basePath := filepath.Join(depPath, "conf", ".env.yaml")
		if data, mtime := loadYAMLFile(basePath); data != nil {
			DeepMerge(merged, data)
			sources[basePath] = mtime
		}
		envPath := filepath.Join(depPath, "conf", ".env."+env+".yaml")
		if data, mtime := loadYAMLFile(envPath); data != nil {
			DeepMerge(merged, data)
			sources[envPath] = mtime
		}
	}

	// Merge own project's source config (highest priority among files).
	basePath := filepath.Join(projectPath, "conf", ".env.yaml")
	if data, mtime := loadYAMLFile(basePath); data != nil {
		DeepMerge(merged, data)
		sources[basePath] = mtime
	}
	envPath := filepath.Join(projectPath, "conf", ".env."+env+".yaml")
	if data, mtime := loadYAMLFile(envPath); data != nil {
		DeepMerge(merged, data)
		sources[envPath] = mtime
	}

	if len(merged) == 0 {
		emit.PhaseEnd("config-merge", "skipped")
		return "SKIP", map[string]any{"reason": "no config values to merge"}, nil
	}

	// Write .gen/conf/.env.{env}.yaml
	outDir := filepath.Join(projectPath, ".gen", "conf")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		emit.PhaseEnd("config-merge", "failed")
		return "FAILED", nil, err
	}

	yamlData, err := yaml.Marshal(merged)
	if err != nil {
		emit.PhaseEnd("config-merge", "failed")
		return "FAILED", nil, err
	}

	outRel := mergedConfigRelPath(env)
	outPath := filepath.Join(projectPath, filepath.FromSlash(outRel))
	if err := os.WriteFile(outPath, yamlData, 0o644); err != nil {
		emit.PhaseEnd("config-merge", "failed")
		return "FAILED", nil, err
	}

	// Write .manifest.json, which records the merge's sources. It is NOT a
	// declared output: generatedAt varies between two equivalent runs, and no
	// consumer reads it.
	manifest := map[string]any{
		"generatedAt": time.Now().UnixMilli(),
		"sources":     sources,
	}
	manifestData, _ := json.MarshalIndent(manifest, "", "  ")
	manifestPath := filepath.Join(outDir, ".manifest.json")
	os.WriteFile(manifestPath, manifestData, 0o644)

	emit.PhaseEnd("config-merge", "success")
	return "OK", map[string]any{
		"output":         outPath,
		MergedConfigPort: outRel,
		"sources":        len(sources),
	}, nil
}

// environment returns the current application environment from APP_ENV.
// Defaults to "local" if not set.
func environment() string {
	if env := os.Getenv("APP_ENV"); env != "" {
		return env
	}
	return "local"
}

// putnamiJSON is the minimal structure we need from putnami.json.
type putnamiJSON struct {
	Dependencies []string `json:"dependencies"`
}

// readDependencies reads the dependencies array from a project's putnami.json.
func readDependencies(projectPath string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(projectPath, "putnami.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var p putnamiJSON
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, err
	}
	return p.Dependencies, nil
}

// loadProjectConfig checks if a project has any config files for the given env.
func loadProjectConfig(projectPath, env string) map[string]any {
	result := make(map[string]any)
	basePath := filepath.Join(projectPath, "conf", ".env.yaml")
	if data, _ := loadYAMLFile(basePath); data != nil {
		DeepMerge(result, data)
	}
	envPath := filepath.Join(projectPath, "conf", ".env."+env+".yaml")
	if data, _ := loadYAMLFile(envPath); data != nil {
		DeepMerge(result, data)
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// loadYAMLFile reads and parses a YAML file. Returns nil if the file doesn't exist.
// Also returns the file's modification time in milliseconds.
func loadYAMLFile(path string) (map[string]any, int64) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, 0
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, 0
	}
	var result map[string]any
	if err := yaml.Unmarshal(data, &result); err != nil {
		return nil, 0
	}
	return result, info.ModTime().UnixMilli()
}

// DeepMerge recursively merges src into dst.
// Nested maps are merged; scalar values from src overwrite dst.
func DeepMerge(dst, src map[string]any) {
	for k, v := range src {
		if srcMap, ok := v.(map[string]any); ok {
			if dstMap, ok := dst[k].(map[string]any); ok {
				DeepMerge(dstMap, srcMap)
				continue
			}
		}
		dst[k] = v
	}
}
