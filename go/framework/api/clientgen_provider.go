package api

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// workspaceConfigFile marks the root of a Putnami workspace.
const workspaceConfigFile = "putnami.workspace.json"

// providerProjectID returns the workspace project id of the provider at
// projectRoot, the value a generated client declares as
// featureAuthority.provider. The id is the provider's path below the nearest
// directory that holds a putnami.workspace.json, with a leading slash and
// without the transparent group folders, such as "(apps)", that the workspace
// leaves out of every id.
//
// A provider outside any workspace, or at the workspace root, has no id to
// name, so generation fails rather than write a client project that no
// feature-coverage step can resolve.
func providerProjectID(projectRoot string) (string, error) {
	provider, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	for dir := provider; ; {
		_, statErr := os.Stat(filepath.Join(dir, workspaceConfigFile))
		if statErr == nil {
			return projectIDBelow(dir, provider)
		}
		if !errors.Is(statErr, fs.ErrNotExist) {
			return "", statErr
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("clientgen: no %s above %s, so the generated client cannot name its provider", workspaceConfigFile, provider)
		}
		dir = parent
	}
}

func projectIDBelow(workspaceRoot, provider string) (string, error) {
	rel, err := filepath.Rel(workspaceRoot, provider)
	if err != nil {
		return "", err
	}
	segments := make([]string, 0)
	for _, segment := range strings.Split(filepath.ToSlash(rel), "/") {
		if segment == "." || len(segment) > 2 && segment[0] == '(' && segment[len(segment)-1] == ')' {
			continue
		}
		segments = append(segments, segment)
	}
	if len(segments) == 0 {
		return "", fmt.Errorf("clientgen: the provider %s is the workspace root, which has no project id for a generated client to name", provider)
	}
	return "/" + strings.Join(segments, "/"), nil
}
