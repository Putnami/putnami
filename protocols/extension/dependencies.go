package extension

import (
	"fmt"
	"sort"
	"strings"

	protocolcli "go.putnami.dev/protocol/cli"
)

// IsRegistryName reports whether name is a registry extension name:
// @scope/name, with one non-empty scope and one non-empty name.
func IsRegistryName(name string) bool {
	rest, ok := strings.CutPrefix(name, "@")
	if !ok {
		return false
	}
	scope, pkg, ok := strings.Cut(rest, "/")
	return ok && scope != "" && pkg != "" && !strings.Contains(pkg, "/")
}

// RegistryDependencies returns, sorted, the names m's extensionDependencies
// lists that are registry names (IsRegistryName). Any other name is left to
// the package manager that installed the extension.
func RegistryDependencies(m *Manifest) []string {
	if m == nil {
		return nil
	}
	var names []string
	for name := range m.ExtensionDeps.List {
		if IsRegistryName(name) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// NamesRegistryDependency reports whether m's extensionDependencies names a
// registry extension. Such a manifest requires ExtensionDependenciesContract
// (RequiredCLIContract).
func NamesRegistryDependency(m *Manifest) bool {
	if m == nil {
		return false
	}
	for name := range m.ExtensionDeps.List {
		if IsRegistryName(name) {
			return true
		}
	}
	return false
}

// CheckDependencyContract refuses a manifest that names a registry dependency
// under a stamp below ExtensionDependenciesContract. The CLI applies it to a
// manifest it loads from the artifact store or from a path the workspace
// declares, because it follows that manifest's dependencies. The npm packager
// stamps without this rung (RequiredLoadContract).
func CheckDependencyContract(path string, m *Manifest) error {
	if !NamesRegistryDependency(m) || m.CLIContract >= protocolcli.ExtensionDependenciesContract {
		return nil
	}
	return fmt.Errorf(
		"extension manifest %s declares CLI contract %d but this putnami requires %d for its extensionDependencies: "+
			"re-package the extension with putnami %d (or run `putnami extensions update` to pull a build that has been)",
		path, m.CLIContract, protocolcli.ExtensionDependenciesContract, protocolcli.ExtensionDependenciesContract)
}
