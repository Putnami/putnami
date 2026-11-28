package cachekey

import (
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/extension"
)

// A directory digest folds a file's executable bit and no other permission
// bit: the umask of the machine that wrote it decides those, and a file
// output folds no mode at all.
func TestArtifactDigest_ADirectoryFoldsTheExecutableBitAlone(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "the-same-artifact-bytes-keep-the-consumer-key")
	tree := func(mode uint32) string {
		return ArtifactDigest(extension.OutputKindDirectory, []ArtifactFile{
			{Path: "a.js", Mode: 0o644, Digest: "sha256:aa"},
			{Path: "bin/run", Mode: mode, Digest: "sha256:bb"},
		})
	}
	base := tree(0o644)
	for _, mode := range []uint32{0o600, 0o664, 0o666, 0o444} {
		if got := tree(mode); got != base {
			t.Errorf("mode %o moved the digest of a file that is not executable: %s -> %s", mode, base, got)
		}
	}
	executable := tree(0o755)
	if executable == base {
		t.Fatal("the executable bit kept the digest")
	}
	for _, mode := range []uint32{0o700, 0o775, 0o744} {
		if got := tree(mode); got != executable {
			t.Errorf("mode %o of an executable file moved the digest: %s -> %s", mode, executable, got)
		}
	}
}

// The digest of the artifact a port names leaves out the subpaths the port
// excludes, and nothing else: a file at an excluded path or under it moves no
// digest, a sibling whose name only starts with it does, and a port that
// excludes nothing, or a file artifact, digests every file.
func TestPortArtifactDigest_LeavesTheExcludedSubpathsOut(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/job-planning-execution", "a-key-folds-the-artifact-it-binds", "the-same-artifact-bytes-keep-the-consumer-key")
	task := extension.TaskDefinition{
		Outputs: map[string]extension.TaskOutputPort{
			"schema": {Kind: extension.OutputKindDirectory, ArtifactExcludes: []string{"feature-evidence"}},
			"plain":  {Kind: extension.OutputKindDirectory},
		},
	}
	contract := ArtifactFile{Path: "openapi.json", Mode: 0o644, Digest: "sha256:aa"}
	evidence := func(digest string) []ArtifactFile {
		return []ArtifactFile{contract, {Path: "feature-evidence/go-framework.json", Mode: 0o644, Digest: digest}}
	}
	want := ArtifactDigest(extension.OutputKindDirectory, []ArtifactFile{contract})
	for _, digest := range []string{"sha256:binding-1", "sha256:binding-2"} {
		if got := PortArtifactDigest(task, "schema", extension.OutputKindDirectory, evidence(digest)); got != want {
			t.Fatalf("the evidence %s moved the schema digest: %s, want %s", digest, got, want)
		}
	}
	if got := PortArtifactDigest(task, "schema", extension.OutputKindDirectory, append(evidence("x"),
		ArtifactFile{Path: "feature-evidence", Mode: 0o644, Digest: "sha256:file"})); got != want {
		t.Fatalf("a file at the excluded path moved the digest: %s, want %s", got, want)
	}
	sibling := []ArtifactFile{contract, {Path: "feature-evidence-other/a.json", Mode: 0o644, Digest: "sha256:cc"}}
	if PortArtifactDigest(task, "schema", extension.OutputKindDirectory, sibling) == want {
		t.Fatal("a sibling whose name starts with the excluded path was left out")
	}
	if PortArtifactDigest(task, "plain", extension.OutputKindDirectory, evidence("sha256:binding-1")) ==
		PortArtifactDigest(task, "plain", extension.OutputKindDirectory, evidence("sha256:binding-2")) {
		t.Fatal("a port that excludes nothing left a file out")
	}
	if got := PortArtifactDigest(task, "schema", extension.OutputKindFile, []ArtifactFile{contract}); got != ArtifactDigest(extension.OutputKindFile, []ArtifactFile{contract}) {
		t.Fatalf("a file artifact = %s", got)
	}
}
