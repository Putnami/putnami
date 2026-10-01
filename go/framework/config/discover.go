package config

import (
	"os"
	"path/filepath"
	"sync"
)

// SourceDiscoverer discovers a configuration source from process state.
// It returns nil when that source is not enabled for the current process.
type SourceDiscoverer = func() Source

var sourceDiscoverers struct {
	sync.RWMutex
	items []SourceDiscoverer
}

// RegisterSourceDiscoverer adds a compile-time opt-in source discoverer.
//
// Cloud and other optional integrations call this from an activation package
// or init hook instead of being imported by core. Registered discoverers run
// after local files and before CONFIG_DATA so their Source.Priority values keep
// the same precedence model as built-in sources.
func RegisterSourceDiscoverer(discover SourceDiscoverer) {
	if discover == nil {
		return
	}
	sourceDiscoverers.Lock()
	defer sourceDiscoverers.Unlock()
	sourceDiscoverers.items = append(sourceDiscoverers.items, discover)
}

func registeredSourceDiscoverers() []SourceDiscoverer {
	sourceDiscoverers.RLock()
	defer sourceDiscoverers.RUnlock()
	items := make([]SourceDiscoverer, len(sourceDiscoverers.items))
	copy(items, sourceDiscoverers.items)
	return items
}

// Environment returns the current application environment from the APP_ENV
// environment variable. Defaults to "local" if not set.
func Environment() string {
	if env := os.Getenv("APP_ENV"); env != "" {
		return env
	}
	return "local"
}

// DiscoverSources returns the default set of configuration sources by scanning
// for YAML files and environment variables. Sources are returned in priority order:
//
//   - conf/.env.yaml (base config, priority 10)
//   - conf/.env.{APP_ENV}.yaml (environment-specific, priority 20)
//   - .gen/conf/.env.{APP_ENV}.yaml (build-merged from dependencies, priority 30)
//   - conf/.secrets.{APP_ENV}.yaml (local secrets, gitignored, priority 35)
//   - Registered optional discoverers (for example @putnami/cloud remote config)
//   - CONFIG_DATA env var (inline YAML, priority 60)
//   - Environment variables via env struct tags (always active, priority 80)
//
// The search starts from the current working directory.
func DiscoverSources() []Source {
	var sources []Source

	dir, err := os.Getwd()
	if err != nil {
		return sources
	}

	// Base config file: conf/.env.yaml
	basePath := filepath.Join(dir, "conf", ".env.yaml")
	if _, err := os.Stat(basePath); err == nil {
		sources = append(sources, NewYAMLFileSource(basePath, 10))
	}

	// Environment-specific: conf/.env.{env}.yaml
	env := Environment()
	envPath := filepath.Join(dir, "conf", ".env."+env+".yaml")
	if _, err := os.Stat(envPath); err == nil {
		sources = append(sources, NewYAMLFileSource(envPath, 20))
	}

	// Generated config: .gen/conf/.env.{env}.yaml (merged from workspace dependencies at build time)
	genPath := filepath.Join(dir, ".gen", "conf", ".env."+env+".yaml")
	if _, err := os.Stat(genPath); err == nil {
		sources = append(sources, NewYAMLFileSource(genPath, 30))
	}

	// Local secrets: conf/.secrets.{env}.yaml (gitignored by template default)
	secretsPath := filepath.Join(dir, "conf", ".secrets."+env+".yaml")
	if _, err := os.Stat(secretsPath); err == nil {
		sources = append(sources, NewYAMLFileSource(secretsPath, 35))
	}

	for _, discover := range registeredSourceDiscoverers() {
		if source := discover(); source != nil {
			sources = append(sources, source)
		}
	}

	// CONFIG_DATA env var: inline YAML for containerized deployments
	if configData := os.Getenv("CONFIG_DATA"); configData != "" {
		sources = append(sources, NewYAMLDataSource("CONFIG_DATA", 60, []byte(configData)))
	}

	return sources
}
