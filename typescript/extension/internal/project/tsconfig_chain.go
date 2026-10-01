package project

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// TsConfigValue is one setting a tsconfig extends chain resolves, with the
// directory of the config file that declares it. tsc resolves a relative path
// in a setting against that directory.
type TsConfigValue struct {
	Value any
	Dir   string
}

// TsConfigChain holds the settings a tsconfig file resolves once its extends
// chain is applied: each compilerOptions entry and the top-level files list,
// taken from the most derived config that declares them.
type TsConfigChain struct {
	CompilerOptions map[string]TsConfigValue
	Files           *TsConfigValue
}

// ReadTsConfigChain reads the tsconfig at path and every config it extends,
// following tsc's precedence: a config overrides the configs it extends, and a
// later entry of an extends array overrides an earlier one. A config that
// cannot be read, parsed or located contributes nothing; tsc reports it when it
// loads the chain itself.
func ReadTsConfigChain(path string) TsConfigChain {
	chain := TsConfigChain{CompilerOptions: map[string]TsConfigValue{}}
	readTsConfigInto(path, &chain, map[string]bool{})
	return chain
}

// utf8BOM is the byte-order mark an editor may write at the start of a
// tsconfig; tsc ignores it.
var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

func readTsConfigInto(path string, chain *TsConfigChain, visiting map[string]bool) {
	if visiting[path] {
		return
	}
	visiting[path] = true
	defer delete(visiting, path)

	data, err := os.ReadFile(path) //nolint:gosec // a tsconfig the project resolves
	if err != nil {
		return
	}
	var cfg struct {
		Extends         json.RawMessage `json:"extends"`
		CompilerOptions map[string]any  `json:"compilerOptions"`
		Files           any             `json:"files"`
	}
	if json.Unmarshal(StripJSONC(bytes.TrimPrefix(data, utf8BOM)), &cfg) != nil {
		return
	}
	dir := filepath.Dir(path)

	var extends []string
	var single string
	if json.Unmarshal(cfg.Extends, &single) == nil {
		extends = []string{single}
	} else {
		_ = json.Unmarshal(cfg.Extends, &extends)
	}
	for _, spec := range extends {
		if base, ok := resolveTsConfigExtends(spec, dir); ok {
			readTsConfigInto(base, chain, visiting)
		}
	}

	for name, value := range cfg.CompilerOptions {
		chain.CompilerOptions[name] = TsConfigValue{Value: value, Dir: dir}
	}
	if cfg.Files != nil {
		chain.Files = &TsConfigValue{Value: cfg.Files, Dir: dir}
	}
}

// resolveTsConfigExtends locates an extends entry the way tsc does: a relative
// or absolute path names a file, with an implied .json extension; any other
// entry names a package, or a file inside one, in the nearest node_modules.
func resolveTsConfigExtends(spec, fromDir string) (string, bool) {
	if spec == "" {
		return "", false
	}
	if filepath.IsAbs(spec) || strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../") || spec == "." || spec == ".." {
		path := spec
		if !filepath.IsAbs(path) {
			path = filepath.Join(fromDir, spec)
		}
		return existingTsConfigFile(path)
	}
	name, subpath := splitPackageSpec(spec)
	for dir := fromDir; ; dir = filepath.Dir(dir) {
		packageDir := filepath.Join(dir, "node_modules", filepath.FromSlash(name))
		if info, err := os.Stat(packageDir); err == nil && info.IsDir() {
			if path, ok := resolvePackageTsConfig(packageDir, subpath); ok {
				// tsc loads a package config through its real path.
				if real, err := filepath.EvalSymlinks(path); err == nil {
					return real, true
				}
				return path, true
			}
		}
		if filepath.Dir(dir) == dir {
			return "", false
		}
	}
}

// splitPackageSpec splits a package specifier into the package name and the
// "."-rooted subpath inside it: "@scope/cfg/base.json" is ("@scope/cfg",
// "./base.json").
func splitPackageSpec(spec string) (string, string) {
	parts := strings.SplitN(spec, "/", 3)
	nameParts := 1
	if strings.HasPrefix(spec, "@") && len(parts) > 1 {
		nameParts = 2
	}
	if len(parts) <= nameParts {
		return spec, "."
	}
	return strings.Join(parts[:nameParts], "/"), "./" + strings.Join(parts[nameParts:], "/")
}

// tsConfigExportConditions are the package.json "exports" conditions tsc
// matches when it resolves a config it extends.
var tsConfigExportConditions = map[string]bool{"default": true, "node": true, "require": true, "types": true}

