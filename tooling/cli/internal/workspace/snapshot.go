package workspace

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/fileindex"
	"go.putnami.dev/tooling/cli/internal/git"
)

// The digest-authoritative workspace snapshot.
//
// `.putnami/workspace-index.json` records what core resolved about the
// workspace and, for every bounded metadata input it resolved that from, the
// CONTENT digest of that input. Validity is decided by comparing each input's
// current content digest with the recorded one.
//
// A current digest is read through the machine-local file index
// (internal/fileindex, protocols/workspace ADR 0009): a file whose stat equals
// the stat the index recorded with its digest is not read again, and a file
// written in the same timestamp tick as the index's clock has no entry and is
// read. The snapshot's own Size and ModUnixNano are not that stat: they are
// recorded on this file, which is committed to no machine, and they never
// decide validity.

// WorkspaceIndexFilename is the snapshot's filename under `.putnami/`.
const WorkspaceIndexFilename = "workspace-index.json"

// snapshotFormatVersion versions the on-disk shape AND the coverage semantics
// of its input digests. Version 2 made `**` recursive for glob inputs. Version 3
// records providers' workspace-root entries in their per-project watchedFiles.
// Version 4 records `observedAt`, the projection's declared observation time:
// a copy that cannot say how old it is cannot be held to a freshness
// bound, and a reader would have to guess.
// Rejecting an older snapshot forces one re-probe after upgrade; without that
// migration a recorded v2 Go answer would omit go.work.sum forever because
// checksum churn is deliberately not a probe-metadata input.
const snapshotFormatVersion = 4

// absentInputDigest marks a metadata input that did not exist when the snapshot
// was taken. Recording absence is load-bearing: creating a project's go.mod
// changes what core resolves, and a snapshot that only listed files that
// existed would stay "valid" across that creation.
const absentInputDigest = "absent"

// ErrSnapshotWriteNotPermitted is returned by WriteSnapshot when the run's
// policy forbids persistence. `--plan` and `--dry-run` may probe in memory but
// must never write: a planning run is allowed to be speculative, and persisting
// its view would let a hypothetical selection become the workspace's recorded
// identity for every later run.
var ErrSnapshotWriteNotPermitted = errors.New("workspace: snapshot writes are suppressed under --plan/--dry-run")

// SnapshotWritePolicy states whether this run may persist the snapshot.
type SnapshotWritePolicy struct {
	// Plan is true under `--plan`.
	Plan bool
	// DryRun is true under `--dry-run`.
	DryRun bool
}

// Persists reports whether the policy allows writing.
func (p SnapshotWritePolicy) Persists() bool { return !p.Plan && !p.DryRun }

// Snapshot is the persisted view of a resolved workspace.
type Snapshot struct {
	// Version is snapshotFormatVersion.
	Version int `json:"version"`
	// ProbeDigest is the normalized aggregate probe digest the snapshot was
	// taken under.
	ProbeDigest string `json:"probeDigest"`
	// IdentityDigest is the workspace identity these projects resolved to.
	IdentityDigest string `json:"identityDigest"`
	// ObservedAt is when the recorded provider answers were observed, RFC 3339
	// in UTC. It is the projection's declared `observed_at` field: a local copy
	// has to be able to say how old it is, or its declared freshness bound is
	// unenforceable and every reader has to guess.
	//
	// It is the one member of this file that is not a function of the tree, so
	// two snapshots of one unchanged workspace differ in exactly this field.
	// That is the point — the timestamp records the READ, not the content — and
	// nothing keys on the snapshot's bytes.
	ObservedAt string `json:"observedAt,omitempty"`
	// Inputs are workspace-level metadata inputs (the workspace config and each
	// scope's putnami.json).
	Inputs []SnapshotInput `json:"inputs,omitempty"`
	// Projects are the resolved projects in canonical ID order.
	Projects []SnapshotProject `json:"projects,omitempty"`
	// Providers are the per-extension probe answers, in extension-name order.
	//
	// The RESULT is stored, not only its digest, for one reason: a stale
	// snapshot re-probes only the providers whose own inputs moved, and the
	// providers that did not move must still contribute their answer to the
	// merge. Without the stored result, "probe only what changed" would
	// silently drop every other provider's dependency edges — a wrong build,
	// not a slower one.
	Providers []SnapshotProvider `json:"providers,omitempty"`
}

