package distributioncli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/sitecontent"
)

// ValidateSiteContentSources checks only the selected project's declared site
// source structure. It shares section/mount and regular-file/path predicates
// with packaging, but never reads payload bytes, hashes them, or builds an
// archive. Generated artifacts, provenance and archive verification belong to
// packaging. Each invocation walks current directory metadata without a cache.
func ValidateSiteContentSources(params map[string]any, args []string, workspaceRoot string, ioctx clicore.IO) error {
	project, found, err := resolveSiteContentProject(params, args, workspaceRoot)
	if err != nil {
		return err
	}
	if !found {
		writePublishResult(map[string]any{"status": "skipped", "reason": "project declares no site-content publication"},
			params, ioctx, "No site-content publication declared — skipping source validation.")
		return nil
	}
	sections, err := DeriveSiteContentSections(project.dir, filepath.Base(project.dir), "", map[string]string{})
	if err != nil {
		return err
	}
	files := 0
	for _, section := range sections {
		count := 0
		if err := walkSiteContentFiles(section.Dir, section.Mount, func(siteContentFile) { count++ }); err != nil {
			return err
		}
		if count == 0 {
			return emptySectionError(section)
		}
		files += count
	}
	writePublishResult(map[string]any{"status": "validated", "project": project.id, "sections": len(sections), "files": files},
		params, ioctx, fmt.Sprintf("Validated site-content sources for %s (%d sections, %d files).", project.id, len(sections), files))
	return nil
}

// walkSiteContentFiles is the source-only half of collection. The visitor gets
// paths, never content. Packaging collects these paths before reading bytes;
// ordinary validation only counts them to reject an empty declared section.
func walkSiteContentFiles(sourceDir, mount string, visit func(siteContentFile)) error {
	info, err := os.Lstat(sourceDir)
	if os.IsNotExist(err) {
		return nil // preserves the direct publisher's --if-present behavior
	}
	if err != nil {
		return clicore.NewError("read site content source: "+err.Error(), clicore.ExitUsage)
	}
	if !info.IsDir() {
		return clicore.NewError("site content source "+sourceDir+" is not a directory", clicore.ExitUsage)
	}
	pathPrefix := strings.TrimPrefix(mount, "/")
	err = filepath.WalkDir(sourceDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file (symlinks and special files cannot ship in a site-content bundle)", path)
		}
		rel, err := filepath.Rel(sourceDir, path)
		if err != nil {
			return err
		}
		payloadPath := pathPrefix + "/" + filepath.ToSlash(rel)
		if !sitecontent.ValidRelPath(payloadPath) {
			return fmt.Errorf("file %s maps to invalid payload path %q", path, payloadPath)
		}
		visit(siteContentFile{payloadPath: payloadPath, absPath: path})
		return nil
	})
	if err != nil {
		return clicore.NewError("collect site content: "+err.Error(), clicore.ExitUsage)
	}
	return nil
}
