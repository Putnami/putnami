package documents

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"

	gitutil "go.putnami.dev/tooling/cli/internal/git"
)

// The public-cut gate deliberately lives in a test: it is release policy over
// the repository that will be copied into the history-free public root, not a
// user-facing CLI command. Its input is the tracked tree plus non-ignored new
// files from `git ls-files`, so the pre-stage candidate is covered while build
// output and ignored local credentials are never read.

const (
	// The two reviewed inventories stay in the CLI module: GOVERNANCE.md links
	// them by path, and moving the scanner is not a reason to break a
	// documented link. They are declared cache-key inputs of THIS project.
	publicCutBaselinePath      = "internal/cli/testdata/public-cut-baseline.tsv"
	publicCutAllowlistPath     = "internal/cli/testdata/public-cut-allowlist.tsv"
	publicCutBaselineRepoPath  = cliModuleDir + "/" + publicCutBaselinePath
	publicCutAllowlistRepoPath = cliModuleDir + "/" + publicCutAllowlistPath
	publicCutScannerRepoPath   = cliDocumentsProjectDir + "/public_cut_test.go"
	publicCutMaxTextBytes      = 8 << 20
)

type publicCutCategory string

const (
	publicCutPrivateRepository publicCutCategory = "private-repository"
	publicCutOrphanedHistory   publicCutCategory = "orphaned-history"
	publicCutSecret            publicCutCategory = "high-confidence-secret"
	publicCutRemovedRoot       publicCutCategory = "removed-root-path"
	publicCutForbiddenFile     publicCutCategory = "forbidden-cut-file"
	publicCutInfrastructure    publicCutCategory = "private-infrastructure"
)

var publicCutCategories = map[publicCutCategory]bool{
	publicCutPrivateRepository: true,
	publicCutOrphanedHistory:   true,
	publicCutSecret:            true,
	publicCutRemovedRoot:       true,
	publicCutForbiddenFile:     true,
	publicCutInfrastructure:    true,
}

// These were repository roots in older layouts. Language-owned directories
// such as go/framework and typescript/samples remain valid: only the first path
// component is checked.
var publicCutRemovedRoots = map[string]bool{
	"apps":       true,
	"core":       true,
	"docs":       true,
	"extensions": true,
	"framework":  true,
	"packages":   true,
	"samples":    true,
	"scripts":    true,
	"web":        true,
}

type publicCutRule struct {
	name string
	re   *regexp.Regexp
}

var publicCutHistoryRules = []publicCutRule{
	{
		name: "private-github-history-url",
		re:   regexp.MustCompile(`(?i)https?://github[.]com/Putnami/(?:putnami|putnami` + `-cloud)/(?:issues|pull)/[0-9]+(?:#[A-Za-z0-9._:-]+)?`),
	},
	{
		name: "qualified-private-history-reference",
		re:   regexp.MustCompile(`(?i)Putnami/(?:putnami|putnami` + `-cloud)#[0-9]+`),
	},
}

var publicCutPrivateRepositoryRules = []publicCutRule{
	{
		name: "private-repository-coordinate",
		// Split the private repository name so this gate does not flag its
		// own matcher when the test file becomes tracked.
		re: regexp.MustCompile(`(?i)(?:(?:https?|ssh)://|git@)?github[.]com[:/]Putnami/putnami` +
			`-cloud(?:[.]git)?(?:/[^\s\"'<>)]*)?|Putnami/putnami` +
			`-cloud(?:[.]git|/[^\s\"'<>)]*)?`),
	},
}

// Tracker references are orphaned in a history-free public root whatever the
// tracker, so these rules run on every line. When a pattern has a capture
// group, the group is the evidence; the surrounding characters only keep CLI
// flags (--pr 42), markdown anchors (#10-heading), CSS colors (#333;) and
// PKCS#1 out. The private names are split so the scanner stays self-safe.
var publicCutTrackerRules = []publicCutRule{
	{
		name: "tracker-reference-word",
		re:   regexp.MustCompile(`(?i)(?:^|[^\w-])((?:issues?|prs?|pull requests?|epics?|tickets?) ?#?[0-9]{2,6})(?:[^\w-]|$)`),
	},
	{
		name: "bare-tracker-reference",
		re:   regexp.MustCompile(`(?:^|[\s(\[,;])(#[0-9]{2,6})(?:[\s).,:\]]|$)`),
	},
	{
		name: "qualified-tracker-reference",
		re:   regexp.MustCompile(`(?:^|[^\w&])([A-Za-z][A-Za-z0-9_.-]*(?:/[A-Za-z0-9_.-]+)?#[0-9]{2,6})(?:[^\w-]|$)`),
	},
}

var publicCutPrivateNameRules = []publicCutRule{
	{
		name: "private-repository-name",
		re:   regexp.MustCompile(`(?i)(?:^|[^\w@/.-])(putnami` + `-cloud|agents` + `-memory|found` + `ry)(?:[^\w-]|$)`),
	},
}

// A pull-request ref and a JSON issue field name a tracker entry as surely as
// "#123" does, so they are orphaned in the history-free root too.
var publicCutHistoryRefRules = []publicCutRule{
	{name: "pull-request-ref", re: regexp.MustCompile(`refs/pull/[0-9]+`)},
	{name: "json-tracker-number", re: regexp.MustCompile(`(?i)"(?:issue|pr|pullRequest)"\s*:\s*[0-9]+`)},
}

