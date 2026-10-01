package template

import (
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// warnedLocks dedupes the unreadable-lock notice per workspace+reason, so the
// several discovery passes a single invocation makes do not repeat it.
var warnedLocks sync.Map

func warnLockOnce(workspaceRoot string, err error) {
	if _, loaded := warnedLocks.LoadOrStore(workspaceRoot+"\x00"+err.Error(), struct{}{}); loaded {
		return
	}
	slog.Warn("lock file could not be read; installed template versions are unknown",
		"workspace", workspaceRoot, "error", err)
}

// DiscoverTemplates finds all templates from three sources:
//  1. Workspace projects containing putnami.template.json
//  2. Convention-based <domain>/templates/*/ directories
//  3. Installed templates via stable symlinks or artifact directories
func DiscoverTemplates(workspaceRoot string, projectPaths []string) ([]*TemplateDescription, error) {
	var templates []*TemplateDescription
	seen := make(map[string]bool)

	// Load lock file for installed template version resolution.
	//
	// Tolerated, but not silent: with no lock the installed templates resolve no
	// version, so they render blank or drop out of the listing entirely. Since
	// the format floor moved to v2 a v1 lock produces exactly
	// that in every unmigrated workspace, and the error names the conversion
	// (B6r/F6). Discovery runs from several call sites per invocation, so the
	// notice is deduped per workspace+reason.
	lockFile, lockErr := lockfile.ReadLockFile(workspaceRoot)
	if lockErr != nil {
		warnLockOnce(workspaceRoot, lockErr)
	}

	// 1. Scan workspace projects for template manifests
	for _, projPath := range projectPaths {
		absPath := filepath.Join(workspaceRoot, projPath)
		tpl := tryLoadTemplate(absPath, projPath)
		if tpl != nil && !seen[tpl.Name] {
			tpl.RelPath = projPath
			seen[tpl.Name] = true
			templates = append(templates, tpl)
		}
	}

	// 2. Scan <domain>/templates/*/ directories
	templates = discoverDomainTemplates(workspaceRoot, templates, seen)

	// 3. Discover installed templates via stable symlinks and artifacts
	templates = discoverInstalledTemplates(workspaceRoot, templates, seen, lockFile)

	return templates, nil
}

// discoverDomainTemplates scans top-level directories for templates/ subdirectories.
func discoverDomainTemplates(workspaceRoot string, templates []*TemplateDescription, seen map[string]bool) []*TemplateDescription {
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return templates
	}
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "." || entry.Name() == ".." {
			continue
		}
		if entry.Name()[0] == '.' || entry.Name() == "node_modules" {
			continue
		}
		templatesDir := filepath.Join(workspaceRoot, entry.Name(), "templates")
		tplEntries, err := os.ReadDir(templatesDir)
		if err != nil {
			continue
		}
		for _, tplEntry := range tplEntries {
			if !tplEntry.IsDir() {
				continue
			}
			absPath := filepath.Join(templatesDir, tplEntry.Name())
			relPath := filepath.Join(entry.Name(), "templates", tplEntry.Name())
			tpl := tryLoadTemplate(absPath, relPath)
			if tpl != nil && !seen[tpl.Name] {
				tpl.RelPath = relPath
				seen[tpl.Name] = true
				templates = append(templates, tpl)
			}
		}
	}
	return templates
}

// discoverInstalledTemplates scans .putnami/bin/templates/ for stable symlinks
// and checks artifacts for lock-file-matched versions.
func discoverInstalledTemplates(workspaceRoot string, templates []*TemplateDescription, seen map[string]bool, lockFile *lockfile.LockFile) []*TemplateDescription {
	stableBase := layout.StableBaseDir(workspaceRoot, layout.Templates)
	entries, err := os.ReadDir(stableBase)
	if err == nil {
		for _, entry := range entries {
			absPath := filepath.Join(stableBase, entry.Name())
			tpl := tryLoadTemplate(absPath, "")
			if tpl != nil && !seen[tpl.Name] {
				seen[tpl.Name] = true
				templates = append(templates, tpl)
			}
		}
	}

	// Also check artifacts for lock-file-matched versions not yet symlinked
	if lockFile != nil {
		for name, entry := range lockFile.Templates {
			if seen[name] {
				continue
			}
			artifactDir := layout.ArtifactDir(workspaceRoot, layout.Templates, name, entry.Version)
			tpl := tryLoadTemplate(artifactDir, "")
			if tpl != nil && !seen[tpl.Name] {
				seen[tpl.Name] = true
				templates = append(templates, tpl)
			}
		}
	}

	return templates
}

func tryLoadTemplate(absPath, refName string) *TemplateDescription {
	manifestPath := filepath.Join(absPath, ManifestFilename)
	manifest, err := LoadManifest(manifestPath)
	if err != nil {
		return nil
	}
	tpl := Resolve(manifest, absPath)
	if tpl.Name == "" {
		tpl.Name = refName
	}
	return tpl
}

// FindByName returns the first template matching the given name.
func FindByName(templates []*TemplateDescription, name string) *TemplateDescription {
	for _, tpl := range templates {
		if tpl.Name == name {
			return tpl
		}
	}
	return nil
}
