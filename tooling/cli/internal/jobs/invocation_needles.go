// CONFINEMENT, first half: what the leak guard treats as a secret.
//
// The invariant this file owns is that the needle set is a BOUNDED, REPORTED
// function of what the producer wrote, and of nothing else. This is the only
// code that reads a sensitive artifact, and every rule about which bytes become
// a needle lives here so that "what counts as a leak" is decided once:
//
//   - BOUNDED. One artifact spends one budget, whether it is a file or a tree,
//     and a tree walk is bounded in entries, files and depth as well as in
//     bytes. A guard whose cost is proportional to the artifact is a guard a
//     provider can turn into a denial of service — but the bound is on the
//     READ, never on coverage, so a credential past the cap is still redacted
//     through the per-line, `KEY=value` and JSON-leaf needles.
//   - REPORTED. Every bound that BITES is recorded and warned about once, so an
//     artifact the guard could not cover completely says so instead of
//     degrading in silence. The warning names the artifact by its DECLARED
//     path, never by the private absolute one.
//   - PAYLOAD ONLY. Structure is not payload. A candidate equal to one of the
//     workspace's own identifiers, to a JSON member name, or to PEM armor is
//     vocabulary that every green run prints by design, and a needle on it
//     cannot distinguish a leak from a success.
//
// Matching is the other half of confinement and lives in invocation_redact.go.
// Keeping the two apart is what makes each locally checkable: this file is
// reviewed against "did the guard read the right bytes, under a bound, and say
// so when it could not", and that one against "can any needle reach a surface
// that publishes".
package jobs

import (
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"go.putnami.dev/tooling/cli/internal/store"
)

// sensitiveSampleCap bounds how much of a sensitive artifact the leak guard
// READS. It is a budget on I/O, not on coverage: the guard must not become a
// cost proportional to the artifact, but a credential that sits past the cap is
// still a credential and must still be redacted.
//
// So the cap bounds the READ and the derivation runs on whatever was read. What
// a truncated sample loses is exactly one needle — the whole DOCUMENT, which
// cannot match a file the guard never saw whole — while the per-LINE, the
// `KEY=value` and the JSON-leaf needles, which are the individual secrets a
// large artifact contains (a PEM chain plus its key clears 64 KiB without
// difficulty, and `cert` is credential vocabulary), survive.
//
// One artifact spends one budget, whether it is a file or a tree.
const sensitiveSampleCap = 64 << 10

// The bounds on sampling a sensitive artifact that is a DIRECTORY.
//
// store.InvocationScratch.Harden already walks a sensitive tree, so a provider
// may legitimately declare `{"kind":"directory","sensitive":true}` for a
// generated credentials tree (ca.pem, client.key, env). The guard has to cover
// it — and has to stay bounded while doing so, because a walk is the one place
// where "read the artifact" can become "read a million files".
//
// The three bounds answer three different unbounded quantities, and each is
// sized one to two orders of magnitude above the shape it serves:
//
//   - FILES bounds the needle set, and through it the per-event matching cost.
//     A credentials tree is a handful of files; 64 is generous for that and
//     keeps the needle count in the same range a single artifact produces.
//   - ENTRIES bounds the WALK itself, which files alone cannot: a tree of a
//     million empty directories contains no file to count. 1024 stats is a
//     millisecond and still finds the credentials in a tree carrying incidental
//     entries.
//   - DEPTH bounds descent. Credentials nest one or two levels (certs/ca.pem);
//     4 covers that with room to spare.
//
// Every bound that BITES is reported (needleCollector.degrade), so a tree the
// guard could not cover completely says so instead of degrading in silence.
const (
	sensitiveTreeFileCap  = 64
	sensitiveTreeEntryCap = 1024
	sensitiveTreeDepthCap = 4
)

// sensitiveNeedleMin is the shortest string the guard will treat as a secret.
// Below it, matching produces false positives against ordinary output far more
// often than it catches a leak.
const sensitiveNeedleMin = 8

