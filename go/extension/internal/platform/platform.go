// Package platform defines cross-compilation targets and helpers shared
// between the build and package jobs.
package platform

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/pkgmeta"
)

// Target maps a GOOS/GOARCH pair to the archive filename suffix.
type Target struct {
	GOOS   string
	GOARCH string
	Suffix string // e.g. "linux-x64", "darwin-arm64"
}

// ArchivePlatforms lists the DISTRIBUTION target matrix: the platforms a
// release artifact is expected to carry. It is the SDK's matrix
// (pkgmeta.ArchivePlatforms), the one every archive packager reads.
//
// It is no longer the default of ordinary `build`.
// Ordinary validation compiles for the host alone and this matrix is reached
// only by the distribution intent (`package`/release) or by an explicit
// `platforms` / `--target` request, because the cross-compiles of one module
// share no GOCACHE objects — they are full compiles whose binaries no step
// of `build` consumes.
var ArchivePlatforms = archiveTargets()

func archiveTargets() []Target {
	platforms := pkgmeta.ArchivePlatforms()
	targets := make([]Target, 0, len(platforms))
	for _, platform := range platforms {
		targets = append(targets, Target{GOOS: platform.GOOS, GOARCH: platform.GOARCH, Suffix: platform.Suffix})
	}
	return targets
}

// Platform spec tokens accepted by the `platforms` project option and the
// `--platforms` flag, beside literal `os/arch` entries.
const (
	// PlatformHost is the machine running the build.
	PlatformHost = "host"
	// PlatformAll is the full distribution matrix (ArchivePlatforms).
	PlatformAll = "all"
)

// HostTarget is the compile target of the machine running the build.
//
// It exists so "host" is a NAMED member of the platform vocabulary rather than
// an implicit fallback: a caller that wants the host says so, and the resolved
// set is always an explicit list of targets that can be reported, logged, and
// compared.
func HostTarget() Target {
	return Target{
		GOOS:   runtime.GOOS,
		GOARCH: runtime.GOARCH,
		Suffix: SuffixFromDockerPlatform(runtime.GOOS + "/" + runtime.GOARCH),
	}
}

// ParsePlatformSpec resolves an explicit platform request into targets.
//
// Entries are `os/arch` pairs, the `host` token, or the `all` token; an entry
// may also be a comma- or space-separated list, so one `--platforms
// linux/amd64,darwin/arm64` flag and a JSON array in putnami.json resolve
// identically. Order is the order requested, with duplicates dropped, so the
// resolved set is a deterministic function of the spec alone.
//
// An `os`-only entry expands to every distribution target for that OS, matching
// how `--target linux` has always behaved.
// An entry that is not a platform is an ERROR rather than a synthesized GOOS.
func ParsePlatformSpec(entries []string) ([]Target, error) {
	var resolved []Target
	seen := make(map[string]bool, len(entries))
	add := func(t Target) {
		key := t.GOOS + "/" + t.GOARCH
		if seen[key] {
			return
		}
		seen[key] = true
		resolved = append(resolved, t)
	}
	for _, entry := range entries {
		for _, field := range strings.FieldsFunc(entry, func(r rune) bool {
			return r == ',' || r == ' ' || r == '\t' || r == '\n'
		}) {
			if field == "" {
				continue
			}
			// One vocabulary, one resolver: TargetsFor owns the token handling
			// too, so `--platforms host` and `--target host` cannot drift apart.
			targets, err := TargetsFor(field)
			if err != nil {
				return nil, err
			}
			for _, p := range targets {
				add(p)
			}
		}
	}
	return resolved, nil
}

