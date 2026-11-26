package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"go.putnami.dev/sdk/extension/gitread"
)

// CacheManager integrates the LocalStore with job execution to provide
// transparent caching. It handles cache key computation, hit/miss
// detection, result restoration, and output storage.
type CacheManager struct {
	store *LocalStore
	// candidates is the run's read of the workspace's candidate cut
	// (SetCandidateSource), nil when the run holds none.
	candidates atomic.Pointer[CandidateSource]
	// exclusion decides whether written paths lie where Git lists no
	// candidate (SetCandidateExclusion), nil when the run holds none.
	exclusion       atomic.Pointer[CandidateExclusion]
	fileHashes      inputHashMemo     // memoizes file hashes: "dir|patterns|scope" → hash
	extraFileHashes inputHashMemo     // memoizes extra-file hashes: workspace root and sorted paths, NUL-joined → hash
	sourceStates    map[string]string // memoizes SourceState: workspace root → state
	sourceStateMu   sync.Mutex
	digests         map[string]*memoizedDigest // memoizes Digest: key → one computation
	digestsMu       sync.Mutex
	values          map[string]*memoizedValue // memoizes Value: key → one value
	valuesMu        sync.Mutex
	inputResolver   InputResolver // expands the input selectors a key reads
	inputResolverMu sync.Mutex
}

// invalidationLogSize is how many invalidations an inputHashMemo remembers
// the regions of. A digest computed across more of them than that is not
// memoized, whatever they named.
const invalidationLogSize = 64

// inputHashMemo memoizes input digests, each with the region every file it
// read lies in, so an invalidation that names the paths a task wrote drops
// exactly the digests those writes can move. The zero value is an empty memo.
type inputHashMemo struct {
	mu      sync.Mutex
	entries map[string]memoizedInputHash
	// generation counts the invalidations, and log holds the regions the last
	// invalidationLogSize of them named, oldest first.
	generation uint64
	log        []invalidatedRegion
}

// memoizedInputHash is one memoized input digest and the region of its files.
type memoizedInputHash struct {
	hash   string
	region InputRegion
	// witness, when set, is what the digest read: a write inside region
	// drops it only when the write can move it (ADR 0109).
	witness *selectionWitness
	// pending marks a digest a write moves only if the cut read after it
	// lists other candidates than the read the digest listed from: the next
	// lookup compares the two (CacheManager.confirmPending).
	pending bool
}

// writeEffect is what one invalidation does to a memoized digest.
type writeEffect int

const (
	effectKeeps writeEffect = iota
	effectDrops
	// effectPends keeps the digest until a read of the cut confirms it.
	effectPends
)

// invalidatedRegion is what one invalidation of an inputHashMemo named.
type invalidatedRegion struct {
	generation uint64
	written    InputRegion
	// excluded answers whether the written paths lie where Git lists no
	// candidate, nil when nothing says so.
	excluded *candidateExclusion
	// listing is what the write left at the paths it names, read once when
	// a witnessed digest asks, nil when the invalidation names no paths.
	listing *writtenListing
}

// effect reports whether a write this invalidation names can move a digest of
// region: the write reaches the region, and either it reaches the part where
// the digest reads files that are not Git candidates, or it lies where Git can
// list a candidate. The second question is asked only when the first leaves
// it open, so a run whose writes reach no candidate digest never asks it.
//
// A digest with a witness is decided by its witness instead when the write
// lies at or under the directory it was collected in: the write moves it only
// when a file it read lies at or under a written path, or a file the write
// left there could be selected by its patterns (ADR 0109).
//
// The effect tells a digest the write drops from one it pends: one whose
// witness answers that the write moves it only if the cut moved.
func (i invalidatedRegion) effect(region InputRegion, witness *selectionWitness) writeEffect {
	drops := func(moves bool) writeEffect {
		if moves {
			return effectDrops
		}
		return effectKeeps
	}
	if !i.written.Overlaps(region) {
		return effectKeeps
	}
	if witness.decides(i.listing) {
		switch witness.moves(i.listing) {
		case writeKeeps:
			return effectKeeps
		case writeMovesIfCandidate:
			return drops(i.excluded == nil || !i.excluded.holds())
		case writeMovesIfCutMoved:
			return effectPends
		default:
			return effectDrops
		}
	}
	if region.nonCandidates == nil || i.excluded == nil || i.written.Overlaps(*region.nonCandidates) {
		return effectDrops
	}
	return drops(!i.excluded.holds())
}

// CandidateExclusion reports whether every one of paths, absolute paths a task
// of the run wrote, lies in a directory Git's rules exclude as a whole and
// under which the work tree's candidate cut lists nothing, so the write adds
// no Git candidate, removes none and changes none.
type CandidateExclusion func(paths []string) bool