// Coordinates of one private deployment: the hosts that serve its private
// modules, its non-production environments, its generated Cloud Run service
// URLs and its service accounts. Product endpoints such as an authentication
// or storage host stay legitimate, like the product name itself.
var publicCutInfrastructureRules = []publicCutRule{
	{name: "private-module-host", re: regexp.MustCompile(`(?i)\b(?:go|registry|npm)\.putnami\.cloud\b`)},
	{name: "environment-host", re: regexp.MustCompile(`(?i)\b[a-z0-9-]+(?:\.[a-z0-9-]+)*\.(?:preview|dev|staging)\.putnami\.cloud\b`)},
	{name: "cloud-run-service-url", re: regexp.MustCompile(`(?i)\b[a-z0-9-]+-[a-z0-9]{10}-[a-z]{2}\.a\.run\.app\b`)},
	{name: "deployment-service-account", re: regexp.MustCompile(`(?i)\b[a-z0-9._-]+@putnami\.iam\.gserviceaccount\.com\b`)},
}

var publicCutSecretRules = []publicCutRule{
	{name: "aws-access-key-id", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[0-9A-Z]{16}\b`)},
	{name: "github-token", re: regexp.MustCompile(`\bgh[pousr]_[A-Za-z0-9]{36,255}\b`)},
	{name: "google-api-key", re: regexp.MustCompile(`\bAIza[0-9A-Za-z_-]{35}\b`)},
	{name: "private-key-block", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`)},
	{name: "slack-token", re: regexp.MustCompile(`\bxox[baprs]-[0-9A-Za-z-]{20,}\b`)},
	{name: "stripe-live-secret", re: regexp.MustCompile(`\bsk_live_[0-9A-Za-z]{20,}\b`)},
}

type publicCutFile struct {
	path        string
	data        []byte
	binary      bool
	unscannable string
}

type publicCutFinding struct {
	category   publicCutCategory
	path       string
	line       int
	rule       string
	evidence   string
	context    string
	unwaivable bool
}

type publicCutKey struct {
	category publicCutCategory
	path     string
	rule     string
	evidence string
}

func (f publicCutFinding) key() publicCutKey {
	return publicCutKey{category: f.category, path: f.path, rule: f.rule, evidence: f.evidence}
}

func (f publicCutFinding) location() string {
	if f.line > 0 {
		return fmt.Sprintf("%s:%d", f.path, f.line)
	}
	return f.path
}

type publicCutInventoryEntry struct {
	key    publicCutKey
	count  int
	reason string
}

func TestPublicCutReleaseGate(t *testing.T) {
	t.Parallel()
	repoRoot := publicCutRepositoryRoot(t)
	files, err := publicCutTrackedFiles(repoRoot)
	if err != nil {
		t.Fatalf("enumerate the versioned public cut: %v", err)
	}
	if len(files) == 0 {
		t.Fatal("git ls-files returned no public-cut files — refusing a vacuous release gate")
	}

	baseline := readPublicCutInventory(t, filepath.Join(cliModuleRoot(t), publicCutBaselinePath), false)
	allowlist := readPublicCutInventory(t, filepath.Join(cliModuleRoot(t), publicCutAllowlistPath), true)
	findings := scanPublicCut(files)
	errors := evaluatePublicCut(findings, baseline, allowlist)
	if len(errors) == 0 {
		return
	}

	for _, gateError := range errors {
		t.Error(gateError)
	}
	if proposed := renderPublicCutBaseline(findings, allowlist); proposed != "" {
		t.Logf("reviewed baseline matching this tree (never copy blindly):\n%s", proposed)
	}
}

func TestPublicCutScannerDetectsEveryCategoryAndSorts(t *testing.T) {
	t.Parallel()
	host := "github" + ".com"
	privateRepo := "Putnami/putnami" + "-cloud"
	historyRepo := "Putnami/" + "putnami"
	secret := "AKIA" + strings.Repeat("0", 16)
	files := []publicCutFile{
		{path: "z.txt", data: []byte("clone https://" + host + "/" + privateRepo + ".git\n")},
		{path: "README.md", data: []byte("Putnami Cloud is a legitimate product name.\n")},
		{path: ".Context/export.json", data: []byte("temporary export\n")},
		{path: "Apps/api/main.go", data: []byte("package main\n")},
		{path: "notes.md", data: []byte("see https://" + host + "/" + historyRepo + "/issues/123\n")},
		{path: "credentials.txt", data: []byte("key=" + secret + "\n")},
		{path: "deploy.md", data: []byte("import \"go." + "putnami.cloud/identity\"\n")},
	}

	findings := scanPublicCut(files)
	wantCategories := map[publicCutCategory]bool{
		publicCutPrivateRepository: false,
		publicCutOrphanedHistory:   false,
		publicCutSecret:            false,
		publicCutRemovedRoot:       false,
		publicCutForbiddenFile:     false,
		publicCutInfrastructure:    false,
	}
	for _, finding := range findings {
		wantCategories[finding.category] = true
		if strings.Contains(finding.context, secret) {
			t.Errorf("secret finding leaked its matched value: %+v", finding)
		}
	}
	for category, found := range wantCategories {
		if !found {
			t.Errorf("scanner did not produce category %q: %+v", category, findings)
		}
	}
	for i := 1; i < len(findings); i++ {
		if publicCutFindingLess(findings[i], findings[i-1]) {
			t.Fatalf("findings are not stable-sorted at %d: %+v then %+v", i, findings[i-1], findings[i])
		}
	}
	for _, finding := range findings {
		if finding.path == "README.md" {
			t.Errorf("the product name Putnami Cloud was mistaken for a private repository coordinate: %+v", finding)
		}
	}
}

