package store

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/sdk/extension/goembed"
	gitutil "go.putnami.dev/tooling/cli/internal/git"
)

// --- hash helpers ---

// writeField writes a string field to a hash, separated by a null byte.
func writeField(h io.Writer, s string) {
	h.Write([]byte(s)) //nolint:errcheck // hash.Hash.Write never errors
	h.Write([]byte{0}) //nolint:errcheck // hash.Hash.Write never errors
}

// hashParams computes a deterministic hash of the task parameters its caller
// already projected from the task contract and owning command. Selection policy
// does not belong in the content-addressed store: every value the jobs layer
// admits is semantic even when its name resembles an operational flag.
func hashParams(params map[string]any) string {
	if len(params) == 0 {
		return ""
	}

	// Sort keys for determinism
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	h := sha256.New()
	for _, k := range keys {
		h.Write([]byte(k))
		h.Write([]byte{0})
		v, _ := json.Marshal(params[k])
		h.Write(v)
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
}

// fileEntry pairs a path with its stat info to avoid double-stat.
type fileEntry struct {
	path         string
	info         os.FileInfo
	gitCandidate bool
	rawEmbed     bool
	// sweptByGlob is set when a pattern whose last segment is a glob selected
	// the file (`**/*.json`, `*.json`, `doc/**`). Such a pattern selects files by
	// type or by directory, so the task reads what it selects as text, and a
	// project config it selects is hashed as raw bytes. A pattern that names
	// the file (`putnami.json`, `**/putnami.json`) keeps the decoded view.
	sweptByGlob bool
}

// globDoubleStar expands a pattern containing "**" by walking the tree under
// dir. The segment before "**" is treated as a literal directory prefix.
// Heavy non-project directories are skipped for performance.
func globDoubleStar(dir, pattern string) []string {
	pattern = filepath.ToSlash(pattern)
	parts := strings.SplitN(pattern, "**", 2)
	prefix := strings.Trim(strings.TrimSuffix(parts[0], "/"), "/")

	searchRoot := dir
	if prefix != "" {
		searchRoot = filepath.Join(dir, filepath.FromSlash(prefix))
	}

	var matches []string
	_ = filepath.WalkDir(searchRoot, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			switch d.Name() {
			case "node_modules", ".git", ".putnami", "out", "dist", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(dir, p)
		if relErr != nil {
			return nil
		}
		if wsproto.MatchFilePattern(filepath.ToSlash(rel), pattern) {
			matches = append(matches, p)
		}
		return nil
	})
	return matches
}

// collectFiles returns a sorted, deduplicated list of files matching
// the given glob patterns under dir. If patterns is empty, all
// non-hidden, non-ignored files are returned. FileInfo is captured
// during collection to avoid redundant stat calls.
//
// Pattern semantics:
//   - "**" matches any number of path segments (recursive). Go's
//     filepath.Glob stops at a single segment, so patterns such as
//     "**/*.go" are expanded by walking the tree.
//   - A pattern prefixed with "!" excludes files that match the remainder,
//     e.g. ["**/*.go", "!**/*_test.go"] hashes every Go source except tests.
//   - Patterns without "**" keep their previous filepath.Glob behavior, so
//     cache keys that do not use "**" or "!" are unchanged.
//   - A pattern whose last segment is a glob marks the files it selects as
//     sweptByGlob. A file two patterns select keeps the mark if either sets it.
func collectFiles(dir string, patterns []string) ([]fileEntry, error) {
	dir, err := gitInputDirectory(dir, patterns)
	if err != nil {
		return nil, err
	}
	var files []fileEntry
	includes, excludes := wsproto.SplitFilePatterns(patterns)
	var ordinaryPatterns, gitPatterns []string
	var embedPatterns []string
	for _, pattern := range includes {
		if _, ok := wsproto.GitFilePattern(pattern); ok {
			gitPatterns = append(gitPatterns, pattern)
		} else if goembed.IsSelector(pattern) {
			embedPatterns = append(embedPatterns, pattern)
		} else if strings.HasPrefix(pattern, "go-embed:") {
			return nil, fmt.Errorf("unsupported Go embed input selector %q", pattern)
		} else {
			ordinaryPatterns = append(ordinaryPatterns, pattern)
		}
	}

	if len(patterns) == 0 {
		err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return nil
			}
			name := info.Name()
			if strings.HasPrefix(name, ".") || name == "node_modules" || name == ".putnami" || name == "out" {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			if !info.IsDir() {
				files = append(files, fileEntry{path: path, info: info})
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	} else {
		for _, pattern := range ordinaryPatterns {
			slashed := filepath.ToSlash(pattern)
			sweptByGlob := strings.ContainsAny(slashed[strings.LastIndex(slashed, "/")+1:], "*?[")
			var matches []string
			if strings.Contains(pattern, "**") {
				matches = globDoubleStar(dir, pattern)
			} else {
				matches, _ = filepath.Glob(filepath.Join(dir, pattern))
			}
			for _, m := range matches {
				info, err := os.Stat(m)
				if err != nil || info.IsDir() {
					continue
				}
				if len(excludes) > 0 {
					rel, _ := filepath.Rel(dir, m)
					if wsproto.MatchesAnyFilePattern(filepath.ToSlash(rel), excludes) {
						continue
					}
				}
				files = append(files, fileEntry{path: m, info: info, sweptByGlob: sweptByGlob})
			}
		}
	}
	if len(gitPatterns) > 0 {
		candidates, err := collectGitFiles(dir, gitPatterns, excludes)
		if err != nil {
			return nil, err
		}
		files = append(files, candidates...)
	}
	for _, selector := range embedPatterns {
		targets, err := goembed.Resolve(dir, selector == goembed.TestSelector)
		if err != nil {
			return nil, fmt.Errorf("resolve %s inputs: %w", selector, err)
		}
		for _, target := range targets {
			info, err := os.Lstat(target)
			if err != nil {
				return nil, err
			}
			files = append(files, fileEntry{path: target, info: info, rawEmbed: true})
		}
	}

	sort.Slice(files, func(i, j int) bool { return slashPathLess(files[i].path, files[j].path, filepath.Separator) })

	// Deduplicate
	if len(files) > 1 {
		j := 0
		for i := 1; i < len(files); i++ {
			if files[i].path != files[j].path {
				j++
				files[j] = files[i]
			} else if files[i].gitCandidate {
				// Raw Git bytes win over a normal pattern's semantic digest,
				// independent of declaration and sort order.
				files[j] = files[i]
			} else if files[i].sweptByGlob {
				// A raw reading wins over a decoded one the same way.
				files[j].sweptByGlob = true
			}
			if files[i].rawEmbed {
				files[j].rawEmbed = true
			}
		}
		files = files[:j+1]
	}

	return files, nil
}

// slashPathLess orders two paths as their slash forms compare, reading
// separator as '/'. The key folds files in this order, so one tree folds in one
// order whatever the host separator. It compares whole absolute paths, never
// paths relative to the hashed directory: a "../" input outside that directory
// sorts where its absolute path does. Two spellings of one slash form order by
// their raw bytes, so the order stays total and equal paths stay adjacent. With
// separator '/' it is plain string order.
func slashPathLess(a, b string, separator byte) bool {
	if separator == '/' {
		return a < b
	}
	for i := 0; i < len(a) && i < len(b); i++ {
		x, y := a[i], b[i]
		if x == separator {
			x = '/'
		}
		if y == separator {
			y = '/'
		}
		if x != y {
			return x < y
		}
	}
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// Git reports physical repository paths. Collection and hashing must use the
// same physical project directory too: cleaning alias/../../file before
// resolving alias would point outside the repository. The directory resolves
// through dirlink, because Git for Windows follows a junction in
// --show-toplevel and filepath.EvalSymlinks does not. Ordinary inputs retain
// their existing directory semantics when no Git source is declared.
func gitInputDirectory(dir string, patterns []string) (string, error) {
	for _, pattern := range patterns {
		if _, ok := wsproto.GitFilePattern(pattern); !ok {
			continue
		}
		absolute, err := filepath.Abs(dir)
		if err != nil {
			return "", err
		}
		return dirlink.Resolve(absolute)
	}
	return dir, nil
}

// collectGitFiles enumerates only candidate names, never walks ignored local
// directories, and uses Lstat so a link's target text is the input. The scanner
// reads exactly this candidate surface, including tracked ignored files.
func collectGitFiles(dir string, includes, excludes []string) ([]fileEntry, error) {
	cmd := exec.Command("git", "rev-parse", "--show-toplevel")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("resolve Git input repository: %w", err)
	}
	root := strings.TrimSuffix(string(out), "\n")
	paths, err := gitutil.CandidatePaths(root)
	if err != nil {
		return nil, err
	}
	var files []fileEntry
	for _, path := range paths {
		rel, err := filepath.Rel(dir, filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			return nil, err
		}
		if !wsproto.MatchesAnyFilePattern(rel, includes) || wsproto.MatchesAnyFilePattern(rel, excludes) {
			continue
		}
		full := filepath.Join(dir, rel)
		info, err := os.Lstat(full)
		if os.IsNotExist(err) {
			continue // An unstaged tracked deletion is absent from the cut.
		}
		if err != nil {
			return nil, fmt.Errorf("git candidate %q: %w", path, err)
		}
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil, fmt.Errorf("git candidate %q is not a regular file or symlink", path)
		}
		files = append(files, fileEntry{path: full, info: info, gitCandidate: true})
	}
	return files, nil
}

// Candidate readers inspect raw bytes: project-config task tuning and version
// stamp buildTime are visible too. Domain-separate types so replacing a file
// with a symlink holding the same bytes also invalidates its verdict.
func gitCandidateDigest(file fileEntry, buf []byte) []byte {
	h := sha256.New()
	if file.info.Mode()&os.ModeSymlink != 0 {
		target, err := os.Readlink(file.path)
		if err != nil {
			return nil
		}
		writeField(h, "git-symlink")
		writeField(h, target)
		return h.Sum(nil)
	}
	input, err := os.Open(file.path)
	if err != nil {
		return nil
	}
	defer input.Close()
	writeField(h, "git-file")
	if _, err := io.CopyBuffer(h, input, buf); err != nil {
		return nil
	}
	return h.Sum(nil)
}

// CollectKeyFiles returns the sorted absolute paths of every file the
// project-side input patterns select under dir — exactly the set hashFiles
// folds into a cache key, through the same collector, so a caller that needs
// to know WHICH files a key reads (the portable admission binding required
// ignored inputs) cannot disagree with the key about it. An empty pattern set
// keeps its key meaning: the whole non-hidden project tree.
func CollectKeyFiles(dir string, patterns []string) ([]string, error) {
	files, err := collectFiles(dir, patterns)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.path)
	}
	return paths, nil
}

