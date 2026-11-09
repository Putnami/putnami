package swift

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/stacks"
)

// ProvisionedEnv names the swift a task installed for its steps.
const ProvisionedEnv = "PUTNAMI_SWIFT_EXECUTABLE"

const (
	// defaultRelease is the release a package gets when it states none.
	defaultRelease = "6.4.2"
	swiftlyVersion = "1.2.0"
	// downloadTimeout bounds the download of swiftly.
	downloadTimeout = 5 * time.Minute
)

// A release swiftly installs, as .swift-version states it: 6, 6.4 or 6.4.2.
// It reaches swiftly as an argument, so it cannot start an option.
var releasePattern = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}$`)

// A toolchain directory swiftly writes: 6.4.2 on Linux,
// swift-6.4.2-RELEASE.xctoolchain on macOS. A snapshot names no release.
var toolchainDirPattern = regexp.MustCompile(`^(?:swift-)?([0-9]+(?:\.[0-9]+){1,2})(?:-RELEASE)?(?:\.xctoolchain)?$`)

// hostPlatform is the platform the extension runs on. Tests name another.
var hostPlatform = runtime.GOOS + "/" + runtime.GOARCH

// swiftlyArtifacts are where the pinned swiftly is published, per platform,
// with the SHA-256 of each file. The Linux archives hold swiftly at their
// root; the macOS package, one for both architectures, holds bin/swiftly.
// Tests point them at a local server.
var swiftlyArtifacts = map[string]swiftlyArtifact{
	"linux/amd64": {
		URL:    "https://download.swift.org/swiftly/linux/swiftly-" + swiftlyVersion + "-x86_64.tar.gz",
		SHA256: "0b26b568811404374c7d7f07276b6aad4145db5d1893a45f307de6a94ca8d9c3",
	},
	"linux/arm64": {
		URL:    "https://download.swift.org/swiftly/linux/swiftly-" + swiftlyVersion + "-aarch64.tar.gz",
		SHA256: "590d855e03807791f1b84aab77471bf7e31f9e3b22151972526077da296ecf96",
	},
	"darwin/amd64": {
		URL:    "https://download.swift.org/swiftly/darwin/swiftly-" + swiftlyVersion + ".pkg",
		SHA256: "a1b48692b00ea6902957423473c87bee7c7f3e18c015a4a09317102f6820b987",
		Pkg:    true,
	},
	"darwin/arm64": {
		URL:    "https://download.swift.org/swiftly/darwin/swiftly-" + swiftlyVersion + ".pkg",
		SHA256: "a1b48692b00ea6902957423473c87bee7c7f3e18c015a4a09317102f6820b987",
		Pkg:    true,
	},
}

// swiftlyArtifact is one published swiftly: a tar.gz archive, or a macOS
// package.
type swiftlyArtifact struct {
	URL    string
	SHA256 string
	Pkg    bool
}

// Provide installs the release .swift-version states, or the default one when
// it states none, with the swiftly the extension pins, in a swiftly home of its
// own under the Putnami home, and returns its swift. A release an earlier task
// installed is reused without running swiftly. swiftly checks the toolchain's
// signature with gpg on Linux, in a keyring of that home. The machine's
// swiftly, toolchains and keyring stay untouched.
func Provide(ctx context.Context, toolchain stacks.Toolchain, expected []stacks.Expected, info func(string)) (stacks.Provided, error) {
	check := toolchain.Checks()[0]
	if check.Command[0] != "swift" || len(expected) == 0 {
		return stacks.Provided{}, fmt.Errorf("%s installs no %s", ExtensionName, check.Command[0])
	}
	stated := expected[0]
	if stated.Version == "" {
		stated = stacks.Expected{Version: defaultRelease, Source: ExtensionName}
	}
	if !releasePattern.MatchString(stated.Version) {
		return stacks.Provided{}, fmt.Errorf("%s states %q, which names no Swift release swiftly installs: put a swift that satisfies it on PATH", stated.Source, stated.Version)
	}
	artifact, ok := swiftlyArtifacts[hostPlatform]
	if !ok {
		return stacks.Provided{}, fmt.Errorf("no Swift is installed on %s, only on %s: put swift on PATH", hostPlatform, strings.Join(supportedPlatforms(), ", "))
	}
	base, release, err := lockedHome()
	if err != nil {
		return stacks.Provided{}, err
	}
	defer release()
	h := home(base)
	admitted := func(version string) bool { return check.Compare(stated, version).Code == "" }
	if dir, ok := h.newest(admitted, true); ok {
		return stacks.Provided{Executable: h.swift(dir)}, nil
	}
	swiftly, err := provideSwiftly(ctx, base, artifact)
	if err != nil {
		return stacks.Provided{}, err
	}
	if err := h.prepare(); err != nil {
		return stacks.Provided{}, err
	}
	if _, err := os.Stat(filepath.Join(h.swiftlyHome, "config.json")); err != nil {
		if err := h.run(ctx, swiftly, "init", "--assume-yes", "--no-modify-profile", "--skip-install", "--quiet-shell-followup"); err != nil {
			return stacks.Provided{}, fmt.Errorf("swiftly init: %w", err)
		}
	}
	info("installing Swift " + stated.Version + " under " + h.toolchains)
	postInstall := filepath.Join(base, "post-install-"+stated.Version+".sh")
	_ = os.Remove(postInstall)
	if err := h.run(ctx, swiftly, "install", stated.Version, "--assume-yes", "--post-install-file", postInstall); err != nil {
		return stacks.Provided{}, fmt.Errorf("swiftly install %s: %w", stated.Version, err)
	}
	// swiftly names, rather than runs, the system packages a toolchain needs
	// on Linux, since that takes root.
	if script, err := os.ReadFile(postInstall); err == nil && strings.TrimSpace(string(script)) != "" { //nolint:gosec // a file of the extension's own folder
		info("Swift " + stated.Version + " needs system packages this machine lacks; run as root:\n" + strings.TrimSpace(string(script)))
	}
	_ = os.Remove(postInstall)
	dir, ok := h.newest(admitted, false)
	if !ok {
		return stacks.Provided{}, fmt.Errorf("swiftly install %s left no toolchain of that release under %s", stated.Version, h.toolchains)
	}
	if err := os.WriteFile(h.marker(dir), nil, 0o644); err != nil { //nolint:gosec // a marker in the extension's own folder
		return stacks.Provided{}, err
	}
	return stacks.Provided{Executable: h.swift(dir)}, nil
}

// swiftlyHome is the layout of the extension's folder: swiftly's home, its
// toolchains, its keyring, and a marker per toolchain an install finished.
type swiftlyHome struct {
	swiftlyHome, bin, toolchains, gnupg, installed string
}

func home(base string) swiftlyHome {
	return swiftlyHome{
		swiftlyHome: filepath.Join(base, "swiftly-home"),
		// swiftly init moves the swiftly it runs into its bin directory, and
		// leaves one that already runs from there in place: the pinned
		// swiftly's own directory is that bin directory.
		bin:        filepath.Join(base, "swiftly-"+swiftlyVersion),
		toolchains: filepath.Join(base, "toolchains"),
		gnupg:      filepath.Join(base, "gnupg"),
		installed:  filepath.Join(base, "installed"),
	}
}

func (h swiftlyHome) prepare() error {
	for _, dir := range []string{h.swiftlyHome, h.bin, h.toolchains, h.installed} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	// gpg refuses a home others can read.
	return os.MkdirAll(h.gnupg, 0o700)
}

func (h swiftlyHome) swift(dir string) string {
	return filepath.Join(h.toolchains, dir, "usr", "bin", "swift")
}

func (h swiftlyHome) marker(dir string) string {
	return filepath.Join(h.installed, dir)
}

// newest returns the toolchain directory of the newest release admitted
// holds, among those an install finished when finished is true, else among
// all of them.
func (h swiftlyHome) newest(admitted func(string) bool, finished bool) (string, bool) {
	entries, err := os.ReadDir(h.toolchains)
	if err != nil {
		return "", false
	}
	best, bestVersion := "", ""
	for _, e := range entries {
		m := toolchainDirPattern.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		// A macOS toolchain of a patch-zero release before 6.4 is named
		// swift-6.0-RELEASE: the release is 6.0.0.
		version := m[1]
		if strings.Count(version, ".") == 1 {
			version += ".0"
		}
		if !admitted(version) {
			continue
		}
		if _, err := os.Stat(h.swift(e.Name())); err != nil {
			continue
		}
		if _, err := os.Stat(h.marker(e.Name())); finished && err != nil {
			continue
		}
		if best == "" || compareVersions(version, bestVersion) > 0 {
			best, bestVersion = e.Name(), version
		}
	}
	return best, best != ""
}

// run runs the pinned swiftly with the extension's home, toolchains and
// keyring, whatever the machine's swiftly or gpg names.
func (h swiftlyHome) run(ctx context.Context, swiftly string, args ...string) error {
	cmd := exec.CommandContext(ctx, swiftly, args...) //nolint:gosec // the pinned swiftly, with a checked release
	cmd.Dir = filepath.Dir(h.swiftlyHome)
	cmd.Env = append(withoutSwiftlyEnv(os.Environ()),
		"SWIFTLY_HOME_DIR="+h.swiftlyHome,
		"SWIFTLY_BIN_DIR="+h.bin,
		"SWIFTLY_TOOLCHAINS_DIR="+h.toolchains,
		"GNUPGHOME="+h.gnupg,
	)
	// The job's stdout carries its protocol. The end of swiftly's stderr,
	// where it says why it failed, also reaches the error.
	tail := &tailWriter{max: 2048}
	cmd.Stdout = os.Stderr
	cmd.Stderr = io.MultiWriter(os.Stderr, tail)
	if err := cmd.Run(); err != nil {
		if said := strings.TrimSpace(string(tail.buf)); said != "" {
			return fmt.Errorf("%w: %s", err, said)
		}
		return err
	}
	return nil
}

// tailWriter keeps the last max bytes written to it.
type tailWriter struct {
	buf []byte
	max int
}

func (w *tailWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	if len(w.buf) > w.max {
		w.buf = w.buf[len(w.buf)-w.max:]
	}
	return len(p), nil
}

// withoutSwiftlyEnv drops what would select another swiftly home or keyring.
func withoutSwiftlyEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "SWIFTLY_") && key != "GNUPGHOME" {
			out = append(out, entry)
		}
	}
	return out
}

// compareVersions orders two dotted numeric versions.
func compareVersions(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := range max(len(as), len(bs)) {
		var x, y int
		if i < len(as) {
			x, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			y, _ = strconv.Atoi(bs[i])
		}
		if x != y {
			return x - y
		}
	}
	return 0
}

// lockedHome returns the extension's folder under the Putnami home, holding the
// lock every install of the machine takes, and the function that releases it.
func lockedHome() (string, func(), error) {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", nil, fmt.Errorf("no Putnami home: set %s", putnamihome.Env)
	}
	base := filepath.Join(home, "tools", "swift")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, err
	}
	lock, err := filelock.Acquire(filepath.Join(base, ".lock"), true, false)
	if err != nil {
		return "", nil, fmt.Errorf("lock %s: %w", base, err)
	}
	// A download an interrupted install left behind.
	if stale, err := filepath.Glob(filepath.Join(base, "download-*")); err == nil {
		for _, dir := range stale {
			_ = os.RemoveAll(dir)
		}
	}
	return base, func() { _ = lock.Release() }, nil
}

// provideSwiftly returns the path of the pinned swiftly, downloading it when it
// is missing, and refusing a file whose SHA-256 is not the pinned one.
func provideSwiftly(ctx context.Context, base string, artifact swiftlyArtifact) (string, error) {
	dir := filepath.Join(base, "swiftly-"+swiftlyVersion)
	path := filepath.Join(dir, "swiftly")
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	if !artifact.Pkg {
		pin := pinnedarchive.Pin{URL: artifact.URL, SHA256: artifact.SHA256, Format: pinnedarchive.TarGz}
		if _, err := pinnedarchive.Install(ctx, pin, dir, pinnedarchive.Options{Limits: pinnedarchive.DefaultLimits(), Complete: func(candidate string) bool {
			_, statErr := os.Stat(filepath.Join(candidate, "swiftly"))
			return statErr == nil
		}}); err != nil {
			return "", fmt.Errorf("download swiftly %s: %w", swiftlyVersion, err)
		}
		return path, nil
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(base, "download-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	pkg := filepath.Join(staging, "swiftly.pkg")
	// pinnedarchive.Download does not open the file, so the format it
	// requires says nothing of a package.
	pin := pinnedarchive.Pin{URL: artifact.URL, SHA256: artifact.SHA256, Format: pinnedarchive.TarGz}
	if err := pinnedarchive.Download(ctx, pin, pkg, pinnedarchive.Options{}); err != nil {
		return "", fmt.Errorf("download swiftly %s: %w", swiftlyVersion, err)
	}
	stage := filepath.Join(staging, "swiftly-"+swiftlyVersion)
	if err := os.MkdirAll(stage, 0o755); err != nil {
		return "", err
	}
	if err := extractPkgFile(pkg, "bin/swiftly", filepath.Join(stage, "swiftly")); err != nil {
		return "", fmt.Errorf("unpack swiftly %s: %w", swiftlyVersion, err)
	}
	return path, robustio.Rename(stage, dir)
}

// Tools appends the directory of the toolchain's llvm-cov to the PATH of a
// package's test task when that PATH holds none, so swiftpm-test exports the
// LCOV with the llvm-cov of the swift that ran the tests. It looks beside the
// swift of the PATH, its links resolved, then in the toolchain swift names as
// its runtime resource path, which a swiftly proxy also answers.
func Tools(ctx context.Context, task string, meta stacks.Metadata, env []string, _ func(string)) ([]string, error) {
	if task != stacks.TaskTest || meta.Variant != VariantSwiftPM {
		return nil, nil
	}
	if _, ok := stacks.LookPathIn(env, "llvm-cov"); ok {
		return nil, nil
	}
	swift, ok := stacks.LookPathIn(env, "swift")
	if !ok {
		return nil, nil
	}
	var candidates []string
	if resolved, err := filepath.EvalSymlinks(swift); err == nil {
		candidates = append(candidates, filepath.Dir(resolved))
	}
	if usr, ok := resourceUsr(ctx, swift, env); ok {
		candidates = append(candidates, filepath.Join(usr, "bin"))
	}
	for _, dir := range candidates {
		if _, ok := stacks.LookPathIn([]string{"PATH=" + dir}, "llvm-cov"); ok {
			return []string{dir}, nil
		}
	}
	return nil, nil
}

// resourceUsr returns the usr directory of the toolchain swift runs: the
// grandparent of its runtime resource path, <toolchain>/usr/lib/swift.
func resourceUsr(ctx context.Context, swift string, env []string) (string, bool) {
	cmd := exec.CommandContext(ctx, swift, "-print-target-info") //nolint:gosec // the stack's own tool
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		return "", false
	}
	var target struct {
		Paths struct {
			RuntimeResourcePath string `json:"runtimeResourcePath"`
		} `json:"paths"`
	}
	if json.Unmarshal(out, &target) != nil || !filepath.IsAbs(target.Paths.RuntimeResourcePath) {
		return "", false
	}
	return filepath.Dir(filepath.Dir(target.Paths.RuntimeResourcePath)), true
}

// supportedPlatforms are the platforms the extension installs Swift on.
func supportedPlatforms() []string {
	var out []string
	for p := range swiftlyArtifacts {
		out = append(out, p)
	}
	slices.Sort(out)
	return out
}
