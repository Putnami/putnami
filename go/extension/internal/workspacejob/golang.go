package workspacejob

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/go/extension/internal/toolchain"
	"go.putnami.dev/sdk/extension/pkgmeta"
)

// defaultGoProxy is the GOPROXY a job exports when the caller named none.
const defaultGoProxy = "https://proxy.golang.org,direct"

// supportedPlatform reports whether a managed Go can be installed for the host,
// emitting the script's diagnostic when it cannot.
func (j *Job) supportedPlatform() bool {
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
	default:
		j.Emit.Diagnostic("error", "Unsupported OS: "+runtime.GOOS, "", 0)
		return false
	}
	switch runtime.GOARCH {
	case "amd64", "arm64":
	default:
		j.Emit.Diagnostic("error", "Unsupported architecture: "+runtime.GOARCH, "", 0)
		return false
	}
	return true
}

// LookPath returns the absolute path of the executable name on the job's
// PATH, or "". An entry that is not an absolute directory is skipped, as
// os/exec refuses to run a program found through the current directory. On
// Windows only an .exe is ever run: a .bat or .cmd file found on PATH would
// start cmd.exe, and the lifecycle jobs start no shell.
func (j *Job) LookPath(name string) string {
	file := pkgmeta.ExecutableName(runtime.GOOS, name)
	for _, dir := range filepath.SplitList(j.Env.Get("PATH")) {
		if dir == "" || !filepath.IsAbs(dir) {
			continue
		}
		candidate := filepath.Join(dir, file)
		if isExecutable(candidate) && j.admits(candidate) {
			return candidate
		}
	}
	return ""
}

// RequestedGoVersion is the Go version the workspace asks for, normalized,
// or "": the go directive of the workspace go.work, else the toolchain or go
// directive of the project go.mod.
func (j *Job) RequestedGoVersion() string {
	if version := j.readGoVersion(); version != "" {
		return NormalizeGoVersion(version)
	}
	return ""
}

func (j *Job) readGoVersion() string {
	if version := GoDirective(filepath.Join(j.WorkspaceRoot, "go.work")); version != "" {
		return version
	}
	if j.ProjectPath == "" {
		return ""
	}
	data, err := os.ReadFile(filepath.Join(j.ProjectPath, "go.mod"))
	if err != nil {
		return ""
	}
	if line, ok := firstLineWithPrefix(string(data), "toolchain go"); ok {
		if version := strings.TrimPrefix(line, "toolchain go"); version != "" {
			return version
		}
	}
	if line, ok := firstLineWithPrefix(string(data), "go "); ok {
		return field(line, 2)
	}
	return ""
}

// GoDirective returns the second field of the first line of the file at path
// that starts with "go ", or "" when the file or the line is missing: the
// script's `grep -m1 '^go ' | awk '{print $2}'`.
func GoDirective(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if line, ok := firstLineWithPrefix(string(data), "go "); ok {
		return field(line, 2)
	}
	return ""
}

func firstLineWithPrefix(content, prefix string) (string, bool) {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, prefix) {
			return line, true
		}
	}
	return "", false
}

var majorMinorPattern = regexp.MustCompile(`^[0-9]+\.[0-9]+$`)

// NormalizeGoVersion strips one "go" prefix and one surrounding space, and
// completes a major.minor version with ".0".
func NormalizeGoVersion(version string) string {
	version = strings.TrimPrefix(version, "go")
	version = strings.TrimPrefix(version, " ")
	version = strings.TrimSuffix(version, " ")
	if majorMinorPattern.MatchString(version) {
		version += ".0"
	}
	return version
}

// GoVersionSatisfies reports whether Go version have is at least want. Both
// must be three numeric fields once normalized; anything else satisfies
// nothing.
func GoVersionSatisfies(have, want string) bool {
	haveParts, ok := numericVersion(NormalizeGoVersion(have))
	if !ok {
		return false
	}
	wantParts, ok := numericVersion(NormalizeGoVersion(want))
	if !ok {
		return false
	}
	return slices.Compare(haveParts[:], wantParts[:]) >= 0
}

func numericVersion(version string) ([3]uint64, bool) {
	var out [3]uint64
	parts := strings.SplitN(version, ".", 3)
	if len(parts) != 3 {
		return out, false
	}
	for i, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return out, false
		}
		value, err := strconv.ParseUint(part, 10, 64)
		if err != nil {
			return out, false
		}
		out[i] = value
	}
	return out, true
}