// candidateExclusion is one invalidation's answer of a CandidateExclusion,
// asked at most once, and only when a digest's entry depends on it.
type candidateExclusion struct {
	once     sync.Once
	paths    []string
	exclude  CandidateExclusion
	excluded bool
}

func (e *candidateExclusion) holds() bool {
	e.once.Do(func() { e.excluded = e.exclude(e.paths) })
	return e.excluded
}

// lookup returns the digest memoized at key, or false with the generation a
// computation of it starts from (memoize).
func (m *inputHashMemo) lookup(key string) (string, uint64, bool) {
	hash, generation, ok, pending := m.lookupPending(key)
	if pending != nil {
		return "", generation, false
	}
	return hash, generation, ok
}

// lookupPending is lookup, also returning a digest a write left pending with
// its witness: the caller confirms it against a read of the cut
// (CacheManager.confirmPending) before it answers with it.
func (m *inputHashMemo) lookupPending(key string) (string, uint64, bool, *selectionWitness) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cached, ok := m.entries[key]; ok {
		if cached.pending {
			return cached.hash, m.generation, true, cached.witness
		}
		return cached.hash, m.generation, true, nil
	}
	return "", m.generation, false, nil
}

// settle ends the pending state of the digest at key after a read of the cut
// was compared with its witness's. A confirmed digest stays, unless an
// invalidation came after the lookup that read it, which judged it again; a
// refuted one is dropped.
func (m *inputHashMemo) settle(key string, generation uint64, witness *selectionWitness, confirmed bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cached, ok := m.entries[key]
	if !ok || !cached.pending || cached.witness != witness {
		return
	}
	if !confirmed {
		delete(m.entries, key)
		return
	}
	if m.generation == generation {
		cached.pending = false
		m.entries[key] = cached
	}
}

// memoize records hash at key unless an invalidation since generation, the one
// its computation started from, named a path in region: the digest may then
// hold the bytes from before that write. An invalidation the log no longer
// holds counts as one that did.
func (m *inputHashMemo) memoize(key string, generation uint64, hash string, region InputRegion, witness *selectionWitness) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.generation != generation {
		if len(m.log) == 0 || m.log[0].generation > generation+1 {
			return
		}
		for _, invalidation := range m.log {
			if invalidation.generation > generation && invalidation.effect(region, witness) != effectKeeps {
				return
			}
		}
	}
	if m.entries == nil {
		m.entries = make(map[string]memoizedInputHash)
	}
	m.entries[key] = memoizedInputHash{hash: hash, region: region, witness: witness}
}

// invalidate drops every memoized digest the write of written can move, and
// logs it for the digests being computed meanwhile. excluded, when set, says
// whether written lies where Git lists no candidate (invalidatedRegion.moves).
func (m *inputHashMemo) invalidate(written InputRegion, excluded *candidateExclusion, listing *writtenListing) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.generation++
	invalidation := invalidatedRegion{generation: m.generation, written: written, excluded: excluded, listing: listing}
	for key, memo := range m.entries {
		switch invalidation.effect(memo.region, memo.witness) {
		case effectDrops:
			delete(m.entries, key)
		case effectPends:
			memo.pending = true
			m.entries[key] = memo
		}
	}
	m.log = append(m.log, invalidation)
	if len(m.log) > invalidationLogSize {
		m.log = append([]invalidatedRegion(nil), m.log[len(m.log)-invalidationLogSize:]...)
	}
}

// memoizedValue is one key's value in CacheManager.Value.
type memoizedValue struct {
	once  sync.Once
	value any
}

// memoizedDigest is one key's computation in CacheManager.Digest.
type memoizedDigest struct {
	once  sync.Once
	value string
	err   error
}

// NewCacheManager creates a CacheManager backed by the given store.
func NewCacheManager(store *LocalStore) *CacheManager {
	return &CacheManager{store: store}
}

