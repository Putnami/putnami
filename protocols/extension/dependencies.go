package extension

import (
	"fmt"
	"regexp"
	"sort"

	protocolcli "go.putnami.dev/protocol/cli"
)

// RegistryNamePattern is the grammar of a registry extension name: @scope/name,
// where the scope and the name each hold lowercase letters, digits, '.', '_'
// and '-' and start with a letter or digit. The CLI builds the registry route
// and the lock key from the two parts, so a name holds nothing a URL path or
// query would read.
const RegistryNamePattern = `^@[a-z0-9][a-z0-9._-]*/[a-z0-9][a-z0-9._-]*$`

// PackageNamePattern is the grammar of an unscoped dependency name, which the
// package manager that installed the extension resolves.
const PackageNamePattern = `^[a-z0-9][a-z0-9._-]*$`

var (
	registryName = regexp.MustCompile(RegistryNamePattern)
	packageName  = regexp.MustCompile(PackageNamePattern)
)

// IsRegistryName reports whether name is a registry extension name
// (RegistryNamePattern).
func IsRegistryName(name string) bool {
	return registryName.MatchString(name)
}

// IsDependencyName reports whether name is a legal extensionDependencies key:
// a registry name (RegistryNamePattern) or an unscoped package name
// (PackageNamePattern).
func IsDependencyName(name string) bool {
	return registryName.MatchString(name) || packageName.MatchString(name)
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
