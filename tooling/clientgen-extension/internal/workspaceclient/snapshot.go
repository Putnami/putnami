package workspaceclient

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	clientcontract "go.putnami.dev/protocol/clientcontract"
	diag "go.putnami.dev/protocol/diagnostic"
)

// SnapshotGeneratedArtifacts is the adoption command's "before": the generated
// client manifests as they stand when the command starts, read again after the
// in-place regeneration to prove an import or construction rename by two real
// emitter outputs (codemod.go). Drift itself is no longer judged from a
// snapshot: the engine compares each generator task's declared output with the
// bytes present before it wrote (protocols/extension ADR 0004), and the check
// reads committed inputs only (InspectCommitted).

// SnapshotGeneratedArtifacts captures the worktree bytes of every generated-client
// closure at the moment it is called. The adoption command takes it before its
// own provider build, so the closure it holds predates every write of the
// command.
func SnapshotGeneratedArtifacts(workspaceRoot string) (string, func(), []Finding, error) {
	projectPaths, err := indexedProjectPaths(workspaceRoot)
	if err != nil {
		return "", func() {}, nil, err
	}
	tempRoot, err := os.MkdirTemp("", "putnami-clientgen-baseline-")
	if err != nil {
		return "", func() {}, nil, err
	}
	cleanup := func() { _ = os.RemoveAll(tempRoot) }
	var findings []Finding
	for _, projectRel := range projectPaths {
		projectRoot := filepath.Join(workspaceRoot, filepath.FromSlash(projectRel))
		walkErr := filepath.WalkDir(projectRoot, func(path string, entry fs.DirEntry, pathErr error) error {
			if pathErr != nil {
				return pathErr
			}
			if entry.IsDir() && path != projectRoot && excludedSnapshotDir(entry.Name()) {
				return filepath.SkipDir
			}
			if entry.IsDir() || entry.Name() != clientcontract.GeneratedManifestFile {
				return nil
			}
			manifestRel, relErr := filepath.Rel(workspaceRoot, path)
			if relErr != nil {
				return relErr
			}
			manifestRel = filepath.ToSlash(manifestRel)
			manifestData, readErr := os.ReadFile(path) //nolint:gosec // discovered inside indexed project
			if readErr != nil {
				findings = append(findings, Finding{Code: "clientgen.unreadable-snapshot-manifest", Path: manifestRel,
					Message: fmt.Sprintf("generated client manifest could not be read before the build: %v", readErr)})
				return nil
			}
			manifest, diags := clientcontract.ParseAndValidateGeneratedManifest(manifestData)
			if manifest == nil || diag.HasErrors(diags) {
				findings = append(findings, Finding{Code: "clientgen.invalid-snapshot-manifest", Path: manifestRel,
					Message: "generated client manifest is invalid, so its file closure cannot be captured before the " +
						"build: " + firstErrorMessage(diags)})
				return nil
			}
			if copyErr := copyOptional(path, filepath.Join(tempRoot, filepath.FromSlash(manifestRel))); copyErr != nil {
				return copyErr
			}
			base := filepath.Dir(path)
			manifestBase := filepath.ToSlash(filepath.Dir(manifestRel))
			for _, file := range manifest.Files {
				if copyErr := copyOptional(filepath.Join(base, filepath.FromSlash(file.Path)),
					filepath.Join(tempRoot, filepath.FromSlash(joinRel(manifestBase, file.Path)))); copyErr != nil {
					return copyErr
				}
			}
			return nil
		})
		if walkErr != nil && !os.IsNotExist(walkErr) {
			cleanup()
			return "", func() {}, nil, walkErr
		}
	}
	return tempRoot, cleanup, findings, nil
}

func firstErrorMessage(diags []diag.Diagnostic) string {
	for _, value := range diags {
		if value.Severity == diag.Error {
			return value.String()
		}
	}
	return "no parseable generated client manifest"
}

func excludedSnapshotDir(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor" || name == "testdata"
}
