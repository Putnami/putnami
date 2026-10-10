package codegen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// clientGenContract is the subset of .gen/clientgen/config.json the runner needs
// to place generated clients. It mirrors the contract the api client describer
// writes (go/framework/api/clients.go) and the TS generator's config.type.ts.
type clientGenContract struct {
	Targets []string `json:"targets"`
	Go      struct {
		Output string `json:"output"`
	} `json:"go"`
}

// ownedClientFiles are the workspace-owned scaffold files at the root of a
// generated client directory. The mirror never deletes and never overwrites
// them: a staged copy is written only when the target has none (scaffold-once),
// an existing target copy is kept byte-for-byte, and go.sum — never staged, a
// module-mode `go mod tidy` product — is kept when present. go.mod and
// putnami.json are the pair the cross-language generator
// (go/framework/api GenerateProjectClients) scaffolds once too, so a Go-emitted
// client module can be a workspace project of its own without the provider
// build wiping its metadata on every run.
var ownedClientFiles = []string{"go.mod", "go.sum", "putnami.json"}

func isOwnedClientFile(rel string) bool {
	return slices.Contains(ownedClientFiles, rel)
}

// clientProjectGenDir is the .gen directory of a client directory that is a
// workspace project of its own. The engine and that project's tasks write it:
// the mirror never deletes it, and the build-describe client output preserves
// it.
const clientProjectGenDir = ".gen"

// mirrorGeneratedClients copies the client code the describe binary staged under
// .gen/clientgen/go/ into the project's configured output directory (clients/go
// by default). The describe binary writes only under .gen — honoring the
// Describer "OutputDir only" contract — so this runner step is the sole writer of
// the generated client in the project tree, and the scheduler captures that
// directory under the project-scoped "clients" cache resource.
//
// The output directory ends with exactly the staged generated entries
// (client.gen.go, the manifest, anything else the stage carries), so a stale
// file whose producer disappeared never survives. The mirror never leaves a
// generated file absent: a project that imports the client can compile while
// the provider's describe runs, and nothing orders it after that step. Each
// staged file replaces its target through a temporary file and a rename, a
// target whose bytes already match is left untouched, and a stale entry is
// removed only once the new set is in place. The workspace-owned scaffold files
// listed in ownedClientFiles are the exception: they are scaffolded only when
// absent while the stage still includes one, and an existing copy is neither
// moved nor overwritten, so a committed or hand-edited module or project file
// never churns and the workspace go.work stays stable. When the stage carries
// no client (e.g. no routes), the generated entries are dropped, the owned
// files are kept, and the directory is removed only once it is empty. Returns
// the project-relative paths present after the mirror — the generated files
// plus every owned file — for the manifest, plus the project-relative output
// directories managed by the client generator so the scheduler can
// cache/restore or remove them on cache hits.
func mirrorGeneratedClients(projectPath, genDir string) ([]string, []string, error) {
	raw, err := os.ReadFile(filepath.Join(genDir, "clientgen", "config.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil // no client generator configured for this project
		}
		return nil, nil, err
	}
	var cfg clientGenContract
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return nil, nil, fmt.Errorf("parse clientgen config: %w", err)
	}
	if !slices.Contains(cfg.Targets, "go") {
		return nil, nil, nil
	}

	output, err := cleanClientOutputDir(cfg.Go.Output)
	if err != nil {
		return nil, nil, err
	}
	outputs := []string{filepath.ToSlash(output)}
	stageDir := filepath.Join(genDir, "clientgen", "go")
	targetDir := filepath.Join(projectPath, output)

	if _, err := os.Stat(stageDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, err
		}
		// Contract written but no client produced (e.g. no routes): keep the
		// owned files and remove the directory only when nothing is left.
		if err := removeStaleClientEntries(targetDir, nil); err != nil {
			return nil, nil, err
		}
		if err := removeDirIfEmpty(targetDir); err != nil {
			return nil, nil, err
		}
		return ownedClientArtifacts(targetDir, output), outputs, nil
	}

	// A non-directory at the output path is the mirror's own stale output.
	if info, err := os.Lstat(targetDir); err == nil && !info.IsDir() {
		if err := os.Remove(targetDir); err != nil {
			return nil, nil, err
		}
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, nil, err
	}
	var artifacts []string
	staged := map[string]bool{}
	walkErr := filepath.WalkDir(stageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(stageDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		staged[rel] = true
		dst := filepath.Join(targetDir, filepath.FromSlash(rel))
		if isOwnedClientFile(rel) {
			// Scaffold-once: write the staged copy only when the target has
			// none. Lstat, so even a dangling symlink counts as present and is
			// never written through. It is reported below with the other owned
			// files.
			if info, statErr := os.Lstat(dst); statErr == nil && !info.IsDir() {
				return nil
			}
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if writeErr := replaceClientFile(targetDir, rel, body); writeErr != nil {
			return writeErr
		}
		if !isOwnedClientFile(rel) {
			artifacts = append(artifacts, filepath.ToSlash(filepath.Join(output, rel)))
		}
		return nil
	})
	if walkErr != nil {
		return nil, nil, walkErr
	}
	if err := removeStaleClientEntries(targetDir, staged); err != nil {
		return nil, nil, err
	}
	artifacts = append(artifacts, ownedClientArtifacts(targetDir, output)...)
	return artifacts, outputs, nil
}