// lookupFileHash computes a combined hash for files matching patterns
// using file contents, with per-session memoization. Multiple pipeline
// steps in the same project with identical file patterns reuse the
// cached result instead of re-reading and re-hashing all source files.
//
// An entry lives until an invalidation names a path in its region
// (FileInputRegion): the scheduler names the outputs every task writes, so a
// key computed after a task ran reads that task's bytes. A digest computed
// while an invalidation named a path in its region may hold bytes from before
// that write, so it is returned but not memoized (inputHashMemo.memoize).
//
// The memo key holds the expander's identity when patterns name an input
// selector: two jobs whose patterns read alike can still name a selector in
// inputs with other patterns, and so read other files.
func (cm *CacheManager) lookupFileHash(dir string, patterns []string, expander InputExpander, scope ProjectConfigScope) (string, error) {
	// The scope selects which bytes of a project config the digest reads, so two
	// scopes are two different values and must never share one memo entry.
	key := dir + "|" + strings.Join(patterns, ",") + "|" + strings.Join(scope.ExtensionLayers, "\x00") +
		"|" + strings.Join(scope.ForeignNamespaces, "\x00")
	if scope.Verbatim {
		key += "|verbatim"
	}
	if namesInputSelector(patterns) && expander != nil {
		key += "|expander\x00" + expander.Identity()
	}

	cached, generation, ok, pending := cm.fileHashes.lookupPending(key)
	if ok && pending != nil {
		confirmed := cm.confirmPending(pending)
		cm.fileHashes.settle(key, generation, pending, confirmed)
		if confirmed {
			return cached, nil
		}
		_, generation, _ = cm.fileHashes.lookup(key)
	} else if ok {
		return cached, nil
	}

	hash, witness, err := memoizedHashFiles(cm.candidateSource(), dir, patterns, expander, scope)
	if err != nil {
		return "", err
	}

	if witness != nil && witness.unmemoized {
		return hash, nil
	}
	cm.fileHashes.memoize(key, generation, hash, FileInputRegion(dir, patterns), witness)
	return hash, nil
}

// confirmPending reports whether the cut, read now, lists what the read a
// pending digest listed its candidates from listed: the writes since then
// added, removed and changed no candidate it reads (ADR 0109). A run with no
// read of the cut for the digest's directory confirms nothing.
func (cm *CacheManager) confirmPending(witness *selectionWitness) bool {
	source := cm.candidateSource()
	if source == nil || witness == nil || witness.cut == "" {
		return false
	}
	read, ok := source(witness.dir)
	return ok && cutFingerprint(read) == witness.cut
}

// memoizedHashFiles is the digest lookupFileHash memoizes (hashFiles). It is a
// variable only so a test can land an invalidation while a digest is computed.
var memoizedHashFiles = witnessedHashFiles

// HashFiles returns the memoized content digest for files selected under dir,
// reading a project config through the given task's option scope. A zero scope
// keeps every project config whole, which is the wider digest of the two.
//
// Scheduler source-rewriter handling captures this value before execution and
// compares it after invalidating the memo, so only actual keyed source edits
// make a clean status entry non-restorable. It must pass the SAME scope the key
// used: a narrower detector misses keyed edits, and a wider one reports edits
// the key ignores. A source rewriter's scope is Verbatim, so a layout-only
// rewrite of a project config is a keyed mutation. It passes the key's
// expander too, which expands the input selectors patterns name.
func (cm *CacheManager) HashFiles(dir string, patterns []string, expander InputExpander, scope ProjectConfigScope) (string, error) {
	if cm == nil {
		return "", fmt.Errorf("cache manager is nil")
	}
	return cm.lookupFileHash(dir, patterns, expander, scope)
}

// SetCandidateSource makes the keys of the run list a whole work tree's
// candidate cut from source, the run's read of it, instead of asking Git
// (CandidateSource). The source answers the current read, which a write of the
// run makes stale as it drops the memoized digests the write can move.
func (cm *CacheManager) SetCandidateSource(source CandidateSource) {
	if cm == nil {
		return
	}
	cm.candidates.Store(&source)
}

func (cm *CacheManager) candidateSource() CandidateSource {
	if cm == nil {
		return nil
	}
	if source := cm.candidates.Load(); source != nil {
		return *source
	}
	return nil
}

// InvalidateFileHashes drops every memoized input digest, project files and
// extra files alike. A task whose rewrites are not confined to declared
// outputs — a source rewriter — calls it once it ran, so its post-run mutation
// check and every later key read the rewritten tree rather than pre-fix bytes.
func (cm *CacheManager) InvalidateFileHashes() {
	if cm == nil {
		return
	}
	everything := InputRegion{unbounded: true}
	cm.fileHashes.invalidate(everything, nil, nil)
	cm.extraFileHashes.invalidate(everything, nil, nil)
}

// InvalidateFileHashesUnder drops every memoized input digest whose region
// reaches a path at, under or above one of paths: the outputs a task just
// wrote or a restore just materialized. A digest outside them keeps its entry,
// because nothing it reads changed. The caller names the paths before any task
// that reads them can start, so no key computed after a write reads a digest
// of the bytes before it.
//
// A digest that reads Git candidates where the paths lie, and nothing else
// there, also keeps its entry when the run's CandidateExclusion answers that
// the paths lie where Git lists no candidate, such as the workspace's .putnami
// directory when Git ignores it (tooling/cli ADR 0106). The exclusion is asked
// only for such a digest.
func (cm *CacheManager) InvalidateFileHashesUnder(paths []string) {
	if cm == nil || len(paths) == 0 {
		return
	}
	written := PathsRegion(paths)
	var excluded *candidateExclusion
	if exclusion := cm.exclusion.Load(); exclusion != nil && *exclusion != nil {
		excluded = &candidateExclusion{paths: slices.Clone(paths), exclude: *exclusion}
	}
	cm.fileHashes.invalidate(written, excluded, &writtenListing{paths: slices.Clone(paths)})
	cm.extraFileHashes.invalidate(written, excluded, nil)
}

