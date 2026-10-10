package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	gitutil "go.putnami.dev/tooling/cli/internal/git"
)

// CacheManager integrates the LocalStore with job execution to provide
// transparent caching. It handles cache key computation, hit/miss
// detection, result restoration, and output storage.
type CacheManager struct {
	store              *LocalStore
	fileHashCache      map[string]string // memoizes file hashes: "dir|patterns" → hash
	fileHashMu         sync.Mutex
	extraFileHashCache map[string]string // memoizes extra-file hashes: workspace root and sorted paths, NUL-joined → hash
	extraFileHashMu    sync.Mutex
	sourceStates       map[string]string // memoizes SourceState: workspace root → state
	sourceStateMu      sync.Mutex
	digests            map[string]*memoizedDigest // memoizes Digest: key → one computation
	digestsMu          sync.Mutex
}

// memoizedDigest is one key's computation in CacheManager.Digest.
type memoizedDigest struct {
	once  sync.Once
	value string
	err   error
}

// NewCacheManager creates a CacheManager backed by the given store.
func NewCacheManager(store *LocalStore) *CacheManager {
	return &CacheManager{
		store:              store,
		fileHashCache:      make(map[string]string),
		extraFileHashCache: make(map[string]string),
	}
}

// lookupFileHash computes a combined hash for files matching patterns
// using file contents, with per-session memoization. Multiple pipeline
// steps in the same project with identical file patterns reuse the
// cached result instead of re-reading and re-hashing all source files.
func (cm *CacheManager) lookupFileHash(dir string, patterns []string, scope ProjectConfigScope) (string, error) {
	// The scope selects which bytes of a project config the digest reads, so two
	// scopes are two different values and must never share one memo entry.
	key := dir + "|" + strings.Join(patterns, ",") + "|" + strings.Join(scope.ExtensionLayers, "\x00") +
		"|" + strings.Join(scope.ForeignNamespaces, "\x00")
	if scope.Verbatim {
		key += "|verbatim"
	}

	cm.fileHashMu.Lock()
	if cached, ok := cm.fileHashCache[key]; ok {
		cm.fileHashMu.Unlock()
		return cached, nil
	}
	cm.fileHashMu.Unlock()

	hash, err := hashFiles(dir, patterns, scope)
	if err != nil {
		return "", err
	}

	cm.fileHashMu.Lock()
	cm.fileHashCache[key] = hash
	cm.fileHashMu.Unlock()

	return hash, nil
}

// HashFiles returns the memoized content digest for files selected under dir,
// reading a project config through the given task's option scope. A zero scope
// keeps every project config whole, which is the wider digest of the two.
//
// Scheduler source-rewriter handling captures this value before execution and
// compares it after invalidating the memo, so only actual keyed source edits
// make a clean status entry non-restorable. It must pass the SAME scope the key
// used: a narrower detector misses keyed edits, and a wider one reports edits
// the key ignores. A source rewriter's scope is Verbatim, so a layout-only
// rewrite of a project config is a keyed mutation.
func (cm *CacheManager) HashFiles(dir string, patterns []string, scope ProjectConfigScope) (string, error) {
	if cm == nil {
		return "", fmt.Errorf("cache manager is nil")
	}
	return cm.lookupFileHash(dir, patterns, scope)
}

// InvalidateFileHashes drops the per-session input-hash memo after a task has
// changed source files. The cache normally assumes file inputs stay stable for
// a scheduler run; source-writing fix tasks are the deliberate exception.
// Clearing the small memo ensures both their post-run mutation check and every
// downstream cache lookup observe the rewritten tree rather than pre-fix bytes.
func (cm *CacheManager) InvalidateFileHashes() {
	if cm == nil {
		return
	}
	cm.fileHashMu.Lock()
	clear(cm.fileHashCache)
	cm.fileHashMu.Unlock()
}

// lookupExtraFilesHash computes the combined content hash of a set of extra
// file paths (cross-project generate assets), with per-session memoization
// mirroring lookupFileHash. hashExtraFiles walks each directory asset
// recursively and reads every byte beneath it, and the same ExtraFiles set is
// hashed once at key-precompute time and again at execute-time lookup for
// every cacheable step that declares it — the memo turns those repeated full
// tree reads into one per CLI invocation. The memo key is the workspace root
// followed by the sorted path set, joined with NUL (which cannot occur in a
// path), so distinct sets never collide and one set named against two roots
// never shares an entry; hashExtraFiles is order-independent, so two orderings
// of the same set correctly share one entry. The cached value is byte-identical
// to hashExtraFiles(workspaceRoot, paths).
//
// The memo assumes ExtraFiles are stable for the lifetime of a CLI invocation —
// cross-project *source* assets (committed docs, `build.assets` public trees),
// not a file another job regenerates mid-run. A file produced during the run
// and declared as an ExtraFile without a modeled dependency edge would keep its
// precompute-time hash at execute-time lookup, so its later content is not seen;
// route such intra-run outputs through the normal dependency graph (which
// carries the producing job's hash) rather than declaring them as ExtraFiles.
func (cm *CacheManager) lookupExtraFilesHash(workspaceRoot string, paths []string) string {
	sorted := make([]string, len(paths))
	copy(sorted, paths)
	sort.Strings(sorted)
	key := workspaceRoot + "\x00" + strings.Join(sorted, "\x00")

	cm.extraFileHashMu.Lock()
	if cached, ok := cm.extraFileHashCache[key]; ok {
		cm.extraFileHashMu.Unlock()
		return cached
	}
	cm.extraFileHashMu.Unlock()

	hash := hashExtraFiles(workspaceRoot, sorted)

	cm.extraFileHashMu.Lock()
	cm.extraFileHashCache[key] = hash
	cm.extraFileHashMu.Unlock()

	return hash
}

