package repositorychecks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicCutReleaseGate(t *testing.T) {
	t.Parallel()
	repoRoot := repositoryRoot(t)
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

func TestPublicCutScannerFlagsThePrivateRepositoryNotTheBinaryName(t *testing.T) {
	t.Parallel()
	name := "putnami" + "-cloud"
	owner := "Putnami"
	host := "github" + ".com"
	cases := []struct {
		line string
		rule string
		want string
	}{
		{"owned by " + owner + "/" + name + " today", "private-repository-coordinate", "putnami/" + name},
		{"see https://" + host + "/" + owner + "/" + name, "private-repository-coordinate", "https://" + host + "/putnami/" + name},
		{"git clone https://" + host + "/" + owner + "/" + name + ".git", "private-repository-coordinate", "https://" + host + "/putnami/" + name + ".git"},
		{"git clone git@" + host + ":" + owner + "/" + name + ".git", "private-repository-coordinate", "git@" + host + ":putnami/" + name + ".git"},
		{"git clone git@" + host + ":" + owner + "/" + name, "private-repository-coordinate", "git@" + host + ":putnami/" + name},
		{"a fork at https://" + host + "/someone/" + name + ".", "private-repository-name", "https://" + host + "/someone/" + name},
		{"git clone git@" + host + ":someone/" + name + ".git", "private-repository-name", "git@" + host + ":someone/" + name + ".git"},
		{"git clone ssh://git@git.example.com/team/" + name + ".git", "private-repository-name", "ssh://git@git.example.com/team/" + name + ".git"},
		{"cd ../" + name + " && make", "private-repository-name", "../" + name},
		{"read ../../" + name + "/README.md", "private-repository-name", "../../" + name},
	}
	for _, tc := range cases {
		findings := scanPublicCut([]publicCutFile{{path: "doc.md", data: []byte(tc.line + "\n")}})
		if len(findings) != 1 || findings[0].category != publicCutPrivateRepository || findings[0].rule != tc.rule || findings[0].evidence != tc.want {
			t.Errorf("%q: findings = %+v, want one %s finding with evidence %q", tc.line, findings, tc.rule, tc.want)
		}
	}

	binaryNames := []string{
		"the " + name + " binary",
		`const reporterBinary = "` + name + `"`,
		name + " credential-provider: ready",
		`filepath.Join(root, ".putnami", "bin", "extensions", "` + name + `")`,
		".putnami/bin/extensions/" + name,
		"the archive's compiled/" + name + ", or compiled/" + name + ".exe on Windows",
		"[`cmd/" + name + "`](../../cmd/" + name + ")",
		"example.com/fork/" + name,
		"archive-" + name + "-linux-x64.tar.gz",
	}
	for _, line := range binaryNames {
		if findings := scanPublicCut([]publicCutFile{{path: "doc.md", data: []byte(line + "\n")}}); len(findings) != 0 {
			t.Errorf("%q: the binary name was flagged as the private repository: %+v", line, findings)
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
	repoRoot := repositoryRoot(t)
	paths := []string{publicCutScannerRepoPath, ProjectDir + "/public_cut_test.go", publicCutBaselineRepoPath, publicCutAllowlistRepoPath}
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

// TestIgnoredCandidatesNamesOnlyWhatTheCutLeavesOut reads this repository's
// ignore rules: local Putnami state is ignored, whether it exists or not, and a
// tracked document is not.
func TestIgnoredCandidatesNamesOnlyWhatTheCutLeavesOut(t *testing.T) {
	t.Parallel()
	ignored, err := ignoredCandidates(repositoryRoot(t), []string{".putnami/absent-probe", "README.md", "tooling/absent-probe.md"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ignored, ",") != ".putnami/absent-probe" {
		t.Fatalf("ignored candidates = %v, want only .putnami/absent-probe", ignored)
	}
}