// SetCandidateExclusion gives the run the answer of whether written paths lie
// where Git lists no candidate (CandidateExclusion), so a write there keeps the
// memoized digests that read only Git candidates where it lies
// (InvalidateFileHashesUnder).
func (cm *CacheManager) SetCandidateExclusion(exclusion CandidateExclusion) {
	if cm == nil {
		return
	}
	cm.exclusion.Store(&exclusion)
}

// lookupExtraFilesHash computes the combined content hash of a set of extra
// file paths (cross-project generate assets), with per-session memoization
// mirroring lookupFileHash. hashExtraFiles walks each directory asset
// recursively and reads every byte beneath it, and the same ExtraFiles set is
// hashed once at key-precompute time and again at execute-time lookup for
// every cacheable step that declares it — the memo turns those repeated full
// tree reads into one per CLI invocation for a set no task writes. The memo
// key is the workspace root followed by the sorted path set, joined with NUL
// (which cannot occur in a path), so distinct sets never collide and one set
// named against two roots never shares an entry; hashExtraFiles is
// order-independent, so two orderings of the same set correctly share one
// entry. The cached value is byte-identical to hashExtraFiles(workspaceRoot,
// paths).
//
// An entry lives under the same rule as lookupFileHash's: until an
// invalidation names a path at, under or above one of its paths. A declared
// output another task writes during the run is therefore hashed again once that
// task ran. A file written outside every declaration is not seen, so an
// intra-run product still belongs in a declared output rather than in an
// undeclared write.
func (cm *CacheManager) lookupExtraFilesHash(workspaceRoot string, paths []string) string {
	sorted := make([]string, len(paths))
	copy(sorted, paths)
	sort.Strings(sorted)
	key := workspaceRoot + "\x00" + strings.Join(sorted, "\x00")

	cached, generation, ok := cm.extraFileHashes.lookup(key)
	if ok {
		return cached
	}

	hash := hashExtraFiles(workspaceRoot, sorted)
	cm.extraFileHashes.memoize(key, generation, hash, PathsRegion(sorted), nil)
	return hash
}

// Digest returns the value compute derives for key, computing it at most once
// for the life of the manager: the first caller computes, and concurrent
// callers for the same key wait for that answer instead of computing again.
// An error is the key's answer too, so one invocation never keys one input two
// ways. A nil manager has no answer to share and computes on every call.
//
// The memo lives as long as the manager, one per run, so a value derived from
// files that may change between runs is derived again by the next run. No
// invalidation reaches it, so it holds only values derived from what no task
// of the run writes: repository history, installed extension trees, and the
// workspace membership the run loaded.
func (cm *CacheManager) Digest(key string, compute func() (string, error)) (string, error) {
	if cm == nil {
		return compute()
	}
	cm.digestsMu.Lock()
	if cm.digests == nil {
		cm.digests = make(map[string]*memoizedDigest)
	}
	entry, ok := cm.digests[key]
	if !ok {
		entry = &memoizedDigest{}
		cm.digests[key] = entry
	}
	cm.digestsMu.Unlock()
	entry.once.Do(func() { entry.value, entry.err = compute() })
	return entry.value, entry.err
}

// Value returns the value build builds for key, building it at most once for
// the life of the manager, one per run: the first caller builds, and
// concurrent callers for the same key wait for that value. It is how the run's
// readers that the key computation and the scheduler share, such as the run's
// tree facts, are one object. A nil manager builds on every call.
func (cm *CacheManager) Value(key string, build func() any) any {
	if cm == nil {
		return build()
	}
	cm.valuesMu.Lock()
	if cm.values == nil {
		cm.values = make(map[string]*memoizedValue)
	}
	entry, ok := cm.values[key]
	if !ok {
		entry = &memoizedValue{}
		cm.values[key] = entry
	}
	cm.valuesMu.Unlock()
	entry.once.Do(func() { entry.value = build() })
	return entry.value
}