// SnapshotProvider is one extension's recorded probe answer and the metadata
// inputs core resolved it from.
type SnapshotProvider struct {
	// Extension is the answering provider's extension name.
	Extension string `json:"extension"`
	// Digest is the normalized ProbeResultDigest of Result. It is recorded
	// alongside the result so a reader can detect a hand-edited snapshot
	// without re-deriving the whole merge.
	Digest string `json:"digest"`
	// Result is the normalized answer.
	Result wsproto.ProbeResult `json:"result"`
	// Inputs are this provider's declared metadata inputs, resolved over the
	// candidate directories core knew about, with their content digests.
	Inputs []SnapshotInput `json:"inputs,omitempty"`
	// Implementation is the identity of the provider implementation that
	// produced Result: a workspace-local extension's runtime input digest, a
	// registry-installed extension's version. A recorded answer is reused only
	// while the bound provider still has this identity; an empty value is a
	// record that predates the field and is re-probed once.
	Implementation string `json:"implementation,omitempty"`
	// InstallInputs are the workspace-root file names the provider's adapter
	// declares as install inputs (ProviderScope.InstallInputs), sorted. Empty
	// when the adapter declares none. They enter no digest. A record whose
	// install inputs or language differ from the declaration, including a
	// record that predates both members, is re-probed once.
	InstallInputs []string `json:"installInputs,omitempty"`
	// Language is the language id the provider's adapter declares
	// (ProviderScope.Language). Empty when the adapter declares none. It enters
	// no digest.
	Language string `json:"language,omitempty"`
}

// SnapshotProject is one resolved project's recorded identity.
type SnapshotProject struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	Name string `json:"name"`
	// MetadataDigest is the per-project identity that enters cache keys.
	MetadataDigest string `json:"metadataDigest"`
	// Inputs are this project's bounded metadata inputs.
	Inputs []SnapshotInput `json:"inputs,omitempty"`
}

// SnapshotInput is one metadata input and the content digest core resolved it
// at. Size and ModUnixNano are ADVISORY: they are never consulted to decide
// validity (see the file comment).
type SnapshotInput struct {
	// Path is workspace-relative and slash-separated, so a snapshot compares
	// equal across checkouts of the same commit. For Kind == inputKindGlob it
	// is the workspace-relative PATTERN rather than one file.
	Path string `json:"path"`
	// Kind distinguishes a single file (empty, the default) from a glob pattern
	// whose whole matched SET is the input. A glob's digest covers the sorted
	// match list and every match's content, so creating or deleting a matching
	// file invalidates — which a per-file record could not express, because a
	// file that does not exist yet has no path to record.
	Kind string `json:"kind,omitempty"`
	// Digest is "sha256:<hex>" of the file's content, or absentInputDigest.
	Digest string `json:"digest"`
	// Size is the recorded byte length. Advisory.
	Size int64 `json:"size,omitempty"`
	// ModUnixNano is the recorded modification time. Advisory.
	ModUnixNano int64 `json:"modUnixNano,omitempty"`
}

// inputKindGlob marks a SnapshotInput whose Path is a pattern and whose digest
// covers the matched set.
const inputKindGlob = "glob"

// SnapshotValidity is the verdict of re-hashing a snapshot's inputs.
type SnapshotValidity struct {
	// Valid is true only when every recorded input still hashes to its
	// recorded digest.
	Valid bool
	// Reason names the first disqualifying condition, for diagnostics.
	Reason string
	// Changed lists the workspace-relative inputs whose content moved.
	Changed []string
}

// projectMetadataInputNames is the bounded set of per-project metadata inputs
// CORE resolves a project from. Bounded is the requirement: an unbounded input
// set would make an "ordinary load" walk the tree, which is exactly the cost the
// snapshot exists to avoid.
//
// It is one file: the adapters that declare a language manifest record it, per
// project and with its content digest (providerInputs). Recording a language
// manifest here too would attribute its change to core, which invalidates EVERY
// provider instead of the one that owns it (planProbe).
var projectMetadataInputNames = []string{
	wsproto.ConfigFilename, // putnami.json
}