func TestPublicCutScannerDetectsTrackerReferencesAndPrivateNames(t *testing.T) {
	t.Parallel()
	hash := "#"
	cases := []struct {
		line string
		want string
	}{
		{"fixed in issue " + hash + "3124.", "issue " + hash + "3124"},
		{"see PR " + "3974 for context", "pr " + "3974"},
		{"the pre-" + hash + "3088 default", "pre-" + hash + "3088"},
		{"(" + hash + "3568) makes the bundle cacheable", hash + "3568"},
		{"Fixes " + hash + "12, relates to more", hash + "12"},
		{"see golang/go" + hash + "22315", "golang/go" + hash + "22315"},
		{"blocked on putnami" + hash + "2931", "putnami" + hash + "2931"},
		{"the " + "putnami" + "-cloud workspace", "putnami" + "-cloud"},
		{"cloned into " + "agents" + "-memory", "agents" + "-memory"},
		{"a consumer named " + "found" + "ry", "found" + "ry"},
	}
	for _, tc := range cases {
		findings := scanPublicCut([]publicCutFile{{path: "doc.md", data: []byte(tc.line + "\n")}})
		if len(findings) != 1 || findings[0].evidence != tc.want {
			t.Errorf("%q: findings = %+v, want one with evidence %q", tc.line, findings, tc.want)
		}
	}

	clean := []string{
		"putnami pr create --pr 42 --issue 42",
		"background: " + hash + "333;",
		"color: '" + hash + "666'",
		"see [section 10](" + hash + "10-pending-cells)",
		"an RSA PKCS" + hash + "1 v1.5 signature",
		"the &" + hash + "39; entity",
		"the @putnami/cloud extension and its @" + "putnami" + "-cloud cache segment",
		"Putnami Cloud is a product name",
		"row " + hash + "1 of the table",
	}
	for _, line := range clean {
		if findings := scanPublicCut([]publicCutFile{{path: "doc.md", data: []byte(line + "\n")}}); len(findings) != 0 {
			t.Errorf("%q: a legitimate line was flagged: %+v", line, findings)
		}
	}
}

func TestPublicCutScannerDetectsHistoryRefsAndPrivateInfrastructure(t *testing.T) {
	t.Parallel()
	cloud := "putnami" + ".cloud"
	cases := []struct {
		line     string
		category publicCutCategory
		rule     string
		want     string
	}{
		{"git fetch origin refs/" + "pull/1/head", publicCutOrphanedHistory, "pull-request-ref", "refs/" + "pull/1"},
		{`  "issue"` + `: 7,`, publicCutOrphanedHistory, "json-tracker-number", `"issue"` + `: 7`},
		{`{"pr"` + `:42}`, publicCutOrphanedHistory, "json-tracker-number", `"pr"` + `:42`},
		{"require go." + cloud + "/example v0.0.0", publicCutInfrastructure, "private-module-host", "go." + cloud},
		{"stack registry." + cloud + "/example/stack", publicCutInfrastructure, "private-module-host", "registry." + cloud},
		{"--target-url https://example.preview." + cloud, publicCutInfrastructure, "environment-host", "example.preview." + cloud},
		{"collector: https://example-service-" + "abcdefghij-ew.a.run" + ".app", publicCutInfrastructure, "cloud-run-service-url", "example-service-" + "abcdefghij-ew.a.run" + ".app"},
		{"callers: example-" + "reader@putnami.iam" + ".gserviceaccount.com", publicCutInfrastructure, "deployment-service-account", "example-" + "reader@putnami.iam" + ".gserviceaccount.com"},
	}
	for _, tc := range cases {
		findings := scanPublicCut([]publicCutFile{{path: "doc.md", data: []byte(tc.line + "\n")}})
		if len(findings) != 1 || findings[0].category != tc.category || findings[0].rule != tc.rule || findings[0].evidence != tc.want {
			t.Errorf("%q: findings = %+v, want one %s/%s with evidence %q", tc.line, findings, tc.category, tc.rule, tc.want)
		}
	}

	clean := []string{
		"discoveryUri: https://auth." + cloud + "/.well-known/openid-configuration",
		"endpoint: https://storage." + cloud,
		`"url": "https://cache.` + cloud + `"`,
		"audience: https://users-abc.a.run" + ".app",
		"runtime@proj.iam" + ".gserviceaccount.com",
		`{"issues": [], "prNumber": "draft"}`,
		"git fetch origin refs/heads/main",
	}
	for _, line := range clean {
		if findings := scanPublicCut([]publicCutFile{{path: "doc.md", data: []byte(line + "\n")}}); len(findings) != 0 {
			t.Errorf("%q: a legitimate line was flagged: %+v", line, findings)
		}
	}
}

func TestPublicCutScannerDetectsSecretsInsideBinaryWithoutLeakingBytes(t *testing.T) {
	t.Parallel()
	secret := "AKIA" + strings.Repeat("0", 16)
	data := append([]byte{0, 1, 2}, []byte("prefix "+secret+" suffix")...)
	findings := scanPublicCut([]publicCutFile{{path: "fixture.bin", data: data, binary: true}})
	if len(findings) != 1 || findings[0].category != publicCutSecret {
		t.Fatalf("binary secret findings = %+v, want one high-confidence-secret", findings)
	}
	if findings[0].context != "[binary content redacted]" || strings.Contains(findings[0].context, secret) || strings.ContainsRune(findings[0].context, 0) {
		t.Fatalf("binary secret context was not safely redacted: %q", findings[0].context)
	}
}

func TestPublicCutScannerDetectsPrivateReferencesInsideBinaryWithoutLeakingBytes(t *testing.T) {
	t.Parallel()
	host := "github" + ".com"
	history := "https://" + host + "/Putnami/" + "putnami/issues/321"
	privateRepo := "git@" + host + ":Putnami/putnami" + "-cloud.git"
	data := append([]byte{0, 1, 2}, []byte(history+"\n"+privateRepo)...)
	findings := scanPublicCut([]publicCutFile{{path: "references.bin", data: data, binary: true}})

	want := map[publicCutCategory]int{publicCutOrphanedHistory: 1, publicCutPrivateRepository: 1}
	for _, finding := range findings {
		want[finding.category]--
		if finding.context != "[binary content redacted]" || strings.Contains(finding.context, history) || strings.Contains(finding.context, privateRepo) || strings.ContainsRune(finding.context, 0) {
			t.Fatalf("binary reference context was not safely redacted: %q", finding.context)
		}
	}
	if len(findings) != 2 || want[publicCutOrphanedHistory] != 0 || want[publicCutPrivateRepository] != 0 {
		t.Fatalf("binary reference findings = %+v, want one orphaned-history and one private-repository", findings)
	}
}