// validPlatformWord reports whether s can be a GOOS or GOARCH.
//
// Go's own values are lowercase letters and digits (linux, darwin, js, amd64,
// arm64, riscv64, wasm), so that is the whole rule. It deliberately does NOT
// check membership in a fixed list: `go tool dist list` grows, and rejecting a
// platform this extension has not heard of would be worse than the problem it
// solves. What it does catch is input that cannot be a platform under any future
// Go release — punctuation, spaces, JSON — which is what a malformed parameter
// or a typo actually looks like.
func validPlatformWord(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// TargetsFor resolves one platform request: the `host` token, the `all` token,
// or an `os` / `os/arch` pair.
//
// It is the SOLE resolver of the platform vocabulary, so every entry point
// accepts the same words. `--target` used to reach the `os/arch` branch
// directly, which made `--target host` synthesize Target{GOOS: "host"} and fail
// as `GOOS=host go build` — a documented word producing a cryptic toolchain
// error, because two call paths disagreed about the vocabulary they shared.
//
// A request that names a distribution platform reuses that entry (and with it
// the archive suffix consumers already index by); an `os`-only request expands
// to every distribution platform for that OS; anything else is synthesized so a
// target outside the matrix (windows/arm64, linux/riscv64) still compiles.
//
// A request that cannot be a platform is rejected here rather than synthesized,
// so a malformed parameter or a typo fails with a message naming the value
// instead of reaching the toolchain as `GOOS=<garbage>`.
func TargetsFor(request string) ([]Target, error) {
	switch request {
	case PlatformHost:
		return []Target{HostTarget()}, nil
	case PlatformAll:
		return append([]Target(nil), ArchivePlatforms...), nil
	}

	goosVal, goarchVal, hasArch := strings.Cut(request, "/")
	if !validPlatformWord(goosVal) || (hasArch && !validPlatformWord(goarchVal)) {
		return nil, fmt.Errorf(
			"invalid platform %q: expected \"os\" or \"os/arch\" (e.g. linux/amd64), or one of %q, %q",
			request, PlatformHost, PlatformAll)
	}

	var selected []Target
	for _, p := range ArchivePlatforms {
		if p.GOOS != goosVal {
			continue
		}
		if goarchVal != "" && p.GOARCH != goarchVal {
			continue
		}
		selected = append(selected, p)
	}
	if len(selected) > 0 {
		return selected, nil
	}

	suffixRequest := request
	if goarchVal == "" {
		suffixRequest = goosVal
	}
	return []Target{{
		GOOS:   goosVal,
		GOARCH: goarchVal,
		Suffix: SuffixFromDockerPlatform(suffixRequest),
	}}, nil
}

// Package channels a `package` invocation can produce, spelled as the job
// parameters and the project's `publish` declaration spell them.
//
// `archives` and `extension-archives` are the SAME channel under two names: the
// publish declaration uses either, and the pipeline step is gated on
// `params.archives`. Both are accepted here so a project cannot be scoped
// differently from the way it is packaged.
const (
	ChannelArchives          = "archives"
	ChannelExtensionArchives = "extension-archives"
	ChannelTemplateArchives  = "template-archives"
	ChannelDocker            = "docker"
	ChannelGoModule          = "go"
)

// DefaultDockerPlatform is the image platform assumed when none is named. It
// mirrors the `platform` flag default of the `package` command, so the platform
// the image is assembled for and the platform its binary is compiled for cannot
// disagree by omission.
const DefaultDockerPlatform = "linux/amd64"

// PackageChannels is the set of channels ONE `package` invocation will produce.
//
// It is the distribution intent's second axis: the
// intent says the compile is for distribution, and the channels say which
// platforms that distribution actually consumes. A docker image consumes ONE
// binary — the image's — while a release archive set consumes the declared
// matrix, and until this type existed both paid for the matrix.
type PackageChannels struct {
	Archives         bool
	TemplateArchives bool
	Docker           bool
	GoModule         bool
}

// Any reports whether the invocation named any channel at all. A caller that
// names none is not asking for "nothing": it has not expressed its demand, and
// the resolver answers such a caller with the declared matrix rather than an
// empty set.
func (c PackageChannels) Any() bool {
	return c.Archives || c.TemplateArchives || c.Docker || c.GoModule
}

// PackageChannelsFor resolves the channel set from a list of channel names —
// either the `publish` declaration or the channel parameters an invocation set.
func PackageChannelsFor(names []string) PackageChannels {
	var channels PackageChannels
	for _, name := range names {
		switch name {
		case ChannelArchives, ChannelExtensionArchives:
			channels.Archives = true
		case ChannelTemplateArchives:
			channels.TemplateArchives = true
		case ChannelDocker:
			channels.Docker = true
		case ChannelGoModule:
			channels.GoModule = true
		}
	}
	return channels
}

// DistributionRequest carries the plan-time inputs that decide the distribution
// platform set. Every field is a resolved job parameter or a declared project
// fact the task's cache key already covers; nothing here may be discovered at
// execution time.
type DistributionRequest struct {
	// Channels is what this invocation will package.
	Channels PackageChannels
	// Declared is the `platforms` spec (project option or --platforms).
	Declared []string
	// DockerPlatform is the image platform (`--platform`), empty for the default.
	DockerPlatform string
	// Target is a `--target` override naming one platform.
	Target string
}

// DeclaredTargets is the RELEASE matrix: the platforms an archive set carries.
//
// It is the declared `platforms` spec when there is one, and ArchivePlatforms
// otherwise. There is no widening: a project that declares nothing gets the
// whole archive matrix, and a project that declares a
// narrower set gets exactly that set — both in the compile that produces the
// binaries and in the archives that stage them, because both call this function.
func DeclaredTargets(spec []string) ([]Target, error) {
	declared, err := ParsePlatformSpec(spec)
	if err != nil {
		return nil, err
	}
	if len(declared) > 0 {
		return declared, nil
	}
	return append([]Target(nil), ArchivePlatforms...), nil
}

// dockerTarget resolves the ONE compile target an image platform names.
//
// An image is a single-platform artifact: its manifest names one os/arch and it
// copies in one binary. The docker channel therefore has an exact platform
// demand, and this is the function that says so — an `os`-only value such as
// "linux" is rejected rather than expanded, because there is no image to put a
// second binary in.
func dockerTarget(dockerPlatform string) (Target, error) {
	if dockerPlatform == "" {
		dockerPlatform = DefaultDockerPlatform
	}
	targets, err := TargetsFor(dockerPlatform)
	if err != nil {
		return Target{}, err
	}
	if len(targets) != 1 {
		return Target{}, fmt.Errorf(
			"image platform %q names %d platforms: an image carries exactly one, so it must be an \"os/arch\" pair (e.g. %s)",
			dockerPlatform, len(targets), DefaultDockerPlatform)
	}
	return targets[0], nil
}

// DistributionTargets resolves the platform set the DISTRIBUTION intent owes,
// scoped to the channels the invocation will actually produce.
//
// The rules, in order:
//
//   - `--target` names one platform and wins over everything: it is the
//     narrowest, most explicit, per-invocation request.
//   - A docker channel owes its IMAGE's platform, and only that one. Four
//     cross-compiles to fill a one-platform image were four full compiles of
//     which three were discarded.
//   - An archive channel owes the declared release matrix (DeclaredTargets).
//   - Channels that consume no binary — the go module channel, template
//     archives — owe nothing. A project that publishes module source and has a
//     `package main` used to cross-compile the whole matrix for a channel that
//     never opens the bin/ tree.
//   - An invocation that names NO channel keeps the declared matrix. Silence is
//     an unexpressed demand, not an empty one, and answering it with nothing
//     would turn a manual or third-party invocation into a silent no-op.
//
// The union is taken when several channels are active, so a project that ships
// both archives and an image still compiles everything both need, in a
// deterministic order and with duplicates dropped.
func DistributionTargets(req DistributionRequest) ([]Target, error) {
	if req.Target != "" {
		return TargetsFor(req.Target)
	}
	if !req.Channels.Any() {
		return DeclaredTargets(req.Declared)
	}

	var resolved []Target
	seen := make(map[string]bool, len(ArchivePlatforms)+1)
	add := func(targets ...Target) {
		for _, t := range targets {
			key := t.GOOS + "/" + t.GOARCH
			if seen[key] {
				continue
			}
			seen[key] = true
			resolved = append(resolved, t)
		}
	}

	if req.Channels.Archives {
		declared, err := DeclaredTargets(req.Declared)
		if err != nil {
			return nil, err
		}
		add(declared...)
	}
	if req.Channels.Docker {
		image, err := dockerTarget(req.DockerPlatform)
		if err != nil {
			return nil, err
		}
		add(image)
	}
	return resolved, nil
}

// PlatformsSpec decodes the resolved `platforms` job parameter.
//
// It accepts the JSON array a putnami.json option naturally carries and the
// string a CLI flag delivers, so `"platforms": ["linux/amd64"]` and
// `--platforms linux/amd64` resolve identically. The caller passes the value
// from its merged, plan-time parameter map and never from the project's config
// file, which is what keeps it a cache-key input.
//
// Bytes that are neither shape are an ERROR, not a platform. Treating them as a
// literal spec (`{"oops":1}` becoming GOOS `{"oops":1}`) turns a typo into a
// cryptic `go build` failure several layers down, and — worse — into a real
// cache key, so the nonsense is recorded as a legitimate platform request and
// served back on the next run.
func PlatformsSpec(raw json.RawMessage) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var list []string
	if json.Unmarshal(raw, &list) == nil {
		return list, nil
	}
	var single string
	if json.Unmarshal(raw, &single) == nil {
		return []string{single}, nil
	}
	return nil, fmt.Errorf(
		"invalid `platforms` parameter %s: expected a platform string (\"linux/amd64\", \"host\", \"all\") "+
			"or an array of them", string(raw))
}