// Digest returns the value compute derives for key, computing it at most once
// for the life of the manager: the first caller computes, and concurrent
// callers for the same key wait for that answer instead of computing again.
// An error is the key's answer too, so one invocation never keys one input two
// ways. A nil manager has no answer to share and computes on every call.
//
// The memo lives as long as the manager, one per run, so a value derived from
// files that may change between runs is derived again by the next run.
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
		fileHash, err := cm.lookupFileHash(k.ProjectRoot, k.FilePatterns, k.ConfigScope)
		if err != nil {
			return "", fmt.Errorf("hash files: %w", err)
		}
		writeField(h, fileHash)
	}

	if k.WorkspaceRoot != "" && len(k.WorkspaceFilePatterns) > 0 {
		// Every project config under the workspace root belongs to some other
		// project, so a workspace-scoped pattern reads it whole.
		workspaceHash, err := cm.lookupFileHash(k.WorkspaceRoot, k.WorkspaceFilePatterns,
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

// Lookup checks for a cache hit. Returns the entry if found, nil if miss.
func (cm *CacheManager) Lookup(hash string) (*Entry, error) {
	return cm.store.Get(hash)
}

// LookupAndTouch is Lookup plus an atomic lastUsed stamp under the store's
// shared lock, so a concurrent GC cannot evict the entry between the read and
// the stamp (closing the cold-entry hit-vs-evict race). Use on the cache-hit
// path instead of Lookup+MarkUsed.
func (cm *CacheManager) LookupAndTouch(hash string) (*Entry, error) {
	return cm.store.getAndTouch(hash)
}

// Save stores a legacy entry, capturing outputDir whole. New scheduler entries
// use the task-owned format; this remains for store compatibility APIs.
func (cm *CacheManager) Save(hash string, result *EntryResult, meta *EntryMetadata, outputDir string) error {
	entry := &Entry{
		Result:   result,
		Metadata: meta,
	}

	// If outputDir exists and has files, include them
	if outputDir != "" {
		if info, err := os.Stat(outputDir); err == nil && info.IsDir() {
			// Check if directory is non-empty
			entries, _ := os.ReadDir(outputDir)
			if len(entries) > 0 {
				entry.FilesDir = outputDir
			}
		}
	}

	return cm.store.Put(hash, entry)
}

// SaveDirs stores a job result whose output spans several sibling directories,
// each captured under its own subpath ("<id>/…") in the entry's files/ tree.
// dirs maps a stable subpath id (e.g. "gen", "clients") to the absolute source
// directory; sources that are absent or empty are skipped. It stages a union
// directory and reuses Put's CAS ingest + manifest + first-writer-wins publish
// unchanged, so the resulting entry is an ordinary blob the remote cache, GC, and
// Materialize treat exactly like a single-dir entry — only the per-subpath split
// (reconstructed by RestoreSubdir) is new. The extra staging copy is negligible
// for generate outputs (small .gen + client trees) and keeps the battle-tested
// ingest/publish path untouched. Used when one job (the TS generate step)
// produces several capture-resource trees at once (.gen and clients/).
func (cm *CacheManager) SaveDirs(hash string, result *EntryResult, meta *EntryMetadata, dirs map[string]string) error {
	staging, err := cm.store.createTmpDir()
	if err != nil {
		return fmt.Errorf("stage output dirs: %w", err)
	}
	defer os.RemoveAll(staging)

	// Deterministic order keeps the staging step stable across runs.
	ids := make([]string, 0, len(dirs))
	for id := range dirs {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	staged := false
	for _, id := range ids {
		src := dirs[id]
		info, statErr := os.Stat(src)
		if statErr != nil || !info.IsDir() {
			continue
		}
		entries, _ := os.ReadDir(src)
		if len(entries) == 0 {
			continue
		}
		if err := copyDir(src, filepath.Join(staging, id)); err != nil {
			return fmt.Errorf("stage %q: %w", id, err)
		}
		staged = true
	}

	entry := &Entry{Result: result, Metadata: meta}
	if staged {
		entry.FilesDir = staging
	}
	return cm.store.Put(hash, entry)
}

// RestoreFiles copies cached output files to the target output directory.
// Returns true if files were restored, false if there were no files to restore.
func (cm *CacheManager) RestoreFiles(entry *Entry, targetDir string) (bool, error) {
	if entry.FilesDir == "" {
		return false, nil
	}

	// Ensure target directory exists
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return false, fmt.Errorf("create target dir: %w", err)
	}

	// Copy files from cache to target
	if err := copyDir(entry.FilesDir, targetDir); err != nil {
		return false, fmt.Errorf("restore files: %w", err)
	}

	return true, nil
}

// RestoreDir replaces targetDir with the entry's cached output files, so the
// directory is byte-identical to the snapshot taken at store time rather than an
// overlay onto whatever is already there. Use it for captured directory outputs
// (e.g. a project's .gen tree) whose consumers expect exactly the tree a fresh
// producing run would leave — overlaying could leave a stale file behind.
// Returns false (no error) when the entry has no files.
func (cm *CacheManager) RestoreDir(entry *Entry, targetDir string) (bool, error) {
	return cm.RestoreDirPreserving(entry, targetDir, nil)
}

// RestoreDirPreserving is RestoreDir with an explicit set of post-capture
// files to ignore during manifest comparison and carry forward during a full
// replacement. preserve receives slash-normalized paths relative to targetDir.
func (cm *CacheManager) RestoreDirPreserving(
	entry *Entry,
	targetDir string,
	preserve func(rel string) bool,
) (bool, error) {
	if entry.FilesDir == "" {
		return false, nil
	}
	if dirMatchesManifest(targetDir, entry.Manifest, "", preserve) {
		return true, nil
	}
	if err := replaceDirPreserving(entry.FilesDir, targetDir, preserve); err != nil {
		return false, fmt.Errorf("restore dir: %w", err)
	}
	return true, nil
}

// RestoreSubdir replaces targetDir with the entry's cached "<subpath>/" subtree
// so targetDir is byte-identical to the snapshot (stale files removed). It is the
// multi-rooted counterpart of RestoreDir for entries written by SaveDirs.
// Returns (false, nil) when the entry has no files or did not capture that
// subpath (e.g. a generate run that produced no client) — restoring a resource
// the producing run never emitted is a no-op, not an error.
func (cm *CacheManager) RestoreSubdir(entry *Entry, subpath, targetDir string) (bool, error) {
	return cm.RestoreSubdirPreserving(entry, subpath, targetDir, nil)
}

// RestoreSubdirPreserving is the multi-rooted counterpart of
// RestoreDirPreserving. preserve paths are relative to targetDir after subpath
// has been stripped from the stored manifest.
func (cm *CacheManager) RestoreSubdirPreserving(
	entry *Entry,
	subpath, targetDir string,
	preserve func(rel string) bool,
) (bool, error) {
	if entry.FilesDir == "" {
		return false, nil
	}
	src := filepath.Join(entry.FilesDir, subpath)
	if info, err := os.Stat(src); err != nil || !info.IsDir() {
		return false, nil
	}
	if dirMatchesManifest(targetDir, entry.Manifest, subpath, preserve) {
		return true, nil
	}
	if err := replaceDirPreserving(src, targetDir, preserve); err != nil {
		return false, fmt.Errorf("restore subdir %q: %w", subpath, err)
	}
	return true, nil
}

// EntryMatchesDir reports whether targetDir already contains exactly the files
// represented by entry. Throwaway artifacts excluded during cache ingest are
// ignored by the same rule during comparison.
func (cm *CacheManager) EntryMatchesDir(entry *Entry, targetDir string) bool {
	return entry != nil && entry.FilesDir != "" && dirMatchesManifest(targetDir, entry.Manifest, "", nil)
}

// OverlayDir copies the entry's cached output files INTO targetDir, merging with
// (not replacing) whatever is already there. Unlike RestoreDir - which removes
// targetDir first so it ends up byte-identical to the snapshot - overlay is the
// right primitive when the directory is co-owned by several steps of one command
// that cache independently: a shared command-output dir may already hold a sibling
// step's real output (put there by a cache miss) that a full replace would delete.
// Files the blob also contains are overwritten; for a deterministic task the cached
// copy is byte-identical, so overlay converges to the same tree a fresh run leaves,
// plus any sibling files the snapshot did not include. Returns false (no error)
// when the entry has no files.
func (cm *CacheManager) OverlayDir(entry *Entry, targetDir string) (bool, error) {
	if entry.FilesDir == "" {
		return false, nil
	}
	// targetDir is a workspace path being rebuilt from a blob the CAS still
	// holds, so skip the per-file fsync (see copyDirUnsynced).
	if err := copyDirUnsynced(entry.FilesDir, targetDir); err != nil {
		return false, fmt.Errorf("overlay dir: %w", err)
	}
	return true, nil
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
// (e.g. the scheduler's OutManager) resolve blob paths into the same store.
func (cm *CacheManager) StoreRoot() string {
	return cm.store.Root()
}

// MarkUsed stamps an entry's lastUsed time on a cache hit so GC's recency
// ordering and grace window account for it. Best-effort and cheap.
func (cm *CacheManager) MarkUsed(hash string) {
	cm.store.markUsed(hash)
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
	if gitutil.Unmanaged(root) != nil {
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
}

// CacheHitResult holds the outcome of a cache lookup for a job.
type CacheHitResult struct {
	Hit      bool
	Hash     string
	Entry    *Entry
	Duration time.Duration
}
