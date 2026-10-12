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

// ValidateSiteContentSources checks only the declared site source structure of
// the project in its working directory. It shares section/mount and
// regular-file/path predicates with packaging, but never reads payload bytes,
// hashes them, or builds an archive. Generated artifacts, provenance and
// archive verification belong to packaging.
//
// The task is cached on the project's putnami.json and on the project's Git
// candidate cut (`git:**`). It reads the sections through siteContentSource,
// as packaging does, so inside a Git work tree it reads that cut and nothing
// else: a file Git ignores and an empty directory are absent, as they are from
// a clone, and a symbolic link is a candidate the key holds as a link, so the
// check refuses it. Outside a work tree no `git:` key exists to serve a
// verdict from, and the check walks the disk.
func ValidateSiteContentSources(params map[string]any, args []string, workspaceRoot string, ioctx clicore.IO) error {
	clicore.AdoptPositionalApp(params, args)
	app, err := clicore.ResolveApp(params, workspaceRoot)
	if err != nil {
		return err
	}
	if err := refuseOtherSiteContentProject(app); err != nil {
		return err
	}
	project, found, err := siteContentProjectNamed(workspaceRoot, app)
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
		if err := section.walk(func(siteContentFile) { count++ }); err != nil {
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

// refuseOtherSiteContentProject refuses a project other than the one in the
// working directory. The own project comes from the working directory's
// putnami.json, never from a workspace walk.
func refuseOtherSiteContentProject(project string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	own, _ := clicore.ProjectNameAt(cwd)
	if own == "" || project != own {
		return clicore.NewError(fmt.Sprintf(
			"validate~cloud-site-content checks only the project in its working directory %s, not %s; run `putnami validate --projects %s` instead",
			cwd, project, project), clicore.ExitUsage)
	}
	return nil
}

// walkSiteContentFiles is the disk half of siteContentSource.walkSection.
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
	err = filepath.WalkDir(sourceDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		payloadPath, err := siteContentPayloadPath(sourceDir, mount, path, entry.Type().IsRegular())
		if err != nil {
			return err
		}
		visit(siteContentFile{payloadPath: payloadPath, absPath: path})
		return nil
	})
	if err != nil {
		return clicore.NewError("collect site content: "+err.Error(), clicore.ExitUsage)
	}
	return nil
}

// siteContentPayloadPath applies the regular-file and path policy to one source
// file of a section and returns the path it ships at: its section-relative
// path under the mount, minus the mount's leading "/".
func siteContentPayloadPath(sourceDir, mount, path string, regular bool) (string, error) {
	if !regular {
		return "", fmt.Errorf("%s is not a regular file (symlinks and special files cannot ship in a site-content bundle)", path)
	}
	rel, err := filepath.Rel(sourceDir, path)
	if err != nil {
		return "", err
	}
	payloadPath := strings.TrimPrefix(mount, "/") + "/" + filepath.ToSlash(rel)
	if !sitecontent.ValidRelPath(payloadPath) {
		return "", fmt.Errorf("file %s maps to invalid payload path %q", path, payloadPath)
	}
	return payloadPath, nil
}
