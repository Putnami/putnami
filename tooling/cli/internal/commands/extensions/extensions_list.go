package extensions

import (
	"os"
	"sort"
	"strings"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// ExtensionsList shows all installed extensions with their versions.
func ExtensionsList(wsRoot string, cfg *wsproto.Config, outputFormat string) error {
	return ExtensionsListPrepared(wsRoot, cfg, outputFormat, nil)
}

// ExtensionsListPrepared lists extensions while honoring the exact roots (or
// explicit unavailability) chosen by bounded read preparation. Config and lock
// rows still render normally; ambient manifests cannot be substituted as a
// workspace discovery result when their registry ref failed preparation.
func ExtensionsListPrepared(wsRoot string, cfg *wsproto.Config, outputFormat string, prepared map[string]string) error {
	lockFile, lockErr := lockfile.ReadLockFile(wsRoot)
	if lockErr != nil {
		// The listing is deliberately tolerant — it still reports what config
		// declares — but the INSTALLED column is read from the lock alone, so an
		// unreadable one renders every row as "-": the same output as a
		// workspace where nothing is installed. Since the format floor moved to
		// v2, a v1 lock produces exactly that, so the reason is
		// named on stderr, out of the table's and the JSONL stream's way.
		noteUnreadableLock(lockErr)
	}

	extMap := shared.BuildExtensionMap(cfg)

	// Also discover workspace extensions
	ws, _ := workspace.Load(wsRoot)
	var projectPaths []string
	if ws != nil {
		for _, p := range ws.Projects {
			projectPaths = append(projectPaths, p.Path)
		}
	}
	var discovered []*extension.ExtensionDescription
	var dependencies []extension.GraphDependency
	var removed []extension.RemovedCapability
	if result, err := extension.DiscoverExtensionsDetailed(wsRoot, cfg, projectPaths); err == nil {
		discovered, dependencies, removed = result.Extensions, result.Dependencies, result.RemovedCapabilities
	}
	discovered = extension.SelectPreparedExtensions(discovered, prepared)
	rows := dependencyRows(dependencies, removed, lockFile)

	if outputFormat == "jsonl" {
		return extensionsListJSONL(extMap, lockFile, discovered, rows)
	}

	iox.Fprintln(os.Stdout)
	iox.Fprintf(os.Stdout, "  %-35s %-15s %-15s %s\n", "EXTENSION", "CONSTRAINT", "INSTALLED", "SOURCE")
	iox.Fprintf(os.Stdout, "  %-35s %-15s %-15s %s\n", "---------", "----------", "---------", "------")

	// Show configured extensions in sorted order for deterministic output.
	extNames := sortedStringKeys(extMap)
	for _, name := range extNames {
		constraint := extMap[name]
		installed := "-"
		source := "config"
		if lockFile != nil {
			if entry, ok := lockFile.GetExtension(name); ok {
				installed = entry.Version
			}
		}
		iox.Fprintf(os.Stdout, "  %-35s %-15s %-15s %s\n", name, constraint, installed, source)
	}

	// Show each dependency with the extensions that require it.
	for _, row := range rows {
		installed := row.Installed
		if installed == "" {
			installed = "-"
		}
		iox.Fprintf(os.Stdout, "  %-35s %-15s %-15s %s\n", row.Name, "-", installed, "required by "+strings.Join(row.RequiredBy, ", "))
	}

	// Show workspace-discovered extensions not in config
	for _, ext := range discovered {
		if _, inConfig := extMap[ext.Name]; inConfig {
			continue
		}
		if isDependencyRow(rows, ext.Name) {
			continue
		}
		version := ext.Version
		if version == "" {
			version = "-"
		}
		iox.Fprintf(os.Stdout, "  %-35s %-15s %-15s %s\n", ext.Name, "-", version, "workspace")
	}

	iox.Fprintln(os.Stdout)
	for _, capability := range removed {
		if capability.RequiredBy != "" {
			iox.Fprintf(os.Stdout, "  ! %v\n", capability.Reason())
		}
	}
	return nil
}

// ExtensionsRemove removes an extension from config and disk.
func ExtensionsRemove(wsRoot string, cfg *wsproto.Config, args []string) error {
	return removeExtension(wsRoot, cfg, args, extensionOps(wsRoot))
}

func extensionsListJSONL(extMap map[string]string, lockFile *lockfile.LockFile, discovered []*extension.ExtensionDescription, rows []artifactListEntry) error {
	var entries []artifactListEntry
	for _, name := range sortedStringKeys(extMap) {
		entry := artifactListEntry{Name: name, Constraint: extMap[name], Source: "config"}
		if lockFile != nil {
			if le, ok := lockFile.GetExtension(name); ok {
				entry.Installed = le.Version
			}
		}
		entries = append(entries, entry)
	}

	entries = append(entries, rows...)

	for _, ext := range discovered {
		if _, inConfig := extMap[ext.Name]; inConfig {
			continue
		}
		if isDependencyRow(rows, ext.Name) {
			continue
		}
		entries = append(entries, artifactListEntry{Name: ext.Name, Installed: ext.Version, Source: "workspace"})
	}

	return printArtifactListJSONL(entries)
}

// dependencyRows returns one list row per dependency of the workspace graph,
// in walk order, with the extensions that require it and the reserved
// provider commands discovery removed from it (ADR 0066).
func dependencyRows(dependencies []extension.GraphDependency, removed []extension.RemovedCapability, lockFile *lockfile.LockFile) []artifactListEntry {
	rows := make([]artifactListEntry, 0, len(dependencies))
	for _, dep := range dependencies {
		row := artifactListEntry{Name: dep.Name, Source: "dependency", RequiredBy: dep.RequiredBy}
		if lockFile != nil {
			if entry, ok := lockFile.GetExtension(dep.Name); ok {
				row.Installed = entry.Version
			}
		}
		for _, capability := range removed {
			if capability.Extension == dep.Name && capability.RequiredBy != "" {
				row.RemovedCapabilities = append(row.RemovedCapabilities, capability.Command)
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func isDependencyRow(rows []artifactListEntry, name string) bool {
	for _, row := range rows {
		if row.Name == name {
			return true
		}
	}
	return false
}

// sortedStringKeys returns map keys in sorted order for deterministic output.
func sortedStringKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