// ExtraKeyFiles returns the sorted absolute paths of every regular file an
// extra-file declaration (a cross-project generate asset) folds into a cache
// key: the path itself when it is a file, every file beneath it when it is a
// directory, nothing when it is absent — the walk hashPathContent performs,
// so the two cannot drift apart.
func ExtraKeyFiles(paths []string) []string {
	var files []string
	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			continue
		}
		if !info.IsDir() {
			files = append(files, p)
			continue
		}
		_ = filepath.WalkDir(p, func(path string, d os.DirEntry, walkErr error) error {
			if walkErr != nil || d.IsDir() {
				return nil
			}
			files = append(files, path)
			return nil
		})
	}
	sort.Strings(files)
	return files
}

// SelectsPath reports whether the project-side input patterns select one named
// path, without touching the filesystem. rel is the path in the form
// filepath.Rel produces against the project root, so a keyed file OUTSIDE that
// root keeps its "../" prefix exactly as collectFiles sees it.
//
// It answers "is this file a cache-key input?" for a caller that owns a file
// whose content must never be answered from cache — a recorded verdict, a
// committed baseline — and wants to assert that in a test rather than trust a
// hand-maintained pattern list. It shares the protocol's file-pattern grammar
// with collectFiles so the answer cannot drift from the selection it describes,
// and all three now delegate to the protocol's one pattern grammar, which
// --impacted reads the same declaration through.
func SelectsPath(rel string, patterns []string) bool {
	return wsproto.SelectsPath(rel, patterns)
}

