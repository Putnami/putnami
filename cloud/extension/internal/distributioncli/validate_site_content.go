package distributioncli

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/protocol/sitecontent"
	"go.putnami.dev/sdk/extension/gitcandidate"
)

// ValidateSiteContentSources checks only the declared site source structure of
// the project in its working directory. It shares section/mount and
// regular-file/path predicates with packaging, but never reads payload bytes,
// hashes them, or builds an archive. Generated artifacts, provenance and
// archive verification belong to packaging.
//
// The task is cached on the project's putnami.json and on the project's Git
// candidate cut (`git:**`), so inside a Git work tree the check reads the
// sections through that cut and nothing else: a file Git ignores and an empty
// directory are absent, as they are from a clone, and a symbolic link is a
// candidate the key holds as a link, so the check refuses it. Outside a work
// tree no `git:` key exists to serve a verdict from, and the check walks the
// disk.
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
	tree, err := gitcandidate.Open(project.dir)
	if err != nil {
		return clicore.NewError("list the candidate files of "+project.dir+": "+err.Error(), clicore.ExitUsage)
	}
	var sections, files int
	if tree != nil {
		sections, files, err = countCandidateSiteContent(tree, project.dir)
	} else {
		sections, files, err = countSiteContentOnDisk(project.dir)
	}
	if err != nil {
		return err
	}
	writePublishResult(map[string]any{"status": "validated", "project": project.id, "sections": sections, "files": files},
		params, ioctx, fmt.Sprintf("Validated site-content sources for %s (%d sections, %d files).", project.id, sections, files))
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

// countCandidateSiteContent applies the section and file policy to the
// candidate cut of a site directory. A section is the first segment of a
// candidate that lies below one: a file directly under the site directory is
// meta, and a dot-directory is tooling state. A section therefore always holds
// a file.
func countCandidateSiteContent(tree *gitcandidate.Tree, siteDir string) (sections, files int, err error) {
	site := filepath.Base(siteDir)
	mounts := map[string]string{}
	for _, candidate := range tree.Paths() {
		section, _, nested := strings.Cut(candidate, "/")
		if !nested || strings.HasPrefix(section, ".") {
			continue
		}
		mount, known := mounts[section]
		if !known {
			if mount, err = siteContentMount(site, section); err != nil {
				return 0, 0, err
			}
			mounts[section] = mount
		}
		mode, _ := tree.Lstat(candidate)
		path := filepath.Join(siteDir, filepath.FromSlash(candidate))
		if _, err := siteContentPayloadPath(filepath.Join(siteDir, section), mount, path, mode.IsRegular()); err != nil {
			return 0, 0, clicore.NewError("collect site content: "+err.Error(), clicore.ExitUsage)
		}
		files++
	}
	return len(mounts), files, nil
}

// countSiteContentOnDisk applies the same policy to the directory itself,
// where an empty section directory exists and is refused.
func countSiteContentOnDisk(siteDir string) (sections, files int, err error) {
	derived, err := DeriveSiteContentSections(siteDir, filepath.Base(siteDir), "", map[string]string{})
	if err != nil {
		return 0, 0, err
	}
	for _, section := range derived {
		count := 0
		if err := walkSiteContentFiles(section.Dir, section.Mount, func(siteContentFile) { count++ }); err != nil {
			return 0, 0, err
		}
		if count == 0 {
			return 0, 0, emptySectionError(section)
		}
		files += count
	}
	return len(derived), files, nil
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