func TestPublicCutScannerPathRulesIgnoreCase(t *testing.T) {
	t.Parallel()
	findings := scanPublicCut([]publicCutFile{
		{path: "Apps/api/main.go", data: []byte("package main\n")},
		{path: "nested/.CoNtExT/export.json", data: []byte("{}\n")},
		{path: "assets/.Ds_StOrE", binary: true},
	})
	byRule := make(map[string]bool)
	for _, finding := range findings {
		byRule[finding.rule] = true
	}
	for _, rule := range []string{"retired-root:apps", "private-workspace-state", "os-metadata"} {
		if !byRule[rule] {
			t.Errorf("mixed-case path did not trigger %q: %+v", rule, findings)
		}
	}
}

func TestPublicCutScannerAllowlistIsExactAndOnlyShrinks(t *testing.T) {
	t.Parallel()
	finding := publicCutFinding{
		category: publicCutPrivateRepository,
		path:     "doc/example.md",
		line:     3,
		rule:     "fixture-coordinate",
		evidence: "example.invalid/private/repository",
		context:  "intentional fixture",
	}
	entry := publicCutInventoryEntry{key: finding.key(), count: 1, reason: "Exact documentation fixture."}

	if errors := evaluatePublicCut([]publicCutFinding{finding}, nil, inventoryMap(entry)); len(errors) != 0 {
		t.Fatalf("exact allowlist entry did not cover its intentional finding: %v", errors)
	}
	if errors := evaluatePublicCut(nil, nil, inventoryMap(entry)); !publicCutErrorsContain(errors, "allowlist entry is stale") {
		t.Fatalf("unused allowlist entry did not fail: %v", errors)
	}
	if errors := evaluatePublicCut([]publicCutFinding{finding, finding}, nil, inventoryMap(entry)); !publicCutErrorsContain(errors, "allowlist is narrower") {
		t.Fatalf("additional finding hid behind an allowlist entry: %v", errors)
	}
}

func TestPublicCutScannerRejectsNewDebt(t *testing.T) {
	t.Parallel()
	oldFinding := publicCutFinding{
		category: publicCutOrphanedHistory,
		path:     "old.md",
		line:     1,
		rule:     "history",
		evidence: "old-reference",
		context:  "old",
	}
	newFinding := publicCutFinding{
		category: publicCutOrphanedHistory,
		path:     "new.md",
		line:     7,
		rule:     "history",
		evidence: "new-reference",
		context:  "new",
	}
	baseline := inventoryMap(publicCutInventoryEntry{key: oldFinding.key(), count: 1, reason: "Existing debt."})
	errors := evaluatePublicCut([]publicCutFinding{newFinding, oldFinding}, baseline, nil)
	if !publicCutErrorsContain(errors, "new debt: orphaned-history new.md:7") {
		t.Fatalf("new finding did not fail with category and exact path: %v", errors)
	}
}

func TestPublicCutScannerRequiresBaselineToShrink(t *testing.T) {
	t.Parallel()
	finding := publicCutFinding{
		category: publicCutOrphanedHistory,
		path:     "history.md",
		line:     4,
		rule:     "history",
		evidence: "reference",
		context:  "history",
	}
	baseline := inventoryMap(publicCutInventoryEntry{key: finding.key(), count: 2, reason: "Two existing references."})
	if errors := evaluatePublicCut([]publicCutFinding{finding}, baseline, nil); !publicCutErrorsContain(errors, "baseline must shrink") {
		t.Fatalf("removed debt left a stale baseline without failing: %v", errors)
	}
	baseline[finding.key()] = publicCutInventoryEntry{key: finding.key(), count: 1, reason: "One existing reference."}
	if errors := evaluatePublicCut([]publicCutFinding{finding}, baseline, nil); len(errors) != 0 {
		t.Fatalf("lowered baseline did not pass: %v", errors)
	}
}

func TestPublicCutScannerSecretsAreUnwaivable(t *testing.T) {
	t.Parallel()
	finding := publicCutFinding{
		category:   publicCutSecret,
		path:       "fixture.txt",
		line:       1,
		rule:       "synthetic-secret",
		evidence:   "sha256:invalid",
		context:    "[REDACTED]",
		unwaivable: true,
	}
	entry := publicCutInventoryEntry{key: finding.key(), count: 1, reason: "Attempted waiver."}
	errors := evaluatePublicCut([]publicCutFinding{finding}, inventoryMap(entry), inventoryMap(entry))
	if !publicCutErrorsContain(errors, "unwaivable") {
		t.Fatalf("secret was hidden by baseline/allowlist: %v", errors)
	}

	finding.path = "internal/example_test.go"
	entry = publicCutInventoryEntry{key: finding.key(), count: 1, reason: "Exact synthetic test fixture."}
	if errors := evaluatePublicCut([]publicCutFinding{finding}, nil, inventoryMap(entry)); len(errors) != 0 {
		t.Fatalf("exact synthetic secret fixture allowlist did not pass: %v", errors)
	}
}

func TestPublicCutInventoryKeepsSecretsOutOfBaselineAndNonFixtures(t *testing.T) {
	t.Parallel()
	entry := publicCutInventoryEntry{
		key: publicCutKey{
			category: publicCutSecret,
			path:     "README.md",
			rule:     "synthetic-secret",
			evidence: "sha256:invalid",
		},
		count:  1,
		reason: "Attempted secret waiver.",
	}
	line := serializePublicCutInventoryEntry(entry) + "\n"
	if _, err := parsePublicCutInventory(line, false); err == nil || !strings.Contains(err.Error(), "never baseline-eligible") {
		t.Fatalf("secret baseline entry was accepted: %v", err)
	}
	if _, err := parsePublicCutInventory(line, true); err == nil || !strings.Contains(err.Error(), "test/fixture") {
		t.Fatalf("secret allowlist entry outside an exact fixture was accepted: %v", err)
	}
}