// SnapshotPath is the snapshot's absolute location for a workspace root.
func SnapshotPath(root string) string {
	return filepath.Join(root, ".putnami", WorkspaceIndexFilename)
}

// NewSnapshot builds the snapshot describing ws as currently resolved.
// probeDigest is the aggregate probe digest the resolution was taken under;
// callers with no probe providers yet pass the workspace's own probe digest.
func NewSnapshot(ws *Workspace, probeDigest string) *Snapshot {
	return NewSnapshotWithProviders(ws, probeDigest, nil)
}

// NewSnapshotWithProviders builds the snapshot and records each provider's
// answer alongside core's own resolution. Providers
// are stored in extension-name order so two runs over one tree produce
// byte-identical snapshots.
func NewSnapshotWithProviders(ws *Workspace, probeDigest string, providers []SnapshotProvider) *Snapshot {
	return newSnapshotAt(ws, probeDigest, providers, time.Now())
}

// newSnapshotAt is NewSnapshotWithProviders with an explicit clock, so a test
// can state the observation time it is arguing about instead of sleeping.
func newSnapshotAt(ws *Workspace, probeDigest string, providers []SnapshotProvider, observedAt time.Time) *Snapshot {
	snapshot := &Snapshot{
		Version:        snapshotFormatVersion,
		ProbeDigest:    probeDigest,
		IdentityDigest: ws.IdentityDigest(),
		ObservedAt:     observedAt.UTC().Format(time.RFC3339Nano),
		Inputs:         workspaceMetadataInputs(ws),
		Providers:      append([]SnapshotProvider(nil), providers...),
	}
	sort.SliceStable(snapshot.Providers, func(i, j int) bool {
		return snapshot.Providers[i].Extension < snapshot.Providers[j].Extension
	})
	for _, project := range ws.Projects {
		snapshot.Projects = append(snapshot.Projects, SnapshotProject{
			ID:             project.ID,
			Path:           wsproto.CleanWorkspacePath(project.Path),
			Name:           project.Name,
			MetadataDigest: ws.MetadataDigestFor(project),
			Inputs:         projectMetadataInputs(ws.Root, project),
		})
	}
	sort.SliceStable(snapshot.Projects, func(i, j int) bool { return snapshot.Projects[i].ID < snapshot.Projects[j].ID })
	return snapshot
}

// workspaceMetadataInputs digests the workspace-level files whose content
// changes what CORE resolves for every project: the workspace config
// (membership, scopes, version) and each scope's putnami.json (namePattern,
// tags and extensions reach projects).
//
// The root package.json is not one of them: whichever adapter declares it
// records it, with its content digest, which is what makes a change to it
// re-probe that one provider instead of invalidating every provider at once.
func workspaceMetadataInputs(ws *Workspace) []SnapshotInput {
	names := []string{wsproto.WorkspaceConfigFilename}
	inputs := make([]SnapshotInput, 0, len(names)+8)
	for _, name := range names {
		inputs = append(inputs, digestInput(ws.Root, name))
	}
	for _, scopePath := range ScopePaths(ws.Root, ws.Config) {
		inputs = append(inputs, digestInput(ws.Root, scopePath+"/"+wsproto.ConfigFilename))
	}
	sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	return inputs
}

func projectMetadataInputs(root string, project *Project) []SnapshotInput {
	base := wsproto.CleanWorkspacePath(project.Path)
	inputs := make([]SnapshotInput, 0, len(projectMetadataInputNames))
	for _, name := range projectMetadataInputNames {
		rel := name
		if base != "" {
			rel = base + "/" + name
		}
		inputs = append(inputs, digestInput(root, rel))
	}
	sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].Path < inputs[j].Path })
	return inputs
}

// digestInput hashes one workspace-relative file. A missing file is recorded as
// absent rather than skipped, so its later creation invalidates the snapshot.
// An unreadable file is also recorded as absent: the next load re-resolves,
// which is the safe direction.
func digestInput(root, rel string) SnapshotInput {
	rel = filepath.ToSlash(filepath.Clean(rel))
	input := SnapshotInput{Path: rel, Digest: absentInputDigest}

	absolute := filepath.Join(root, filepath.FromSlash(rel))
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() {
		return input
	}
	digest, err := fileContentDigest(absolute)
	if err != nil {
		return input
	}
	input.Digest = digest
	input.Size = info.Size()
	input.ModUnixNano = info.ModTime().UnixNano()
	return input
}

