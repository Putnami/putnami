package engine

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/store"
)

// The run raises a permissive remote cache trust to ci when a command it runs
// declares registry side effects among the extensions it loaded, which the
// first-use bootstrap may have installed after the CLI resolved the trust. It
// never lowers a trust, and a command whose loaded jobs declare no registry
// side effect keeps it.
func TestRaiseReleaseCacheTrust(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "vocabulary-default-traits", "side-effects-come-only-from-loaded-jobs")
	loaded := func(command, sideEffects string) []*extension.ExtensionDescription {
		job := &extension.JobDefinition{Name: command}
		job.Traits.SideEffects = sideEffects
		return []*extension.ExtensionDescription{{Name: "@acme/lang", Jobs: map[string]*extension.JobDefinition{command: job}}}
	}
	for name, test := range map[string]struct {
		trust      store.CacheTrust
		command    string
		extensions []*extension.ExtensionDescription
		want       store.CacheTrust
	}{
		"a registry release raises any":          {store.CacheTrustAny, "publish", loaded("publish", extension.SideEffectsRegistry), store.CacheTrustCI},
		"a release step raises any":              {store.CacheTrustAny, "ship~upload", loaded("ship", extension.SideEffectsRegistry), store.CacheTrustCI},
		"a cloud side effect keeps any":          {store.CacheTrustAny, "deploy", loaded("deploy", extension.SideEffectsCloud), store.CacheTrustAny},
		"a publish that declares none keeps any": {store.CacheTrustAny, "publish", loaded("publish", ""), store.CacheTrustAny},
		"a command no extension loads keeps any": {store.CacheTrustAny, "publish", nil, store.CacheTrustAny},
		"ci stays ci":                            {store.CacheTrustCI, "publish", loaded("publish", extension.SideEffectsRegistry), store.CacheTrustCI},
		"none stays none":                        {store.CacheTrustNone, "publish", loaded("publish", extension.SideEffectsRegistry), store.CacheTrustNone},
	} {
		req := &Request{Commands: []string{test.command}}
		req.Global.CacheTrust = string(test.trust)
		raiseReleaseCacheTrust(req, test.extensions)
		if got := store.CacheTrust(req.Global.CacheTrust); got != test.want {
			t.Errorf("%s: trust %s, want %s", name, got, test.want)
		}
	}
}