// loadNeedles derives the leak guard's needle set from every sensitive artifact
// currently on disk. The caller holds rel.mu.
//
// It is idempotent in effect — sensitiveNeedles is a function of the file, and
// redactSensitive is a set membership test — so re-deriving after a retry costs
// a re-read and changes nothing.
func (rel *invocationRelation) loadNeedles() {
	if rel.scratch == nil {
		return
	}
	public := rel.publicStructure()
	for _, output := range sortedInvocationOutputs(rel.producer) {
		if output.PathFrom != "" || !output.Sensitive {
			continue
		}
		rel.needles = append(rel.needles, sensitiveNeedles(rel.scratch, output.Path, public)...)
	}
}

// publicStructure collects the workspace identifiers of every participant in
// the relation: each project's ID, name and path, and every path segment of
// them. These strings appear in ordinary output by design — job headers, the
// extensions' own log lines, artifact paths — so a byte sample equal to one of
// them can never serve as a leak needle.
func (rel *invocationRelation) publicStructure() map[string]bool {
	public := map[string]bool{}
	record := func(identifier string) {
		identifier = strings.TrimSpace(identifier)
		if identifier == "" {
			return
		}
		public[identifier] = true
		for _, segment := range strings.Split(identifier, "/") {
			if segment != "" {
				public[segment] = true
			}
		}
	}
	participants := append([]*ScheduledJob{rel.producer, rel.finalizer}, rel.consumers...)
	for _, job := range participants {
		if job == nil || job.Project == nil {
			continue
		}
		record(job.Project.ID)
		record(job.Project.Name)
		record(job.Project.SourceName)
		record(job.Project.Path)
	}
	return public
}

// needleSet returns a copy of the relation's leak-guard needles.
func (rel *invocationRelation) needleSet() []string {
	rel.mu.Lock()
	defer rel.mu.Unlock()
	if len(rel.needles) == 0 {
		return nil
	}
	return append([]string(nil), rel.needles...)
}

// sensitiveNeedles derives what the leak guard matches on for one sensitive
// artifact: its absolute PATH, and samples of its BYTES.
//
// Both halves are required by the contract, and they fail differently. The path
// is what a task echoes when it logs its own arguments; the bytes are what it
// echoes when it prints a connection string it just read.
//
// The bytes half samples the artifact at three granularities, because a task
// that leaks rarely prints the file whole:
//
//   - the document, and each of its LINES;
//   - the VALUE half of a `KEY=value` line — the env-shaped artifact;
//   - every STRING LEAF of a JSON document — the object-shaped artifact.
//
// The JSON granularity is not an optional refinement. A single-line JSON
// document has exactly one line, which is the whole file, so without leaf
// extraction the only byte needle is the entire document and a task printing
// the parsed DSN or password it just read defeats the bytes half completely.
// Leaves are the same rule the `KEY=value` half already applies, expressed for
// the container the artifact actually uses; they carry the same false-positive
// floor (sensitiveNeedleMin) and, like it, they match VALUES only — an object's
// member names are structure, not payload. The accepted cost is also the same
// one that rule already accepts: an ordinary-looking value long enough to clear
// the floor becomes a needle, so a participant echoing it is failed. That is the
// direction a fail-closed guard must err in — a task that lost control of a
// credential is a worse outcome than a task told to stop printing one.
//
// A URL-shaped candidate additionally contributes its userinfo PASSWORD. That
// component is the one part of a connection string a task can print on its own
// (a driver echoing credentials, a test printing what it connected as) while the
// surrounding URL never appears.
//
// public carries the workspace's own identifiers — the participants' project
// names and their path segments. A byte candidate that IS one of them is
// excluded for the same reason JSON member names are: it is structure, not
// payload. An artifact value equal to the producing project's name (test envs
// carry exactly that) would otherwise turn every event that mentions the
// project — including the extension's own "Tests passed for <project>" line —
// into a leak verdict, and no needle that every logger prints by design can
// distinguish a leak from a green run.
//
// The artifact's KIND does not change any of that. A directory artifact is
// sampled file by file under ONE shared budget, and an artifact larger than the
// budget is sampled up to it: the cap bounds what the guard reads, never what
// it guards. Both used to return the path alone, which meant the values inside
// were never redacted from the event stream or the result, silently.
func sensitiveNeedles(scratch *store.InvocationScratch, rel string, public map[string]bool) []string {
	path, err := scratch.Resolve(rel)
	if err != nil {
		return nil
	}
	collector := &needleCollector{
		needles: []string{path},
		seen:    map[string]bool{path: true},
		public:  public,
		budget:  sensitiveSampleCap,
		files:   sensitiveTreeFileCap,
		entries: sensitiveTreeEntryCap,
	}
	collector.sampleArtifact(path)
	collector.report(scratch, rel)
	return collector.needles
}

