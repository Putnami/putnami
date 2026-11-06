package clitest

import (
	"path/filepath"
	"testing"

	"go.putnami.dev/sdk/extension/gittest"
)

func TestInitGitRepoIsTheSameOnAForeignMachine(t *testing.T) {
	gittest.AssertMachineIndependent(t, func() string {
		dir := t.TempDir()
		WriteFile(t, filepath.Join(dir, "README.md"), "# Fixture\n")
		InitGitRepo(t, dir)
		return dir
	})
}