// goBinaryVersion is the version binary reports under the job's GOTOOLCHAIN,
// or under mode when mode is set, normalized, or "".
func (j *Job) goBinaryVersion(binary, mode string) string {
	var env []string
	if mode != "" {
		env = []string{"GOTOOLCHAIN=" + mode}
	}
	// The output is read even when the command fails, as the script's
	// `$(cmd | sed ...) || true` did.
	out, _ := j.Output(env, binary, "env", "GOVERSION")
	version := mapLines(out, func(line string) string { return strings.TrimPrefix(line, "go") })
	if version == "" {
		out, _ = j.Output(env, binary, "version")
		version = mapLines(mapLines(out, func(line string) string { return field(line, 3) }),
			func(line string) string { return strings.TrimPrefix(line, "go") })
	}
	if version == "" {
		return ""
	}
	return NormalizeGoVersion(version)
}

// LocalGoVersion is the version the resolved go command runs as under
// GOTOOLCHAIN=local, normalized, or "".
func (j *Job) LocalGoVersion() string {
	return j.goBinaryVersion(j.GoBinary, "local")
}

// GoVersion is the version the resolved go command prints, without its "go"
// prefix: the script's `go version | awk '{print $3}' | sed 's/^go//'`.
func (j *Job) GoVersion() string {
	out, _ := j.Output(nil, j.GoBinary, "version")
	return mapLines(mapLines(out, func(line string) string { return field(line, 3) }),
		func(line string) string { return strings.TrimPrefix(line, "go") })
}

// toolchainBinary returns the go command of a toolchain the go command at
// binary already resolves for requested, when that toolchain runs a version
// that satisfies it, or "".
//
// Such a binary runs the required version under GOTOOLCHAIN=local, so it stays
// offline-safe.
func (j *Job) toolchainBinary(binary, requested string) string {
	if requested == "" {
		return ""
	}
	root, err := j.Output([]string{"GOTOOLCHAIN=auto"}, binary, "env", "GOROOT")
	if err != nil || root == "" {
		return ""
	}
	candidate := filepath.Join(root, "bin", pkgmeta.ExecutableName(runtime.GOOS, "go"))
	if !isExecutable(candidate) || !j.admits(candidate) {
		return ""
	}
	if version := j.goBinaryVersion(candidate, "local"); version != "" && GoVersionSatisfies(version, requested) {
		return candidate
	}
	return ""
}

// ResolveGoBinary selects the go command the job runs and records it in
// GoBinary: the one findGoBinary finds, else the Go the workspace lock pins,
// which it installs.
//
// It reports false after emitting the reason when no go command qualifies.
func (j *Job) ResolveGoBinary() bool {
	if !j.supportedPlatform() {
		return false
	}
	requested := j.RequestedGoVersion()
	if binary := j.findGoBinary(requested); binary != "" {
		j.GoBinary = binary
		return true
	}
	binary, ok := j.installLockedGo(requested)
	if !ok {
		return false
	}
	j.GoBinary = binary
	return true
}

// FindGoBinary records in GoBinary the go command ResolveGoBinary would select
// without installing one, and reports whether there is one.
func (j *Job) FindGoBinary() bool {
	binary := j.findGoBinary(j.RequestedGoVersion())
	if binary == "" {
		return false
	}
	j.GoBinary = binary
	return true
}

