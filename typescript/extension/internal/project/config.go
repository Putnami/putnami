package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// putnamiRC represents putnami.json project configuration.
type putnamiRC struct {
	Name    string                     `json:"name"`
	Type    string                     `json:"type"`
	Tags    []string                   `json:"tags"`
	Options map[string]json.RawMessage `json:"options"`
}

// readPutnamiRC reads and parses a putnami.json file.
func readPutnamiRC(path string) (*putnamiRC, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var rc putnamiRC
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, err
	}
	return &rc, nil
}

// ConfigPath returns the path to the project config file in dir, preferring
// putnami.json and falling back to .putnamirc.json.
func ConfigPath(dir string) string {
	p := filepath.Join(dir, "putnami.json")
	if FileExists(p) {
		return p
	}
	legacy := filepath.Join(dir, ".putnamirc.json")
	if FileExists(legacy) {
		return legacy
	}
	return p
}

// CommitsCapabilityManifest reports whether the project config at rcPath sets
// options.generate.capabilities to true. A missing file, a missing block and a
// block that does not decode answer false; a file that cannot be read or
// parsed is an error, because the answer decides whether a build removes a
// tracked file.
func CommitsCapabilityManifest(rcPath string) (bool, error) {
	rc, err := readPutnamiRC(rcPath)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	raw, ok := rc.Options["generate"]
	if !ok {
		return false, nil
	}
	var opts struct {
		Capabilities *bool `json:"capabilities"`
	}
	if json.Unmarshal(raw, &opts) != nil || opts.Capabilities == nil {
		return false, nil
	}
	return *opts.Capabilities, nil
}

// GenerateAsset represents a {from, to} entry in options.generate.assets.
type GenerateAsset struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// GetGenerateAssets reads options.generate.assets from a project config file.
func GetGenerateAssets(rcPath string) []GenerateAsset {
	rc, err := readPutnamiRC(rcPath)
	if err != nil {
		return nil
	}

	genRaw, ok := rc.Options["generate"]
	if !ok {
		return nil
	}

	var genOpts struct {
		Assets []GenerateAsset `json:"assets"`
	}
	if json.Unmarshal(genRaw, &genOpts) != nil {
		return nil
	}
	return genOpts.Assets
}

// ResolveTsConfig finds the appropriate tsconfig.json for a project.
// Checks: tsconfig.app.json → tsconfig.lib.json → tsconfig.json → workspace tsconfig.json
func ResolveTsConfig(projectRoot, workspaceRoot string) string {
	candidates := []string{
		filepath.Join(projectRoot, "tsconfig.app.json"),
		filepath.Join(projectRoot, "tsconfig.lib.json"),
		filepath.Join(projectRoot, "tsconfig.json"),
		filepath.Join(workspaceRoot, "tsconfig.json"),
	}
	for _, c := range candidates {
		if FileExists(c) {
			return c
		}
	}
	return ""
}

// ConfiguredPort is the port the project's own configuration gives its HTTP
// server in environment env: putnami.port of conf/.env.<env>.yaml, else of
// conf/.env.yaml, the two project files the runtime reads it from (the
// runtime's conf/.env.<env>.yaml outranks the base file). It answers 0 when
// neither file sets a port that is an integer between 1 and 65535, written
// as a number or as a numeric string; like the runtime, it skips a file it
// cannot read or parse, and where a mapping repeats a key, the last one wins.
func ConfiguredPort(projectDir, env string) int {
	for _, name := range []string{".env." + env + ".yaml", ".env.yaml"} {
		data, err := os.ReadFile(filepath.Join(projectDir, "conf", name))
		if err != nil {
			continue
		}
		var doc yaml.Node
		if yaml.Unmarshal(data, &doc) != nil || len(doc.Content) == 0 {
			continue
		}
		node := lastMappingValue(lastMappingValue(doc.Content[0], "putnami"), "port")
		if node == nil || node.Kind != yaml.ScalarNode {
			continue
		}
		var value any
		if node.Decode(&value) != nil {
			continue
		}
		if port := ParsePort(value); port > 0 {
			return port
		}
	}
	return 0
}

// lastMappingValue is the value of key in the mapping node, read as Bun's
// YAML reads it: an alias stands for the node it names, the last of repeated
// keys wins, and a key the mapping does not set comes from its << merge
// sources, the first source that sets it winning. It answers nil when no
// value is found.
func lastMappingValue(node *yaml.Node, key string) *yaml.Node {
	return mappingValue(node, key, 0)
}

// maxMergeDepth bounds the merge chains mappingValue follows.
const maxMergeDepth = 16

func mappingValue(node *yaml.Node, key string, depth int) *yaml.Node {
	node = resolveAlias(node)
	if node == nil || node.Kind != yaml.MappingNode || depth > maxMergeDepth {
		return nil
	}
	var value *yaml.Node
	var merges []*yaml.Node
	for i := 0; i+1 < len(node.Content); i += 2 {
		switch k := node.Content[i]; {
		case k.Value == key && k.Tag != "!!merge":
			value = node.Content[i+1]
		case k.Tag == "!!merge" || k.Value == "<<":
			merges = append(merges, node.Content[i+1])
		}
	}
	if value != nil {
		return resolveAlias(value)
	}
	for _, merge := range merges {
		merge = resolveAlias(merge)
		sources := []*yaml.Node{merge}
		if merge != nil && merge.Kind == yaml.SequenceNode {
			sources = merge.Content
		}
		for _, source := range sources {
			if found := mappingValue(source, key, depth+1); found != nil {
				return found
			}
		}
	}
	return nil
}

// resolveAlias follows an alias node to the node its anchor names.
func resolveAlias(node *yaml.Node) *yaml.Node {
	for range maxMergeDepth {
		if node == nil || node.Kind != yaml.AliasNode {
			return node
		}
		node = node.Alias
	}
	return nil
}

// ParsePort reads a TCP port from a decoded configuration value: an integer
// between 1 and 65535, as a number or a numeric string. Anything else is 0.
func ParsePort(value any) int {
	var port int
	switch v := value.(type) {
	case int:
		port = v
	case int64:
		port = int(v)
	case uint64:
		port = int(v)
	case float64:
		if v != float64(int(v)) {
			return 0
		}
		port = int(v)
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0
		}
		port = n
	default:
		return 0
	}
	if port < 1 || port > 65535 {
		return 0
	}
	return port
}
