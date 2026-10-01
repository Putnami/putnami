// Package toolchain resolves Go environment settings, lint tool binaries,
// and configuration files relative to the Putnami extension root.
package toolchain

import (
	"debug/buildinfo"
	"errors"
	"fmt"
	goversion "go/version"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"

	"go.putnami.dev/go/extension/tools"
	"go.putnami.dev/sdk/extension/envkeys"
	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/pkgmeta"
	"go.putnami.dev/sdk/extension/scratch"
	"golang.org/x/mod/modfile"
)

// extRoot returns the .putnami/bin/extensions/putnami-go path for managed tooling.
// This uses the stable symlink path that survives version upgrades.
func extRoot(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".putnami", "bin", "extensions", "putnami-go")
}

// ResolvePinnedToolBinary returns the first discovered binary that reports the
// version pinned in tools/versions.json. A stale PATH binary never masks a
// compatible managed binary.
func ResolvePinnedToolBinary(tool, workspaceRoot string) (string, error) {
	spec, ok := tools.Lookup(tool)
	if !ok {
		return "", &unknownToolError{tool}
	}

	var failures []string
	for _, binary := range toolCandidates(tool, workspaceRoot) {
		matches, reported, err := toolVersionMatches(binary, tool, spec.Version)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s could not report its version (%v)", binary, err))
			continue
		}
		if !matches {
			failures = append(failures, fmt.Sprintf("%s reports %q", binary, reported))
			continue
		}
		if !toolBuildMatchesCurrentGoVersion(binary) {
			failures = append(failures, fmt.Sprintf("%s was built with a different Go toolchain", binary))
			continue
		}
		return binary, nil
	}

	if len(failures) == 0 {
		return "", &toolUnavailableError{tool: tool, expected: spec.Version}
	}
	return "", &toolVersionMismatchError{tool: tool, expected: spec.Version, found: strings.Join(failures, "; ")}
}