// globInput digests a whole matched SET.
//
// The digest covers the sorted list of matches AND every match's content, so
// three different edits all invalidate: changing a matched file, adding a file
// the pattern matches, and deleting one. Only the first is expressible as a
// per-file record, which is why an adapter that declares a glob metadata input
// gets this shape instead.
//
// `**` is the recursive segment wildcard used everywhere else in extension
// file contracts. filepath.Glob treats it as an ordinary single-segment `*`,
// so recursive patterns are expanded by a deterministic tree walk instead.
// Names enter the digest workspace-relative and slash-separated, so the value
// is identical across checkouts of one commit.
func globInput(root, pattern string) SnapshotInput {
	input := SnapshotInput{Path: filepath.ToSlash(pattern), Kind: inputKindGlob}
	recursive := strings.Contains(filepath.ToSlash(pattern), "**")
	memoKey := root + "\x00" + filepath.ToSlash(pattern)
	var ignored string
	if recursive {
		ignored = ignoredFingerprint(root)
		loadGlobWitnesses(root)
		useGlobWitness(root, filepath.ToSlash(pattern))
		if held, ok := globWitnesses.Load(memoKey); ok {
			if w, typed := held.(*globWitness); typed && w.ignored == ignored && w.holds() {
				input.Digest = w.digest
				return input
			}
		}
	}
	witness := &globWitness{ignored: ignored, paths: map[string]fileindex.Stat{}}
	trusted := recursive && fileindex.TrustsStats(root)
	// The clock is read before the first stat: a path whose time lies within
	// the margin of it may change again with an equal stat (ADR 0009 §2 of
	// protocols/workspace).
	// Round(0) drops the monotonic reading, so the comparisons below see a
	// wall clock that went back.
	start := time.Now().Round(0)
	rootStat, rootOK := fileindex.LstatAny(root)
	trusted = trusted && rootOK
	var walked map[string]fileindex.Stat
	var matches []string
	if trusted {
		matches, walked = workspaceGlobMatchesWitnessed(root, pattern, rootStat.Dev)
		trusted = walked != nil
	} else {
		matches = workspaceGlobMatches(root, pattern)
	}
	rels := make([]string, 0, len(matches))
	for _, match := range matches {
		rel, relErr := filepath.Rel(root, match)
		if relErr != nil {
			continue
		}
		rels = append(rels, filepath.ToSlash(rel))
	}
	sort.Strings(rels)

	h := sha256.New()
	for _, rel := range rels {
		absolute := filepath.Join(root, filepath.FromSlash(rel))
		info, statErr := os.Stat(absolute)
		if statErr != nil || info.IsDir() {
			trusted = false // a path the witness cannot hold
			continue
		}
		if linkInfo, err := os.Lstat(absolute); err != nil || linkInfo.Mode()&os.ModeSymlink != 0 {
			trusted = false // a link's stat does not answer for its target
		}
		before, statOK := fileindex.LstatAny(absolute)
		digest, digestErr := fileContentDigest(absolute)
		if digestErr != nil {
			trusted = false
			continue
		}
		if trusted {
			after, afterOK := fileindex.LstatAny(absolute)
			if !statOK || !afterOK || after != before || before.Dev != rootStat.Dev {
				// A file that moved while read, or that lies on another
				// file system than the workspace's, is not held.
				trusted = false
			}
			witness.paths[absolute] = before
		}
		h.Write([]byte(rel))    //nolint:errcheck // hash.Hash.Write never errors
		h.Write([]byte{0})      //nolint:errcheck // hash.Hash.Write never errors
		h.Write([]byte(digest)) //nolint:errcheck // hash.Hash.Write never errors
		h.Write([]byte{0})      //nolint:errcheck // hash.Hash.Write never errors
	}
	input.Digest = "sha256:" + hex.EncodeToString(h.Sum(nil))
	if trusted && !time.Now().Round(0).Before(start) {
		for dir, st := range walked {
			witness.paths[dir] = st
		}
		witness.digest = input.Digest
		if witness.settled(start) {
			globWitnesses.Store(memoKey, witness)
			markGlobWitnessesDirty(root)
		} else {
			globWitnesses.Delete(memoKey)
		}
	}
	return input
}

