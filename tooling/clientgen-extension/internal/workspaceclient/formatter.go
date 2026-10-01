package workspaceclient

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// The TypeScript emitter canonicalizes every file it writes by running Biome's
// format and lint writer phases over it, with the file's FINAL path as
// --stdin-file-path, so overrides and EditorConfig are evaluated against the
// real identity of the generated file. That makes the formatter configuration a
// generation input as literally as the provider contract is: change a rule, an
// override or an indent width and the emitted bytes change.
//
// Two consequences follow, and this file is the single place both are derived
// from.
//
//   - The isolated render must carry the same configuration as the worktree,
//     otherwise "expected" bytes come from a different formatter than the ones
//     on disk and every generated file reads as drift.
//   - The clientgen-ts cache key must include those files, otherwise a rule
//     change is a cache HIT on bytes formatted by the previous rules.
//
// The resolution order below mirrors resolveBiomeConfig in
// typescript/framework/client/src/generator/generated-file-canonicalizer.ts.
// FormatterInputsAreCovered is the guard that fails when that resolution starts
// reaching a file the manifest does not declare.

const (
	biomeConfigFile  = "biome.json"
	editorConfigFile = ".editorconfig"
)

type biomeConfig struct {
	Extends []string `json:"extends"`
}

// FormatterInputs returns every workspace-relative file the TypeScript emitter's
// canonicalizer reads to decide the bytes it writes for one provider project,
// in deterministic order. Only files that exist are returned: a candidate the
// resolution would have used if present is not an input of the run that did not
// read it, and the declared cache-key globs are what make its later appearance
// a miss.
func FormatterInputs(workspaceRoot, projectRel string) ([]string, error) {
	projectConfig := joinRel(projectRel, biomeConfigFile)
	configRel := projectConfig
	if !regularFile(workspaceRoot, configRel) {
		configRel = biomeConfigFile
		if !regularFile(workspaceRoot, configRel) {
			return nil, nil
		}
	}
	configRoot := filepath.ToSlash(filepath.Dir(configRel))
	inputs := map[string]bool{}
	if err := collectBiomeConfigChain(workspaceRoot, configRel, inputs, 0); err != nil {
		return nil, err
	}
	if editorConfig := joinRel(configRoot, editorConfigFile); regularFile(workspaceRoot, editorConfig) {
		inputs[editorConfig] = true
	}
	resolved := make([]string, 0, len(inputs))
	for path := range inputs {
		resolved = append(resolved, path)
	}
	sort.Strings(resolved)
	return resolved, nil
}

// collectBiomeConfigChain follows `extends` the way Biome does: each entry is
// resolved against the directory of the file that declares it.
func collectBiomeConfigChain(workspaceRoot, configRel string, inputs map[string]bool, depth int) error {
	const maxExtendsDepth = 16
	if depth > maxExtendsDepth {
		return fmt.Errorf("biome configuration %q extends more than %d levels deep", configRel, maxExtendsDepth)
	}
	if inputs[configRel] {
		return nil
	}
	inputs[configRel] = true
	data, err := os.ReadFile(filepath.Join(workspaceRoot, filepath.FromSlash(configRel))) //nolint:gosec // workspace-relative configuration
	if err != nil {
		return fmt.Errorf("read biome configuration %q: %w", configRel, err)
	}
	var config biomeConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return fmt.Errorf("parse biome configuration %q: %w", configRel, err)
	}
	base := filepath.ToSlash(filepath.Dir(configRel))
	for _, extended := range config.Extends {
		if strings.TrimSpace(extended) == "" {
			continue
		}
		// A bare specifier resolves through node module resolution, which is not
		// a workspace path. It is reported rather than guessed at, because a
		// silently unhashed formatter input is exactly the cache lie this guard
		// exists to prevent.
		if !strings.HasPrefix(extended, ".") && !strings.HasPrefix(extended, "/") {
			return fmt.Errorf("biome configuration %q extends the package specifier %q, "+
				"which is not a declarable workspace cache-key path", configRel, extended)
		}
		next, err := safeWorkspacePath(joinRel(base, extended))
		if err != nil {
			return fmt.Errorf("resolve biome extends %q from %q: %w", extended, configRel, err)
		}
		if !regularFile(workspaceRoot, next) {
			return fmt.Errorf("biome configuration %q extends %q, which is not a workspace file", configRel, extended)
		}
		if err := collectBiomeConfigChain(workspaceRoot, next, inputs, depth+1); err != nil {
			return err
		}
	}
	return nil
}

func regularFile(workspaceRoot, rel string) bool {
	info, err := os.Stat(filepath.Join(workspaceRoot, filepath.FromSlash(rel))) //nolint:gosec // workspace-relative path
	return err == nil && info.Mode().IsRegular()
}