// toolCandidates lists, in priority order, the binaries that could satisfy a
// pinned tool: PATH, the machine tool home, then read-only workspace locations.
func toolCandidates(tool, workspaceRoot string) []string {
	candidates := make([]string, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(path string) {
		if path == "" {
			return
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			return
		}
		seen[path] = struct{}{}
		if !isExecutable(path) {
			return
		}
		candidates = append(candidates, path)
	}

	if binary, err := exec.LookPath(tool); err == nil {
		add(binary)
	}
	add(managedToolPath(tool, workspaceRoot))
	for _, legacy := range LegacyManagedToolPaths(tool, workspaceRoot) {
		add(legacy)
	}
	return candidates
}

// managedToolPath is where InstallTool writes a pinned tool: the machine tool
// home, or the workspace extension directory when no home directory exists.
func managedToolPath(tool, workspaceRoot string) string {
	if home := ToolHome(tool); home != "" {
		return home
	}
	if workspaceRoot == "" {
		return ""
	}
	return filepath.Join(extRoot(workspaceRoot), "bin", "tools", toolBinaryName(tool))
}

func toolVersionMatches(binary, tool, expected string) (bool, string, error) {
	args := []string{"-version"}
	if tool == "golangci-lint" {
		args = []string{"version", "--short"}
	}
	cmd := exec.Command(binary, args...)
	cmd.Env = GoCommandEnv(os.Environ(), CurrentGoBinary())
	out, err := cmd.CombinedOutput()
	reported := strings.TrimSpace(string(out))
	if err != nil {
		return false, reported, err
	}
	version := strings.TrimPrefix(expected, "v")
	matched := regexp.MustCompile(`(^|[^0-9])` + regexp.QuoteMeta(version) + `([^0-9]|$)`).MatchString(reported)
	return matched, reported, nil
}

// CurrentGoBinary returns the local Go binary used for managed tool installs.
// It delegates to ResolveGo so tool installation and workload jobs use the same
// deterministic toolchain selection.
func CurrentGoBinary() string {
	if binary, err := ResolveGo(); err == nil {
		return binary
	}
	return "go"
}

// ResolveGo returns the first compatible executable Go toolchain in this
// priority order:
//
//  1. the caller's explicit GOROOT/bin/go;
//  2. the first go executable on PATH;
//  3. a Go release installed inside the workspace, through its bin/go link;
//  4. a Go binary under the resolved extension root;
//  5. a Go binary under the workspace's stable extension root.
//
// Compatibility is measured against the governing go.work, falling back to the
// current project's go.mod. Candidate versions are probed with GOTOOLCHAIN=local
// so a stale system Go cannot pass by downloading and re-executing a newer
// toolchain that workload commands are forbidden to use.
//
// The runtime intentionally reads only process environment and workspace-owned
// paths. It never falls back to the GOROOT that built this precompiled extension,
// because that compiler may not exist on the consumer machine.
func ResolveGo() (string, error) {
	return ResolveGoFor(goResolutionFromEnv())
}

// GoResolution names the workspace-owned inputs ResolveGo reads from the job
// environment. A caller outside a job supplies them directly: the read-only
// documentation tool receives its workspace and extension roots in the tool
// request and knows the project it answers for, so it must not depend on the
// PUTNAMI_PROJECT_PATH a job would have exported.
type GoResolution struct {
	WorkspaceRoot string
	ExtensionRoot string
	ProjectPath   string
}

func (in GoResolution) normalized() GoResolution {
	in.WorkspaceRoot = strings.TrimSpace(in.WorkspaceRoot)
	in.ExtensionRoot = strings.TrimSpace(in.ExtensionRoot)
	in.ProjectPath = strings.TrimSpace(in.ProjectPath)
	return in
}

// goResolutionFromEnv reads the roots a running job exports. It is the only
// place those variable names are consumed, so an explicit caller and a job
// resolve through identical logic.
func goResolutionFromEnv() GoResolution {
	return GoResolution{
		WorkspaceRoot: os.Getenv(workspaceRootEnvVar),
		ExtensionRoot: os.Getenv(extensionRootEnvVar),
		ProjectPath:   currentProjectPath(),
	}
}

// ResolveGoFor is ResolveGo with explicit roots. GOROOT still comes from the
// process environment, because it is the caller's own override rather than a
// workspace fact.
func ResolveGoFor(in GoResolution) (string, error) {
	return resolveGoOn(runtime.GOOS, in)
}

// resolveGoOn is ResolveGoFor on a host running goos.
func resolveGoOn(goos string, in GoResolution) (string, error) {
	in = in.normalized()
	requirement, err := resolveGoRequirementFor(in)
	if err != nil {
		return "", err
	}

	binaryName := pkgmeta.ExecutableName(goos, "go")
	checked := make([]string, 0, 5)
	seen := make(map[string]struct{}, 4)
	var incompatible []goCandidateFailure

	tryPath := func(path string) (string, bool) {
		if path == "" {
			return "", false
		}
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			return "", false
		}
		seen[path] = struct{}{}
		checked = append(checked, path)
		if !isExecutable(path) {
			return "", false
		}
		if requirement.version == "" {
			return path, true
		}

		reported, err := probeLocalGoVersion(path)
		if err != nil {
			incompatible = append(incompatible, goCandidateFailure{path: path, err: err})
			return "", false
		}
		if goversion.Compare(reported, requirement.version) < 0 {
			incompatible = append(incompatible, goCandidateFailure{path: path, version: reported})
			return "", false
		}
		return path, true
	}

	if goRoot := strings.TrimSpace(os.Getenv("GOROOT")); goRoot != "" {
		if binary, ok := tryPath(filepath.Join(goRoot, "bin", binaryName)); ok {
			return binary, nil
		}
	}

	checked = append(checked, "PATH")
	if binary, err := exec.LookPath(binaryName); err == nil {
		if binary, ok := tryPath(binary); ok {
			return binary, nil
		}
	}

	workspaceRoot := in.WorkspaceRoot
	if workspaceRoot != "" {
		legacy := filepath.Join(
			workspaceRoot,
			".putnami",
			"extensions",
			"@putnami-go",
			"bin",
			binaryName,
		)
		if binary, ok := tryPath(legacy); ok {
			return binary, nil
		}
		// A Windows workspace has no bin/go link, so the Go releases
		// installed inside it are the candidates there.
		if goos == "windows" {
			for _, binary := range managedGoInstalls(workspaceRoot, binaryName) {
				if binary, ok := tryPath(binary); ok {
					return binary, nil
				}
			}
		}
	}

	extensionRoot := in.ExtensionRoot
	if extensionRoot != "" {
		if binary, ok := tryPath(filepath.Join(extensionRoot, "bin", binaryName)); ok {
			return binary, nil
		}
	}

	if workspaceRoot != "" {
		if binary, ok := tryPath(filepath.Join(extRoot(workspaceRoot), "bin", binaryName)); ok {
			return binary, nil
		}
	}

	return "", &errGoNotFound{
		checked:      checked,
		requirement:  requirement,
		incompatible: incompatible,
	}
}