// needleCollector accumulates one sensitive artifact's needles under the
// guard's bounds, and records every place a bound stopped it from covering the
// artifact completely.
//
// It exists because a directory artifact turns "derive needles from a file"
// into "derive needles from a tree under a SHARED budget": the budget, the file
// and entry allowances and the deduplicating needle set all have to be one
// state threaded through the walk, not per-file locals.
type needleCollector struct {
	needles []string
	seen    map[string]bool
	public  map[string]bool
	// budget is the number of artifact bytes the guard may still read, shared by
	// every file of a directory artifact.
	budget int64
	// files and entries are the remaining allowances of a directory walk.
	files   int
	entries int
	// degraded names the bounds that bit, deduplicated, in the order they were
	// first hit. A non-empty slice is what report turns into the warning.
	degraded []string
}

// degrade records that the guard could not cover part of the artifact. It never
// carries a path or a byte of the artifact — the reason is a fixed sentence, so
// the warning it becomes cannot itself leak what the guard is protecting.
func (c *needleCollector) degrade(reason string) {
	for _, existing := range c.degraded {
		if existing == reason {
			return
		}
	}
	c.degraded = append(c.degraded, reason)
}

// report emits the ONE warning that makes a partially covered artifact visible.
//
// It names the artifact by its DECLARED relative path and its provider, never
// by the private absolute path: that path is a needle precisely because it must
// not be published, and a guard that printed it to make a point would be the
// leak it exists to prevent. slog.Warn is the channel the rest of the scheduler
// degrades on (remote.go, task_capture.go), so this needs no new surface.
func (c *needleCollector) report(scratch *store.InvocationScratch, rel string) {
	if len(c.degraded) == 0 {
		return
	}
	slog.Warn("leak guard could not fully sample a sensitive invocation artifact; "+
		"bytes it did not read cannot be redacted from events or results",
		"artifact", rel,
		"provider", scratch.Lease().Provider,
		"reasons", strings.Join(c.degraded, "; "))
}

// sampleArtifact derives byte needles from whatever the declared path actually
// is on disk.
//
// SYMLINKS are decided differently at the top level and inside a tree, and the
// asymmetry is deliberate. The declared artifact itself is followed when it
// resolves to a regular file: that file's bytes are what the consumers read
// through the declared path, so NOT following it would drop the needles for the
// exact value being handed out — fail-open, in the one place the contract is
// about. Inside a tree the walk never follows, because there the link target is
// not the declared artifact and following could take the guard out of the
// private root and mint needles from arbitrary files on the machine.
//
// A socket, fifo or device artifact has no bytes to sample and is not a
// degradation: the path needle is the whole of what such an artifact can leak.
func (c *needleCollector) sampleArtifact(path string) {
	info, err := os.Lstat(path)
	switch {
	case err != nil:
		c.degrade("the artifact could not be inspected")
	case info.IsDir():
		c.sampleTree(path)
	case info.Mode()&fs.ModeSymlink != 0:
		target, err := os.Stat(path)
		if err != nil || !target.Mode().IsRegular() {
			c.degrade("the artifact is a symlink to something other than a regular file")
			return
		}
		c.sampleFile(path)
	case info.Mode().IsRegular():
		c.sampleFile(path)
	}
}

