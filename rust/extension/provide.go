package rust

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/filelock"
	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/stacks"
)

// ProvisionedEnv names the Rust compiler a task installed for its steps.
const ProvisionedEnv = "PUTNAMI_RUST_EXECUTABLE"

const (
	// defaultChannel is the release a repository gets when it states none.
	defaultChannel = "1.99.1"
	rustupVersion  = "1.29.3"
	nextestVersion = "0.9.151"
	llvmCovVersion = "0.9.1"
	// downloadTimeout bounds the download of rustup.
	downloadTimeout = 5 * time.Minute
	// installedMarker is the file of a toolchain whose rustup install finished.
	installedMarker = ".putnami-installed"
)

// A rustup channel, as a rust-toolchain file states it: it reaches rustup as
// an argument and names a directory, so it cannot start an option or a path.
var channelPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// hostPlatform is the platform the extension runs on. Tests name another.
var hostPlatform = runtime.GOOS + "/" + runtime.GOARCH

// hostTriples are the Rust names of the platforms the extension installs on.
var hostTriples = map[string]string{
	"darwin/amd64":  "x86_64-apple-darwin",
	"darwin/arm64":  "aarch64-apple-darwin",
	"linux/amd64":   "x86_64-unknown-linux-gnu",
	"linux/arm64":   "aarch64-unknown-linux-gnu",
	"windows/amd64": "x86_64-pc-windows-msvc",
	"windows/arm64": "aarch64-pc-windows-msvc",
}

// rustupDigests are the SHA-256 of the pinned rustup-init, per platform.
// Tests pin the file their local server publishes.
var rustupDigests = map[string]string{
	"darwin/amd64":  "bc107f1fb0228de7fa32413c89c1b305cea9cd256e94b01150cd054128145429",
	"darwin/arm64":  "63752a1919a3a4793173ec70f8f212621a7ce38ba6a246095984482f4de20f9f",
	"linux/amd64":   "b1c8cb14e30889e7e2999e6b69e62d9e9b9ffc0f4df8180564d306d9680e6c55",
	"linux/arm64":   "9f608c142e0e38a78957d0661ba1dfaf0b6fb37184cfaa4b9a0aa57af6184282",
	"windows/amd64": "38027d51d69a5d05493627a0634dbea52d6cd793c8f072d2373d3f4163eec7db",
	"windows/arm64": "407e0d72f81d26a01f9d032b8317908cbb2917ba22765474b682b41390792ad9",
}

// rustupURL is where rustup-init is published for a triple and an executable
// suffix. Tests point it at a local server.
var rustupURL = func(triple, suffix string) string {
	return "https://static.rust-lang.org/rustup/archive/" + rustupVersion + "/" + triple + "/rustup-init" + suffix
}

// tool is a cargo subcommand the test task runs, published as a tar.gz archive
// that holds its binary at the root.
type tool struct {
	binary  string
	version string
	url     func(triple string) string
	digests map[string]string
}

// testTools are the tools of the test task. Tests point them at a local server.
var testTools = []tool{
	{
		binary:  "cargo-nextest",
		version: nextestVersion,
		url: func(triple string) string {
			if strings.HasSuffix(triple, "-apple-darwin") {
				triple = "universal-apple-darwin"
			}
			return "https://github.com/nextest-rs/nextest/releases/download/cargo-nextest-" + nextestVersion + "/cargo-nextest-" + nextestVersion + "-" + triple + ".tar.gz"
		},
		digests: map[string]string{
			"darwin/amd64":  "d1d7d025d4bfc9fb154cd76aea3949bd78534829a0589abefa6e5dd4edb135dd",
			"darwin/arm64":  "d1d7d025d4bfc9fb154cd76aea3949bd78534829a0589abefa6e5dd4edb135dd",
			"linux/amd64":   "1d4ce302b820c41f4b89c5fb9bce45f0f0c94882c1cf08fc360caf08be6e7df4",
			"linux/arm64":   "3e35e8f6913af92558ba09d7155f74e35dd1f0024ef3cd891637433ae4eacbdd",
			"windows/amd64": "e51bdb4b5883df48f335381b287378edb244318fac65ae0adaf6b9e69298246b",
			"windows/arm64": "f41cd710db6f2e8f787ec11b7d1007aa66fbea2b8a6f59fd5fe453f5c6fe3699",
		},
	},
	{
		binary:  "cargo-llvm-cov",
		version: llvmCovVersion,
		url: func(triple string) string {
			return "https://github.com/taiki-e/cargo-llvm-cov/releases/download/v" + llvmCovVersion + "/cargo-llvm-cov-" + triple + ".tar.gz"
		},
		digests: map[string]string{
			"darwin/amd64":  "ddf949b82a63017441ea76aa068bdd7e2dde8055f47cd3ef736fdbbd633bc33e",
			"darwin/arm64":  "7e821a6c90c0884f79f759f5d9be50799ade67bb82515186d922f26032ef43b8",
			"linux/amd64":   "b3f68e625481fed9b16444174f3fa5ebcdbde4a1878803a35eabe2dcefcdc41a",
			"linux/arm64":   "abf5f13c1520f8756d2192bfaaeadb0208f5b7eaa8cadc15bf847befa42b7360",
			"windows/amd64": "15eefd5b90fcdd4006b727c6e710d70a61aff509fcd7bb37f9c2a35c4f024948",
			"windows/arm64": "a827e57b93feab3d81ceb24c0602a1b778d6be72231ca83adaec909c1af0f7ef",
		},
	},
}