// CacheKey holds the inputs that contribute to a cache key.
type CacheKey struct {
	// Extension is the extension name (e.g., "@putnami/typescript").
	Extension string

	// ExtensionVersion is the resolved version of the extension that runs the
	// job. The hash carries it only when ExtensionImplementationDigest is empty,
	// because then the version is the only identity of the implementation: an
	// extension upgrade can change a task's output for byte-identical sources
	// (new codegen, changed defaults). A digest identifies the implementation
	// itself, so two versions of an unchanged extension share their entries.
	ExtensionVersion string

	// ExtensionImplementationDigest identifies the implementation of the
	// extension that runs the job: the prepared-runtime input digest of a
	// workspace-local extension, or the content digest of an installed
	// extension's tree without its version metadata. It is empty for a local
	// extension that declares no prepared runtime. An installed tree holds the
	// executables built for the host platform, so an extension that ships
	// platform executables has a different digest on each platform.
	ExtensionImplementationDigest string

	// ToolchainVersion identifies the language toolchains that produced the
	// cached output: the CLI's Go runtime signal, plus the resolved identity of
	// every runtime toolchain the task declares. Coverage instrumentation and
	// compiler output differ across toolchain versions, so a result cached under
	// one toolchain must not be served under another. A toolchain pin moves the
	// key through this field, whatever the extension's implementation digest.
	//
	// Its CLI part is the same on every machine: runtime.Version() is
	// "go1.24.0" on every operating system and architecture. Its runtime
	// toolchain part differs between platforms, because each resolved identity
	// hashes the host platform and that platform's lock integrity, so a task
	// that uses a runtime toolchain, its own or one its extension's runtime
	// declares for every task, keys differently on each platform. That is a
	// consequence of the toolchain identity, not a declaration: a task whose
	// output or verdict depends on the host platform says so through
	// RuntimeIdentity.
	ToolchainVersion string

	// RuntimeIdentity holds resolved `name=value` pairs for the ambient runtime
	// facts a task DECLARED as cache-key inputs (`inputs.<name>.from = "runtime"`).
	//
	// It exists because some outputs are a function of the machine and not only
	// of the sources: a host-platform compile produces Mach-O on darwin and ELF
	// on linux, and a host-platform compile CHECK returns a different verdict on
	// each, because `//go:build linux` files are only compiled on linux. Two
	// other fields differ between platforms as a consequence of what they
	// identify: ToolchainVersion for a task that uses a runtime toolchain, and
	// ExtensionImplementationDigest for an installed extension that ships
	// platform executables. A task with neither shares its entries between a
	// developer's laptop and CI on another platform. Neither field declares that
	// an output depends on the platform, so a task never relies on them to
	// separate platforms.
	//
	// Only tasks that declare a runtime input carry values here, and the hash
	// omits the field entirely when it is empty, so declaring one moves that
	// task's key alone and leaves every other key at its existing address.
	RuntimeIdentity []string

	// OSClass names the file model of the host that computed the key:
	// OSClassWindows on Windows, empty on every POSIX host. A Windows capture
	// records no executable bit and a Windows tool can emit backslash paths or
	// `.exe` names, so its entries must never serve a POSIX host, nor the reverse.
	// It is not the architecture: a task that depends on that declares the
	// hostPlatform runtime input. The hash omits the field when it is empty, so
	// every Linux and macOS key keeps its address.
	OSClass string

	// SourceState names what the workspace root can say about its own source:
	// SourceStateUnmanaged where Git does not manage it, empty inside a
	// repository. The scheduler stamps no source binding in the first state, so
	// an emitter writes its manifest without feature evidence there; an entry
	// stored in one state must never serve the other, in either direction. The
	// hash omits the field when it is empty, so every key computed inside a
	// repository keeps its address.
	SourceState string

	// EmptiedOutputs lists the ids of the declared directory outputs the engine
	// empties before the task executes (ADR 0064, ADR 0065). An entry captured
	// from an emptied directory holds only what its own run wrote; one
	// captured from a directory left as it stood can hold an earlier run's
	// files. The second kind must never serve a run that expects the first, so
	// a task with such an output keys apart from an engine that emptied
	// nothing. The hash omits the field when it is empty, so a task with no
	// such output keeps its address.
	EmptiedOutputs []string

	// Task is the task name (e.g., "build~transpile").
	Task string

	// TaskContractDigest is the canonical digest of the task's declared
	// contract (protocols/extension TaskContractDigest). It moves whenever the
	// manifest task's contract moves — command, args, ports, cache policy,
	// declared outputs and effects — so a contract change is a key change
	// (binding invariant 6): entries written under one contract can never
	// satisfy a task running under another. Empty for jobs with no manifest
	// task (CLI-synthesized jobs), which is itself a stable value for those
	// jobs.
	TaskContractDigest string

	// Project is the project name.
	Project string

	// ProjectMetadataDigest is the normalized workspace-probe digest of the
	// project's metadata and dependency edges
	// (workspace.Workspace.MetadataDigestFor).
	//
	// It closes the last "the inputs changed but the key did not" hole in this
	// key: the project's type, tags, publish channels, runs-with services,
	// requested extensions, source identity and resolved dependency PATHS all
	// select what a task does and what its output contains, and none of them
	// was reliably covered. FilePatterns cover them only when the extension
	// happens to glob the manifest file they live in, and a scope-level rename
	// or namePattern change is not in any project file at all. An earlier gap
	// was the same class one layer down: Go dependency edges that were
	// invisible to the key produced source-blind entries.
	//
	// It deliberately does NOT include the authored version (that is
	// EmbeddedVersion's job, and only for version-bearing tasks) or any
	// timestamp, absolute path, or stat value — the digest must be identical
	// for two checkouts of the same commit.
	ProjectMetadataDigest string

	// EmbeddedVersion is the version string that the task injects into its
	// output (e.g., via Go -X ldflags, or stamped into an npm/Go module
	// package): the line's base version and the per-commit suffix. Set when the
	// task signals a version-bearing output — either a non-empty `version-var`
	// (or `versionVar`) param or a `cache.versionAware` task policy; otherwise
	// empty. Including it in the key prevents serving cached bytes that embed a
	// stale version.
	//
	// It is the only field through which the commit or the line's base version
	// reaches a key, apart from a build stamp a generate asset copies into the
	// output (ExtraFiles). Every other field describes the tree, so a task that
	// leaves it empty keys the same at two commits that share one tree, whatever
	// base version their histories and tags resolve (ADR 0060).
	EmbeddedVersion string

	// SelectedProjects identifies the resolved command selection for a
	// workspace-aggregate job. Empty means the job is project-scoped or does not
	// observe selection.
	SelectedProjects []string

	// Params are the resolved parameters valid for the task and owning command.
	Params map[string]any

	// FilePatterns are glob patterns relative to ProjectRoot used to
	// select source files for hashing.
	FilePatterns []string

	// ConfigScope names the extension whose option layers a hashed project
	// config contributes to this key. The zero value keeps the file whole, so a
	// key built without it is the wider of the two and never the wrong one.
	ConfigScope ProjectConfigScope

	// InputExpander expands the input selectors FilePatterns and
	// WorkspaceFilePatterns name. Without one, a pattern set that names a
	// selector is an invalid input selection (ErrInputSelector). The hash reads
	// the files it lists, never the expander itself.
	InputExpander InputExpander

	// ProjectRoot is the absolute path to the project directory.
	ProjectRoot string

	// WorkspaceFilePatterns are glob patterns relative to WorkspaceRoot. They
	// model shared inputs such as a monorepo lockfile or root compiler config
	// without forcing every project to duplicate those files.
	WorkspaceFilePatterns []string

	// WorkspaceRoot is the absolute path to the workspace directory.
	WorkspaceRoot string

	// EnvVars are environment variable names whose values affect the cache.
	EnvVars []string

	// ExtraFiles are absolute paths to individual files or directories outside
	// the project root whose content should contribute to the cache key (e.g.,
	// cross-project generate assets). A path under WorkspaceRoot contributes
	// under its workspace-relative slash form, so the key does not depend on the
	// checkout directory; any other path contributes as given (hashExtraFiles).
	// A build stamp inside an asset keeps its commit fields, because the asset
	// copies it into the output.
	ExtraFiles []string

	// UpstreamHashes are hashes from upstream task outputs that this
	// task depends on.
	UpstreamHashes []string
}