// findGoBinary returns the go command that qualifies for requested without an
// install, or "".
//
// A Go the workspace lock pins is exact, as it is for the tasks the CLI runs
// with the extension's runtime toolchain: only a go on PATH that runs the
// pinned release, or the managed install of it, qualifies. Those are places
// the tasks look too, and the CLI puts the pinned go it resolved first on the
// job's PATH. A pin older than requested is not exact: installLockedGo refuses
// it, and until the lock pins the requested release, the version is a minimum
// as it is without a pin. Then a go on PATH that satisfies it is used, then a
// toolchain that go resolves, then a managed install.
func (j *Job) findGoBinary(requested string) string {
	if pinned := j.pinnedGoVersion(requested); pinned != "" {
		return j.findPinnedGo(pinned)
	}

	// Jobs run with GOTOOLCHAIN=local (SetupGoEnv), so a candidate is judged by
	// the version it runs as in that mode, not the one it would switch to.
	if system := j.LookPath("go"); system != "" {
		systemVersion := j.goBinaryVersion(system, "local")
		if requested == "" || (systemVersion != "" && GoVersionSatisfies(systemVersion, requested)) {
			return system
		}
		if toolchain := j.toolchainBinary(system, requested); toolchain != "" {
			return toolchain
		}
		shown := systemVersion
		if shown == "" {
			shown = "unknown"
		}
		j.Emit.Log("info", "System Go "+shown+" is older than workspace Go "+requested+"; using managed Go")
	}

	if requested != "" {
		if pinned := managedGoBinary(j.ExtensionStateRoot(), requested); isExecutable(pinned) && j.admits(pinned) {
			return pinned
		}
	}

	// bin/go links to the last managed install (linkManagedGo). It is the
	// fallback when no install matches the requested version by name.
	if managed := filepath.Join(j.ExtensionStateRoot(), "bin", pkgmeta.ExecutableName(runtime.GOOS, "go")); isExecutable(managed) && j.admits(managed) {
		if version := j.goBinaryVersion(managed, ""); requested == "" || version == requested {
			return managed
		}
	}
	return ""
}

// pinnedGoVersion returns the Go release the workspace lock pins when it
// satisfies requested, or "" when the lock pins none, cannot be read, or pins
// an older release.
func (j *Job) pinnedGoVersion(requested string) string {
	lock, err := ReadGoLock(filepath.Join(j.WorkspaceRoot, LockFileName))
	if err != nil {
		return ""
	}
	if requested != "" && !GoVersionSatisfies(lock.Version, requested) {
		return ""
	}
	return lock.Version
}

// findPinnedGo returns the go on PATH when it runs version under
// GOTOOLCHAIN=local, else the managed install of version, or "".
func (j *Job) findPinnedGo(version string) string {
	if system := j.LookPath("go"); system != "" {
		systemVersion := j.goBinaryVersion(system, "local")
		if systemVersion == version {
			return system
		}
		shown := systemVersion
		if shown == "" {
			shown = "unknown"
		}
		j.Emit.Log("info", "System Go "+shown+" is not the Go "+version+" the workspace lock pins; using managed Go")
	}
	if pinned := managedGoBinary(j.ExtensionStateRoot(), version); isExecutable(pinned) && j.admits(pinned) {
		return pinned
	}
	return ""
}

// OnlyProgramsOutsideTheWorkspace makes the job find no program inside the
// workspace tree: LookPath skips a PATH entry that resolves into it, and Go
// selection skips the managed installs, which live in it. workspace-fetch
// calls it before it selects a go command. A program the repository commits
// into its own tree is repository code, and that job runs while it holds the
// read credential. The go command then comes from the engine's toolchain
// store or the runner's PATH.
func (j *Job) OnlyProgramsOutsideTheWorkspace() {
	j.outsideWorkspaceOnly = true
}

// admits reports whether the job may run the program at path.
func (j *Job) admits(path string) bool {
	return !j.outsideWorkspaceOnly || !within(j.WorkspaceRoot, path)
}

// within reports whether path is root or lies below it once both are resolved
// through their symbolic links. A root that cannot be resolved contains every
// path, so the check fails closed.
func within(root, path string) bool {
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(resolvedRoot, resolveExisting(path))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// resolveExisting resolves path through the symbolic links of its nearest
// existing ancestor, and keeps the part below that ancestor as written.
func resolveExisting(path string) string {
	path = filepath.Clean(path)
	rest := ""
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			return filepath.Join(resolved, rest)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Join(path, rest)
		}
		rest = filepath.Join(filepath.Base(path), rest)
		path = parent
	}
}

// managedGoBinary is where a managed install of Go version keeps its go
// command.
func managedGoBinary(stateRoot, version string) string {
	return filepath.Join(managedGoDir(stateRoot, version), "go", "bin", pkgmeta.ExecutableName(runtime.GOOS, "go"))
}

func managedGoDir(stateRoot, version string) string {
	return filepath.Join(stateRoot, "libs", "go-"+version)
}

// SetupGoEnv exports the Go environment every command of the job runs with:
// the machine-global caches, GOTOOLCHAIN=local, the proxy chain around the
// declared module origin host, and GOROOT for a managed go command. On a
// hosted run (OfflineDependencies) it also forbids every module download, as
// toolchain.GoCommandEnv does for a task.
func (j *Job) SetupGoEnv(originHost string) {
	j.setupGoEnv(originHost)
	if j.OfflineDependencies() {
		j.ForbidModuleDownloads()
	}
}

