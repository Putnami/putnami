package build

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.putnami.dev/typescript/extension/internal/project"
)

// The modules the bundled serve entry (.gen/src/serve.bundled.ts) imports
// from its own directory.
const (
	// bundledServeConfigFile registers the project's embedded conf/ files.
	bundledServeConfigFile = "serve.bundled.config.ts"
	// bundledServeLoadersFile registers the generated server modules and the
	// start guard.
	bundledServeLoadersFile = "serve.bundled.loaders.ts"
)

// defaultServeEntry is the serve entry a project writes by convention.
const defaultServeEntry = "src/serve.ts"

// ProjectServeEntry is the project's own serve entry, relative to
// projectPath, that the bundled serve entry runs: the file package.json's
// ./serve export names, the entry `putnami serve` runs, when it is a
// TypeScript file under src/, which the generate key reads; else src/serve.ts
// when it exists. It answers "" when the project has neither, and the bundled
// entry then starts the app entrypoint's `app` itself.
func ProjectServeEntry(projectPath string) string {
	if pkg := project.ReadPackageJSONSafe(filepath.Join(projectPath, "package.json")); pkg != nil {
		exported := projectFile(projectPath, project.ResolveExportPath(pkg.Exports, "./serve"))
		if strings.HasPrefix(exported, "src/") && (strings.HasSuffix(exported, ".ts") || strings.HasSuffix(exported, ".tsx")) {
			return exported
		}
	}
	return projectFile(projectPath, defaultServeEntry)
}

// projectFile cleans rel and answers it when it names an existing file inside
// projectPath and outside its generated .gen directory, else "".
func projectFile(projectPath, rel string) string {
	if rel == "" || filepath.IsAbs(rel) {
		return ""
	}
	clean := filepath.Clean(filepath.FromSlash(rel))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return ""
	}
	if clean == ".gen" || strings.HasPrefix(clean, ".gen"+string(filepath.Separator)) {
		return ""
	}
	info, err := os.Stat(filepath.Join(projectPath, clean))
	if err != nil || info.IsDir() {
		return ""
	}
	return filepath.ToSlash(clean)
}

// machineLocalEnvs are the environments whose conf/.env.<env>.yaml is a
// machine-local override: a workspace's .gitignore ignores
// conf/.env.local.yaml and conf/.env.prod.yaml, which may hold credentials.
// They are never embedded, and the generate key excludes them.
var machineLocalEnvs = []string{"local", "prod"}

// EmbeddedConfigFiles lists the project's conf/ files a compiled entry
// embeds: conf/.env.yaml and every conf/.env.<env>.yaml, the files the
// runtime reads its base and environment config from, except the
// machine-local overrides (machineLocalEnvs). Secrets files
// (conf/.secrets.<env>.yaml) are never embedded. The answer maps an
// environment name to its file, with "" for the base file.
func EmbeddedConfigFiles(projectPath string) (map[string]string, error) {
	confDir := filepath.Join(projectPath, "conf")
	entries, err := os.ReadDir(confDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", confDir, err)
	}
	files := make(map[string]string)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ".env.") || !strings.HasSuffix(name, ".yaml") {
			continue
		}
		env := strings.TrimSuffix(strings.TrimPrefix(name, ".env."), ".yaml")
		if name == ".env.yaml" {
			env = ""
		} else if env == "" || slices.Contains(machineLocalEnvs, env) {
			continue
		}
		files[env] = filepath.Join(confDir, name)
	}
	return files, nil
}

// embeddedConfigModule is the source of the module that registers the
// project's conf/ files (see EmbeddedConfigFiles) as embedded config, their
// YAML text as string literals, in a fixed order, so one conf/ tree always
// writes the same bytes.
func embeddedConfigModule(projectPath string) (string, error) {
	files, err := EmbeddedConfigFiles(projectPath)
	if err != nil {
		return "", err
	}
	envs := make([]string, 0, len(files))
	for env := range files {
		envs = append(envs, env)
	}
	sort.Strings(envs)

	var lines []string
	lines = append(lines, "import { registerEmbeddedConfig } from '"+bundledServeRuntime+"';")
	lines = append(lines, "")
	lines = append(lines, "// The project's conf/ files, embedded so the executable resolves its base and")
	lines = append(lines, "// environment config wherever it runs.")
	lines = append(lines, "registerEmbeddedConfig({")
	var envLines []string
	for _, env := range envs {
		data, err := os.ReadFile(files[env])
		if err != nil {
			return "", fmt.Errorf("reading %s: %w", files[env], err)
		}
		text, err := json.Marshal(string(data))
		if err != nil {
			return "", fmt.Errorf("encoding %s: %w", files[env], err)
		}
		if env == "" {
			lines = append(lines, "  base: "+string(text)+",")
			continue
		}
		key, err := json.Marshal(env)
		if err != nil {
			return "", fmt.Errorf("encoding environment %q: %w", env, err)
		}
		envLines = append(envLines, "    "+string(key)+": "+string(text)+",")
	}
	if len(envLines) > 0 {
		lines = append(lines, "  envs: {")
		lines = append(lines, envLines...)
		lines = append(lines, "  },")
	}
	lines = append(lines, "});")
	lines = append(lines, "")
	return strings.Join(lines, "\n"), nil
}

// importSpecifier is the relative specifier of a module the bundled serve
// entry imports from its own directory.
func importSpecifier(file string) string {
	return normalizeImportPath("./" + file)
}