// cacheKeyVersion prefixes every computed key, so an entry written under one
// key format is never served as a hit under another. It changes when a key
// computation change could serve an existing address under a different
// meaning. It stays when the change only moves keys to addresses no earlier
// entry occupies (ADR 0059). Each change is a one-time whole-cache miss. v9
// removed the line's base version, which moved every key (ADR 0060).
const cacheKeyVersion = "v9"

// ComputeHashUsing computes a SHA-256 cache key from all CacheKey fields,
// using the CacheManager's file hash memoization to avoid redundant I/O when
// multiple pipeline steps in the same project hash identical file patterns.
func (k *CacheKey) ComputeHashUsing(cm *CacheManager) (string, error) {
	h := sha256.New()

	writeField(h, cacheKeyVersion)
	writeField(h, k.Extension)
	// The version field is empty whenever a digest follows it, so the pair
	// (version, digest) is either (version, "") or ("", digest) and no key of
	// one shape equals a key of the other.
	if k.ExtensionImplementationDigest != "" {
		writeField(h, "")
	} else {
		writeField(h, k.ExtensionVersion)
	}
	writeField(h, k.ExtensionImplementationDigest)
	writeField(h, k.ToolchainVersion)
	// Written only when non-empty, and behind its own marker, so adding a
	// runtime input to one task cannot move any other task's key. This is the
	// same shape as the selectedProjects block below, and it is what lets a
	// host-platform declaration land without the whole-cache miss a
	// cacheKeyVersion bump would cost every user and every CI namespace.
	if len(k.RuntimeIdentity) > 0 {
		writeField(h, "runtimeIdentity")
		identity := append([]string(nil), k.RuntimeIdentity...)
		sort.Strings(identity)
		for _, value := range identity {
			writeField(h, value)
		}
	}
	// Same shape: absent on POSIX hosts, so their keys are unchanged, and
	// present on Windows, so no Windows key equals a POSIX one.
	if k.OSClass != "" {
		writeField(h, "osClass")
		writeField(h, k.OSClass)
	}
	// Same shape: absent inside a repository, so those keys are unchanged, and
	// present where Git does not manage the root, so no such key equals one.
	if k.SourceState != "" {
		writeField(h, "sourceState")
		writeField(h, k.SourceState)
	}
	// Same shape: absent for a task that owns no emptied directory, so its key
	// is unchanged, and present for one that does, so no such key equals a key
	// computed without the marker.
	if len(k.EmptiedOutputs) > 0 {
		writeField(h, "emptiedOutputs")
		emptied := append([]string(nil), k.EmptiedOutputs...)
		sort.Strings(emptied)
		for _, id := range emptied {
			writeField(h, id)
		}
	}
	writeField(h, k.Task)
	writeField(h, k.TaskContractDigest)
	writeField(h, k.Project)
	writeField(h, k.ProjectMetadataDigest)
	writeField(h, k.EmbeddedVersion)
	if len(k.SelectedProjects) > 0 {
		writeField(h, "selectedProjects")
		selected := append([]string(nil), k.SelectedProjects...)
		sort.Strings(selected)
		for _, project := range selected {
			writeField(h, project)
		}
	}

	// Stable params hash
	paramsHash := hashParams(k.Params)
	writeField(h, paramsHash)

	// File content hash
	if k.ProjectRoot != "" {
		fileHash, err := cm.lookupFileHash(k.ProjectRoot, k.FilePatterns, k.InputExpander, k.ConfigScope)
		if err != nil {
			return "", fmt.Errorf("hash files: %w", err)
		}
		writeField(h, fileHash)
	}

	if k.WorkspaceRoot != "" && len(k.WorkspaceFilePatterns) > 0 {
		// Every project config under the workspace root belongs to some other
		// project, so a workspace-scoped pattern reads it whole.
		workspaceHash, err := cm.lookupFileHash(k.WorkspaceRoot, k.WorkspaceFilePatterns, k.InputExpander,
			ProjectConfigScope{Verbatim: k.ConfigScope.Verbatim})
		if err != nil {
			return "", fmt.Errorf("hash workspace files: %w", err)
		}
		writeField(h, "workspaceFiles")
		writeField(h, workspaceHash)
	}

	// Extra files outside the project root (e.g., cross-project generate
	// assets) — memoized through the CacheManager like the project file hash,
	// so each asset tree is walked and read once per CLI invocation instead of
	// once per (job × precompute/execute) key computation.
	if len(k.ExtraFiles) > 0 {
		extraHash := cm.lookupExtraFilesHash(k.WorkspaceRoot, k.ExtraFiles)
		writeField(h, extraHash)
	}

	// Environment variables
	if len(k.EnvVars) > 0 {
		envHash := hashEnvVars(k.EnvVars)
		writeField(h, envHash)
	}

	// Upstream hashes — sort for determinism since dependency resolution
	// order may vary between runs (Go map iteration is non-deterministic).
	if len(k.UpstreamHashes) > 0 {
		sorted := make([]string, len(k.UpstreamHashes))
		copy(sorted, k.UpstreamHashes)
		sort.Strings(sorted)
		for _, uh := range sorted {
			writeField(h, uh)
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// Store returns the underlying store, or nil for a nil manager (caching off),
// so a caller can hand it to NewRunBudget without a branch.
func (cm *CacheManager) Store() *LocalStore {
	if cm == nil {
		return nil
	}
	return cm.store
}

// StoreRoot returns the absolute CAS root backing this manager, so callers
// resolve blob paths into the same store.
func (cm *CacheManager) StoreRoot() string {
	return cm.store.Root()
}

// BuildCacheKey constructs a CacheKey from job execution parameters.
// embeddedVersion should be set (typically to the project's Full version) when
// the task injects the version string into its output, so successive builds
// with different commit suffixes don't share cached bytes; otherwise empty.
func BuildCacheKey(
	extensionName, extensionVersion, extensionImplementationDigest, toolchainVersion, taskName, taskContractDigest, projectName, projectMetadataDigest, embeddedVersion string,
	selectedProjects []string,
	params map[string]any,
	projectRoot, workspaceRoot string,
	cachePolicy CacheKeyPolicy,
	upstreamHashes []string,
) *CacheKey {
	return &CacheKey{
		Extension:                     extensionName,
		ExtensionVersion:              extensionVersion,
		ExtensionImplementationDigest: extensionImplementationDigest,
		ToolchainVersion:              toolchainVersion,
		RuntimeIdentity:               cachePolicy.RuntimeIdentity,
		OSClass:                       osClassFor(runtime.GOOS),
		Task:                          taskName,
		TaskContractDigest:            taskContractDigest,
		Project:                       projectName,
		ProjectMetadataDigest:         projectMetadataDigest,
		EmbeddedVersion:               embeddedVersion,
		SelectedProjects:              selectedProjects,
		Params:                        params,
		FilePatterns:                  cachePolicy.Files,
		ProjectRoot:                   projectRoot,
		WorkspaceFilePatterns:         cachePolicy.WorkspaceFiles,
		WorkspaceRoot:                 workspaceRoot,
		EnvVars:                       cachePolicy.Env,
		ExtraFiles:                    cachePolicy.ExtraFiles,
		ConfigScope:                   cachePolicy.ConfigScope,
		InputExpander:                 cachePolicy.InputExpander,
		UpstreamHashes:                upstreamHashes,
	}
}

// SourceStateUnmanaged is the CacheKey.SourceState of a workspace root Git does
// not manage: no git program is on PATH, or the root is outside every
// repository.
const SourceStateUnmanaged = "unmanaged"

// SourceState returns the CacheKey.SourceState of workspaceRoot. It is the one
// answer of the manager's invocation: the execution keys and the scheduler's
// version stamp both read it, so an output stamped with no source claim is
// never stored under a key computed inside a repository. Git is asked at most
// once per root for the life of the manager, and not at all for a root
// RecordManagedRoot named. The lock is held while Git answers, so concurrent
// callers wait for that answer instead of asking again.
//
// A git failure that is neither a missing program nor a missing repository
// reads as managed: the git commands that follow fail with their own detail.
// A nil manager has no answer to share and asks Git on every call.
func (cm *CacheManager) SourceState(workspaceRoot string) string {
	root := filepath.Clean(workspaceRoot)
	if cm != nil {
		cm.sourceStateMu.Lock()
		defer cm.sourceStateMu.Unlock()
		if state, ok := cm.sourceStates[root]; ok {
			return state
		}
	}
	state := ""
	if gitread.Unmanaged(root) != nil {
		state = SourceStateUnmanaged
	}
	if cm != nil {
		if cm.sourceStates == nil {
			cm.sourceStates = make(map[string]string)
		}
		cm.sourceStates[root] = state
	}
	return state
}

// RecordManagedRoot records that Git manages workspaceRoot, learned from a git
// command that already succeeded there, so SourceState answers for it without
// starting a git process. An answer the manager already holds is kept: one
// invocation has one answer per root.
func (cm *CacheManager) RecordManagedRoot(workspaceRoot string) {
	root := filepath.Clean(workspaceRoot)
	cm.sourceStateMu.Lock()
	defer cm.sourceStateMu.Unlock()
	if _, ok := cm.sourceStates[root]; ok {
		return
	}
	if cm.sourceStates == nil {
		cm.sourceStates = make(map[string]string)
	}
	cm.sourceStates[root] = ""
}

// OSClassWindows is the CacheKey.OSClass value of every Windows host.
const OSClassWindows = "windows"

// osClassFor maps a GOOS to its CacheKey.OSClass: OSClassWindows for windows on
// any architecture, and empty for every POSIX system, present or future.
// BuildCacheKey passes runtime.GOOS.
func osClassFor(goos string) string {
	if goos == "windows" {
		return OSClassWindows
	}
	return ""
}

// CacheKeyPolicy holds the policy for computing cache keys, extracted from
// extension manifests.
type CacheKeyPolicy struct {
	Files           []string
	WorkspaceFiles  []string
	Env             []string
	ExtraFiles      []string // absolute paths outside the project root, keyed workspace-relative when under the workspace root
	RuntimeIdentity []string // resolved `name=value` pairs for declared runtime inputs
	// ConfigScope names the extension whose option layers a hashed project
	// config contributes. Empty keeps the file whole.
	ConfigScope ProjectConfigScope
	// InputExpander expands the input selectors Files and WorkspaceFiles name.
	InputExpander InputExpander
}