// HasMatchingFiles reports whether the project-side input patterns select at
// least one input. Batch dispatch uses this as a conservative guard:
// many tools report an unmatched project only when it runs alone, so folding
// an empty root into a non-empty group could hide that project's failure.
func HasMatchingFiles(dir string, patterns []string) (bool, error) {
	for _, pattern := range patterns {
		_, gitCandidate := wsproto.GitFilePattern(pattern)
		if gitCandidate || goembed.IsSelector(pattern) || strings.HasPrefix(pattern, "go-embed:") {
			files, err := collectFiles(dir, patterns)
			return len(files) > 0, err
		}
	}
	includes, excludes := wsproto.SplitFilePatterns(patterns)
	matched := false
	err := filepath.WalkDir(dir, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if entry.IsDir() {
			if filePath == dir {
				return nil
			}
			switch entry.Name() {
			case "node_modules", ".git", ".putnami", "out", "dist", "vendor":
				return filepath.SkipDir
			}
			if strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(entry.Name(), ".") {
			return nil
		}
		rel, relErr := filepath.Rel(dir, filePath)
		if relErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if len(includes) > 0 && !wsproto.MatchesAnyFilePattern(rel, includes) {
			return nil
		}
		if wsproto.MatchesAnyFilePattern(rel, excludes) {
			return nil
		}
		matched = true
		return filepath.SkipAll
	})
	if err != nil {
		return false, err
	}
	return matched, nil
}