func TestPublicCutScannerControlFilesAreSelfSafe(t *testing.T) {
	t.Parallel()
	repoRoot := publicCutRepositoryRoot(t)
	paths := []string{publicCutScannerRepoPath, publicCutBaselineRepoPath, publicCutAllowlistRepoPath}
	files := make([]publicCutFile, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(filepath.Join(repoRoot, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read scanner control file %s: %v", path, err)
		}
		files = append(files, publicCutFile{path: path, data: data})
	}
	if findings := scanPublicCut(files); len(findings) != 0 {
		t.Fatalf("scanner control files recursively triggered their own gate: %+v", findings)
	}
}

func TestPublicCutTrackedFilesIncludeNonIgnoredCandidatesAndHandleUnusualNames(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	publicCutRunGit(t, root, "init", "-q")
	tracked := []string{".gitignore", "name with spaces.txt", "name\nwith-newline.txt", "tracked-deleted.txt"}
	want := []string{".gitignore", "candidate.txt", "name with spaces.txt", "name\nwith-newline.txt"}
	if runtime.GOOS == "windows" {
		// A Windows file name cannot contain a newline; the other unusual
		// names still run.
		tracked = slices.DeleteFunc(tracked, func(name string) bool { return strings.Contains(name, "\n") })
		want = slices.DeleteFunc(want, func(name string) bool { return strings.Contains(name, "\n") })
	}
	for _, name := range tracked {
		contents := []byte("tracked\n")
		if name == ".gitignore" {
			contents = []byte("ignored.txt\n")
		}
		if err := os.WriteFile(filepath.Join(root, name), contents, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "candidate.txt"), []byte("candidate\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ignored.txt"), []byte("ignored\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	publicCutRunGit(t, root, append([]string{"add", "--"}, tracked...)...)
	if err := os.Remove(filepath.Join(root, "tracked-deleted.txt")); err != nil {
		t.Fatal(err)
	}
	sort.Strings(want)

	files, err := publicCutTrackedFiles(root)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, file := range files {
		got = append(got, file.path)
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("candidate paths = %q, want %q (non-ignored included, ignored/deleted excluded, names preserved)", got, want)
	}
}

func publicCutRepositoryRoot(t *testing.T) string {
	t.Helper()
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = projectRoot(t)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	root := strings.TrimSpace(string(out))
	if root == "" {
		t.Fatal("git returned an empty repository root")
	}
	return root
}

func publicCutTrackedFiles(root string) ([]publicCutFile, error) {
	paths, err := gitutil.CandidatePaths(root)
	if err != nil {
		return nil, err
	}

	files := make([]publicCutFile, 0, len(paths))
	for _, rel := range paths {
		full := filepath.Join(root, filepath.FromSlash(rel))
		info, statErr := os.Lstat(full)
		if statErr != nil {
			if os.IsNotExist(statErr) {
				// A tracked deletion is absent from the candidate cut even when
				// the finalizer has not staged it yet.
				continue
			}
			return nil, fmt.Errorf("candidate path %q: %w", rel, statErr)
		}
		file := publicCutFile{path: filepath.ToSlash(rel)}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, readErr := os.Readlink(full)
			if readErr != nil {
				return nil, fmt.Errorf("read candidate symlink %q: %w", rel, readErr)
			}
			file.data = []byte(target)
		case !info.Mode().IsRegular():
			file.unscannable = "tracked-non-regular-file"
		case info.Size() > publicCutMaxTextBytes:
			file.unscannable = "oversized-unscannable-file"
		default:
			data, readErr := os.ReadFile(full)
			if readErr != nil {
				return nil, fmt.Errorf("read candidate file %q: %w", rel, readErr)
			}
			file.data = data
			file.binary = bytes.IndexByte(data, 0) >= 0
		}
		files = append(files, file)
	}
	return files, nil
}

func scanPublicCut(files []publicCutFile) []publicCutFinding {
	var findings []publicCutFinding
	for _, file := range files {
		path := filepath.ToSlash(file.path)
		if !utf8.ValidString(path) || strings.ContainsAny(path, "\r\n\t") {
			findings = append(findings, publicCutFinding{
				category: publicCutForbiddenFile, path: strconv.QuoteToASCII(path),
				rule: "unsafe-path-encoding", evidence: "unsafe-path", context: "path must be portable",
				unwaivable: true,
			})
			continue
		}
		if root := strings.ToLower(strings.SplitN(path, "/", 2)[0]); publicCutRemovedRoots[root] {
			findings = append(findings, publicCutFinding{
				category: publicCutRemovedRoot, path: path, rule: "retired-root:" + root,
				evidence: root, context: "tracked file remains under retired root " + root + "/",
			})
		}
		if rule := forbiddenPublicCutPath(path); rule != "" {
			findings = append(findings, publicCutFinding{
				category: publicCutForbiddenFile, path: path, rule: rule,
				evidence: filepath.Base(path), context: "tracked file is forbidden in the public cut",
			})
		}
		if file.unscannable != "" {
			findings = append(findings, publicCutFinding{
				category: publicCutForbiddenFile, path: path, rule: file.unscannable,
				evidence: file.unscannable, context: "content could not be scanned safely",
				unwaivable: true,
			})
			continue
		}
		// These two exact control files contain debt evidence by design. Skip
		// only their recursive history/repository rules; secret rules still run.
		controlFile := path == publicCutBaselineRepoPath || path == publicCutAllowlistRepoPath
		if file.binary {
			findings = append(findings, scanPublicCutContent(path, file.data, controlFile, true)...)
			continue
		}
		findings = append(findings, scanPublicCutContent(path, file.data, controlFile, false)...)
	}
	sort.Slice(findings, func(i, j int) bool { return publicCutFindingLess(findings[i], findings[j]) })
	return findings
}

func forbiddenPublicCutPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		lowerPart := strings.ToLower(part)
		switch lowerPart {
		case ".context":
			return "private-workspace-state"
		case ".ds_store":
			return "os-metadata"
		case ".npmrc", ".pypirc", ".netrc", "id_rsa", "id_ed25519":
			return "credential-bearing-filename"
		case "sessions", "cache":
			if i > 0 && strings.EqualFold(parts[i-1], ".putnami") {
				return "putnami-local-state"
			}
		}
	}
	base := strings.ToLower(filepath.Base(path))
	if base == ".env" || (strings.HasPrefix(base, ".env.") &&
		!strings.HasSuffix(base, ".yaml") && !strings.HasSuffix(base, ".yml") &&
		!strings.HasSuffix(base, ".example") && !strings.HasSuffix(base, ".sample") &&
		!strings.HasSuffix(base, ".template")) {
		return "environment-secret-file"
	}
	for _, suffix := range []string{".dump", ".key", ".p12", ".pem", ".pfx", ".sqlite", ".tfstate"} {
		if strings.HasSuffix(base, suffix) {
			return "sensitive-file-type"
		}
	}
	return ""
}