// Provide installs the stated Rust release, or the default one when the
// repository states none, with the rustup the extension pins, in a rustup home
// of its own under the Putnami home. rustup checks each component against the
// channel manifest. The rustup of the machine and its toolchains stay untouched.
func Provide(ctx context.Context, expected stacks.Expected, info func(string)) (stacks.Provided, error) {
	channel := expected.Version
	if channel == "" {
		channel = defaultChannel
	}
	if !channelPattern.MatchString(channel) {
		return stacks.Provided{}, fmt.Errorf("%s states %q, which is no rustup channel", expected.Source, channel)
	}
	triple, ok := hostTriples[hostPlatform]
	if !ok {
		return stacks.Provided{}, fmt.Errorf("no Rust is installed on %s: put rustc on PATH", hostPlatform)
	}
	base, release, err := lockedHome()
	if err != nil {
		return stacks.Provided{}, err
	}
	defer release()
	rustupHome := filepath.Join(base, "rustup-home")
	name := channel + "-" + triple
	toolchain := filepath.Join(rustupHome, "toolchains", name)
	provided := stacks.Provided{
		Executable: filepath.Join(toolchain, "bin", "rustc"+executableSuffix()),
		// A cargo proxy of the machine's rustup, which cargo finds in
		// ~/.cargo/bin before PATH, must answer with this toolchain too.
		Env: []string{"RUSTUP_HOME=" + rustupHome, "RUSTUP_TOOLCHAIN=" + name},
	}
	if _, err := os.Stat(filepath.Join(toolchain, installedMarker)); err == nil {
		return provided, nil
	}
	rustup, err := provideRustup(ctx, base, triple)
	if err != nil {
		return stacks.Provided{}, err
	}
	info("installing Rust " + channel + " under " + rustupHome)
	cmd := exec.CommandContext(ctx, rustup, "toolchain", "install", channel, "--profile", "minimal", "-c", "rustfmt", "-c", "clippy", "-c", "llvm-tools-preview", "--no-self-update") //nolint:gosec // the pinned rustup, with a checked channel
	cmd.Dir = base
	cmd.Env = append(withoutRustupEnv(os.Environ()), "RUSTUP_HOME="+rustupHome)
	// The job's stdout carries its protocol.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return stacks.Provided{}, fmt.Errorf("rustup toolchain install %s: %w", channel, err)
	}
	if err := os.WriteFile(filepath.Join(toolchain, installedMarker), nil, 0o644); err != nil { //nolint:gosec // a marker in the extension's own folder
		return stacks.Provided{}, err
	}
	return provided, nil
}

