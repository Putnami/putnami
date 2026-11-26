package repositorychecks

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The public-cut scan is release policy over the repository that will be
// copied into the history-free public root, not a user-facing CLI command. Its
// input is the candidate cut: the tracked tree plus non-ignored new files, as
// `git ls-files --cached --others --exclude-standard` lists them, so the
// pre-stage candidate is covered while build output and ignored local
// credentials are never read.

const (
	// The two reviewed inventories stay in the CLI module: GOVERNANCE.md links
	// them by path. They are inputs of this project's whole-tree cache key.
	publicCutBaselinePath      = "internal/cli/testdata/public-cut-baseline.tsv"
	publicCutAllowlistPath     = "internal/cli/testdata/public-cut-allowlist.tsv"
	publicCutBaselineRepoPath  = cliModuleDir + "/" + publicCutBaselinePath
	publicCutAllowlistRepoPath = cliModuleDir + "/" + publicCutAllowlistPath
	publicCutScannerRepoPath   = ProjectDir + "/publiccut.go"
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

// Two private names are flagged wherever they stand alone. The third is also
// the name of the @putnami/cloud extension binary, so only a reference to its
// repository is flagged: a GitHub URL under any owner, a clone URL ending in
// .git, or a sibling checkout path such as ../<name>. The owner-qualified name
// is publicCutPrivateRepositoryRules' match. The names are split so the
// scanner stays self-safe.
var publicCutPrivateNameRules = []publicCutRule{
	{
		name: "private-repository-name",
		re:   regexp.MustCompile(`(?i)(?:^|[^\w@/.-])(agents` + `-memory|found` + `ry)(?:[^\w-]|$)`),
	},
	{
		name: "private-repository-name",
		re: regexp.MustCompile(`(?i)(?:^|[^\w-])(` +
			`(?:(?:https?|ssh)://|git@)?github[.]com[:/][\w.-]+/putnami` + `-cloud(?:[.]git)?` +
			`|[\w.:/@~-]*putnami` + `-cloud[.]git` +
			`|(?:[.][.]/)+putnami` + `-cloud` +
			`)(?:[^\w-]|$)`),
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

func publicCutTrackedFiles(root string) ([]publicCutFile, error) {
	paths, err := candidatePaths(root)
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

// candidatePaths lists the candidate cut of the repository at root: every
// tracked path and every non-ignored untracked path, relative to root, sorted
// and unique. A tracked path deleted from the work tree is listed;
// publicCutTrackedFiles drops it.
func candidatePaths(root string) ([]string, error) {
	cmd := exec.Command("git", "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	cmd.Dir = root
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	if len(data) == 0 {
		return nil, nil
	}
	if data[len(data)-1] != 0 {
		return nil, fmt.Errorf("git ls-files did not terminate its NUL-delimited output")
	}
	parts := bytes.Split(data[:len(data)-1], []byte{0})
	paths := make([]string, 0, len(parts))
	for _, part := range parts {
		if len(part) == 0 {
			return nil, fmt.Errorf("git ls-files returned an empty candidate path")
		}
		paths = append(paths, string(part))
	}
	sort.Strings(paths)
	return slices.Compact(paths), nil
}

// ignoredCandidates returns the paths among paths, relative to root, that the
// candidate cut leaves out: untracked paths Git's ignore rules match. A path
// need not exist; a tracked path is never ignored.
func ignoredCandidates(root string, paths []string) ([]string, error) {
	cmd := exec.Command("git", "check-ignore", "-z", "--stdin")
	cmd.Dir = root
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	data, err := cmd.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("git check-ignore: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var ignored []string
	for _, part := range bytes.Split(bytes.TrimSuffix(data, []byte{0}), []byte{0}) {
		if len(part) > 0 {
			ignored = append(ignored, string(part))
		}
	}
	sort.Strings(ignored)
	return ignored, nil
}

// candidatePathsOnly lists the candidate cut of the repository at root
// without reading its files: the paths ReadCandidateCut would return.
func candidatePathsOnly(root string) ([]string, error) {
	paths, err := candidatePaths(root)
	if err != nil {
		return nil, err
	}
	present := paths[:0]
	for _, rel := range paths {
		if _, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("candidate path %q: %w", rel, err)
		}
		present = append(present, filepath.ToSlash(rel))
	}
	return present, nil
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
		var historySpans, coordinateSpans [][2]int
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
					coordinateSpans = append(coordinateSpans, [2]int{span[0], span[1]})
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
			for _, span := range publicCutMatchSpans(publicCutPrivateNameRules, line, append(historySpans, coordinateSpans...)) {
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
