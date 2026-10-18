package language

import "strings"

// repositoryEnvironment removes the variables that point Git at another
// repository or configuration, so a scan reads the repository at Root. It
// matches the core's helper of the same purpose.
func repositoryEnvironment(environ []string) []string {
	cleaned := make([]string, 0, len(environ))
	for _, entry := range environ {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "GIT_CONFIG") {
			continue
		}
		switch name {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR", "GIT_OBJECT_DIRECTORY",
			"GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_PREFIX", "GIT_CEILING_DIRECTORIES",
			"GIT_DISCOVERY_ACROSS_FILESYSTEM", "GIT_NAMESPACE":
			continue
		}
		cleaned = append(cleaned, entry)
	}
	return cleaned
}
