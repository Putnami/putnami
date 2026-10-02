package versioncmd

import (
	"context"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	supportproto "go.putnami.dev/protocol/support"
	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
)

// The bump reads the support status of the projects a commit touches inside
// its line: a commit that touches no project the catalog lists as stable
// advances a patch at most, and a workspace with no catalog reads every commit
// as stable.
func TestVersionTagBumpFollowsTheSupportCatalog(t *testing.T) {
	spectest.Proves(t, "cli/version-from-git", "conventional-advance", "bump-follows-the-support-catalog")
	for _, testCase := range []struct {
		name, last, subject string
		files               []string
		noCatalog           bool
		want                string
	}{
		{name: "breaking in an experimental project is a patch", last: "0.4.0",
			subject: "feat(lab)!: drop the flag", files: []string{"typescript/lab/src.ts"}, want: "ts/v0.4.1"},
		{name: "breaking that also touches a stable project is a minor before 1.0", last: "0.4.0",
			subject: "feat(web)!: drop the flag", files: []string{"typescript/lab/src.ts", "typescript/web/src.ts"}, want: "ts/v0.5.0"},
		{name: "breaking that also touches a stable project is a major from 1.0", last: "1.2.0",
			subject: "feat(web)!: drop the flag", files: []string{"typescript/lab/src.ts", "typescript/web/src.ts"}, want: "ts/v2.0.0"},
		{name: "breaking in an experimental project is a patch from 1.0", last: "1.2.0",
			subject: "feat(lab)!: drop the flag", files: []string{"typescript/lab/src.ts"}, want: "ts/v1.2.1"},
		{name: "feat in an experimental project is a patch from 1.0", last: "1.2.0",
			subject: "feat(lab): add a flag", files: []string{"typescript/lab/src.ts"}, want: "ts/v1.2.1"},
		{name: "feat in a stable project is a minor from 1.0", last: "1.2.0",
			subject: "feat(web): add a flag", files: []string{"typescript/web/src.ts"}, want: "ts/v1.3.0"},
		{name: "a file no project owns promises nothing", last: "0.4.0",
			subject: "feat(lab)!: drop the flag", files: []string{"typescript/lab/src.ts", "typescript/NOTES.md"}, want: "ts/v0.4.1"},
		{name: "a project the catalog does not list promises nothing", last: "0.4.0",
			subject: "feat(kit)!: drop the flag", files: []string{"typescript/kit/src.ts"}, want: "ts/v0.4.1"},
		{name: "a stable file outside the line does not count", last: "0.4.0",
			subject: "feat!: drop the flag", files: []string{"typescript/lab/src.ts", "tooling/cli/main.go"}, want: "ts/v0.4.1"},
		{name: "without a catalog every commit is stable", last: "0.4.0", noCatalog: true,
			subject: "feat(lab)!: drop the flag", files: []string{"typescript/lab/src.ts"}, want: "ts/v0.5.0"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			dir := supportCatalogRepo(t, !testCase.noCatalog)
			gitInRepo(t, dir, "tag", "-a", "ts/v"+testCase.last, "-m", "release")
			for _, file := range testCase.files {
				writeRepoFile(t, dir, file, "export const changed = true\n")
			}
			gitInRepo(t, dir, "add", "-A")
			gitInRepo(t, dir, "commit", "-m", testCase.subject)

			out, err := sharedtest.CaptureStdout(t, func() error {
				return VersionTag(context.Background(), dir, []string{"--scope", "typescript", "--dry-run"})
			})
			if err != nil {
				t.Fatalf("VersionTag --dry-run: %v", err)
			}
			if !strings.Contains(out, testCase.want) {
				t.Fatalf("output = %q, want %s", out, testCase.want)
			}
		})
	}
}

// A release refuses a catalog it cannot read rather than reading it as absent,
// which would raise the bump of every preview project without a word.
func TestVersionTagRefusesAnInvalidSupportCatalog(t *testing.T) {
	dir := supportCatalogRepo(t, false)
	writeRepoFile(t, dir, supportproto.CatalogFilename, `{"protocolVersion": 1, "entries": [{"id": "@putnami/web"}]}`)
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-m", "chore: break the catalog")

	err := VersionTag(context.Background(), dir, []string{"--scope", "typescript", "--dry-run"})
	if err == nil || !strings.Contains(err.Error(), "putnami.support.json") {
		t.Fatalf("err = %v, want a refusal naming putnami.support.json", err)
	}
}

// supportCatalogRepo is versionLineRepo with two more projects on the
// typescript line, @putnami/lab and @putnami/kit, and, when withCatalog is set,
// a catalog that lists @putnami/web as stable, @putnami/lab as experimental and
// @putnami/cli (on the tooling line) as stable, and does not list
// @putnami/kit.
func supportCatalogRepo(t *testing.T, withCatalog bool) string {
	t.Helper()
	dir := versionLineRepo(t)
	writeRepoFile(t, dir, "typescript/"+wsproto.ConfigFilename,
		`{"line": {"tag": "ts/v{version}"}, "includes": ["web", "lab", "kit"]}`)
	writeRepoFile(t, dir, "typescript/lab/"+wsproto.ConfigFilename, `{"name": "@putnami/lab"}`)
	writeRepoFile(t, dir, "typescript/kit/"+wsproto.ConfigFilename, `{"name": "@putnami/kit"}`)
	if withCatalog {
		catalog, err := supportproto.MarshalCatalog(&supportproto.Catalog{
			ProtocolVersion: 1,
			Entries: []supportproto.Entry{
				{ID: "@putnami/lab", Kind: supportproto.SubjectKindPackage, Status: supportproto.StatusExperimental},
				{ID: "@putnami/web", Kind: supportproto.SubjectKindPackage, Status: supportproto.StatusStable},
				{ID: "@putnami/cli", Kind: supportproto.SubjectKindPackage, Status: supportproto.StatusStable},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		writeRepoFile(t, dir, supportproto.CatalogFilename, string(catalog))
	}
	gitInRepo(t, dir, "add", "-A")
	gitInRepo(t, dir, "commit", "-m", "chore: the lab project")
	return dir
}