func scanPublicCutContent(path string, data []byte, skipDebtReferences, binary bool) []publicCutFinding {
	var findings []publicCutFinding
	for index, rawLine := range bytes.Split(data, []byte{'\n'}) {
		line := string(bytes.TrimSuffix(rawLine, []byte{'\r'}))
		lineNumber := index + 1
		var historySpans [][2]int
		lower := ""
		if !skipDebtReferences {
			lower = strings.ToLower(line)
		}
		if !skipDebtReferences && strings.Contains(lower, "putnami/") {
			for _, rule := range publicCutHistoryRules {
				for _, span := range rule.re.FindAllStringIndex(line, -1) {
					historySpans = append(historySpans, [2]int{span[0], span[1]})
					match := line[span[0]:span[1]]
					findings = append(findings, publicCutFinding{
						category: publicCutOrphanedHistory, path: path, line: lineNumber, rule: rule.name,
						evidence: strings.ToLower(match), context: publicCutFindingContext(line, binary),
					})
				}
			}
			for _, rule := range publicCutPrivateRepositoryRules {
				for _, span := range rule.re.FindAllStringIndex(line, -1) {
					if publicCutSpanOverlaps(span, historySpans) {
						continue
					}
					match := line[span[0]:span[1]]
					findings = append(findings, publicCutFinding{
						category: publicCutPrivateRepository, path: path, line: lineNumber, rule: rule.name,
						evidence: strings.ToLower(match), context: publicCutFindingContext(line, binary),
					})
				}
			}
		}
		if !skipDebtReferences && publicCutHasDigitPair(line) {
			for _, span := range publicCutMatchSpans(publicCutTrackerRules, line, historySpans) {
				findings = append(findings, publicCutFinding{
					category: publicCutOrphanedHistory, path: path, line: lineNumber, rule: span.rule,
					evidence: strings.ToLower(line[span.start:span.end]), context: publicCutFindingContext(line, binary),
				})
			}
		}
		if !skipDebtReferences && publicCutHistoryRefPossible(lower) {
			for _, span := range publicCutMatchSpans(publicCutHistoryRefRules, line, historySpans) {
				findings = append(findings, publicCutFinding{
					category: publicCutOrphanedHistory, path: path, line: lineNumber, rule: span.rule,
					evidence: strings.ToLower(line[span.start:span.end]), context: publicCutFindingContext(line, binary),
				})
			}
		}
		if !skipDebtReferences && publicCutInfrastructurePossible(lower) {
			for _, span := range publicCutMatchSpans(publicCutInfrastructureRules, line, nil) {
				findings = append(findings, publicCutFinding{
					category: publicCutInfrastructure, path: path, line: lineNumber, rule: span.rule,
					evidence: strings.ToLower(line[span.start:span.end]), context: publicCutFindingContext(line, binary),
				})
			}
		}
		if !skipDebtReferences && publicCutPrivateNamePossible(lower) {
			for _, span := range publicCutMatchSpans(publicCutPrivateNameRules, line, historySpans) {
				findings = append(findings, publicCutFinding{
					category: publicCutPrivateRepository, path: path, line: lineNumber, rule: span.rule,
					evidence: strings.ToLower(line[span.start:span.end]), context: publicCutFindingContext(line, binary),
				})
			}
		}
		for _, rule := range publicCutSecretRules {
			if !publicCutSecretRulePossible(rule.name, line) {
				continue
			}
			for _, match := range rule.re.FindAllString(line, -1) {
				digest := sha256.Sum256([]byte(match))
				findings = append(findings, publicCutFinding{
					category: publicCutSecret, path: path, line: lineNumber, rule: rule.name,
					evidence: "sha256:" + hex.EncodeToString(digest[:8]), context: publicCutFindingContext(line, binary),
					unwaivable: true,
				})
			}
		}
	}
	return findings
}

// publicCutHasDigitPair reports whether line holds two consecutive ASCII
// digits, which every tracker rule needs. It keeps the tracker regexes off
// most lines.
func publicCutHasDigitPair(line string) bool {
	for index := 1; index < len(line); index++ {
		if line[index] >= '0' && line[index] <= '9' && line[index-1] >= '0' && line[index-1] <= '9' {
			return true
		}
	}
	return false
}