// globWitnesses holds, for the life of the process, each recursive glob
// input's digest with its witness: the directories its walk listed and the
// files it read, by their stats (the directory rule of CLI ADR 0113 §2). An
// unchanged probe is answered from it with a stat per path instead of a walk
// and a digest per file; a stat that differs collects the input again.
var globWitnesses sync.Map // root NUL pattern → *globWitness

// globWalks counts the witnessed walks, for tests.
var globWalks atomic.Int64

type globWitness struct {
	// ignored is the git-ignored set the walk skipped: another one walks
	// otherwise.
	ignored string
	digest  string
	paths   map[string]fileindex.Stat
}

// holds reports whether every path still has its recorded stat.
func (w *globWitness) holds() bool {
	for path, recorded := range w.paths {
		if now, ok := fileindex.LstatAny(path); !ok || now != recorded {
			return false
		}
	}
	return true
}

// settled reports whether every recorded time lies more than the file
// index's margin before start, the clock read before the first stat: a path
// written within it could change again with an equal stat (ADR 0009 §2 of
// protocols/workspace).
func (w *globWitness) settled(start time.Time) bool {
	limit := start.Add(-fileindex.Margin).UnixNano()
	for _, st := range w.paths {
		if st.ModNano >= limit || st.ChangeNano >= limit {
			return false
		}
	}
	return true
}

// ignoredFingerprints memoizes ignoredFingerprint per root, for the life of
// ignoredWalkDirsCache.
var ignoredFingerprints sync.Map // root → string

// ignoredFingerprint is the git-ignored set of root, as a comparable string.
func ignoredFingerprint(root string) string {
	if held, ok := ignoredFingerprints.Load(root); ok {
		if fingerprint, typed := held.(string); typed {
			return fingerprint
		}
	}
	fingerprint := ignoredFingerprintUncached(root)
	ignoredFingerprints.Store(root, fingerprint)
	return fingerprint
}

func ignoredFingerprintUncached(root string) string {
	dirs := ignoredWalkDirs(root)
	keys := make([]string, 0, len(dirs))
	for dir := range dirs {
		keys = append(keys, dir)
	}
	sort.Strings(keys)
	return strings.Join(keys, "\x00")
}

// workspaceGlobMatches expands one workspace-relative metadata-input pattern.
// Non-recursive patterns keep filepath.Glob's established semantics. A
// recursive pattern walks only from the literal prefix before its first glob
// segment, so `project/**/*.go` never scans an unrelated sibling project.
func workspaceGlobMatches(root, pattern string) []string {
	return workspaceGlobMatchesObserving(root, pattern, nil)
}

// workspaceGlobMatchesObserving is workspaceGlobMatches that calls observe,
// when set, with every directory a recursive walk lists, before it lists it,
// and with every path the walk could not read, with its error.
func workspaceGlobMatchesObserving(root, pattern string, observe func(string, error)) []string {
	pattern = filepath.ToSlash(pattern)
	if !strings.Contains(pattern, "**") {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(pattern)))
		if err != nil {
			// An invalid pattern can never match; recording the empty set keeps
			// the snapshot decidable instead of permanently stale.
			return nil
		}
		return matches
	}

	searchRoot := globSearchRoot(root, pattern)
	ignored := ignoredWalkDirs(root)
	var matches []string
	_ = filepath.WalkDir(searchRoot, func(candidate string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			// An unreadable member is omitted just as filepath.Glob omits a path
			// it cannot stat. If it becomes readable later, joining the matched
			// set changes the digest and invalidates the snapshot.
			if observe != nil {
				observe(candidate, walkErr)
			}
			return nil
		}
		if entry.IsDir() {
			// These are core state, never provider source. They are excluded
			// from project discovery under the same cross-provider rule.
			if entry.Name() == ".git" || entry.Name() == ".putnami" {
				return fs.SkipDir
			}
			if rel, err := filepath.Rel(root, candidate); err == nil && ignored[filepath.ToSlash(rel)] {
				return fs.SkipDir
			}
			if observe != nil {
				observe(candidate, nil)
			}
			return nil
		}
		rel, err := filepath.Rel(root, candidate)
		if err != nil {
			return nil
		}
		if matchWorkspaceGlob(filepath.ToSlash(rel), pattern) {
			matches = append(matches, candidate)
		}
		return nil
	})
	return matches
}

