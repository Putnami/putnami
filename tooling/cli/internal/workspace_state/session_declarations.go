package workspace_state

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	architecture "go.putnami.dev/protocol/architecture"
	"go.putnami.dev/protocol/cli"
	features "go.putnami.dev/protocol/features"
)

// declarationDigestHeader opens the canonical declaration byte stream defined
// in protocols/cli/doc/02-result-v2.md § Declaration digest.
const declarationDigestHeader = "putnami-declarations/1\n"

// declarationFilenames are the declaration files a directory can hold beside
// its specs/ directory.
var declarationFilenames = []string{
	features.ManifestFilename,
	features.DecisionsFilename,
	features.SpecsBaselineFilename,
	architecture.ManifestFilename,
	architecture.BaselineFilename,
}

// DeclarationDigest digests, by content, the declaration files the declared
// checks read: in the workspace root, in every project directory and in every
// directory between the two, the files named by declarationFilenames and every
// `specs/*.json`. projectPaths are slash-separated and relative to root.
//
// A missing file is not a declaration; any other read failure returns an
// error, so a caller records no digest rather than one over a partial set.
func DeclarationDigest(root string, projectPaths []string) (*cli.SessionDeclarations, error) {
	files := make(map[string]struct{})
	for _, dir := range declarationDirs(projectPaths) {
		for _, name := range declarationFilenames {
			files[path.Join(dir, name)] = struct{}{}
		}
		specs, err := os.ReadDir(filepath.Join(root, filepath.FromSlash(path.Join(dir, features.SpecDirectory))))
		if err != nil && !errors.Is(err, fs.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
			return nil, err
		}
		for _, entry := range specs {
			if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), ".json") {
				files[path.Join(dir, features.SpecDirectory, entry.Name())] = struct{}{}
			}
		}
	}

	paths := make([]string, 0, len(files))
	for file := range files {
		paths = append(paths, file)
	}
	sort.Strings(paths)

	digest := sha256.New()
	var entries strings.Builder
	count := 0
	for _, file := range paths {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		content := sha256.Sum256(data)
		fmt.Fprintf(&entries, "%s %d\n%s\n", hex.EncodeToString(content[:]), len(file), file)
		count++
	}
	fmt.Fprintf(digest, "%sfiles %d\n%s", declarationDigestHeader, count, entries.String())
	return &cli.SessionDeclarations{Digest: hex.EncodeToString(digest.Sum(nil)), Files: count}, nil
}

// declarationDirs returns the workspace root (".") and every project directory
// with its ancestors, deduplicated and sorted.
func declarationDirs(projectPaths []string) []string {
	dirs := map[string]struct{}{".": {}}
	for _, projectPath := range projectPaths {
		dir := path.Clean(strings.TrimPrefix(filepath.ToSlash(projectPath), "/"))
		for dir != "." && dir != "/" && dir != "" && !strings.HasPrefix(dir, "..") {
			dirs[dir] = struct{}{}
			dir = path.Dir(dir)
		}
	}
	sorted := make([]string, 0, len(dirs))
	for dir := range dirs {
		sorted = append(sorted, dir)
	}
	sort.Strings(sorted)
	return sorted
}

// CaptureDeclarations records the declaration digest this session runs
// against. Like CaptureTree, it must be called before the first task runs, and
// a failure records no block.
func (s *Session) CaptureDeclarations(root string, projectPaths []string) {
	if root == "" {
		return
	}
	declarations, err := DeclarationDigest(root, projectPaths)
	if err != nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.declarations = declarations
}