// Tools makes cargo-nextest and cargo-llvm-cov available to the test task,
// each only when the PATH of env holds none, and returns their directories.
func Tools(ctx context.Context, task string, env []string, info func(string)) ([]string, error) {
	if task != stacks.TaskTest {
		return nil, nil
	}
	var missing []tool
	for _, t := range testTools {
		if !onPath(env, t.binary+executableSuffix()) {
			missing = append(missing, t)
		}
	}
	if len(missing) == 0 {
		return nil, nil
	}
	triple, ok := hostTriples[hostPlatform]
	if !ok {
		return nil, fmt.Errorf("the test tools are not installed on %s: put cargo-nextest and cargo-llvm-cov on PATH", hostPlatform)
	}
	base, release, err := lockedHome()
	if err != nil {
		return nil, err
	}
	defer release()
	dirs := make([]string, 0, len(missing))
	for _, t := range missing {
		dir := filepath.Join(base, t.binary+"-"+t.version)
		binary := filepath.Join(dir, t.binary+executableSuffix())
		pin := pinnedarchive.Pin{URL: t.url(triple), SHA256: t.digests[hostPlatform], Format: pinnedarchive.TarGz}
		installed, err := pinnedarchive.Install(ctx, pin, dir, pinnedarchive.Options{Limits: pinnedarchive.DefaultLimits(), Complete: func(candidate string) bool {
			_, statErr := os.Stat(filepath.Join(candidate, filepath.Base(binary)))
			return statErr == nil
		}})
		if err != nil {
			return nil, fmt.Errorf("install %s %s: %w", t.binary, t.version, err)
		}
		if installed {
			info("installed " + t.binary + " " + t.version + " under " + dir)
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

// lockedHome returns the extension's folder under the Putnami home, holding the
// lock every install of the machine takes, and the function that releases it.
func lockedHome() (string, func(), error) {
	home := putnamihome.Resolve(os.Getenv)
	if home == "" {
		return "", nil, fmt.Errorf("no Putnami home: set %s", putnamihome.Env)
	}
	base := filepath.Join(home, "tools", "rust")
	if err := os.MkdirAll(base, 0o755); err != nil {
		return "", nil, err
	}
	lock, err := filelock.Acquire(filepath.Join(base, ".lock"), true, false)
	if err != nil {
		return "", nil, fmt.Errorf("lock %s: %w", base, err)
	}
	return base, func() { _ = lock.Release() }, nil
}

// provideRustup returns the path of the pinned rustup, downloading it when it
// is missing. rustup-init is rustup under another name: it dispatches on the
// name it runs as.
func provideRustup(ctx context.Context, base, triple string) (string, error) {
	digest, ok := rustupDigests[hostPlatform]
	if !ok {
		return "", fmt.Errorf("no rustup is pinned for %s", hostPlatform)
	}
	path := filepath.Join(base, "rustup-"+rustupVersion, "rustup"+executableSuffix())
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", err
	}
	staging, err := os.MkdirTemp(filepath.Dir(path), "download-*")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	stage := filepath.Join(staging, filepath.Base(path))
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	// pinnedarchive.Download does not open the file, so the format it
	// requires says nothing of a binary.
	pin := pinnedarchive.Pin{URL: rustupURL(triple, executableSuffix()), SHA256: digest, Format: pinnedarchive.TarGz}
	if err := pinnedarchive.Download(ctx, pin, stage, pinnedarchive.Options{}); err != nil {
		return "", fmt.Errorf("download rustup %s: %w", rustupVersion, err)
	}
	if err := os.Chmod(stage, 0o755); err != nil { //nolint:gosec // an executable
		return "", err
	}
	return path, robustio.Rename(stage, path)
}

// executableSuffix is the suffix of an executable on the host.
func executableSuffix() string {
	if strings.HasPrefix(hostPlatform, "windows/") {
		return ".exe"
	}
	return ""
}

// onPath reports whether a directory of the PATH of env holds name.
func onPath(env []string, name string) bool {
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || !strings.EqualFold(key, "PATH") {
			continue
		}
		for _, dir := range filepath.SplitList(value) {
			if info, err := os.Stat(filepath.Join(dir, name)); err == nil && !info.IsDir() {
				return true
			}
		}
		return false
	}
	return false
}

// withoutRustupEnv drops what would select another toolchain or home for the
// rustup the extension runs.
func withoutRustupEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, entry := range env {
		if key, _, _ := strings.Cut(entry, "="); key != "RUSTUP_HOME" && key != "RUSTUP_TOOLCHAIN" {
			out = append(out, entry)
		}
	}
	return out
}