// managedGoLibs is the directory under workspaceRoot that holds the Go
// releases installed inside the workspace, one go-<version> directory each.
func managedGoLibs(workspaceRoot string) string {
	return filepath.Join(workspaceRoot, ".putnami", "extensions", "@putnami-go", "libs")
}

// managedGoInstalls returns the go commands, named binaryName, of the Go
// releases installed inside workspaceRoot, newest release first.
func managedGoInstalls(workspaceRoot, binaryName string) []string {
	libs := managedGoLibs(workspaceRoot)
	entries, err := os.ReadDir(libs)
	if err != nil {
		return nil
	}
	var versions []string
	for _, entry := range entries {
		if version, ok := managedGoVersion(entry.Name()); ok && entry.IsDir() {
			versions = append(versions, version)
		}
	}
	slices.SortStableFunc(versions, func(a, b string) int { return goversion.Compare(b, a) })
	binaries := make([]string, 0, len(versions))
	for _, version := range versions {
		binaries = append(binaries, filepath.Join(libs, "go-"+strings.TrimPrefix(version, "go"), "go", "bin", binaryName))
	}
	return binaries
}

// managedGoVersion returns the Go version, such as "go1.26.1", that the name of
// a managed install directory, such as "go-1.26.1", holds.
func managedGoVersion(name string) (string, bool) {
	version, ok := strings.CutPrefix(name, "go-")
	if !ok || !goversion.IsValid("go"+version) {
		return "", false
	}
	return "go" + version, true
}

func goBinaryName() string {
	if runtime.GOOS == "windows" {
		return "go.exe"
	}
	return "go"
}

type goRequirement struct {
	version string
	source  string
}

func resolveGoRequirementFor(in GoResolution) (goRequirement, error) {
	in = in.normalized()
	workspaceRoot := in.WorkspaceRoot
	projectPath := in.ProjectPath

	goWorkPath := ""
	if projectPath != "" {
		goWorkPath = FindGoWork(projectPath)
	}
	if goWorkPath == "" && workspaceRoot != "" {
		goWorkPath = filepath.Join(workspaceRoot, "go.work")
	}
	if goWorkPath != "" && fileExists(goWorkPath) {
		requirement, err := readGoRequirement(goWorkPath, true)
		if err != nil {
			return goRequirement{}, err
		}
		if requirement.version != "" {
			return requirement, nil
		}
	}

	if projectPath != "" {
		goModPath := filepath.Join(projectPath, "go.mod")
		if fileExists(goModPath) {
			return readGoRequirement(goModPath, false)
		}
	}
	return goRequirement{}, nil
}

func currentProjectPath() string {
	for _, key := range []string{"PUTNAMI_PROJECT_PATH", "PUTNAMI_PROJECT_ROOT"} {
		if path := strings.TrimSpace(os.Getenv(key)); path != "" {
			return path
		}
	}
	return ""
}