// publicCutPrivateNamePossible reports whether the lowercased line holds a
// private repository name, so the case-insensitive regex runs only then.
func publicCutPrivateNamePossible(lower string) bool {
	return strings.Contains(lower, "putnami"+"-cloud") || strings.Contains(lower, "agents"+"-memory") ||
		strings.Contains(lower, "found"+"ry")
}

// publicCutHistoryRefPossible reports whether the lowercased line can hold a
// pull-request ref or a JSON tracker field, so those regexes run only then.
func publicCutHistoryRefPossible(lower string) bool {
	return strings.Contains(lower, "refs/pull/") || strings.Contains(lower, `"issue"`) ||
		strings.Contains(lower, `"pr"`) || strings.Contains(lower, `"pullrequest"`)
}

// publicCutInfrastructurePossible reports whether the lowercased line can hold
// a private deployment coordinate, so those regexes run only then.
func publicCutInfrastructurePossible(lower string) bool {
	return strings.Contains(lower, ".putnami.cloud") || strings.Contains(lower, ".run.app") ||
		strings.Contains(lower, "@putnami.iam.")
}

type publicCutMatchSpan struct {
	rule       string
	start, end int
}

// publicCutMatchSpans returns each rule's evidence span (the first capture
// group, or the whole match), skipping spans an earlier history rule already
// reported and spans a previous rule of the same list claimed.
func publicCutMatchSpans(rules []publicCutRule, line string, claimed [][2]int) []publicCutMatchSpan {
	var spans []publicCutMatchSpan
	taken := append([][2]int(nil), claimed...)
	for _, rule := range rules {
		for _, match := range rule.re.FindAllStringSubmatchIndex(line, -1) {
			start, end := match[0], match[1]
			if len(match) >= 4 && match[2] >= 0 {
				start, end = match[2], match[3]
			}
			if publicCutSpanOverlaps([]int{start, end}, taken) {
				continue
			}
			taken = append(taken, [2]int{start, end})
			spans = append(spans, publicCutMatchSpan{rule: rule.name, start: start, end: end})
		}
	}
	return spans
}

func publicCutFindingContext(line string, binary bool) string {
	if binary {
		return "[binary content redacted]"
	}
	return safePublicCutContext(redactPublicCutSecrets(line))
}

func publicCutSecretRulePossible(rule, line string) bool {
	switch rule {
	case "aws-access-key-id":
		return strings.Contains(line, "AKIA") || strings.Contains(line, "ASIA")
	case "github-token":
		return strings.Contains(line, "gh") && strings.Contains(line, "_")
	case "google-api-key":
		return strings.Contains(line, "AIza")
	case "private-key-block":
		return strings.Contains(line, "PRIVATE KEY")
	case "slack-token":
		return strings.Contains(line, "xox")
	case "stripe-live-secret":
		return strings.Contains(line, "sk_live_")
	default:
		return true
	}
}

func redactPublicCutSecrets(line string) string {
	redacted := line
	for _, rule := range publicCutSecretRules {
		redacted = rule.re.ReplaceAllString(redacted, "[REDACTED]")
	}
	return redacted
}

func safePublicCutContext(line string) string {
	line = strings.Join(strings.Fields(line), " ")
	runes := []rune(line)
	if len(runes) > 160 {
		line = string(runes[:157]) + "..."
	}
	return line
}

func publicCutSpanOverlaps(span []int, others [][2]int) bool {
	for _, other := range others {
		if span[0] < other[1] && other[0] < span[1] {
			return true
		}
	}
	return false
}

func publicCutFindingLess(a, b publicCutFinding) bool {
	left := []string{string(a.category), a.path, a.rule, a.evidence, fmt.Sprintf("%09d", a.line)}
	right := []string{string(b.category), b.path, b.rule, b.evidence, fmt.Sprintf("%09d", b.line)}
	return strings.Join(left, "\x00") < strings.Join(right, "\x00")
}

func readPublicCutInventory(t *testing.T, path string, allowlist bool) map[publicCutKey]publicCutInventoryEntry {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read public-cut inventory %s: %v", path, err)
	}
	entries, err := parsePublicCutInventory(string(data), allowlist)
	if err != nil {
		t.Fatalf("parse public-cut inventory %s: %v", path, err)
	}
	return entries
}

func parsePublicCutInventory(data string, allowlist bool) (map[publicCutKey]publicCutInventoryEntry, error) {
	entries := make(map[publicCutKey]publicCutInventoryEntry)
	previous := ""
	for lineNumber, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.SplitN(raw, "\t", 6)
		if len(fields) != 6 {
			return nil, fmt.Errorf("line %d: want 6 tab-separated fields, got %d", lineNumber+1, len(fields))
		}
		key := publicCutKey{
			category: publicCutCategory(fields[0]),
			path:     fields[1],
			rule:     fields[2],
			evidence: fields[3],
		}
		if !publicCutCategories[key.category] {
			return nil, fmt.Errorf("line %d: unknown category %q", lineNumber+1, key.category)
		}
		if key.category == publicCutSecret && !allowlist {
			return nil, fmt.Errorf("line %d: high-confidence secrets are never baseline-eligible", lineNumber+1)
		}
		if key.category == publicCutSecret && !publicCutFixturePath(key.path) {
			return nil, fmt.Errorf("line %d: a secret allowlist entry must name one exact test/fixture file", lineNumber+1)
		}
		if key.path == "" || filepath.IsAbs(key.path) || key.path != filepath.ToSlash(filepath.Clean(key.path)) || strings.ContainsAny(key.path, "\r\n\t") {
			return nil, fmt.Errorf("line %d: invalid repository-relative path %q", lineNumber+1, key.path)
		}
		if key.rule == "" || key.evidence == "" || fields[5] == "" {
			return nil, fmt.Errorf("line %d: rule, evidence and review reason are required", lineNumber+1)
		}
		count, countErr := strconv.Atoi(fields[4])
		if countErr != nil || count < 1 {
			return nil, fmt.Errorf("line %d: count %q must be a positive integer", lineNumber+1, fields[4])
		}
		entry := publicCutInventoryEntry{key: key, count: count, reason: fields[5]}
		serialized := serializePublicCutInventoryEntry(entry)
		if previous != "" && serialized <= previous {
			return nil, fmt.Errorf("line %d: entries must be unique and sorted", lineNumber+1)
		}
		if _, duplicate := entries[key]; duplicate {
			return nil, fmt.Errorf("line %d: duplicate inventory key", lineNumber+1)
		}
		entries[key] = entry
		previous = serialized
	}
	return entries, nil
}