// unreadableSentinel is the byte string a selected-but-unreadable file
// contributes to a hash in place of its content digest. Both hashing entry
// points (hashFiles for keyed project inputs, hashFileBytes for extra-file
// assets) share it so they cannot drift apart: a file that cannot be read must
// hash differently from one that is absent and from any real content, whichever
// path folded it in.
const unreadableSentinel = "__unreadable__"

// hashFiles computes a SHA-256 hash of file contents matching glob patterns.
//
// Reading the file bytes off disk dominates the cost, so the per-file content
// digests are computed concurrently with a bounded worker pool and then folded
// into the final hash in sorted-path order. collectFiles already returns files
// sorted by path, so the fold order — and therefore the resulting hash — stays
// deterministic regardless of the order workers finish in.
//
// A file that collectFiles selected but no worker could read contributes its
// relative path plus unreadableSentinel. Dropping it entirely — the
// previous behavior — made the key byte-identical to the key for a tree in
// which that file does not exist, so a build produced without the file could be
// served as a hit for a tree that has it. collectFiles stats each match and the
// pool opens it later, so the TOCTOU window is real and watch mode sits inside
// it: an editor's atomic-save rename, a concurrent generate rewriting the file,
// or an NFS/permission blip all yield a nil digest. The permanent variant needs
// no race — a source file the running user cannot read (mode 0600 in a shared
// checkout, a restrictive ACL) was excluded from every key forever, so edits to
// it never invalidated anything.
//
// A sentinel rather than an error: hashFiles' error return is swallowed by
// closureInputsDigest (jobs/executor.go), which drops the whole member from the
// closure digest on error and so reproduces exactly this silent-drop one level
// coarser. The sentinel keeps the invariant checkable inside this function and
// matches hashFileBytes, which already encodes the same policy. It deliberately
// makes the key differ between a run where the file read and a run where it did
// not — those runs are genuinely different inputs, and a miss is the honest
// answer.
func hashFiles(dir string, patterns []string, scope ProjectConfigScope) (string, error) {
	projectDir := dir
	dir, err := gitInputDirectory(dir, patterns)
	if err != nil {
		return "", err
	}
	files, err := collectFiles(dir, patterns)
	if err != nil {
		return "", err
	}

	digests := hashFileContents(files, scope, projectDir)
	for i, file := range files {
		if file.rawEmbed && digests[i] == nil {
			return "", fmt.Errorf("cannot read Go embedded input %s", file.path)
		}
	}

	h := sha256.New()
	for i, f := range files {
		rel, _ := filepath.Rel(dir, f.path)
		writeField(h, filepath.ToSlash(rel))
		if digests[i] == nil {
			// Unreadable, per fileContentDigest — including the .gen stamp path,
			// whose versionStampDigest returns nil on the same failures and
			// reaches this same fold, so it needs no sentinel of its own.
			h.Write([]byte(unreadableSentinel)) //nolint:errcheck // hash.Hash.Write never errors
		} else {
			h.Write(digests[i]) //nolint:errcheck // hash.Hash.Write never errors
		}
		h.Write([]byte{0}) //nolint:errcheck // hash.Hash.Write never errors
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// hashFileContents returns the SHA-256 digest of each file's bytes, indexed to
// line up with files. An unreadable file yields a nil digest so the caller can
// fold in the unreadable sentinel for it. The work is spread over a bounded
// pool of workers, each reusing a copy buffer to keep syscall count and
// allocations down.
func hashFileContents(files []fileEntry, scope ProjectConfigScope, projectDir string) [][]byte {
	// The scope says which option blocks THIS job's extension reads in its own
	// project's config and in a closure member's. A config a project declares
	// ACROSS its root — `options.test.filePatterns: ["../../**/putnami.json"]`
	// — is read by the job as a whole document, typically by a test asserting
	// over every block of it, so it is hashed whole. A relative path is inside
	// by construction. A config a glob swept up is read as text wherever it
	// lives, so it is hashed verbatim.
	scopeFor := func(file fileEntry) ProjectConfigScope {
		path := file.path
		if file.sweptByGlob {
			return ProjectConfigScope{Verbatim: true}
		}
		if projectDir == "" || !filepath.IsAbs(path) {
			return scope
		}
		rel, err := filepath.Rel(projectDir, path)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return ProjectConfigScope{Verbatim: scope.Verbatim}
		}
		return scope
	}
	digests := make([][]byte, len(files))
	if len(files) == 0 {
		return digests
	}

	workers := runtime.GOMAXPROCS(0)
	if workers > len(files) {
		workers = len(files)
	}

	indexes := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 128*1024)
			for i := range indexes {
				if files[i].gitCandidate {
					digests[i] = gitCandidateDigest(files[i], buf)
				} else if files[i].rawEmbed {
					file, err := os.Open(files[i].path)
					if err != nil {
						continue
					}
					h := sha256.New()
					_, readErr := io.CopyBuffer(h, file, buf)
					closeErr := file.Close()
					if readErr == nil && closeErr == nil {
						digests[i] = h.Sum(nil)
					}
				} else {
					digests[i] = fileContentDigest(files[i].path, buf, scopeFor(files[i]))
				}
			}
		}()
	}
	for i := range files {
		indexes <- i
	}
	close(indexes)
	wg.Wait()

	return digests
}

