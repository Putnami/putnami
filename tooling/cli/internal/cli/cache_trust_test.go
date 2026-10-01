package cli

import (
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/store"
)

func TestResolveCacheTrustPrecedenceAndDefaults(t *testing.T) {
	tests := []struct {
		name     string
		flag     string
		env      string
		ci       string
		options  map[string]map[string]any
		commands []string
		want     store.CacheTrust
	}{
		{name: "local default", commands: []string{"build"}, want: store.CacheTrustAny},
		{name: "ci default", ci: "true", commands: []string{"build"}, want: store.CacheTrustCI},
		{
			name:     "per-command config",
			options:  map[string]map[string]any{"build": {cacheTrustOption: "ci"}},
			commands: []string{"build"}, want: store.CacheTrustCI,
		},
		{
			name:     "environment beats config",
			env:      "any",
			options:  map[string]map[string]any{"build": {cacheTrustOption: "ci"}},
			commands: []string{"build"}, want: store.CacheTrustAny,
		},
		{
			name: "flag beats environment",
			flag: "none", env: "any", ci: "true",
			commands: []string{"build"}, want: store.CacheTrustNone,
		},
		{
			name: "multi-command uses strictest config",
			options: map[string]map[string]any{
				"lint":  {cacheTrustOption: "any"},
				"build": {cacheTrustOption: "ci"},
			},
			commands: []string{"lint", "build"}, want: store.CacheTrustCI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PUTNAMI_CACHE_TRUST", tt.env)
			t.Setenv("CI", tt.ci)
			g := &GlobalFlags{CacheTrust: tt.flag}
			cfg := &wsproto.Config{Options: tt.options}
			if err := resolveCacheTrust(g, cfg, tt.commands, ""); err != nil {
				t.Fatalf("resolveCacheTrust: %v", err)
			}
			if got := store.CacheTrust(g.CacheTrust); got != tt.want {
				t.Fatalf("CacheTrust = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveCacheTrustReleasePathsForceAuthoritative(t *testing.T) {
	t.Setenv("PUTNAMI_CACHE_TRUST", "")
	t.Setenv("CI", "")
	tests := []struct {
		name       string
		commands   []string
		subcommand string
		flag       string
		want       store.CacheTrust
	}{
		{"publish raises any to ci", []string{"publish"}, "", "any", store.CacheTrustCI},
		{"tag raises any to ci", []string{"tag"}, "", "any", store.CacheTrustCI},
		{"version tag raises any to ci", []string{"version"}, "tag", "any", store.CacheTrustCI},
		{"publish preserves stricter none", []string{"publish"}, "", "none", store.CacheTrustNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GlobalFlags{CacheTrust: tt.flag}
			if err := resolveCacheTrust(g, &wsproto.Config{}, tt.commands, tt.subcommand); err != nil {
				t.Fatalf("resolveCacheTrust: %v", err)
			}
			if got := store.CacheTrust(g.CacheTrust); got != tt.want {
				t.Fatalf("CacheTrust = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveCacheTrustRejectsInvalidSources(t *testing.T) {
	t.Setenv("CI", "")
	t.Run("flag", func(t *testing.T) {
		t.Setenv("PUTNAMI_CACHE_TRUST", "ci")
		g := &GlobalFlags{CacheTrust: "unsafe"}
		err := resolveCacheTrust(g, &wsproto.Config{}, []string{"build"}, "")
		if err == nil || !strings.Contains(err.Error(), "--cache-trust") {
			t.Fatalf("error = %v, want flag-specific validation", err)
		}
	})
	t.Run("environment", func(t *testing.T) {
		t.Setenv("PUTNAMI_CACHE_TRUST", "unsafe")
		err := resolveCacheTrust(&GlobalFlags{}, &wsproto.Config{}, []string{"build"}, "")
		if err == nil || !strings.Contains(err.Error(), "PUTNAMI_CACHE_TRUST") {
			t.Fatalf("error = %v, want env-specific validation", err)
		}
	})
	t.Run("config", func(t *testing.T) {
		t.Setenv("PUTNAMI_CACHE_TRUST", "")
		cfg := &wsproto.Config{Options: map[string]map[string]any{
			"build": {cacheTrustOption: "unsafe"},
		}}
		err := resolveCacheTrust(&GlobalFlags{}, cfg, []string{"build"}, "")
		if err == nil || !strings.Contains(err.Error(), "options.build.cache-trust") {
			t.Fatalf("error = %v, want config-specific validation", err)
		}
	})
}
