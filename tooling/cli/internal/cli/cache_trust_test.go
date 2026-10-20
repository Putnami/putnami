package cli

import (
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/extension"
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
			if err := resolveCacheTrust(g, cfg, nil, tt.commands); err != nil {
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
	// The publish job of a language extension declares its registry side
	// effect: the core gives the command none.
	loaded := []*extension.ExtensionDescription{{
		Name: "@test/lang",
		Jobs: map[string]*extension.JobDefinition{
			"publish": {Traits: extension.CommandTraits{SideEffects: extension.SideEffectsRegistry, InjectPublishChannels: true}},
		},
	}}
	tests := []struct {
		name     string
		commands []string
		flag     string
		want     store.CacheTrust
	}{
		{"publish raises any to ci", []string{"publish"}, "any", store.CacheTrustCI},
		{"publish preserves stricter none", []string{"publish"}, "none", store.CacheTrustNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := &GlobalFlags{CacheTrust: tt.flag}
			if err := resolveCacheTrust(g, &wsproto.Config{}, loaded, tt.commands); err != nil {
				t.Fatalf("resolveCacheTrust: %v", err)
			}
			if got := store.CacheTrust(g.CacheTrust); got != tt.want {
				t.Fatalf("CacheTrust = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveCacheTrustReleaseFollowsTheDeclaredSideEffects pins that the
// release floor reads the registry side effect a loaded job declares, not the
// command name: a verb of the extension's own that declares it is raised, a
// publish whose declared traits name none is not, nor is a publish no
// extension declares, because the core gives no command a side effect, and a
// cloud side effect alone does not raise the trust. The flag fixes the
// requested trust, so neither CI nor PUTNAMI_CACHE_TRUST is read.
func TestResolveCacheTrustReleaseFollowsTheDeclaredSideEffects(t *testing.T) {
	t.Parallel()
	loaded := []*extension.ExtensionDescription{{
		Name: "@test/release",
		Jobs: map[string]*extension.JobDefinition{
			"release":  {Traits: extension.CommandTraits{SideEffects: extension.SideEffectsRegistry}},
			"converge": {Traits: extension.CommandTraits{SideEffects: extension.SideEffectsCloud}},
			"publish":  {Traits: extension.CommandTraits{InjectPublishChannels: true}},
		},
	}}
	tests := []struct {
		name       string
		extensions []*extension.ExtensionDescription
		commands   []string
		want       store.CacheTrust
	}{
		{"a verb declaring registry side effects raises any to ci", loaded, []string{"release"}, store.CacheTrustCI},
		{"it raises a run among ordinary commands", loaded, []string{"build", "release"}, store.CacheTrustCI},
		{"publish declared without side effects keeps any", loaded, []string{"publish"}, store.CacheTrustAny},
		{"publish no extension declares keeps any", nil, []string{"publish"}, store.CacheTrustAny},
		{"a verb declaring cloud side effects keeps any", loaded, []string{"converge"}, store.CacheTrustAny},
		{"deploy keeps any", nil, []string{"deploy"}, store.CacheTrustAny},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			g := &GlobalFlags{CacheTrust: string(store.CacheTrustAny)}
			if err := resolveCacheTrust(g, &wsproto.Config{}, tt.extensions, tt.commands); err != nil {
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
		err := resolveCacheTrust(g, &wsproto.Config{}, nil, []string{"build"})
		if err == nil || !strings.Contains(err.Error(), "--cache-trust") {
			t.Fatalf("error = %v, want flag-specific validation", err)
		}
	})
	t.Run("environment", func(t *testing.T) {
		t.Setenv("PUTNAMI_CACHE_TRUST", "unsafe")
		err := resolveCacheTrust(&GlobalFlags{}, &wsproto.Config{}, nil, []string{"build"})
		if err == nil || !strings.Contains(err.Error(), "PUTNAMI_CACHE_TRUST") {
			t.Fatalf("error = %v, want env-specific validation", err)
		}
	})
	t.Run("config", func(t *testing.T) {
		t.Setenv("PUTNAMI_CACHE_TRUST", "")
		cfg := &wsproto.Config{Options: map[string]map[string]any{
			"build": {cacheTrustOption: "unsafe"},
		}}
		err := resolveCacheTrust(&GlobalFlags{}, cfg, nil, []string{"build"})
		if err == nil || !strings.Contains(err.Error(), "options.build.cache-trust") {
			t.Fatalf("error = %v, want config-specific validation", err)
		}
	})
}