// sampleTree derives needles from every file of a directory artifact, under the
// artifact's single shared budget and the walk's entry, file and depth bounds.
//
// Candidates are collected first and sampled SMALLEST FIRST. A credential is
// small, so spending the budget on the smallest files covers the most secrets
// per byte read: a tree holding a 200 KiB bundle next to a 200 byte DSN would
// otherwise let the bundle eat the whole budget and leave the DSN — the value a
// task actually echoes — unguarded, purely because of lexical order. Ties break
// on path so the needle set is deterministic.
//
// The contained files' own paths are NOT added as needles: every one of them
// has the artifact's directory path as a prefix, which is already a needle, so
// a task printing one is detected and redacted by the prefix. Adding them would
// multiply the per-event matching cost to sharpen a marker.
func (c *needleCollector) sampleTree(root string) {
	type candidate struct {
		path string
		size int64
	}
	var candidates []candidate

	walkErr := filepath.WalkDir(root, func(entry string, d fs.DirEntry, err error) error {
		if err != nil {
			c.degrade("part of the artifact tree could not be read")
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry == root {
			return nil
		}
		c.entries--
		if c.entries < 0 {
			c.degrade("the artifact tree holds more entries than the guard walks")
			return fs.SkipAll
		}
		depth := strings.Count(strings.TrimPrefix(entry, root+string(filepath.Separator)),
			string(filepath.Separator)) + 1
		if d.IsDir() {
			if depth >= sensitiveTreeDepthCap {
				c.degrade("the artifact tree is deeper than the guard descends")
				return fs.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			c.degrade("a symlink inside the artifact tree was not followed")
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			c.degrade("a file inside the artifact tree could not be read")
			return nil
		}
		if info.Size() > 0 {
			candidates = append(candidates, candidate{path: entry, size: info.Size()})
		}
		return nil
	})
	if walkErr != nil {
		c.degrade("the artifact tree could not be walked")
	}

	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].size != candidates[j].size {
			return candidates[i].size < candidates[j].size
		}
		return candidates[i].path < candidates[j].path
	})
	for _, file := range candidates {
		if c.files <= 0 {
			c.degrade("the artifact tree holds more files than the guard samples")
			return
		}
		if c.budget <= 0 {
			c.degrade("the artifact tree is larger than the guard's byte budget")
			return
		}
		c.files--
		c.sampleFile(file.path)
	}
}

// sampleFile reads at most the remaining budget from one regular file and
// derives the artifact's byte needles from that sample.
//
// The read is bounded, not skipped, which is the whole point of the repair: an
// artifact over the cap used to contribute no byte needle at all, so a task that
// echoed the DSN embedded in a large credentials file was reported green.
func (c *needleCollector) sampleFile(path string) {
	if c.budget <= 0 {
		c.degrade("the artifact is larger than the guard's byte budget")
		return
	}
	f, err := os.Open(path)
	if err != nil {
		c.degrade("the artifact could not be read")
		return
	}
	defer func() { _ = f.Close() }()

	// budget+1 is how truncation is observed without a second stat: reading one
	// byte past the budget proves there was more.
	data, err := io.ReadAll(io.LimitReader(f, c.budget+1))
	if err != nil {
		c.degrade("the artifact could not be read")
		return
	}
	truncated := int64(len(data)) > c.budget
	if truncated {
		data = data[:c.budget]
	}
	c.budget -= int64(len(data))
	c.derive(string(data), truncated)
	if truncated {
		c.degrade("the artifact is larger than the guard's byte budget")
	}
}

// derive turns one file's SAMPLE into needles at the granularities the bytes
// half covers, and is where a truncated sample differs from a whole one.
//
// Two needles are dropped when the sample is truncated, and only those two:
//
//   - the whole DOCUMENT, which can no longer match — the guard never saw the
//     document, and a needle equal to a prefix of it would match nothing a task
//     could print except by accident;
//   - the trailing partial LINE, which is half a record. Half of a `KEY=value`
//     is not the value, and treating it as one would put an arbitrary prefix
//     into the needle set.
//
// Everything else — the complete lines, their credential-named values, the
// URL userinfo passwords, the JSON leaves of a sample that still parses — is
// derived exactly as it is for a whole file. Those are the individual secrets,
// and they are what the cap must not be allowed to drop.
func (c *needleCollector) derive(sample string, truncated bool) {
	whole := ""
	if truncated {
		// Locate the boundary before trimming. If the budget ends exactly on a
		// newline, trimming first removes that delimiter and makes the last fully
		// sampled line look partial, dropping a secret the guard actually read.
		cut := strings.LastIndexByte(sample, '\n')
		if cut < 0 {
			return
		}
		whole = strings.TrimSpace(sample[:cut])
	} else {
		whole = strings.TrimSpace(sample)
		c.addValue(whole)
	}
	for _, line := range strings.Split(whole, "\n") {
		c.addValue(line)
		if key, value, ok := strings.Cut(line, "="); ok {
			value = strings.Trim(strings.TrimSpace(value), `"'`)
			if credentialKeyPattern.MatchString(key) || urlUserinfoPassword(value) != "" {
				c.addValue(value)
			}
		}
	}
	for _, leaf := range jsonCredentialLeaves(whole) {
		c.addValue(leaf)
	}
}

