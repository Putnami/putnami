package distributioncli

import (
	"fmt"
	"os"
	"testing"
)

// gitRepositoryEnv names the variables that point git at a repository other
// than the one in its working directory.
var gitRepositoryEnv = []string{
	"GIT_DIR", "GIT_WORK_TREE", "GIT_IMPLICIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
	"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_SHALLOW_FILE", "GIT_GRAFT_FILE",
	"GIT_NO_REPLACE_OBJECTS", "GIT_REPLACE_REF_BASE", "GIT_PREFIX", "GIT_NAMESPACE",
	"GIT_CONFIG", "GIT_CONFIG_PARAMETERS", "GIT_CONFIG_COUNT",
	"GIT_CEILING_DIRECTORIES", "GIT_DISCOVERY_ACROSS_FILESYSTEM",
}

// TestMain unsets gitRepositoryEnv for the package, so every git process a
// test starts reads the repository of its working directory.
func TestMain(m *testing.M) {
	for _, key := range gitRepositoryEnv {
		if err := os.Unsetenv(key); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}
