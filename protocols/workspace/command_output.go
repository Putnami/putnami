package workspace

import (
	"path"
	"path/filepath"
	"strings"
)

// CommandOutputMarker prefixes the command segment of a command output
// directory. A command name starts with an ASCII letter or digit, so a marked
// segment never equals a project directory segment once CommandOutputRel has
// escaped the project segments that start with the marker themselves.
const CommandOutputMarker = "~"

// CommandOutputRel returns the slash-form path of a project's command output
// directory relative to the workspace root:
// .putnami/out/<project path>/~<command>.
//
// The marker keeps every project's command directories out of every other
// project's tree. Without it, project /a's `test` directory
// (.putnami/out/a/test) holds the whole output tree of project /a/test. A
// project path segment that starts with the marker gains a second one, so no
// segment of the project part ever reads as a command segment, and two
// different (project, command) pairs never give paths where one contains the
// other. The workspace root project (path "") owns .putnami/out/~<command>.
func CommandOutputRel(projectPath, command string) string {
	segments := []string{".putnami", "out"}
	if clean := CleanWorkspacePath(projectPath); clean != "" {
		for _, segment := range strings.Split(clean, "/") {
			if strings.HasPrefix(segment, CommandOutputMarker) {
				segment = CommandOutputMarker + segment
			}
			segments = append(segments, segment)
		}
	}
	segments = append(segments, CommandOutputMarker+command)
	return path.Join(segments...)
}

// CommandOutputDir returns the absolute command output directory of a project
// under workspaceRoot, in the host's path form. See CommandOutputRel.
func CommandOutputDir(workspaceRoot, projectPath, command string) string {
	return filepath.Join(workspaceRoot, filepath.FromSlash(CommandOutputRel(projectPath, command)))
}