func readGoRequirement(path string, workFile bool) (goRequirement, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return goRequirement{}, fmt.Errorf("read Go toolchain requirement from %s: %w", path, err)
	}

	var goDirective, toolchainDirective string
	if workFile {
		work, err := modfile.ParseWork(path, data, nil)
		if err != nil {
			return goRequirement{}, fmt.Errorf("read Go toolchain requirement from %s: %w", path, err)
		}
		if work.Go != nil {
			goDirective = work.Go.Version
		}
		if work.Toolchain != nil {
			toolchainDirective = work.Toolchain.Name
		}
	} else {
		mod, err := modfile.Parse(path, data, nil)
		if err != nil {
			return goRequirement{}, fmt.Errorf("read Go toolchain requirement from %s: %w", path, err)
		}
		if mod.Go != nil {
			goDirective = mod.Go.Version
		}
		if mod.Toolchain != nil {
			toolchainDirective = mod.Toolchain.Name
		}
	}

	version, err := effectiveGoRequirement(goDirective, toolchainDirective)
	if err != nil {
		return goRequirement{}, fmt.Errorf("read Go toolchain requirement from %s: %w", path, err)
	}
	return goRequirement{version: version, source: path}, nil
}

func effectiveGoRequirement(goDirective, toolchainDirective string) (string, error) {
	required := ""
	if goDirective != "" {
		required = "go" + strings.TrimPrefix(goDirective, "go")
		if !goversion.IsValid(required) {
			return "", fmt.Errorf("invalid go directive %q", goDirective)
		}
	}

	if toolchainDirective == "" || toolchainDirective == "default" {
		return required, nil
	}
	if !goversion.IsValid(toolchainDirective) {
		return "", fmt.Errorf("invalid toolchain directive %q", toolchainDirective)
	}
	if required == "" || goversion.Compare(toolchainDirective, required) > 0 {
		required = toolchainDirective
	}
	return required, nil
}

func probeLocalGoVersion(binary string) (string, error) {
	// ResolveGo enumerates candidates only from explicit GOROOT/PATH/managed
	// roots and verifies each is executable immediately before this direct,
	// no-shell probe.
	//nolint:gosec // The validated executable path is intentionally dynamic.
	cmd := exec.Command(binary, "env", "GOVERSION")
	cmd.Dir = os.TempDir()
	cmd.Env = setEnvValue(GoCommandEnv(os.Environ(), binary), goWorkEnvVar, "off")
	out, err := cmd.Output()
	reported := strings.TrimSpace(string(out))
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			if stderr := strings.TrimSpace(string(exitErr.Stderr)); stderr != "" {
				return "", fmt.Errorf("%w: %s", err, stderr)
			}
		}
		return "", err
	}
	if !goversion.IsValid(reported) {
		return "", fmt.Errorf("reported invalid local version %q", reported)
	}
	return reported, nil
}

type goCandidateFailure struct {
	path    string
	version string
	err     error
}

type errGoNotFound struct {
	checked      []string
	requirement  goRequirement
	incompatible []goCandidateFailure
}

func (e *errGoNotFound) Error() string {
	const installHint = "run `putnami install` or install Go from https://go.dev/dl/"
	if len(e.incompatible) > 0 {
		failures := make([]string, 0, len(e.incompatible))
		for _, failure := range e.incompatible {
			if failure.err != nil {
				failures = append(failures, fmt.Sprintf(
					"%s could not report its local version (%v)",
					failure.path,
					failure.err,
				))
			} else {
				failures = append(failures, fmt.Sprintf("%s reports %s", failure.path, failure.version))
			}
		}
		return fmt.Sprintf(
			"Go toolchains were found but none satisfies %s required by %s; incompatible candidates: %s: %s",
			e.requirement.version,
			e.requirement.source,
			strings.Join(failures, "; "),
			installHint,
		)
	}
	if len(e.checked) == 0 {
		return "go toolchain not found: " + installHint
	}
	requirement := ""
	if e.requirement.version != "" {
		requirement = fmt.Sprintf(
			" satisfying %s required by %s",
			e.requirement.version,
			e.requirement.source,
		)
	}
	return "go toolchain not found" + requirement + "; checked " + strings.Join(e.checked, ", ") +
		": " + installHint
}

