package versioncmd

import (
	"slices"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/git"
)

// A generated client commits no generated code, so its line moves on its
// provider's contract: a commit that changes only that contract is one of the
// line's commits, and a provider commit that leaves the contract alone is not.
func TestAContractOnlyCommitAdvancesTheClientsLine(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/version-from-git", "a-client-line-follows-its-contract",
		"a-contract-only-commit-advances-the-clients-line")
	dir := versionLineRepo(t)
	writeRepoFile(t, dir, "typescript/"+wsproto.ConfigFilename,
		`{"line": {"tag": "ts/v{version}"}, "includes": ["web", "client"]}`)
	writeRepoFile(t, dir, "typescript/client/"+wsproto.ConfigFilename,
		`{"name": "@putnami/cli-client", "generatedFrom": {"service": "cli.api", "language": "ts"}}`)
	writeRepoFile(t, dir, "tooling/cli/schema/openapi.json", cliContract(`{}`))
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-m", "feat(client): the cli client")
	gitInRepo(t, dir, "tag", "-a", "ts/v1.2.0", "-m", "release")

	writeRepoFile(t, dir, "tooling/cli/main.go", "package main\n")
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-m", "fix(cli): a provider change that leaves its contract alone")
	writeRepoFile(t, dir, "tooling/cli/schema/openapi.json", cliContract(`{"/items":{}}`))
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-m", "feat(cli): list the items")

	specs, err := LineSpecs(dir)
	if err != nil {
		t.Fatalf("LineSpecs: %v", err)
	}
	lines, err := SelectLines(specs, "typescript")
	if err != nil {
		t.Fatalf("SelectLines: %v", err)
	}
	if want := []string{"typescript", "tooling/cli/schema/openapi.json"}; !slices.Equal(lines[0].Pathspecs, want) {
		t.Fatalf("pathspecs = %v, want %v", lines[0].Pathspecs, want)
	}
	head, err := git.ReadHead(dir, nil)
	if err != nil {
		t.Fatalf("ReadHead: %v", err)
	}
	report, err := ReportLine(dir, head, lines[0])
	if err != nil {
		t.Fatalf("ReportLine: %v", err)
	}
	if report.Release == nil || report.Release.Tag != "ts/v1.3.0" {
		t.Fatalf("proposed release = %+v, want ts/v1.3.0", report.Release)
	}
	subjects := make([]string, 0, len(report.Commits))
	for _, commit := range report.Commits {
		subjects = append(subjects, commit.Subject)
	}
	if want := []string{"feat(cli): list the items"}; !slices.Equal(subjects, want) {
		t.Fatalf("line commits = %v, want %v", subjects, want)
	}
}

// cliContract is a committed contract that declares the cli.api service, with
// the given paths.
func cliContract(paths string) string {
	return `{"openapi":"3.1.0","x-putnami-client":{"protocolVersion":1,` +
		`"service":{"id":"cli.api","audience":"api://cli.api"}},"paths":` + paths + `}`
}
