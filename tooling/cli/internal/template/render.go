package template

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	templateproto "go.putnami.dev/protocol/template"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
)

// RenderVars holds the variables available for template substitution.
type RenderVars struct {
	ProjectName           string
	ProjectPath           string
	ProjectModule         string
	PutnamiVersion        string
	WorkspaceRelativePath string
}

// RenderDir copies a template directory to the target, performing
// template variable substitution. Files ending in ".template" are processed
// with <%= var %> substitution and the suffix is stripped.
// The putnami.template.json manifest is skipped (it's metadata, not project
// content), and so is the artifact store's bookkeeping at the template root (an
// installed template is an artifact store entry; see
// artifactstore.IsBookkeeping). So are the root entries a template archive
// leaves out (see templateproto.IsTemplateRootPackagingExclusion): a template
// rendered from its source directory in a workspace creates the same project
// as its installed archive, without the declarations of the template project.
func RenderDir(src, dst string, vars RenderVars) error {
	// Resolve the link so WalkDir descends into the real directory. WalkDir
	// uses Lstat for the root entry, so a linked src would report
	// IsDir()==false and the walk would read the directory as a file. An
	// installed template is a directory link: a junction on Windows, which
	// filepath.EvalSymlinks does not follow.
	src, err := dirlink.Resolve(src)
	if err != nil {
		return err
	}

	root, err := os.OpenRoot(src)
	if err != nil {
		return err
	}
	defer root.Close() //nolint:errcheck // best-effort cleanup

	return filepath.WalkDir(src, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		// Skip the template manifest — it's metadata, not project content
		if rel == ManifestFilename {
			return nil
		}
		// Skip the store's recency sidecar and its temporary files: they are
		// bookkeeping of the store entry the template is installed in.
		if !d.IsDir() && filepath.Dir(rel) == "." && artifactstore.IsBookkeeping(rel) {
			return nil
		}
		// Skip what the archive leaves out at the root, so a template in the
		// workspace renders what its archive would.
		if rel != "." && filepath.Dir(rel) == "." && templateproto.IsTemplateRootPackagingExclusion(rel) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// Substitute magic directory names in the target path
		targetRel := strings.ReplaceAll(rel, "__module__", vars.ProjectModule)
		target := filepath.Join(dst, targetRel)

		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}

		f, err := root.Open(rel)
		if err != nil {
			return err
		}
		data, err := io.ReadAll(f)
		f.Close()
		if err != nil {
			return err
		}

		info, _ := d.Info()
		mode := os.FileMode(0o644)
		if info != nil {
			mode = info.Mode()
		}

		// Files ending in .template get variable substitution and suffix stripping
		if strings.HasSuffix(target, ".template") {
			target = strings.TrimSuffix(target, ".template")
			content := EvalTemplate(string(data), vars)
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			return os.WriteFile(target, []byte(content), mode)
		}

		// Non-template files are copied as-is
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		return os.WriteFile(target, data, mode)
	})
}

// EvalTemplate replaces <%= varName %> placeholders with values from vars.
func EvalTemplate(content string, vars RenderVars) string {
	replacements := map[string]string{
		"projectName":           vars.ProjectName,
		"projectPath":           vars.ProjectPath,
		"projectModule":         vars.ProjectModule,
		"putnamiVersion":        vars.PutnamiVersion,
		"workspaceRelativePath": vars.WorkspaceRelativePath,
	}
	for key, val := range replacements {
		if val == "" {
			continue
		}
		old := "<%= " + key + " %>"
		content = strings.ReplaceAll(content, old, val)
	}
	return content
}

// NormalizeProjectModule converts a project name to a valid Go module/package name.
func NormalizeProjectModule(name string) string {
	base := strings.ToLower(strings.ReplaceAll(name, "@", ""))
	var buf strings.Builder
	for _, r := range base {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			buf.WriteRune(r)
		} else {
			buf.WriteRune('_')
		}
	}
	normalized := strings.ReplaceAll(buf.String(), "__", "_")
	normalized = strings.Trim(normalized, "_")
	if normalized == "" {
		return "pkg"
	}
	if normalized[0] >= '0' && normalized[0] <= '9' {
		return "pkg_" + normalized
	}
	return normalized
}

// DefaultTestVars returns variables suitable for testing a template.
func DefaultTestVars() RenderVars {
	return RenderVars{
		ProjectName:           "test-project",
		ProjectPath:           "test-project",
		ProjectModule:         "test_project",
		PutnamiVersion:        "latest",
		WorkspaceRelativePath: "..",
	}
}