// localGoVersion returns the base ("local") Go toolchain version reported by
// goBinary, e.g. "go1.24.7". It runs with GOTOOLCHAIN=local so the answer is the
// real installed toolchain and NOT a go.work-required toolchain that the default
// GOTOOLCHAIN=auto would silently download, re-exec, and report instead — the
// masking that made version guards always pass in remote sandboxes. It
// falls back to the version that built this process only when the probe fails.
func localGoVersion(goBinary string) string {
	cmd := exec.Command(goBinary, "env", "GOVERSION")
	cmd.Env = GoCommandEnv(os.Environ(), goBinary)
	out, err := cmd.Output()
	if v := strings.TrimSpace(string(out)); err == nil && v != "" {
		return v
	}
	return runtime.Version()
}

// toolBuildMatchesCurrentGoVersion reports whether a Go-built tool binary
// serves the LOCAL Go toolchain, the one InstallTool builds tools with
// (ToolServesLocalGo). It deliberately does NOT compare against
// runtime.Version(): the extension binary is prebuilt in CI, so its own Go
// version can differ from the local toolchain, which would flag every managed
// tool as mismatched and reinstall it on every run.
func toolBuildMatchesCurrentGoVersion(path string) bool {
	info, err := buildinfo.ReadFile(path)
	if err != nil || info.GoVersion == "" {
		return true
	}
	return ToolServesLocalGo(info.GoVersion, localGoVersion(CurrentGoBinary()))
}

// ToolServesLocalGo reports whether a lint tool built with the Go version
// built can check a workspace the local Go version local runs: the tool's Go
// minor must be the local one or a newer one.
//
// The go/types a tool embeds refuses a package whose language version is
// newer than the Go that built the tool ("package requires newer Go version
// go1.26 (application built with go1.25)"), so a tool built with an older minor
// fails. A tool built with a newer minor checks the workspace against the
// local standard library, the one `go list` reports, and finds what a tool
// built with the local minor finds. Both versions may carry the "go" prefix, a
// patch, a pre-release or a suffix such as " X:boringcrypto"; an unreadable
// version, such as a devel build's, serves nothing.
//
// workspace-install restores the prebuilt tools with the same rule, so what
// `putnami install` warms is what `putnami lint` accepts.
func ToolServesLocalGo(built, local string) bool {
	builtLang, localLang := goLanguageVersion(built), goLanguageVersion(local)
	return builtLang != "" && localLang != "" && goversion.Compare(builtLang, localLang) >= 0
}

// goLanguageVersion returns the "go1.N" language version of a Go version
// string, or "" when it names none.
func goLanguageVersion(version string) string {
	fields := strings.Fields(version)
	if len(fields) == 0 {
		return ""
	}
	return goversion.Lang("go" + strings.TrimPrefix(fields[0], "go"))
}