// fileContentDigest returns the SHA-256 digest of the file at path, or nil if
// it cannot be opened or read. buf is reused across files within a worker to
// cut the syscall count relative to io.Copy's default chunking.
func fileContentDigest(path string, buf []byte, scope ProjectConfigScope) []byte {
	if isVersionStamp(path) {
		return versionStampDigest(path)
	}
	if isProjectConfig(path) {
		return projectConfigDigest(path, scope)
	}

	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()

	h := sha256.New()
	if _, err := io.CopyBuffer(h, file, buf); err != nil {
		return nil
	}
	return h.Sum(nil)
}

// versionStampFieldBuildTime is the one field of the .gen/version.json stamp
// that describes the invocation rather than the tree it was taken from.
const versionStampFieldBuildTime = "buildTime"

// isVersionStamp reports whether path is a project's .gen/version.json — the
// build stamp the CLI writes for every planned project before jobs run.
func isVersionStamp(p string) bool {
	dir, file := filepath.Split(p)
	if file != "version.json" {
		return false
	}
	return filepath.Base(filepath.Clean(dir)) == ".gen"
}

// versionStampDigest hashes .gen/version.json with buildTime blanked, so two
// stamps that differ only in build time contribute the same digest.
//
// Hashing the raw bytes makes the stamp invalidate the very key it was hashed
// into. The scheduler refreshes buildTime *after* a job's key is computed and
// only for a job that is about to execute (scheduler_exec.go), so a miss stores
// its entry under a key describing a tree state that no longer exists on disk.
// The next run keys on the refreshed stamp, misses again, refreshes again — a
// project that takes one genuine miss can never hit again. Any task whose input
// globs reach the stamp is affected; TypeScript lint (**/*.json) is the one that
// bites in practice.
//
// Every field a task can legitimately depend on — version, sha, branch, isDirty
// and the capability stamps — stays hashed, so a HEAD change still invalidates
// the entry. This mirrors versionFileMatchesBuild, which
// already compares stamps on every field except buildTime.
func versionStampDigest(path string) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// A stamp that is not the JSON object we write is hashed verbatim: better a
	// spurious miss than silently ignoring a file we do not understand.
	var stamp map[string]any
	if json.Unmarshal(data, &stamp) != nil {
		return sum256(data)
	}
	delete(stamp, versionStampFieldBuildTime)
	// json.Marshal sorts map keys, so the normalized form is deterministic.
	normalized, err := json.Marshal(stamp)
	if err != nil {
		return sum256(data)
	}
	return sum256(normalized)
}

