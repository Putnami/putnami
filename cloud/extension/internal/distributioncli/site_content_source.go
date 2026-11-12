package distributioncli

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	clicore "go.putnami.dev/cloud/extension/internal/clicore"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/gitcandidate"
)

// siteContentSource is the tree one site directory's sections and files are
// read from. Inside a Git work tree it is the directory's Git candidate files,
// the cut the validation step's `git:**` key holds: a file Git ignores and an
// empty directory are absent, as they are from a clone. Outside a work tree
// it is the disk. Validation, the workspace probe, packaging and publication
// all read site content through it, so a project that passes validation
// packages the sections and files validation checked.
//
// A putnami.verdict-policy.json file is never site content. The CI plane
// reads it, and no task key holds it unless a pattern names it, so neither
// the validation key nor the package's selection fingerprint would move when
// it changes. The source leaves it out of every section and walk.
type siteContentSource struct {
	dir string
	// tree is nil outside a Git work tree.
	tree *gitcandidate.Tree
	// candidates holds, sorted, the candidate files and the unkeyable
	// candidates of tree.
	candidates []string
	// generated holds, sorted, the generated sections the project document
	// declares (readSiteContentGenerators). Each one is read from the disk
	// under .gen/site-content/<section>, inside a Git work tree too: the
	// build step writes it, and no Git candidate holds it.
	generated []string
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
	generators, err := readSiteContentGenerators(siteDir)
	if err != nil {
		return nil, err
	}
	for _, generator := range generators {
		source.generated = append(source.generated, generator.section)
	}
	return source, nil
}

// generatedSection reports the declared generated section whose directory is
// sourceDir.
func (s *siteContentSource) generatedSection(sourceDir string) (string, bool) {
	for _, section := range s.generated {
		if filepath.Clean(sourceDir) == siteContentGeneratedSectionDir(s.dir, section) {
			return section, true
		}
	}
	return "", false
}

// sectionNames returns the sorted names of the site's sections: its section
// directories and its declared generated sections. On disk a section
// directory is an immediate subdirectory. In the cut it is the first
// directory of a candidate path, or a submodule directly under the site
// directory, so a section always holds a candidate. A dot-directory is
// tooling state, never a section: once a site directory is a project, putnami
// writes .gen/ into it, and a directory under .gen/site-content is a section
// only when the project declares its name. A declared generated section that
// is also a section directory is refused: a section has one source. The names
// never depend on whether .gen/site-content exists, so the workspace probe,
// which runs before build, lists the same sections packaging reads.
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
			if wsproto.IsNamedOnlyInput(candidate) {
				continue
			}
			section, _, nested := strings.Cut(candidate, "/")
			if mode, _ := s.tree.Lstat(candidate); nested || mode.IsDir() {
				names = append(names, section)
			}
		}
		slices.Sort(names)
		names = slices.Compact(names)
	}
	names = slices.DeleteFunc(names, func(name string) bool { return strings.HasPrefix(name, ".") })
	for _, section := range s.generated {
		if slices.Contains(names, section) {
			return nil, clicore.NewError(fmt.Sprintf(
				"section %q of %s is declared generated and is also a section directory; a section has one source: remove the directory or the declaration",
				section, s.dir), clicore.ExitUsage)
		}
	}
	names = append(names, s.generated...)
	slices.Sort(names)
	return names, nil
}

// walkSection visits the files of the section directory sourceDir, served
// under mount, and applies the regular-file and path policy to each. The
// visitor gets paths, never content: packaging collects them before reading
// bytes, and validation only counts them. In the cut, a submodule, a nested
// repository or any other candidate Git lists that is not a regular file or a
// symbolic link is refused like a symbolic link, since no `git:` key holds it.
//
// A declared generated section is read from the disk, and its directory must
// exist: an absent one means the build step that writes it has not run.
func (s *siteContentSource) walkSection(sourceDir, mount string, visit func(siteContentFile)) error {
	if section, generated := s.generatedSection(sourceDir); generated {
		if _, err := os.Lstat(sourceDir); os.IsNotExist(err) {
			return clicore.NewError(fmt.Sprintf(
				"generated section %q of %s has no directory %s; run `putnami build` for the project, whose cloud-site-content-generate step writes it",
				section, s.dir, sourceDir), clicore.ExitUsage)
		}
		return walkSiteContentFiles(sourceDir, mount, visit)
	}
	if s.tree == nil {
		return walkSiteContentFiles(sourceDir, mount, visit)
	}
	section, err := filepath.Rel(s.dir, sourceDir)
	if err == nil && (section == "." || !filepath.IsLocal(section) || filepath.Dir(section) != ".") {
		err = fmt.Errorf("%s is not a section directory of %s", sourceDir, s.dir)
	}
	if err != nil {
		return clicore.NewError("read site content source: "+err.Error(), clicore.ExitUsage)
	}
	for _, candidate := range s.candidates {
		if candidate != section && !strings.HasPrefix(candidate, section+"/") {
			continue
		}
		if wsproto.IsNamedOnlyInput(candidate) {
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