func publicCutFixturePath(path string) bool {
	return strings.HasSuffix(path, "_test.go") || strings.Contains(path, "/testdata/") || strings.Contains(path, "/fixtures/")
}

func evaluatePublicCut(findings []publicCutFinding, baseline, allowlist map[publicCutKey]publicCutInventoryEntry) []string {
	actual := make(map[publicCutKey]int)
	samples := make(map[publicCutKey]publicCutFinding)
	var errors []string
	for _, finding := range findings {
		if finding.unwaivable {
			if _, explicitlyAllowedFixture := allowlist[finding.key()]; explicitlyAllowedFixture && finding.category == publicCutSecret && publicCutFixturePath(finding.path) {
				actual[finding.key()]++
				if _, exists := samples[finding.key()]; !exists {
					samples[finding.key()] = finding
				}
				continue
			}
			errors = append(errors, fmt.Sprintf("unwaivable %s at %s (%s): %s",
				finding.category, finding.location(), finding.rule, finding.context))
			continue
		}
		key := finding.key()
		actual[key]++
		if _, exists := samples[key]; !exists {
			samples[key] = finding
		}
	}

	for key := range allowlist {
		if _, overlap := baseline[key]; overlap {
			errors = append(errors, fmt.Sprintf("%s %s appears in both baseline and allowlist", key.category, key.path))
		}
	}

	for _, entry := range sortedPublicCutInventory(allowlist) {
		got := actual[entry.key]
		switch {
		case got == 0:
			errors = append(errors, fmt.Sprintf("allowlist entry is stale: %s %s (%s); remove it", entry.key.category, entry.key.path, entry.key.rule))
		case got < entry.count:
			errors = append(errors, fmt.Sprintf("allowlist entry must shrink: %s %s (%s) has %d finding(s), listed %d", entry.key.category, entry.key.path, entry.key.rule, got, entry.count))
		case got > entry.count:
			errors = append(errors, fmt.Sprintf("allowlist is narrower than current findings: %s %s (%s) has %d finding(s), listed %d", entry.key.category, entry.key.path, entry.key.rule, got, entry.count))
		}
		delete(actual, entry.key)
	}

	for _, entry := range sortedPublicCutInventory(baseline) {
		got := actual[entry.key]
		switch {
		case got < entry.count:
			errors = append(errors, fmt.Sprintf("baseline must shrink: %s %s (%s) has %d finding(s), baseline %d", entry.key.category, entry.key.path, entry.key.rule, got, entry.count))
		case got > entry.count:
			sample := samples[entry.key]
			errors = append(errors, fmt.Sprintf("new debt: %s %s (%s) grew from %d to %d; %s", entry.key.category, sample.location(), entry.key.rule, entry.count, got, sample.context))
		}
		delete(actual, entry.key)
	}

	for _, key := range sortedPublicCutKeys(actual) {
		sample := samples[key]
		errors = append(errors, fmt.Sprintf("new debt: %s %s (%s): %s", key.category, sample.location(), key.rule, sample.context))
	}
	sort.Strings(errors)
	return errors
}

func sortedPublicCutInventory(entries map[publicCutKey]publicCutInventoryEntry) []publicCutInventoryEntry {
	result := make([]publicCutInventoryEntry, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry)
	}
	sort.Slice(result, func(i, j int) bool {
		return serializePublicCutInventoryEntry(result[i]) < serializePublicCutInventoryEntry(result[j])
	})
	return result
}

func sortedPublicCutKeys(entries map[publicCutKey]int) []publicCutKey {
	keys := make([]publicCutKey, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return serializePublicCutKey(keys[i]) < serializePublicCutKey(keys[j]) })
	return keys
}

func serializePublicCutKey(key publicCutKey) string {
	return strings.Join([]string{string(key.category), key.path, key.rule, key.evidence}, "\t")
}

func serializePublicCutInventoryEntry(entry publicCutInventoryEntry) string {
	return serializePublicCutKey(entry.key) + "\t" + strconv.Itoa(entry.count) + "\t" + entry.reason
}

func renderPublicCutBaseline(findings []publicCutFinding, allowlist map[publicCutKey]publicCutInventoryEntry) string {
	counts := make(map[publicCutKey]int)
	for _, finding := range findings {
		if finding.unwaivable {
			continue
		}
		if _, allowed := allowlist[finding.key()]; allowed {
			continue
		}
		counts[finding.key()]++
	}
	var lines []string
	for _, key := range sortedPublicCutKeys(counts) {
		reason := "Existing reviewed debt; remove or replace this exact reference before the public cut."
		lines = append(lines, serializePublicCutInventoryEntry(publicCutInventoryEntry{
			key: key, count: counts[key], reason: reason,
		}))
	}
	return strings.Join(lines, "\n")
}

func inventoryMap(entries ...publicCutInventoryEntry) map[publicCutKey]publicCutInventoryEntry {
	result := make(map[publicCutKey]publicCutInventoryEntry, len(entries))
	for _, entry := range entries {
		result[entry.key] = entry
	}
	return result
}

func publicCutErrorsContain(errors []string, fragment string) bool {
	for _, err := range errors {
		if strings.Contains(err, fragment) {
			return true
		}
	}
	return false
}

func publicCutRunGit(t *testing.T, root string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
}
