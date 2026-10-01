package codegen

import (
	"encoding/json"
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
// Every generated entry of the output directory (client.gen.go, the manifest,
// anything else the stage carries) is removed and rewritten each run, so a
// stale file whose producer disappeared never survives. The workspace-owned
// scaffold files listed in ownedClientFiles are the exception: they are
// scaffolded only when absent while the stage still includes one, and an
// existing copy is neither moved nor overwritten, so a committed or hand-edited
// module or project file never churns and the workspace go.work stays stable.
// When the stage carries no client (e.g. no routes), the generated entries are
// dropped, the owned files are kept, and the directory is removed only once it
// is empty. Returns the project-relative paths present after the mirror — the
// generated files plus every owned file — for the manifest, plus the
// project-relative output directories managed by the client generator so the
// scheduler can cache/restore or remove them on cache hits.
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

	// Drop the generated entries first, in place: owned files never move, so a
	// reader of clients/go/putnami.json or go.mod never observes them vanish.
	if err := removeGeneratedClientEntries(targetDir); err != nil {
		return nil, nil, err
	}

	if _, err := os.Stat(stageDir); err != nil {
		if !os.IsNotExist(err) {
			return nil, nil, err
		}
		// Contract written but no client produced (e.g. no routes): keep the
		// owned files and remove the directory only when nothing is left.
		if err := removeDirIfEmpty(targetDir); err != nil {
			return nil, nil, err
		}
		return ownedClientArtifacts(targetDir, output), outputs, nil
	}

	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return nil, nil, err
	}
	var artifacts []string
	walkErr := filepath.WalkDir(stageDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(stageDir, path)
		if relErr != nil {
			return relErr
		}
		dst := filepath.Join(targetDir, rel)
		if isOwnedClientFile(rel) {
			// Scaffold-once: write the staged copy only when the target has
			// none. Lstat, so even a dangling symlink counts as present and is
			// never written through. It is reported below with the other owned
			// files.
			if _, statErr := os.Lstat(dst); statErr == nil {
				return nil
			}
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0o755); mkErr != nil {
			return mkErr
		}
		if writeErr := os.WriteFile(dst, body, 0o644); writeErr != nil {
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
	artifacts = append(artifacts, ownedClientArtifacts(targetDir, output)...)
	return artifacts, outputs, nil
}

// removeGeneratedClientEntries deletes every entry of targetDir except the
// owned scaffold files and the client project's .gen directory, leaving those
// untouched in place. A missing directory is not an error; a non-directory at
// that path is removed, since the mirror owns it.
func removeGeneratedClientEntries(targetDir string) error {
	info, err := os.Lstat(targetDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return os.RemoveAll(targetDir)
	}
	entries, err := os.ReadDir(targetDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() && isOwnedClientFile(entry.Name()) {
			continue
		}
		if entry.IsDir() && entry.Name() == clientProjectGenDir {
			continue
		}
		if err := os.RemoveAll(filepath.Join(targetDir, entry.Name())); err != nil {
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