// isProjectConfig recognizes the project-config filenames extension manifests
// declare as config inputs. Per-task tuning is execution-only, so its JSON
// block needs a semantic digest instead of raw byte hashing whenever either
// spelling reaches a cache key.
func isProjectConfig(path string) bool {
	name := filepath.Base(path)
	return name == wsproto.ConfigFilename || name == wsproto.LegacyConfigFilename
}

// ProjectConfigScope names the extension whose option layers a project config
// contributes to one task's cache key.
//
// A project config is a shared file: `options` holds one block per addressee,
// and the CLI's layer contract spells an extension's blocks `options.<ext>` and
// `options.<ext>:<command>`, where `<ext>` is the extension's canonical name
// (`@putnami/go`) or the workspace path reference a project declares it by
// (`/go/extension`). Those blocks are that extension's input and nobody else's,
// so hashing them into another extension's task key re-runs work for bytes the
// task cannot read.
//
// ExtensionLayers holds the hashing task's OWN spellings, and ForeignNamespaces
// the bare namespaces another extension declared. The digest keeps every other
// key — identity fields, command layers (`options.publish`), and any namespace
// it cannot positively attribute to a different extension. An empty scope keeps
// the whole file, so a caller that has no task in hand pays a wider key rather
// than a wrong one.
type ProjectConfigScope struct {
	ExtensionLayers []string
	// ForeignNamespaces are the bare `options.<name>` blocks another extension
	// DECLARED as its own (manifest `optionNamespaces`) and this one did not.
	// A namespace nobody declares, and one this extension declares too, is
	// absent from this list and stays in the key: the hasher drops a block only
	// when a manifest says who reads it and the answer is somebody else.
	ForeignNamespaces []string
	// Verbatim hashes every byte of a project config, as for any other file,
	// and ignores the two lists above. A task that rewrites its sources reads a
	// config as text: its edits depend on layout, on the tasks block, and on
	// every other extension's options, and a decoded projection hides exactly
	// the edits a formatter makes. The .gen/version.json build stamp keeps its
	// buildTime-free digest in this mode too: the CLI writes that file, not a
	// task. A config read as a whole document — across the project root, or
	// through a workspace-relative pattern — drops the two lists and keeps
	// this field. A config a glob pattern selects is read verbatim whatever
	// the scope says (fileEntry.sweptByGlob).
	Verbatim bool
}

// projectConfigDigest canonicalizes a project config after dropping its tasks
// block and the option layers that address a DIFFERENT extension. Every
// ProjectTaskTuning field is explicitly execution-only; hashing raw
// putnami.json bytes would otherwise re-key a task when an operator only
// changes CPU scheduling or a deadline, or when they change a deploy option
// belonging to an extension that never runs this task. A Verbatim scope hashes
// the raw bytes instead.
//
// Two shapes are left exactly as they are: JSON the decoder rejects is hashed
// raw, and an `options` member that is not an object keeps every byte. The
// hasher never reinterprets a shape it does not recognize, because dropping
// bytes on a guess is how a stale result gets served.
func projectConfigDigest(path string, scope ProjectConfigScope) []byte {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	if scope.Verbatim {
		// The tag keeps the two readings apart: a file whose bytes equal the
		// decoded view's canonical JSON still has a different verbatim digest.
		return sum256(append([]byte("project-config-bytes\x00"), data...))
	}

	var config map[string]any
	if json.Unmarshal(data, &config) != nil {
		return sum256(data)
	}
	delete(config, "tasks")
	scoped := len(scope.ExtensionLayers) > 0 || len(scope.ForeignNamespaces) > 0
	if options, ok := config["options"].(map[string]any); ok && scoped {
		for key := range options {
			// An extension layer is spelled as a package name (`@scope/name`) or
			// a workspace path reference (`/path/to/extension`), bare or with a
			// `:<command>` suffix. A bare name matches neither and is dropped
			// only when a manifest declared it for somebody else; a command
			// layer and an undeclared namespace stay in every key.
			if !strings.HasPrefix(key, "@") && !strings.HasPrefix(key, "/") {
				for _, foreign := range scope.ForeignNamespaces {
					if key == foreign {
						delete(options, key)
						break
					}
				}
				continue
			}
			owned := false
			for _, layer := range scope.ExtensionLayers {
				if key == layer || strings.HasPrefix(key, layer+":") {
					owned = true
					break
				}
			}
			if !owned {
				delete(options, key)
			}
		}
		// An options block emptied by the scope is dropped, so a project whose
		// only options address other extensions keys identically to one that
		// declares none: for this task the two configs say the same thing.
		if len(options) == 0 {
			delete(config, "options")
		}
	}
	normalized, err := json.Marshal(config)
	if err != nil {
		return sum256(data)
	}
	return sum256(normalized)
}