// SetupFetchGoEnv exports the environment SetupGoEnv exports, with module
// downloads allowed whatever the offline signal says: workspace-fetch is the
// job that downloads the modules every other job of a hosted run then reads
// offline.
func (j *Job) SetupFetchGoEnv(originHost string) {
	j.setupGoEnv(originHost)
}

// OfflineDependencies reports whether the engine runs the job on a hosted run
// whose dependencies workspace-fetch already downloaded
// (toolchain.OfflineDependencies).
func (j *Job) OfflineDependencies() bool {
	return toolchain.OfflineDependencies(j.Env.Environ())
}

// ForbidModuleDownloads exports the offline policy of
// toolchain.OfflineGoOverrides: no command the job runs afterwards can
// download a module or a toolchain.
func (j *Job) ForbidModuleDownloads() {
	for _, entry := range toolchain.OfflineGoOverrides(j.Env.Get("GOFLAGS")) {
		name, value, _ := strings.Cut(entry, "=")
		j.Env.Set(name, value)
	}
}

func (j *Job) setupGoEnv(originHost string) {
	if originHost == "" && j.Env.Get("GO_REGISTRY_URL") != "" {
		originHost = OriginHost(j.Env.Get("GO_REGISTRY_URL"))
	}
	pattern := ""
	if originHost != "" {
		pattern = originHost + "/*"
	}

	root := j.GoCacheRoot()
	buildCache := filepath.Join(root, "build")
	moduleCache := filepath.Join(root, "mod")
	j.Env.Set("GOCACHE", buildCache)
	j.Env.Set("GOMODCACHE", moduleCache)
	_ = os.MkdirAll(buildCache, 0o755)
	_ = os.MkdirAll(moduleCache, 0o755)

	// NETRC is left alone, so Go reads the standard ~/.netrc that
	// `putnami cloud registry-token --materialize` writes.

	// The job selected a go command whose own version satisfies the workspace,
	// so Go must not download and switch to another one.
	j.Env.Set("GOTOOLCHAIN", "local")

	// The declared origin never leads GOPROXY: Go asks only the first proxy
	// whether it relays the checksum database, and a vanity catch-all answers
	// yes and then serves nothing. GONOPROXY routes the workspace's own modules
	// to the origin instead, and it exists only beside the default proxy the job
	// supplied: an inherited GONOPROXY loses the entry when the caller names its
	// own proxy.
	if j.Env.Get("GOPROXY") == "" {
		j.Env.Set("GOPROXY", defaultGoProxy)
		if pattern != "" {
			j.Env.Set("GONOPROXY", appendCommaValue(j.Env.Get("GONOPROXY"), pattern))
		}
	} else if pattern != "" && j.Env.Get("GONOPROXY") != "" {
		j.Env.Set("GONOPROXY", removeCommaValue(j.Env.Get("GONOPROXY"), pattern))
	}
	// Modules the declared origin serves are not in the public checksum database.
	if pattern != "" {
		j.Env.Set("GONOSUMCHECK", appendCommaValue(j.Env.Get("GONOSUMCHECK"), pattern))
		j.Env.Set("GONOSUMDB", appendCommaValue(j.Env.Get("GONOSUMDB"), pattern))
	}

	// A managed go command runs from its own GOROOT, ahead of any other go.
	if strings.Contains(filepath.ToSlash(j.GoBinary), ".putnami/extensions/") {
		actual := j.GoBinary
		if info, err := os.Lstat(actual); err == nil && info.Mode()&os.ModeSymlink != 0 {
			if resolved, err := filepath.EvalSymlinks(actual); err == nil {
				actual = resolved
			}
		}
		goRoot, err := filepath.Abs(filepath.Join(filepath.Dir(actual), ".."))
		if err == nil {
			j.Env.Set("GOROOT", goRoot)
			j.Env.PrependPath(filepath.Join(goRoot, "bin"))
		}
	}
}

// OriginHost returns the host of a declared Go origin URL or bare host: the
// scheme, the path and any user information are dropped.
func OriginHost(origin string) string {
	if _, rest, ok := strings.Cut(origin, "://"); ok {
		origin = rest
	}
	if host, _, ok := strings.Cut(origin, "/"); ok {
		origin = host
	}
	if at := strings.LastIndex(origin, "@"); at >= 0 {
		origin = origin[at+1:]
	}
	return origin
}
