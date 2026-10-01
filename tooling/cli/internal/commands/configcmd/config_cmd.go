package configcmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/jsonutil"
)

// ConfigShow displays the effective configuration after all scopes are merged.
func ConfigShow(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	if outputFormat == "jsonl" {
		data, err := json.Marshal(cfg)
		if err != nil {
			return fmt.Errorf("marshal config: %w", err)
		}
		iox.Fprintln(os.Stdout, string(data))
		return nil
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal config: %w", err)
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintln(os.Stdout, "  Effective configuration (merged from all scopes):")
	iox.Fprintln(os.Stdout)

	// Show scope sources
	sources := configSources(wsRoot)
	if len(sources) > 0 {
		iox.Fprintln(os.Stdout, "  Sources:")
		for _, s := range sources {
			iox.Fprintf(os.Stdout, "    %s\n", s)
		}
		iox.Fprintln(os.Stdout)
	}

	iox.Fprintln(os.Stdout, string(data))
	iox.Fprintln(os.Stdout)
	return nil
}

// ConfigSet sets a configuration key in the workspace-level config file.
func ConfigSet(wsRoot string, args []string) error {
	if len(args) < 2 {
		return fmt.Errorf("usage: config set <key> <value>")
	}

	key := args[0]
	value := args[1]

	cfgPath := wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename)

	// Read existing config
	raw, err := jsonutil.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config: %w", err)
	}

	// Handle nested keys (e.g., "options.build.target")
	parts := strings.Split(key, ".")
	setNestedValue(raw, parts, value)

	// Write back
	if err := jsonutil.WriteFile(cfgPath, raw); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	iox.Fprintf(os.Stdout, "  Set %s = %s\n", key, value)
	return nil
}

// configSources returns the config file paths that were merged.
func configSources(wsRoot string) []string {
	var sources []string

	homeDir, _ := os.UserHomeDir()
	if homeDir != "" {
		globalPath := filepath.Join(homeDir, wsproto.GlobalConfigDir, wsproto.GlobalConfigFilename)
		if _, err := os.Stat(globalPath); err == nil {
			sources = append(sources, globalPath+" (global)")
		}
	}

	if wsRoot != "" {
		path := wsproto.ResolveFile(wsRoot, wsproto.WorkspaceConfigFilename)
		if _, err := os.Stat(path); err == nil {
			sources = append(sources, path+" (workspace)")
		}
	}

	cwd, _ := os.Getwd()
	if cwd != "" && cwd != wsRoot {
		path := wsproto.ResolveFile(cwd, wsproto.ConfigFilename)
		if _, err := os.Stat(path); err == nil {
			sources = append(sources, path+" (project)")
		}
	}

	return sources
}

// setNestedValue sets a value at a nested key path in a map.
func setNestedValue(m *jsonutil.OrderedMap, keys []string, value string) {
	if len(keys) == 0 {
		return
	}
	if len(keys) == 1 {
		// Try to parse as JSON for booleans, numbers, arrays
		var parsed any
		if err := json.Unmarshal([]byte(value), &parsed); err == nil {
			m.Set(keys[0], parsed)
		} else {
			m.Set(keys[0], value)
		}
		return
	}

	// Navigate/create nested objects
	key := keys[0]
	sub := m.GetMap(key)
	if sub == nil {
		sub = jsonutil.New()
		m.Set(key, sub)
	}

	setNestedValue(sub, keys[1:], value)
}
