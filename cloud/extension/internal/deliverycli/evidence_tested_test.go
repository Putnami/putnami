package deliverycli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

// evidenceHistory is a repository whose change was merged onto a base that
// moved: fork is where the branch left main, head is the branch tip, baseTip
// is main after the fork, and merge is the merge of head onto baseTip, the
// commit a GitHub pull request job checks out at refs/pull/<n>/merge.
type evidenceHistory struct {
	dir, fork, head, baseTip, merge string
}

func testGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{
		"-c", "user.name=Putnami Test", "-c", "user.email=test@putnami.invalid",
		"-c", "commit.gpgsign=false", "-c", "init.defaultBranch=main",
	}, args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func newEvidenceHistory(t *testing.T) evidenceHistory {
	t.Helper()
	dir := t.TempDir()
	commit := func(file, content, message string) string {
		if err := os.WriteFile(filepath.Join(dir, file), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		testGit(t, dir, "add", file)
		testGit(t, dir, "commit", "-q", "-m", message)
		return testGit(t, dir, "rev-parse", "HEAD")
	}
	testGit(t, dir, "init", "-q")
	h := evidenceHistory{dir: dir}
	h.fork = commit("pricing.txt", "price 10\n", "pricing")
	testGit(t, dir, "checkout", "-q", "-b", "feature")
	h.head = commit("pricing.txt", "price 12\n", "raise the price")
	testGit(t, dir, "checkout", "-q", "main")
	h.baseTip = commit("readme.txt", "shop\n", "readme")
	testGit(t, dir, "merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	h.merge = testGit(t, dir, "rev-parse", "HEAD")
	return h
}

func (h evidenceHistory) tree(t *testing.T, commit string) string {
	t.Helper()
	return testGit(t, h.dir, "rev-parse", commit+"^{tree}")
}

// evidenceDocAt writes the fixture with its subject moved to sha and base.
func evidenceDocAt(t *testing.T, fixture, sha, base string) string {
	t.Helper()
	data, err := os.ReadFile(evidenceFixture(fixture))
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	subject := doc["subject"].(map[string]any)
	subject["sha"] = sha
	subject["baseSha"] = base
	return writeEvidenceDoc(t, doc)
}

func evidenceJobEnv(dir string, vars ...string) map[string]string {
	env := evidenceEnv()
	env[evidenceInvocationDirEnv] = dir
	for i := 0; i+1 < len(vars); i += 2 {
		env[vars[i]] = vars[i+1]
	}
	return env
}

// operationalOf returns the operational member of the run the command opened.
func operationalOf(t *testing.T, fake *evidenceFakeServer) map[string]any {
	t.Helper()
	if len(fake.requests) == 0 || fake.requests[0].Path != recordsIngestPath {
		t.Fatalf("requests = %v, want the run first", fake.paths())
	}
	operational, _ := fake.requests[0].Body["operational"].(map[string]any)
	return operational
}

func wantOperational(t *testing.T, got map[string]any, want map[string]string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("operational = %v, want %v", got, want)
	}
	for key, value := range want {
		if got[key] != value {
			t.Fatalf("operational %s = %v, want %s (operational = %v)", key, got[key], value, got)
		}
	}
}

func TestEvidenceReportSendsTheMergeTreeOfAGitHubPullRequest(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-tested-tree", "a-github-pull-request-sends-the-merge-tree")
	h := newEvidenceHistory(t)

	t.Run("the job tested the merge ref", func(t *testing.T) {
		fake := &evidenceFakeServer{}
		env := evidenceJobEnv(h.dir, "GITHUB_REF", "refs/pull/42/merge", "GITHUB_SHA", h.merge)
		_, stderr, err := runEvidenceReportLogged(t, fake, env, "--file", evidenceDocAt(t, "turbo-pull-request.json", h.merge, h.baseTip))
		if err != nil {
			t.Fatalf("evidence-report: %v", err)
		}
		wantOperational(t, operationalOf(t, fake), map[string]string{
			"testedTree":        h.tree(t, h.merge),
			"baselineMergeBase": h.baseTip,
		})
		if stderr != "" {
			t.Fatalf("stderr = %q, want no warning", stderr)
		}
	})

	t.Run("the job checked out the head of the pull request", func(t *testing.T) {
		fake := &evidenceFakeServer{}
		env := evidenceJobEnv(h.dir, "GITHUB_REF", "refs/pull/42/merge", "GITHUB_SHA", h.merge)
		if _, err := runEvidenceReport(t, fake, env, "--file", evidenceDocAt(t, "turbo-pull-request.json", h.head, h.baseTip)); err != nil {
			t.Fatalf("evidence-report: %v", err)
		}
		wantOperational(t, operationalOf(t, fake), map[string]string{
			"testedHeadTree":    h.tree(t, h.head),
			"baselineMergeBase": h.fork,
		})
	})

	t.Run("a push to a branch", func(t *testing.T) {
		fake := &evidenceFakeServer{}
		env := evidenceJobEnv(h.dir, "GITHUB_REF", "refs/heads/main", "GITHUB_SHA", h.merge)
		if _, err := runEvidenceReport(t, fake, env, "--file", evidenceDocAt(t, "turbo-pull-request.json", h.merge, h.baseTip)); err != nil {
			t.Fatalf("evidence-report: %v", err)
		}
		wantOperational(t, operationalOf(t, fake), map[string]string{
			"testedHeadTree":    h.tree(t, h.merge),
			"baselineMergeBase": h.baseTip,
		})
	})
}

func TestEvidenceReportSendsTheHeadTreeAndMergeBaseOfAGitLabMergeRequest(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-tested-tree", "a-gitlab-merge-request-sends-the-head-tree-and-merge-base")
	h := newEvidenceHistory(t)

	t.Run("a merge request pipeline", func(t *testing.T) {
		fake := &evidenceFakeServer{}
		env := evidenceJobEnv(h.dir, "CI_MERGE_REQUEST_EVENT_TYPE", "detached", "CI_COMMIT_SHA", h.head,
			"CI_MERGE_REQUEST_DIFF_BASE_SHA", h.fork)
		stdout, err := runEvidenceReport(t, fake, env, "--file", evidenceDocAt(t, "symfony-merge-request.json", h.head, h.fork), "--output=json")
		if err != nil {
			t.Fatalf("evidence-report: %v", err)
		}
		want := map[string]string{"testedHeadTree": h.tree(t, h.head), "baselineMergeBase": h.fork}
		wantOperational(t, operationalOf(t, fake), want)
		if !strings.Contains(stdout, `"operational"`) || !strings.Contains(stdout, want["testedHeadTree"]) {
			t.Fatalf("stdout = %q, want the operational members the run carries", stdout)
		}
	})

	t.Run("a merged results pipeline", func(t *testing.T) {
		fake := &evidenceFakeServer{}
		env := evidenceJobEnv(h.dir, "CI_MERGE_REQUEST_EVENT_TYPE", "merged_result", "CI_COMMIT_SHA", h.merge,
			"CI_MERGE_REQUEST_DIFF_BASE_SHA", h.fork)
		if _, err := runEvidenceReport(t, fake, env, "--file", evidenceDocAt(t, "symfony-merge-request.json", h.merge, h.fork)); err != nil {
			t.Fatalf("evidence-report: %v", err)
		}
		wantOperational(t, operationalOf(t, fake), map[string]string{
			"testedTree":        h.tree(t, h.merge),
			"baselineMergeBase": h.fork,
		})
	})
}

func TestEvidenceReportLeavesOutWhatAShallowCloneCannotCompute(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-tested-tree", "a-shallow-clone-leaves-out-what-it-cannot-compute")
	h := newEvidenceHistory(t)
	shallow := filepath.Join(t.TempDir(), "shallow")
	testGit(t, h.dir, "clone", "-q", "--depth", "1", "--branch", "feature", "file://"+h.dir, shallow)

	t.Run("the merge base is beyond the shallow clone", func(t *testing.T) {
		fake := &evidenceFakeServer{}
		env := evidenceJobEnv(shallow, "CI_MERGE_REQUEST_EVENT_TYPE", "detached", "CI_COMMIT_SHA", h.head)
		stdout, stderr, err := runEvidenceReportLogged(t, fake, env, "--file", evidenceDocAt(t, "symfony-merge-request.json", h.head, h.baseTip))
		if err != nil {
			t.Fatalf("evidence-report: %v, want the upload to go on", err)
		}
		wantOperational(t, operationalOf(t, fake), map[string]string{"testedHeadTree": h.tree(t, h.head)})
		if !strings.Contains(stderr, "sends no baselineMergeBase") || !strings.Contains(stderr, "the clone is shallow") {
			t.Fatalf("stderr = %q, want the left-out merge base and why", stderr)
		}
		if len(fake.requests) != 3 || !strings.Contains(stdout, "run run-1") {
			t.Fatalf("requests = %v, stdout = %q: want the run and both batches", fake.paths(), stdout)
		}
	})

	t.Run("the commit is not in the checkout", func(t *testing.T) {
		fake := &evidenceFakeServer{}
		env := evidenceJobEnv(shallow, "GITHUB_REF", "refs/pull/42/merge", "GITHUB_SHA", h.merge)
		_, stderr, err := runEvidenceReportLogged(t, fake, env, "--file", evidenceDocAt(t, "turbo-pull-request.json", h.merge, h.baseTip))
		if err != nil {
			t.Fatalf("evidence-report: %v, want the upload to go on", err)
		}
		if operational, ok := fake.requests[0].Body["operational"]; ok {
			t.Fatalf("operational = %v, want none", operational)
		}
		if !strings.Contains(stderr, "sends no testedTree: the commit "+shortSHA(h.merge)+" is not in the Git checkout of "+shallow) ||
			!strings.Contains(stderr, "sends no baselineMergeBase") {
			t.Fatalf("stderr = %q, want both left-out members and why", stderr)
		}
	})
}

func TestEvidenceReportOpensTheRunWithoutOperationalWhenDeliveryRefusesIt(t *testing.T) {
	spectest.Proves(t, evidenceFeature, "sends-the-tested-tree", "a-shallow-clone-leaves-out-what-it-cannot-compute")
	h := newEvidenceHistory(t)
	fake := &evidenceFakeServer{refuseOperational: true}
	env := evidenceJobEnv(h.dir, "CI_MERGE_REQUEST_EVENT_TYPE", "detached", "CI_COMMIT_SHA", h.head)
	stdout, stderr, err := runEvidenceReportLogged(t, fake, env, "--file", evidenceDocAt(t, "symfony-merge-request.json", h.head, h.fork), "--output=json")
	if err != nil {
		t.Fatalf("evidence-report: %v, want the upload to go on without operational", err)
	}
	want := []string{"POST " + recordsIngestPath, "POST " + recordsIngestPath, "POST " + recordsReportsIngestPath, "POST " + recordsReportsIngestPath}
	if got := fake.paths(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("requests = %v, want the refused run, the run again and both batches", got)
	}
	if _, ok := fake.requests[1].Body["operational"]; ok || fake.requests[1].Body["submissionId"] != fake.requests[0].Body["submissionId"] {
		t.Fatalf("second run = %v, want the same submission without operational", fake.requests[1].Body)
	}
	if !strings.Contains(stderr, "sends no operational") || !strings.Contains(stderr, "operational metrics require a run credential") {
		t.Fatalf("stderr = %q, want the refusal and its reason", stderr)
	}
	if !strings.Contains(stdout, `"operational_left_out"`) || strings.Contains(stdout, `"operational": {`) {
		t.Fatalf("stdout = %q, want operational reported as left out", stdout)
	}
}

func TestEvidenceGitIgnoresALocatingVariable(t *testing.T) {
	h := newEvidenceHistory(t)
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	tree, err := evidenceGit(h.dir, "rev-parse", h.head+"^{tree}")
	if err != nil || tree == "" {
		t.Fatalf("evidenceGit = %q, %v, want the tree of the checkout in its directory", tree, err)
	}
}

func TestPullRequestMergeRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"refs/pull/42/merge": true,
		"refs/pull/42/head":  false,
		"refs/pull//merge":   false,
		"refs/pull/4x/merge": false,
		"refs/heads/main":    false,
		"":                   false,
	} {
		if got := pullRequestMergeRef(ref); got != want {
			t.Errorf("pullRequestMergeRef(%q) = %v, want %v", ref, got, want)
		}
	}
}