// resolvePackageTsConfig resolves subpath inside an installed package. A
// package.json "exports" map decides alone when present. Otherwise the subpath
// names a file or a directory, and the package root resolves to the file its
// package.json "tsconfig" field names, then to its tsconfig.json.
func resolvePackageTsConfig(packageDir, subpath string) (string, bool) {
	var pkg struct {
		Exports  json.RawMessage `json:"exports"`
		TsConfig string          `json:"tsconfig"`
	}
	if data, err := os.ReadFile(filepath.Join(packageDir, "package.json")); err == nil { //nolint:gosec // an installed package manifest
		_ = json.Unmarshal(bytes.TrimPrefix(data, utf8BOM), &pkg)
	}
	if len(pkg.Exports) > 0 && string(pkg.Exports) != "null" {
		target, ok := exportsTarget(pkg.Exports, subpath)
		if !ok {
			return "", false
		}
		return existingTsConfigFile(filepath.Join(packageDir, filepath.FromSlash(target)))
	}
	target := filepath.Join(packageDir, filepath.FromSlash(subpath))
	if path, ok := existingTsConfigFile(target); ok {
		return path, true
	}
	if subpath == "." && pkg.TsConfig != "" {
		if path, ok := existingTsConfigFile(filepath.Join(packageDir, filepath.FromSlash(pkg.TsConfig))); ok {
			return path, true
		}
	}
	return existingTsConfigFile(filepath.Join(target, "tsconfig.json"))
}

// exportsTarget resolves subpath through a package.json "exports" value: a
// target string, an array of alternatives, a map of subpaths (with at most one
// "*" per key), or a map of conditions matched in declaration order.
func exportsTarget(exports json.RawMessage, subpath string) (string, bool) {
	keys, values, isObject := orderedJSONObject(exports)
	if !isObject || len(keys) == 0 || !strings.HasPrefix(keys[0], ".") {
		if subpath != "." {
			return "", false
		}
		return conditionalTarget(exports, "")
	}
	// Among matching patterns, the longest prefix wins, then the longest key.
	bestPrefix, bestKey, bestIndex, bestMatch := -1, -1, -1, ""
	for i, key := range keys {
		if key == subpath {
			return conditionalTarget(values[i], "")
		}
		prefix, suffix, pattern := strings.Cut(key, "*")
		if !pattern || !strings.HasPrefix(subpath, prefix) || !strings.HasSuffix(subpath, suffix) ||
			len(subpath) < len(prefix)+len(suffix) {
			continue
		}
		if len(prefix) < bestPrefix || (len(prefix) == bestPrefix && len(key) <= bestKey) {
			continue
		}
		bestPrefix, bestKey, bestIndex, bestMatch = len(prefix), len(key), i, subpath[len(prefix):len(subpath)-len(suffix)]
	}
	if bestIndex < 0 {
		return "", false
	}
	target, ok := conditionalTarget(values[bestIndex], bestMatch)
	return target, ok
}

// conditionalTarget resolves one exports value to a target path, replacing
// "*" with match.
func conditionalTarget(value json.RawMessage, match string) (string, bool) {
	var target string
	if json.Unmarshal(value, &target) == nil {
		return strings.ReplaceAll(target, "*", match), strings.HasPrefix(target, "./")
	}
	var alternatives []json.RawMessage
	if json.Unmarshal(value, &alternatives) == nil {
		for _, alternative := range alternatives {
			if target, ok := conditionalTarget(alternative, match); ok {
				return target, true
			}
		}
		return "", false
	}
	keys, values, isObject := orderedJSONObject(value)
	if !isObject {
		return "", false
	}
	for i, key := range keys {
		if tsConfigExportConditions[key] {
			if target, ok := conditionalTarget(values[i], match); ok {
				return target, true
			}
		}
	}
	return "", false
}

// orderedJSONObject returns the keys and raw values of a JSON object in
// declaration order.
func orderedJSONObject(data json.RawMessage) ([]string, []json.RawMessage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if token, err := decoder.Token(); err != nil || token != json.Delim('{') {
		return nil, nil, false
	}
	var keys []string
	var values []json.RawMessage
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, nil, false
		}
		key, _ := token.(string)
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, nil, false
		}
		keys = append(keys, key)
		values = append(values, value)
	}
	return keys, values, true
}

// existingTsConfigFile returns path, or path with a .json extension, when it
// names a regular file.
func existingTsConfigFile(path string) (string, bool) {
	candidates := []string{path}
	if !strings.HasSuffix(path, ".json") {
		candidates = append(candidates, path+".json")
	}
	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() {
			return candidate, true
		}
	}
	return "", false
}

// StripJSONC turns JSON with comments and trailing commas, the dialect tsc
// accepts in a tsconfig, into strict JSON. String contents are kept verbatim.
func StripJSONC(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		c := data[i]
		switch {
		case c == '"':
			start := i
			for i++; i < len(data) && data[i] != '"'; i++ {
				if data[i] == '\\' {
					i++
				}
			}
			end := min(i+1, len(data))
			out = append(out, data[start:end]...)
		case c == '/' && i+1 < len(data) && data[i+1] == '/':
			for i < len(data) && data[i] != '\n' {
				i++
			}
			if i < len(data) {
				out = append(out, '\n')
			}
		case c == '/' && i+1 < len(data) && data[i+1] == '*':
			i += 2
			for i+1 < len(data) && (data[i] != '*' || data[i+1] != '/') {
				i++
			}
			i++
			out = append(out, ' ')
		case c == '}' || c == ']':
			// Drop a comma that only whitespace separates from this closer.
			j := len(out) - 1
			for j >= 0 && isJSONSpace(out[j]) {
				j--
			}
			if j >= 0 && out[j] == ',' {
				out = append(out[:j], out[j+1:]...)
			}
			out = append(out, c)
		default:
			out = append(out, c)
		}
	}
	return out
}

func isJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
