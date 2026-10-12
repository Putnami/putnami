package distributioncli

import (
	"os"
	"path/filepath"
	"slices"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	"go.putnami.dev/sdk/extension/gitcandidate"
)

// siteContentSource is the tree one site directory's sections and files are
// read from. Inside a Git work tree it is the directory's Git candidate files,
// the cut the validation step's `git:**` key holds: a file Git ignores and an
// empty directory are absent, as they are from a clone. Outside a work tree
// it is the disk. Validation, the workspace probe, packaging and publication
// all read site content through it, so a project that passes validation
// packages the sections and files validation checked.
type siteContentSource struct {
	dir string
	// tree is nil outside a Git work tree.
	tree *gitcandidate.Tree
	// candidates holds, sorted, the candidate files and the unkeyable
	// candidates of tree.
	candidates []string
}

func openSiteContentSource(siteDir string) (*siteContentSource, error) {
	tree, err := gitcandidate.Open(siteDir)
	if err != nil {
		return nil, clicore.NewError("list the candidate files of "+siteDir+": "+err.Error(), clicore.ExitUsage)
	}
	source := &siteContentSource{dir: siteDir, tree: tree}
	if tree != nil {
		source.candidates = slices.Concat(tree.Paths(), tree.Unkeyable())
		slices.Sort(source.candidates)
	}
	return source, nil
}

// sectionNames returns the sorted names of the site's section directories.
// On disk a section is an immediate subdirectory. In the cut it is the first
// directory of a candidate path, or a submodule directly under the site
// directory, so a section always holds a candidate. A dot-directory is
// tooling state, never a section: once a site directory is a project, putnami
// writes .gen/ into it.
func (s *siteContentSource) sectionNames() ([]string, error) {
	var names []string
	if s.tree == nil {
		entries, err := os.ReadDir(s.dir)
		if err != nil {
			return nil, clicore.NewError("read site directory "+s.dir+": "+err.Error(), clicore.ExitUsage)
		}
		for _, entry := range entries {
			if entry.IsDir() {
				names = append(names, entry.Name())
			}
		}
	} else {
		for _, candidate := range s.candidates {
			section, _, nested := strings.Cut(candidate, "/")
			if mode, _ := s.tree.Lstat(candidate); nested || mode.IsDir() {
				names = append(names, section)
			}
		}
		slices.Sort(names)
		names = slices.Compact(names)
	}
	return slices.DeleteFunc(names, func(name string) bool { return strings.HasPrefix(name, ".") }), nil
}

// walkSection visits the files of the section directory sourceDir, served
// under mount, and applies the regular-file and path policy to each. The
// visitor gets paths, never content: packaging collects them before reading
// bytes, and validation only counts them. In the cut, a submodule, a nested
// repository, a socket, a pipe or a device in the section is refused like a
// symbolic link, since no `git:` key holds it.
func (s *siteContentSource) walkSection(sourceDir, mount string, visit func(siteContentFile)) error {
	if s.tree == nil {
		return walkSiteContentFiles(sourceDir, mount, visit)
	}
	rel, err := filepath.Rel(s.dir, sourceDir)
	if err != nil {
		return clicore.NewError("read site content source: "+err.Error(), clicore.ExitUsage)
	}
	section := filepath.ToSlash(rel)
	for _, candidate := range s.candidates {
		if candidate != section && !strings.HasPrefix(candidate, section+"/") {
			continue
		}
		mode, found := s.tree.Lstat(candidate)
		path := filepath.Join(s.dir, filepath.FromSlash(candidate))
		payloadPath, err := siteContentPayloadPath(sourceDir, mount, path, found && mode.IsRegular())
		if err != nil {
			return clicore.NewError("collect site content: "+err.Error(), clicore.ExitUsage)
		}
		visit(siteContentFile{payloadPath: payloadPath, absPath: path})
	}
	return nil
}