// SuffixFromDockerPlatform converts a Docker platform string (e.g. "linux/amd64")
// to the archive suffix (e.g. "linux-x64").
func SuffixFromDockerPlatform(dockerPlatform string) string {
	for _, p := range ArchivePlatforms {
		if dockerPlatform == p.GOOS+"/"+p.GOARCH {
			return p.Suffix
		}
	}
	// Best-effort fallback: replace "/" with "-" and map known arch names.
	s := strings.ReplaceAll(dockerPlatform, "/", "-")
	s = strings.ReplaceAll(s, "amd64", "x64")
	return s
}

// resolveProjectConfig prefers putnami.json and reads .putnamirc.json when it is absent.
func resolveProjectConfig(dir string) string {
	current := filepath.Join(dir, "putnami.json")
	if _, err := os.Stat(current); err == nil {
		return current
	}
	legacy := filepath.Join(dir, ".putnamirc.json")
	if _, err := os.Stat(legacy); err == nil {
		return legacy
	}
	return current
}

// ReadGoEntrypoint reads the @putnami/go entrypoint from the project's config.
// Returns the entrypoint path (e.g. "./cmd/putnami") or "./cmd/putnami-go" as default.
func ReadGoEntrypoint(projectRoot string) string {
	data, err := os.ReadFile(resolveProjectConfig(projectRoot))
	if err != nil {
		return "./cmd/putnami-go"
	}
	var rc struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	if json.Unmarshal(data, &rc) != nil {
		return "./cmd/putnami-go"
	}
	goOpts, ok := rc.Options["@putnami/go"]
	if !ok {
		return "./cmd/putnami-go"
	}
	var opts struct {
		Entrypoint string `json:"entrypoint"`
	}
	if json.Unmarshal(goOpts, &opts) != nil || opts.Entrypoint == "" {
		return "./cmd/putnami-go"
	}
	return opts.Entrypoint
}