// InstallTool makes the pinned tool available on this machine and returns its
// path. It tries three sources under one lock, cheapest first:
//
//  1. the machine tool home, when it already holds the pinned version built
//     with the current Go minor or a newer one;
//  2. the prebuilt binary the running extension artifact ships
//     (compiled/tools/<tool>), copied into the tool home;
//  3. `go install`, into a throwaway GOPATH and then into the tool home.
//
// The destination is machine-global (ToolHome), never the extension root: for a
// registry-installed extension that root is a symlink into the content-addressed
// artifact store, so writing there would mutate an entry meant to be immutable
// and shared. It falls back to the workspace extension directory only when no machine
// root can be resolved at all.
//
// Uses file locking to serialize concurrent installations of the same key.
func InstallTool(tool, goBinary, workspaceRoot string) (_ string, resultErr error) {
	spec, ok := tools.Lookup(tool)
	if !ok {
		return "", &unknownToolError{tool}
	}

	dest := managedToolPath(tool, workspaceRoot)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("create tool directory for %s: %w", tool, err)
	}

	// Acquire an exclusive file lock to serialize concurrent installations.
	lockPath := dest + ".lock"
	unlock, err := lockFile(lockPath)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := unlock(); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("release tool install lock: %w", err))
		}
	}()

	// Re-check after acquiring lock — another process may have installed it. A
	// cached copy built with a mismatched Go toolchain (e.g. an older build made
	// with GOTOOLCHAIN=auto against the tool's own go.mod) is rebuilt, not reused,
	// so "install once" yields a toolchain-correct binary rather than a stale one
	// that panics on every lint run.
	if isExecutable(dest) && toolBuildMatchesCurrentGoVersion(dest) {
		matches, _, err := toolVersionMatches(dest, tool, spec.Version)
		if err == nil && matches {
			return dest, nil
		}
	}

	if artifact := ExtensionArtifactToolPath(tool); artifact != "" {
		restoreErr := installToolFromArtifact(artifact, dest, tool, spec.Version)
		if restoreErr == nil {
			slog.Info("restored tool from the extension artifact",
				slog.String("tool", tool), slog.String("version", spec.Version), slog.String("source", artifact))
			return dest, nil
		}
		// Not an error: a source checkout ships no artifact, and a machine on a
		// newer Go minor than the artifact's must rebuild. Say which, so the 40 s compile that
		// follows is explained rather than mysterious.
		slog.Info("building the pinned tool from source",
			slog.String("tool", tool), slog.String("version", spec.Version), slog.String("reason", restoreErr.Error()))
	}

	// `go install` writes to $GOPATH/bin, so GOPATH is a throwaway directory:
	// the module cache stays shared because GoCommandEnv sets GOMODCACHE
	// explicitly, and nothing is left behind inside a workspace or an artifact.
	gopathScratch, err := scratch.New("putnami-go-tool-")
	if err != nil {
		return "", fmt.Errorf("create temporary GOPATH for %s: %w", tool, err)
	}
	defer func() { _ = gopathScratch.Remove() }()
	gopath := gopathScratch.Path()

	cmd := exec.Command(goBinary, "install", spec.Install)
	cmd.Env = appendEnv(GoCommandEnv(os.Environ(), goBinary), "GOPATH="+gopath)
	// Build the tool with the LOCAL Go toolchain, not the one the tool's own
	// go.mod requests. `go install pkg@version` otherwise resolves its toolchain
	// from the tool module (e.g. golangci-lint's `go 1.25`), producing a linter
	// compiled against a different Go than the workspace (go.work) uses — which
	// then panics type-checking newer workspace sources.
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		return "", err
	}

	// go install puts binaries in $GOPATH/bin
	installed := filepath.Join(gopath, "bin", toolBinaryName(tool))
	if !isExecutable(installed) {
		return "", &installFailedError{tool}
	}
	matches, reported, err := toolVersionMatches(installed, tool, spec.Version)
	if err != nil {
		return "", fmt.Errorf("installed %s could not report its version: %w", tool, err)
	}
	if !matches {
		return "", fmt.Errorf("installed %s reports %q, expected %s", tool, reported, spec.Version)
	}
	if err := replaceBinary(installed, dest); err != nil {
		return "", fmt.Errorf("publish %s into %s: %w", tool, dest, err)
	}
	return dest, nil
}

