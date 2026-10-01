package extension

import (
	"fmt"
	"strings"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/sdk/extension/profiles"
)

// ResolveInstalledProfiles builds the workspace's ecosystem registry from the
// extensions actually installed, plus the profiles the framework owns itself.
//
// This is the ONE place the CLI learns which ecosystems exist. It names none of
// them: `npm` is what the TypeScript extension says it is, `go` what the Go
// extension says it is, and `oci` what the SDK says it is. Adding a language is
// therefore a manifest and a publish job, never a CLI change (D12).
//
// The manifest handed to the protocol carries only what resolution reads — the
// declared profiles, the `uses` list, and the command NAMES, since a profile's
// publish job must be a command of its own manifest. Diagnostics collapse into
// one error listing every defect: a workspace with two extensions claiming the
// same ecosystem must see both, not the first.
func ResolveInstalledProfiles(discovered *DiscoveryResult) (*extproto.ProfileRegistry, error) {
	var manifests []extproto.NamedManifest
	if discovered != nil {
		for _, description := range discovered.Extensions {
			if description == nil {
				continue
			}
			commands := make(map[string]extproto.CommandDefinition, len(description.Commands))
			for name := range description.Commands {
				commands[name] = extproto.CommandDefinition{}
			}
			manifests = append(manifests, extproto.NamedManifest{
				Name: description.Name,
				Manifest: &extproto.Manifest{
					Name:       description.Name,
					Commands:   commands,
					Ecosystems: description.Ecosystems,
					Uses:       description.Uses,
				},
			})
		}
	}
	registry, diagnostics := extproto.ResolveProfiles(manifests, profiles.Builtin())
	var reported []string
	for _, diagnostic := range diagnostics {
		reported = append(reported, diagnostic.String())
	}
	if len(reported) > 0 {
		return nil, fmt.Errorf("resolve ecosystem profiles: %s", strings.Join(reported, "; "))
	}
	return registry, nil
}
