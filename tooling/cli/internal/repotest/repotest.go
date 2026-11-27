// Package repotest marks the tests of @putnami/cli that read this repository's
// own files: its manifests, decisions, templates, agent content, extension
// sources and contracts. The CLI's own suite keys only on the CLI's sources and
// fixtures, so such a test skips there; @putnami/cli-repository
// (tooling/cli-repository) runs it, keyed on the repository files it reads.
//
// It is test-only: nothing here may be imported by a production (non-_test.go)
// file, which keeps "testing" out of the shipped CLI binary's dependency graph.
package repotest

import (
	"os"
	"testing"
)

// Env is set to "1" by @putnami/cli-repository when it runs the marked tests.
const Env = "PUTNAMI_CLI_REPOSITORY_TESTS"

// Call is the statement that marks a test; @putnami/cli-repository finds the
// marked tests by it, so it is the first statement of the test function.
const Call = "repotest.Require"

// Require skips t unless @putnami/cli-repository runs it. Call it first, before
// spectest.Proves or anything that reads the repository: a skipped test then
// reports no proof and reads nothing the CLI's suite does not key on.
func Require(t testing.TB) {
	t.Helper()
	if os.Getenv(Env) != "1" {
		t.Skip("reads this repository's files: @putnami/cli-repository runs it")
	}
}

// Reads fails t unless @putnami/cli-repository runs it. Every helper that
// finds this repository's root calls it first, so a test that reaches the
// repository without starting with Require fails in the CLI's suite, whose
// key does not hold the files it would read, instead of passing on them.
func Reads(t testing.TB) {
	t.Helper()
	if os.Getenv(Env) != "1" {
		t.Fatalf("this test reads this repository's files, which the CLI's suite does not key on: " +
			"make repotest.Require(t) the first statement of the test function, so @putnami/cli-repository runs it")
	}
}