// globSearchRoot is where a recursive pattern's walk starts: the literal
// prefix before its first glob segment, under root.
func globSearchRoot(root, pattern string) string {
	var prefix []string
	for _, segment := range strings.Split(filepath.ToSlash(pattern), "/") {
		if strings.ContainsAny(segment, "*?[") {
			break
		}
		prefix = append(prefix, segment)
	}
	if len(prefix) == 0 {
		return root
	}
	return filepath.Join(root, filepath.FromSlash(strings.Join(prefix, "/")))
}

// globWalkHook, when a test sets it, runs after a witnessed walk records a
// directory's stat.
var globWalkHook func(dir string)

// workspaceGlobMatchesWitnessed is workspaceGlobMatches for a recursive
// pattern that also returns the stat of every directory its walk listed,
// taken before the listing. It returns no witness when one cannot hold the
// answer: the walk could not read a path, did not list its search root (a
// missing prefix, or one that is not a directory), reached a directory on
// another device than dev, or saw a directory's stat move during the walk.
func workspaceGlobMatchesWitnessed(root, pattern string, dev uint64) ([]string, map[string]fileindex.Stat) {
	dirs := map[string]fileindex.Stat{}
	held := true
	observe := func(dir string, err error) {
		if err != nil {
			held = false
			return
		}
		st, ok := fileindex.LstatAny(dir)
		if !ok || st.Dev != dev {
			held = false
			return
		}
		dirs[dir] = st
		if globWalkHook != nil {
			globWalkHook(dir)
		}
	}
	globWalks.Add(1)
	matches := workspaceGlobMatchesObserving(root, pattern, observe)
	if _, listed := dirs[globSearchRoot(root, pattern)]; !held || !listed {
		return matches, nil
	}
	for dir, before := range dirs {
		if after, ok := fileindex.LstatAny(dir); !ok || after != before {
			return matches, nil
		}
	}
	return matches, dirs
}

// ignoredWalkDirsCache memoizes one root's fully-ignored directory set. The
// answer is read once per glob input AND again for every one of them on the
// validity pass, and `git ls-files` is a subprocess; the tree does not move
// under a single CLI invocation, which is the same lifetime the option
// ownership resolver memoizes over.
var ignoredWalkDirsCache sync.Map // workspace root → map[string]bool

// ignoredWalkDirs is the set of workspace-relative directories git ignores
// entirely — an install tree, a build output, a generated `.gen`.
//
// A recursive metadata input must not descend into them, for the reason the
// adapter contract already states about excludes: core excludes gitignored
// directories for every extension, and a witness that reads them would be a
// witness on bytes no provider answer is derived from. It would also make a
// cold clone and a warm checkout of one commit disagree about the same
// pattern, which is exactly what the digest exists to rule out.
//
// Outside a git repository the set is empty and the walk is the plain one.
func ignoredWalkDirs(root string) map[string]bool {
	if cached, ok := ignoredWalkDirsCache.Load(root); ok {
		if dirs, typed := cached.(map[string]bool); typed {
			return dirs
		}
	}
	dirs := git.IgnoredDirs(root)
	if dirs == nil {
		dirs = map[string]bool{}
	}
	actual, _ := ignoredWalkDirsCache.LoadOrStore(root, dirs)
	if stored, typed := actual.(map[string]bool); typed {
		return stored
	}
	return dirs
}

// matchWorkspaceGlob matches slash-separated paths and treats a whole `**`
// segment as zero or more path segments.
func matchWorkspaceGlob(rel, pattern string) bool {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	return matchWorkspaceGlobSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"))
}

