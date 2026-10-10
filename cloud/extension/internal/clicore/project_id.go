package clicore

import "strings"

// ProjectIDFromPath returns the project ID the framework derives from a
// project's workspace-relative path: "/" followed by the path without its
// grouping folders, "/" for the workspace root. A grouping folder is a path
// segment of more than two characters that starts with "(" and ends with ")",
// such as "(web)"; it stays in the physical path and never appears in the ID.
// The release-set plan's ProjectID uses this ID, and a member's project is the
// same value without its leading slash.
//
// It copies the framework's rule, ProjectIDFromPath in
// tooling/cli-model/workspace/identity.go, which no published protocol module
// exports. relativePath uses "/" separators; "" and "." name
// the workspace root.
func ProjectIDFromPath(relativePath string) string {
	segments := make([]string, 0, strings.Count(relativePath, "/")+1)
	for _, segment := range strings.Split(relativePath, "/") {
		if segment == "" || segment == "." || isGroupingFolder(segment) {
			continue
		}
		segments = append(segments, segment)
	}
	return "/" + strings.Join(segments, "/")
}

func isGroupingFolder(segment string) bool {
	return len(segment) > 2 && segment[0] == '(' && segment[len(segment)-1] == ')'
}