// replaceClientFile makes targetDir/rel hold body without the path ever being
// absent: a regular file with the same bytes is left untouched, otherwise body
// goes to a temporary file beside the target that a rename moves over it. The
// temporary name starts with a dot, which the go tool ignores, so a concurrent
// build never compiles it. A stale entry in the way — a directory at the file
// path or a file at a parent directory path — is removed first: the stage no
// longer carries it.
func replaceClientFile(targetDir, rel string, body []byte) (resultErr error) {
	dst := filepath.Join(targetDir, filepath.FromSlash(rel))
	if err := clearClientParents(targetDir, rel); err != nil {
		return err
	}
	if info, err := os.Lstat(dst); err == nil {
		if info.Mode().IsRegular() {
			if current, readErr := os.ReadFile(dst); readErr == nil && bytes.Equal(current, body) {
				return nil
			}
		} else if info.IsDir() {
			if err := os.RemoveAll(dst); err != nil {
				return err
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(dst), "."+filepath.Base(dst)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if resultErr != nil {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(body); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// clearClientParents removes every non-directory entry that sits where a
// parent directory of targetDir/rel must be.
func clearClientParents(targetDir, rel string) error {
	dir := targetDir
	parts := strings.Split(rel, "/")
	for _, part := range parts[:len(parts)-1] {
		dir = filepath.Join(dir, part)
		info, err := os.Lstat(dir)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if !info.IsDir() {
			return os.Remove(dir)
		}
	}
	return nil
}

// removeStaleClientEntries deletes every entry of targetDir the stage no longer
// carries: each file whose slash-separated relative path is not in staged, then
// each directory left empty. The owned scaffold files and the client project's
// .gen directory at the root stay untouched. A missing directory is not an
// error.
func removeStaleClientEntries(targetDir string, staged map[string]bool) error {
	info, err := os.Lstat(targetDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return os.Remove(targetDir)
	}
	var dirs []string
	walkErr := filepath.WalkDir(targetDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == targetDir {
			return nil
		}
		rel, relErr := filepath.Rel(targetDir, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel == clientProjectGenDir {
				return filepath.SkipDir
			}
			dirs = append(dirs, path)
			return nil
		}
		if staged[rel] || isOwnedClientFile(rel) {
			return nil
		}
		return os.Remove(path)
	})
	if walkErr != nil {
		return walkErr
	}
	// WalkDir visits a parent before its children: the reverse order empties
	// the deepest directories first.
	for i := len(dirs) - 1; i >= 0; i-- {
		if err := removeDirIfEmpty(dirs[i]); err != nil {
			return err
		}
	}
	return nil
}

// removeDirIfEmpty removes targetDir when it exists and holds no entry.
func removeDirIfEmpty(targetDir string) error {
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if len(entries) != 0 {
		return nil
	}
	return os.Remove(targetDir)
}

// ownedClientArtifacts lists, in a fixed order, the project-relative path of
// every owned scaffold file present in targetDir after the mirror, so the
// manifest records them whether this run scaffolded them or an earlier one did.
func ownedClientArtifacts(targetDir, output string) []string {
	var artifacts []string
	for _, name := range ownedClientFiles {
		info, err := os.Lstat(filepath.Join(targetDir, name))
		if err != nil || info.IsDir() {
			continue
		}
		artifacts = append(artifacts, filepath.ToSlash(filepath.Join(output, name)))
	}
	return artifacts
}

func cleanClientOutputDir(output string) (string, error) {
	if output == "" {
		output = "clients/go"
	}
	clean := filepath.Clean(filepath.FromSlash(output))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("clientgen go.output %q escapes project root", output)
	}
	return clean, nil
}
