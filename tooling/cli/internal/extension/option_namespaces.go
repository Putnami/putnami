package extension

import (
	"os"
	"path/filepath"
	"strings"
	"sync"

	extproto "go.putnami.dev/protocol/extension"
)

// OptionOwnership answers, for one project config, who may read a bare
// `options.<name>` block.
//
// Namespaces maps a declared namespace to the extension names that declare it.
// Commands holds every command name any resolved extension provides; a command
// layer is an input of every extension providing it, so it is never attributed
// to one.
type OptionOwnership struct {
	Namespaces map[string][]string
	Commands   map[string]bool
}

// namespaceOwnershipCache memoizes one workspace's answer. Manifests are read
// from the checked-out tree and do not change during a CLI invocation, and the
// same answer is needed once per cache key.
var namespaceOwnershipCache sync.Map // workspace root → *OptionOwnership

// ResolveOptionOwnership reads the manifest of every extension the workspace's
// projects reference by PATH and collects what each one declares.
//
// Path references are the resolvable set: `/tooling/sdd-extension` names a
// directory under this workspace root, so the answer is the same whatever the
// run selected — a cache key may not vary with the selection. A reference by
// registry name resolves to an installed copy whose location is a machine-local
// fact, so it contributes nothing and its namespaces stay in every key, which
// is the same fail-closed default an undeclared namespace gets.
func ResolveOptionOwnership(workspaceRoot string, references []string) *OptionOwnership {
	if cached, ok := namespaceOwnershipCache.Load(workspaceRoot); ok {
		if owned, typed := cached.(*OptionOwnership); typed {
			return owned
		}
	}
	ownership := &OptionOwnership{
		Namespaces: map[string][]string{},
		Commands:   map[string]bool{},
	}
	seen := make(map[string]bool, len(references))
	for _, ref := range references {
		if !strings.HasPrefix(ref, "/") || seen[ref] {
			continue
		}
		seen[ref] = true
		manifest := readManifestNoDiagnostics(filepath.Join(workspaceRoot, filepath.FromSlash(strings.TrimPrefix(ref, "/"))))
		if manifest == nil {
			continue
		}
		for command := range manifest.Commands {
			ownership.Commands[command] = true
		}
		for _, namespace := range manifest.OptionNamespaces {
			if namespace == "" {
				continue
			}
			ownership.Namespaces[namespace] = append(ownership.Namespaces[namespace], manifest.Name)
		}
	}
	actual, _ := namespaceOwnershipCache.LoadOrStore(workspaceRoot, ownership)
	if owned, typed := actual.(*OptionOwnership); typed {
		return owned
	}
	return ownership
}

// ForeignNamespaces returns the bare namespaces another extension declares and
// this one does not — the blocks a task of this extension cannot read.
//
// A namespace that is also a command name is never foreign: the CLI merges
// `options.<command>` into resolved parameters for every extension providing
// the command, so that block is an input of all of them.
func (o *OptionOwnership) ForeignNamespaces(extensionName string) []string {
	if o == nil || len(o.Namespaces) == 0 {
		return nil
	}
	var foreign []string
	for namespace, owners := range o.Namespaces {
		if o.Commands[namespace] {
			continue
		}
		mine := false
		for _, owner := range owners {
			if owner == extensionName {
				mine = true
				break
			}
		}
		if !mine {
			foreign = append(foreign, namespace)
		}
	}
	return foreign
}

// readManifestNoDiagnostics parses an extension manifest for its declarations
// alone. A manifest that will not read or will not parse yields nothing, so an
// unreadable extension makes every key wider rather than dropping a block on a
// file nobody could check.
func readManifestNoDiagnostics(dir string) *extproto.Manifest {
	data, err := os.ReadFile(filepath.Join(dir, "putnami.extension.json"))
	if err != nil {
		return nil
	}
	manifest, _ := extproto.ParseManifest(data)
	return manifest
}
