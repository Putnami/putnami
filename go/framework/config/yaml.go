package config

import (
	"os"

	"go.putnami.dev/errors"
	"gopkg.in/yaml.v3"
)

// YAMLFileSource loads configuration from a YAML file.
type YAMLFileSource struct {
	path     string
	priority int
}

// NewYAMLFileSource creates a source that reads from a YAML file.
// The file is read on each Load call. If the file does not exist, Load returns nil.
func NewYAMLFileSource(path string, priority int) *YAMLFileSource {
	return &YAMLFileSource{path: path, priority: priority}
}

// Name returns the source name.
func (s *YAMLFileSource) Name() string { return "yaml:" + s.path }

// Priority returns the source priority.
func (s *YAMLFileSource) Priority() int { return s.priority }

// Load reads and parses the YAML file. Returns nil if the file does not exist.
func (s *YAMLFileSource) Load() (map[string]any, error) {
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, errors.Wrapf(err, CodeConfigSource, "failed to read config file", errors.String("path", s.path))
	}
	return parseYAML(data)
}

// YAMLDataSource loads configuration from raw YAML bytes (e.g. CONFIG_DATA env var).
type YAMLDataSource struct {
	name     string
	priority int
	raw      []byte
}

// NewYAMLDataSource creates a source from raw YAML content.
func NewYAMLDataSource(name string, priority int, raw []byte) *YAMLDataSource {
	return &YAMLDataSource{name: name, priority: priority, raw: raw}
}

// Name returns the source name.
func (s *YAMLDataSource) Name() string { return s.name }

// Priority returns the source priority.
func (s *YAMLDataSource) Priority() int { return s.priority }

// Load parses and returns the raw YAML data. Returns nil if data is empty.
func (s *YAMLDataSource) Load() (map[string]any, error) {
	if len(s.raw) == 0 {
		return nil, nil
	}
	return parseYAML(s.raw)
}

func parseYAML(data []byte) (map[string]any, error) {
	var result map[string]any
	if err := yaml.Unmarshal(data, &result); err != nil {
		return nil, errors.Wrapf(err, CodeConfigSource, "failed to parse YAML")
	}
	return result, nil
}