// Executable is one extra program a project ships beside its runtime binary.
// Package is the Go package `go build` compiles; Name is the file it becomes
// under compiled/ in every platform archive.
type Executable struct {
	Name    string `json:"name"`
	Package string `json:"package"`
}

// ReadGoExecutables reads `options["@putnami/go"].executables` from the
// project's config: the programs the cross-compile step builds for every
// platform beside the entrypoint, and the archive packager stages and checks
// in compiled/. A project that declares none returns nil.
//
// The list is read from putnami.json, which both tasks already key on, so a
// change to it invalidates the cross-compile and the archives it feeds.
func ReadGoExecutables(projectRoot string) ([]Executable, error) {
	data, err := os.ReadFile(resolveProjectConfig(projectRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read project config: %w", err)
	}
	var rc struct {
		Options map[string]json.RawMessage `json:"options"`
	}
	if err := json.Unmarshal(data, &rc); err != nil {
		return nil, fmt.Errorf("parse project config: %w", err)
	}
	goOpts, ok := rc.Options["@putnami/go"]
	if !ok {
		return nil, nil
	}
	var opts struct {
		Executables []Executable `json:"executables"`
	}
	if err := json.Unmarshal(goOpts, &opts); err != nil {
		return nil, fmt.Errorf(`parse options["@putnami/go"].executables: %w`, err)
	}
	seen := make(map[string]bool, len(opts.Executables))
	for _, e := range opts.Executables {
		if e.Name == "" || e.Name != filepath.Base(e.Name) || e.Name == "." || e.Name == ".." || strings.ContainsAny(e.Name, `/\`) {
			return nil, fmt.Errorf(`options["@putnami/go"].executables: name %q must be a plain file name`, e.Name)
		}
		if strings.TrimSpace(e.Package) == "" {
			return nil, fmt.Errorf(`options["@putnami/go"].executables: %q declares no package`, e.Name)
		}
		if seen[e.Name] {
			return nil, fmt.Errorf(`options["@putnami/go"].executables: %q is declared twice`, e.Name)
		}
		seen[e.Name] = true
	}
	return opts.Executables, nil
}

// DeriveBinaryName extracts the binary name from the entrypoint path.
// Falls back to deriving from the package name (e.g. "@putnami/ci" → "putnami-ci").
func DeriveBinaryName(entrypoint, packageName string) string {
	base := filepath.Base(entrypoint)
	// If entrypoint is just "./cmd" or "cmd", derive from package name
	if base == "cmd" || base == "." {
		name := strings.TrimPrefix(packageName, "@")
		name = strings.ReplaceAll(name, "/", "-")
		return name
	}
	return base
}

// HostBinaryName is the file name run and serve build an entrypoint to before
// they start it on a goos host: DeriveBinaryName with the .exe suffix Windows
// needs to start a file.
func HostBinaryName(goos, entrypoint, packageName string) string {
	return pkgmeta.ExecutableName(goos, DeriveBinaryName(entrypoint, packageName))
}

// servePreferredNames lists cmd/ subdirectory names preferred for serving,
// in priority order.
var servePreferredNames = []string{"serve", "server", "", "api"}

// ResolveServeEntrypoint determines which Go package to run for the serve command.
// Priority: explicit entrypoint > root main.go > cmd/ subdirectory auto-detection.
// For cmd/ with multiple candidates, it applies preference heuristics (serve, server, <project-name>, api).
func ResolveServeEntrypoint(projectPath, projectName, explicitEntrypoint string) (string, error) {
	if explicitEntrypoint != "" {
		return explicitEntrypoint, nil
	}

	// Root main.go — simple project layout.
	if _, err := os.Stat(filepath.Join(projectPath, "main.go")); err == nil {
		return ".", nil
	}

	// Scan cmd/ for subdirectories with Go files.
	candidates := scanCmdSubdirs(projectPath)

	if len(candidates) == 0 {
		return "", fmt.Errorf("no main package found in project root or cmd/. Use --entrypoint to specify one, or add a main.go")
	}
	if len(candidates) == 1 {
		return "./cmd/" + candidates[0], nil
	}

	// Multiple candidates — apply preference heuristics.
	baseName := filepath.Base(projectName)
	for _, pref := range servePreferredNames {
		name := pref
		if name == "" {
			name = baseName
		}
		for _, c := range candidates {
			if c == name {
				return "./cmd/" + c, nil
			}
		}
	}

	return "", fmt.Errorf("multiple cmd/ entries found (%s). Use --entrypoint to specify which to serve (e.g. --entrypoint ./cmd/%s)", strings.Join(candidates, ", "), candidates[0])
}

// scanCmdSubdirs returns sorted names of cmd/ subdirectories that contain Go files.
func scanCmdSubdirs(projectPath string) []string {
	cmdDir := filepath.Join(projectPath, "cmd")
	entries, err := os.ReadDir(cmdDir)
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		if HasMainGo(filepath.Join(cmdDir, entry.Name())) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// HasMainGo checks if the given directory contains a main.go file
// (directly or as the only Go file pattern for a main package).
func HasMainGo(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err == nil {
		return true
	}
	// Check for any .go files (the directory itself may be the main package)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			return true
		}
	}
	return false
}