func matchWorkspaceGlobSegments(pattern, rel []string) bool {
	if len(pattern) == 0 {
		return len(rel) == 0
	}
	if pattern[0] == "**" {
		if matchWorkspaceGlobSegments(pattern[1:], rel) {
			return true
		}
		for i := range rel {
			if matchWorkspaceGlobSegments(pattern[1:], rel[i+1:]) {
				return true
			}
		}
		return false
	}
	if len(rel) == 0 {
		return false
	}
	matched, err := path.Match(pattern[0], rel[0])
	return err == nil && matched && matchWorkspaceGlobSegments(pattern[1:], rel[1:])
}

// hasGlobMeta reports whether a pattern names a set rather than one path.
func hasGlobMeta(pattern string) bool {
	return strings.ContainsAny(pattern, "*?[")
}

// fileContentDigest is "sha256:<hex>" of the file's content, read through the
// machine-local file index (internal/fileindex): a file whose stat is the one
// recorded with its digest is not read again. Its variant is the raw SHA-256
// the input hashes of the store share.
func fileContentDigest(absolute string) (string, error) {
	digest, err := fileindex.Digest(absolute, fileindex.RawContent, func() ([]byte, error) {
		file, err := os.Open(absolute) //nolint:gosec // path is workspace-relative, joined under the workspace root
		if err != nil {
			return nil, err
		}
		defer file.Close() //nolint:errcheck // read-only handle

		h := sha256.New()
		if _, err := io.Copy(h, file); err != nil {
			return nil, err
		}
		return h.Sum(nil), nil
	})
	if err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(digest), nil
}

// Validate re-hashes every recorded input and reports whether the snapshot
// still describes the tree.
//
// It hashes EVERY input, in recorded order, and there is no early exit: the
// caller (planProbe) consumes the complete Changed list to attribute each
// changed file to the provider that declared it, so stopping at the first
// disagreement would mis-attribute the rest and re-probe the wrong providers.
//
// An input is accepted when its content digest, read through the file index,
// equals the recorded digest. The index answers without reading only for a
// file whose stat it recorded a tick before its clock, so a same-size rewrite
// in the same tick is read and detected.
func (s *Snapshot) Validate(root string) SnapshotValidity {
	if s == nil {
		return SnapshotValidity{Reason: "no snapshot"}
	}
	if s.Version != snapshotFormatVersion {
		return SnapshotValidity{Reason: fmt.Sprintf("snapshot format v%d, want v%d", s.Version, snapshotFormatVersion)}
	}

	all := make([]SnapshotInput, 0, len(s.Inputs))
	all = append(all, s.Inputs...)
	for _, project := range s.Projects {
		all = append(all, project.Inputs...)
	}
	for _, provider := range s.Providers {
		all = append(all, provider.Inputs...)
	}

	// One file can be recorded by more than one owner — `package.json` is both a
	// core project input and a TypeScript adapter input — so the changed list is
	// deduplicated. It is a diagnostic AND the input to provider attribution;
	// a duplicate would report the same change twice in both.
	seen := make(map[string]bool, len(all))
	var changed []string
	for _, input := range all {
		if seen[input.Path] || inputStillMatches(root, input) {
			continue
		}
		seen[input.Path] = true
		changed = append(changed, input.Path)
	}
	if len(changed) > 0 {
		sort.Strings(changed)
		return SnapshotValidity{Reason: "metadata inputs changed", Changed: changed}
	}
	return SnapshotValidity{Valid: true}
}

// statDisagrees reports whether cheap stat data already contradicts the record.
// A false answer means "no cheap evidence of change", NOT "unchanged".
func statDisagrees(root string, input SnapshotInput) bool {
	if input.Kind == inputKindGlob {
		// A glob's identity is a SET. There is no single file to stat, and
		// "the set may have changed" is exactly the case stat cannot see, so
		// it is always hashed rather than guessed at.
		return true
	}
	info, err := os.Stat(filepath.Join(root, filepath.FromSlash(input.Path)))
	if err != nil || info.IsDir() {
		return input.Digest != absentInputDigest
	}
	if input.Digest == absentInputDigest {
		return true
	}
	return info.Size() != input.Size || info.ModTime().UnixNano() != input.ModUnixNano
}