// installToolFromArtifact copies a prebuilt tool out of the extension artifact
// into the machine tool home, after proving the artifact IS the pinned build.
//
// Both facts come from the binary's own embedded build information rather than
// from the archive it arrived in: the module version must equal the pin, and the
// Go toolchain that produced it must serve the local one (ToolServesLocalGo).
// An artifact that fails either check is not an error — the caller compiles instead — so the
// returned error is the REASON, meant for a log line.
func installToolFromArtifact(artifact, dest, tool, version string) error {
	info, err := buildinfo.ReadFile(artifact)
	if err != nil {
		return fmt.Errorf("no usable %s artifact at %s: %w", tool, artifact, err)
	}
	if info.Main.Version != version {
		return fmt.Errorf("the %s artifact embeds %s, not the pinned %s", tool, info.Main.Version, version)
	}
	if local := localGoVersion(CurrentGoBinary()); !ToolServesLocalGo(info.GoVersion, local) {
		return fmt.Errorf(
			"the %s artifact was built with %s, which cannot check the newer %s this machine runs",
			tool, info.GoVersion, local,
		)
	}
	return replaceBinary(artifact, dest)
}

// replaceBinary publishes src at dest atomically: a full copy into a sibling
// temporary file, then ReplaceExecutable over dest.
//
// The final move is what makes it safe for a concurrent lint job that holds no
// lock — it observes either the previous binary or the complete new one, never
// a half-written file it would then try to execute. The temporary lives beside
// dest so the move stays within one filesystem.
func replaceBinary(src, dest string) (resultErr error) {
	source, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, source.Close()) }()

	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+filepath.Base(dest)+".tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() {
		if resultErr != nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := io.Copy(tmp, source); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o755); err != nil {
		return err
	}
	return ReplaceExecutable(tmpName, dest)
}

// lockFile waits for an exclusive file lock on the given path.
// Returns an unlock function that must be called to release the lock.
func lockFile(path string) (func() error, error) {
	f, err := filelock.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := filelock.LockFile(f, true, false); err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return func() error {
		unlockErr := filelock.UnlockFile(f)
		closeErr := f.Close()
		return errors.Join(unlockErr, closeErr)
	}, nil
}

// ResolveGolangciConfig walks from projectPath up to workspaceRoot looking for
// .golangci.yml or .golangci.yaml. Falls back to the extension's bundled config.
func ResolveGolangciConfig(projectPath, workspaceRoot, extensionRoot, explicit string) string {
	if explicit != "" {
		return explicit
	}

	dir := projectPath
	for strings.HasPrefix(dir, workspaceRoot) {
		for _, name := range []string{".golangci.yml", ".golangci.yaml"} {
			candidate := filepath.Join(dir, name)
			if fileExists(candidate) {
				return candidate
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}

	// Extension default
	candidate := filepath.Join(extensionRoot, "config", ".golangci.yml")
	if fileExists(candidate) {
		return candidate
	}
	return ""
}

func isExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir() && (runtime.GOOS == "windows" || info.Mode()&0o111 != 0)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func appendEnv(env []string, kv string) []string {
	key := kv[:strings.IndexByte(kv, '=')]
	for i, e := range env {
		if _, ok := envkeys.Host.Value(e, key); ok {
			env[i] = kv
			return env
		}
	}
	return append(env, kv)
}

func goMajorMinor(version string) string {
	version = strings.TrimPrefix(version, "go")
	if idx := strings.Index(version, " "); idx >= 0 {
		version = version[:idx]
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return version
	}
	return parts[0] + "." + parts[1]
}

type unknownToolError struct{ tool string }

func (e *unknownToolError) Error() string { return "unknown tool: " + e.tool }

type toolUnavailableError struct {
	tool     string
	expected string
}

func (e *toolUnavailableError) Error() string {
	return fmt.Sprintf("%s is missing; expected %s", e.tool, e.expected)
}

type toolVersionMismatchError struct {
	tool     string
	expected string
	found    string
}

func (e *toolVersionMismatchError) Error() string {
	return fmt.Sprintf("%s version mismatch; expected %s, found %s", e.tool, e.expected, e.found)
}

type installFailedError struct{ tool string }

func (e *installFailedError) Error() string { return "failed to install " + e.tool }