func sum256(data []byte) []byte {
	digest := sha256.Sum256(data)
	return digest[:]
}

// hashEnvVars computes a hash from the values of specified environment variables.
func hashEnvVars(names []string) string {
	sorted := make([]string, len(names))
	copy(sorted, names)
	sort.Strings(sorted)

	h := sha256.New()
	for _, name := range sorted {
		h.Write([]byte(name))
		h.Write([]byte("="))
		h.Write([]byte(os.Getenv(name)))
		h.Write([]byte{0})
	}

	return hex.EncodeToString(h.Sum(nil))
}

// hashExtraFiles computes a hash from the contents of individual paths outside
// the project root (e.g., cross-project generate assets). A path may name a
// file or a directory: a directory is walked recursively so every file beneath
// it contributes its tree-relative path and content. A doc site that pulls in
// `/sites/putnami.dev/doc` as a generate asset is the motivating case — the
// prior implementation io.Copy'd a directory file descriptor, which reads zero
// bytes, so edits to files under a declared asset directory never invalidated
// the generate cache key.
//
// The per-path encoding for a regular file or a missing path is byte-identical
// to the previous implementation, so cache keys that referenced only files are
// unchanged; only directory paths (which previously contributed nothing) now
// fold in their contents.
func hashExtraFiles(paths []string) string {
	sorted := make([]string, len(paths))
	copy(sorted, paths)
	sort.Strings(sorted)

	h := sha256.New()
	for _, p := range sorted {
		h.Write([]byte(p)) //nolint:errcheck // hash.Hash.Write never errors
		h.Write([]byte{0}) //nolint:errcheck // hash.Hash.Write never errors

		hashPathContent(h, p)

		h.Write([]byte{0}) //nolint:errcheck // hash.Hash.Write never errors
	}

	return hex.EncodeToString(h.Sum(nil))
}

// hashPathContent folds the content of a single path into h. A regular file
// contributes its raw bytes; a directory contributes every file beneath it,
// each keyed by its directory-relative (forward-slash) path so additions,
// removals, and renames are all observed, folded in slash-form lexical order
// (slashPathLess) so the result does not depend on the host separator. A path
// that does not exist contributes the stable "__missing__" sentinel, matching
// the prior behavior so its absence stays distinguishable from an empty file.
func hashPathContent(h io.Writer, p string) {
	info, err := os.Stat(p)
	if err != nil {
		h.Write([]byte("__missing__")) //nolint:errcheck // hash.Hash.Write never errors
		return
	}
	if !info.IsDir() {
		hashFileBytes(h, p)
		return
	}

	var files []string
	_ = filepath.WalkDir(p, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d.IsDir() {
			return nil
		}
		files = append(files, path)
		return nil
	})
	sort.Slice(files, func(i, j int) bool { return slashPathLess(files[i], files[j], filepath.Separator) })
	for _, f := range files {
		rel, relErr := filepath.Rel(p, f)
		if relErr != nil {
			rel = f
		}
		writeField(h, filepath.ToSlash(rel))
		hashFileBytes(h, f)
		h.Write([]byte{0}) //nolint:errcheck // hash.Hash.Write never errors
	}
}

// hashFileBytes folds a file's bytes into h. A file that exists (per a prior
// stat) but cannot be opened contributes the stable unreadableSentinel so the
// hash stays defined rather than silently treating it as empty.
func hashFileBytes(h io.Writer, p string) {
	// A declared asset directory can carry another project's build stamp; fold
	// in its buildTime-free digest for the same reason fileContentDigest does.
	if isVersionStamp(p) {
		digest := versionStampDigest(p)
		if digest == nil {
			h.Write([]byte(unreadableSentinel)) //nolint:errcheck // hash.Hash.Write never errors
			return
		}
		h.Write(digest) //nolint:errcheck // hash.Hash.Write never errors
		return
	}

	file, err := os.Open(p)
	if err != nil {
		h.Write([]byte(unreadableSentinel)) //nolint:errcheck // hash.Hash.Write never errors
		return
	}
	defer file.Close()
	io.Copy(h, file) //nolint:errcheck // hashing best-effort
}