// inputStillMatches is the oracle: the content digest, read through the file
// index.
func inputStillMatches(root string, input SnapshotInput) bool {
	if input.Kind == inputKindGlob {
		return globInput(root, input.Path).Digest == input.Digest
	}
	absolute := filepath.Join(root, filepath.FromSlash(input.Path))
	info, err := os.Stat(absolute)
	if err != nil || info.IsDir() {
		return input.Digest == absentInputDigest
	}
	if input.Digest == absentInputDigest {
		return false
	}
	digest, err := fileContentDigest(absolute)
	if err != nil {
		return false
	}
	return digest == input.Digest
}

// LoadSnapshot reads and strict-parses the persisted snapshot. A missing file
// returns (nil, nil): "no snapshot yet" is an ordinary state, not an error. A
// malformed file is reported so a corrupted snapshot is never silently treated
// as an empty one.
func LoadSnapshot(root string) (*Snapshot, error) {
	data, err := os.ReadFile(SnapshotPath(root))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read workspace snapshot: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var snapshot Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return nil, fmt.Errorf("parse workspace snapshot: %w", err)
	}
	return &snapshot, nil
}

// WriteSnapshot persists the snapshot atomically, or refuses when the policy
// forbids it.
//
// Atomicity matters for the same reason it matters for a cache entry: a reader
// concurrent with a writer must see either the previous complete snapshot or
// the new complete one, never a truncated prefix that parses as a workspace
// with half its projects. The write stages a temp file in the SAME directory
// (so rename(2) stays within one filesystem) and renames it into place.
func WriteSnapshot(root string, snapshot *Snapshot, policy SnapshotWritePolicy) error {
	if !policy.Persists() {
		return ErrSnapshotWriteNotPermitted
	}
	if snapshot == nil {
		return errors.New("workspace: cannot write a nil snapshot")
	}

	target := SnapshotPath(root)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return fmt.Errorf("create snapshot directory: %w", err)
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return fmt.Errorf("encode workspace snapshot: %w", err)
	}
	data = append(data, '\n')

	temp, err := os.CreateTemp(filepath.Dir(target), "."+WorkspaceIndexFilename+".*")
	if err != nil {
		return fmt.Errorf("stage workspace snapshot: %w", err)
	}
	tempName := temp.Name()
	defer os.Remove(tempName) //nolint:errcheck // no-op once renamed into place

	if _, err := temp.Write(data); err != nil {
		temp.Close() //nolint:errcheck,gosec // write already failed
		return fmt.Errorf("write workspace snapshot: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close workspace snapshot: %w", err)
	}
	if err := os.Chmod(tempName, 0o644); err != nil {
		return fmt.Errorf("chmod workspace snapshot: %w", err)
	}
	if err := os.Rename(tempName, target); err != nil {
		return fmt.Errorf("publish workspace snapshot: %w", err)
	}
	return nil
}

// RefreshSnapshot rebuilds and persists the CORE half of the snapshot for ws:
// the resolved projects and their metadata inputs. Returns the snapshot it built
// even when the policy suppressed the write, so a `--plan`/`--dry-run` run can
// still report what it WOULD have recorded.
//
// The recorded PROVIDER answers are carried forward from the snapshot on disk
// rather than dropped. Writing a provider-less snapshot over one that has
// answers is the shape that deletes every project's language identity and
// dependency edges from the index, and the next load — which adopts whatever the
// index says — then resolves a different workspace, and therefore different
// cache keys, for an unchanged tree. Synchronize refuses that write for exactly
// this reason (the zero-bindings branch); a helper that quietly did it anyway
// would be a way around the refusal.
//
// Re-probing is NOT this function's job and it has no provider set to do it
// with: Synchronize is the path that resolves provider answers, decides what is
// stale, and persists the result. This is the narrow "core's own resolution
// moved" refresh.
func RefreshSnapshot(ws *Workspace, policy SnapshotWritePolicy) (*Snapshot, error) {
	recorded, _ := LoadSnapshot(ws.Root)
	var carried []SnapshotProvider
	if recorded != nil {
		carried = recorded.Providers
	}
	snapshot := NewSnapshotWithProviders(ws, ws.ProbeDigest(), carried)
	if !policy.Persists() {
		return snapshot, ErrSnapshotWriteNotPermitted
	}
	if err := WriteSnapshot(ws.Root, snapshot, policy); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}
