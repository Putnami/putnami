package pkg

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"go.putnami.dev/go/extension/internal/platform"
)

// checkWindowsArchive applies to the stage of a Windows archive the two rules a
// Windows CLI applies when it installs one, so a stage that breaks either fails
// the package job instead of every Windows install. It returns executables with
// the runtime's Windows name added when the project ships one without building
// it. The file is named for the target, not the host: the packager builds a
// Windows archive on any OS, so this file carries no build constraint.
func checkWindowsArchive(run *platformPackaging, stageDir string, p platform.Target, executables []string) ([]string, error) {
	if !run.hasBinary && run.isExtension {
		runtimeExecutable, err := windowsRuntimeWithoutBinary(stageDir, p)
		if err != nil {
			return nil, err
		}
		if runtimeExecutable != "" {
			// A copy: the shared base stage's list is read by every worker.
			executables = append(slices.Clone(executables), runtimeExecutable)
		}
	}
	if err := refuseWindowsArchiveLinks(stageDir, p); err != nil {
		return nil, err
	}
	return executables, nil
}

// windowsRuntimeWithoutBinary checks the runtime of a Windows archive whose
// project builds no Go binary. Packaging then stages no Windows build of the
// runtime, so the archive carries one only when the project ships it under its
// Windows name (pkgmeta.ExecutableName, through
// validateStagedRuntimeExecutable). A Windows CLI runs that name and nothing
// else. It returns the runtime's archive path, or "" when the manifest
// declares no runtime.
func windowsRuntimeWithoutBinary(stageDir string, p platform.Target) (string, error) {
	runtimeExecutable, err := validateStagedRuntimeExecutable(stageDir, p.GOOS)
	if err != nil {
		return "", fmt.Errorf(
			"%s archive: the project builds no Go binary, so packaging stages no Windows build of its runtime: %w; "+
				"build the runtime from a Go main package, or leave windows out of the platforms option",
			p.Suffix, err)
	}
	return runtimeExecutable, nil
}

// refuseWindowsArchiveLinks fails when the stage of a Windows archive holds a
// symbolic link, and names the first one in walk order. A Windows CLI creates
// no link from an archive: it refuses the entry, and with it the install.
//
// The link is refused, not replaced by a copy of its target. A copy would give
// the Windows archive other entries than the Unix archives of the same stage,
// could copy files from outside the stage, and never ends on a link to an
// ancestor directory.
func refuseWindowsArchiveLinks(stageDir string, p platform.Target) error {
	rel, target, err := firstStagedLink(stageDir)
	if err != nil || rel == "" {
		return err
	}
	return fmt.Errorf(
		"%s archive entry %s is a symbolic link to %s: a Windows CLI creates no link from an archive, "+
			"so the project must ship a regular file or a directory at that path",
		p.Suffix, rel, target)
}

// firstStagedLink returns the slash-separated path, relative to stageDir, and
// the target of the first symbolic link in walk order, or "" when the stage
// holds none. The walk never follows a link.
func firstStagedLink(stageDir string) (rel, target string, err error) {
	err = filepath.WalkDir(stageDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink == 0 {
			return nil
		}
		relPath, err := filepath.Rel(stageDir, path)
		if err != nil {
			return err
		}
		if target, err = os.Readlink(path); err != nil {
			return err
		}
		rel = filepath.ToSlash(relPath)
		return fs.SkipAll
	})
	return rel, target, err
}