// addValue records a candidate and, when it is a URL carrying userinfo, the
// password component on its own.
func (c *needleCollector) addValue(candidate string) {
	c.add(candidate)
	c.add(urlUserinfoPassword(candidate))
}

// add records one byte candidate unless a rule says it is not payload: the
// false-positive floor, the needle set it is already in, or the workspace's own
// public structure. Those rules are keyed on the CANDIDATE and on the
// relation, never on the artifact's declared name, which is what lets a file
// inside a directory artifact go through exactly the same scoping as a
// single-file artifact does.
func (c *needleCollector) add(candidate string) {
	candidate = strings.TrimSpace(candidate)
	if len(candidate) < sensitiveNeedleMin || c.seen[candidate] || c.public[candidate] {
		return
	}
	if pemArmorPattern.MatchString(candidate) {
		return
	}
	c.seen[candidate] = true
	c.needles = append(c.needles, candidate)
}

// pemArmorPattern matches a PEM delimiter line. Armor is the document's shape,
// identical in every certificate and key ever written, so it is the same kind
// of shared vocabulary as a JSON member name or a project's own name: a needle
// on it fails a task for printing an unrelated public certificate and can never
// distinguish a leak from a green run — the same doctrine applies here because
// certificate material (`cert` is credential vocabulary) is caught by the line
// rule. The base64 body lines still cover the key itself.
var pemArmorPattern = regexp.MustCompile(`^-{5}(BEGIN|END) [A-Z0-9 ]+-{5}$`)

// credentialKeyPattern selects the member names whose VALUES the bytes half
// treats as payload. A value under any other name — an engine, a host, a
// schema, a database name — is a schema-level enumerator: shared vocabulary
// that ordinary output prints by design (`postgres` inside a node_modules
// stack frame, a database name in a migration log), which can never
// distinguish a leak from a green run. Those values stay covered by the
// whole-document and full-line rules — a task that prints the artifact still
// fails — and a URL carrying userinfo is payload under ANY name.
var credentialKeyPattern = regexp.MustCompile(`(?i)pass|secret|token|key|dsn|credential|cert`)

// jsonCredentialLeaves returns the string VALUES of a JSON document that the
// guard treats as payload, in sorted order so the needle set is deterministic.
//
// A leaf qualifies when the member name nearest to it matches
// credentialKeyPattern — an array leaf inherits the enclosing member's name —
// or when the value itself is a URL carrying userinfo, whatever it is named.
// Member NAMES are never needles, and neither are values under structural
// names: both are the document's shape, shared by every artifact of the same
// kind, and matching them fails tasks for printing ordinary vocabulary. Text
// that is not JSON yields nothing, so this costs one failed parse for an
// env-shaped artifact.
func jsonCredentialLeaves(document string) []string {
	var decoded any
	if err := json.Unmarshal([]byte(document), &decoded); err != nil {
		return nil
	}
	seen := make(map[string]bool)
	var walk func(name string, value any)
	walk = func(name string, value any) {
		switch typed := value.(type) {
		case string:
			if credentialKeyPattern.MatchString(name) || urlUserinfoPassword(typed) != "" {
				seen[typed] = true
			}
		case map[string]any:
			for member, child := range typed {
				walk(member, child)
			}
		case []any:
			for _, element := range typed {
				walk(name, element)
			}
		}
	}
	walk("", decoded)

	leaves := make([]string, 0, len(seen))
	for leaf := range seen {
		leaves = append(leaves, leaf)
	}
	sort.Strings(leaves)
	return leaves
}

// urlUserinfoPassword returns the password component of a URL-shaped candidate,
// or "" when the candidate is not a URL or carries no password. The scheme
// check keeps url.Parse — which accepts almost any string — from turning
// ordinary text into a spurious credential.
func urlUserinfoPassword(candidate string) string {
	if !strings.Contains(candidate, "://") {
		return ""
	}
	parsed, err := url.Parse(candidate)
	if err != nil || parsed.User == nil {
		return ""
	}
	password, ok := parsed.User.Password()
	if !ok {
		return ""
	}
	return password
}
