package dart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/stacks"
)

// Environment variables naming the SDK a task installed for its steps.
const (
	DartProvisionedEnv    = "PUTNAMI_DART_EXECUTABLE"
	FlutterProvisionedEnv = "PUTNAMI_FLUTTER_EXECUTABLE"
)

const (
	// installedMarker is the file of an SDK whose install finished.
	installedMarker = ".putnami-installed"
	// downloadTimeout bounds the download of one archive, a Flutter one of up
	// to 2.5 GB, within the 30 minutes a task may run.
	downloadTimeout = 25 * time.Minute
	// manifestTimeout bounds the read of a release list or a checksum.
	manifestTimeout = 2 * time.Minute
)

// hostPlatform is the platform the extension runs on. Tests name another.
var hostPlatform = runtime.GOOS + "/" + runtime.GOARCH

// Where the publishers list their stable releases. Tests point them at a
// local server.
var (
	dartArchiveURL     = "https://storage.googleapis.com/dart-archive/channels/stable/release"
	dartReleaseListURL = "https://storage.googleapis.com/storage/v1/b/dart-archive/o?prefix=channels/stable/release/&delimiter=/"
	flutterReleasesURL = "https://storage.googleapis.com/flutter_infra_release/releases"
)

// tarCommand unpacks a checked Flutter archive. A Linux archive is a .tar.xz,
// which the extension SDK does not unpack; the host's tar reads every format
// the publisher ships.
var tarCommand = "tar"

// stableVersion is a stable SDK release: numbers only.
var stableVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+$`)

// Provide installs the SDK the toolchain of a package checks: the Dart SDK
// for a dart command, the Flutter SDK for a flutter one. It picks the newest
// stable release every check admits, an install of an earlier task first, and
// checks the archive against the SHA-256 the publisher states (ADR 0001).
func Provide(ctx context.Context, toolchain stacks.Toolchain, expected []stacks.Expected, info func(string)) (stacks.Provided, error) {
	checks := toolchain.Checks()
	if len(expected) != len(checks) {
		return stacks.Provided{}, fmt.Errorf("%d expected versions for %d toolchain checks", len(expected), len(checks))
	}
	base, release, err := stacks.LockedToolsHome("dart")
	if err != nil {
		return stacks.Provided{}, err
	}
	defer release()
	switch checks[0].Command[0] {
	case "dart":
		return provideDart(ctx, base, checks, expected, info)
	case "flutter":
		return provideFlutter(ctx, base, checks, expected, info)
	}
	return stacks.Provided{}, fmt.Errorf("@putnami/dart installs no %s", checks[0].Command[0])
}

// admits reports whether a release passes a check: in the stated range, or
// nothing stated.
func admits(check stacks.Toolchain, expected stacks.Expected, version string) bool {
	v := check.Compare(expected, version)
	return v.Code == "" || v.Code == stacks.CodeToolchainUnpinned
}

func provideDart(ctx context.Context, base string, checks []stacks.Toolchain, expected []stacks.Expected, info func(string)) (stacks.Provided, error) {
	platform, ok := dartPlatforms[hostPlatform]
	if !ok {
		return stacks.Provided{}, fmt.Errorf("no Dart SDK is published for %s: put dart on PATH", hostPlatform)
	}
	admitted := func(version string) bool { return admits(checks[0], expected[0], version) }
	executable := func(dir string) string {
		return filepath.Join(dir, "dart-sdk", "bin", "dart"+stacks.ExecutableSuffix(hostPlatform))
	}
	if dir, ok := installed(base, "dart-sdk-", "-"+platform, admitted); ok {
		return stacks.Provided{Executable: executable(dir)}, nil
	}
	versions, err := dartReleases(ctx)
	if err != nil {
		return stacks.Provided{}, err
	}
	version, ok := newest(versions, admitted)
	if !ok {
		return stacks.Provided{}, fmt.Errorf("no stable Dart SDK is in %s, which %s states", expected[0].Version, expected[0].Source)
	}
	archive := dartArchiveURL + "/" + version + "/sdk/dartsdk-" + platform + "-release.zip"
	digest, err := dartDigest(ctx, archive)
	if err != nil {
		return stacks.Provided{}, err
	}
	dir := filepath.Join(base, "dart-sdk-"+version+"-"+platform)
	info("installing the Dart SDK " + version + " under " + dir)
	pin := pinnedarchive.Pin{URL: archive, SHA256: digest, Format: pinnedarchive.Zip}
	if _, err := pinnedarchive.Install(ctx, pin, dir, pinnedarchive.Options{
		Client: &http.Client{Timeout: downloadTimeout},
		Limits: pinnedarchive.DefaultLimits(),
		Complete: func(candidate string) bool {
			_, statErr := os.Stat(executable(candidate))
			return statErr == nil
		},
	}); err != nil {
		return stacks.Provided{}, fmt.Errorf("install the Dart SDK %s: %w", version, err)
	}
	if err := os.WriteFile(filepath.Join(dir, installedMarker), []byte(version+"\n"), 0o644); err != nil { //nolint:gosec // a marker in the extension's own folder
		return stacks.Provided{}, err
	}
	return stacks.Provided{Executable: executable(dir)}, nil
}

// dartPlatforms are the dart-archive names of the platforms the extension
// installs on.
var dartPlatforms = map[string]string{
	"darwin/amd64":  "macos-x64",
	"darwin/arm64":  "macos-arm64",
	"linux/amd64":   "linux-x64",
	"linux/arm64":   "linux-arm64",
	"windows/amd64": "windows-x64",
	"windows/arm64": "windows-arm64",
}

// dartReleases lists the stable releases of the dart-archive.
func dartReleases(ctx context.Context) ([]string, error) {
	var versions []string
	token := ""
	for {
		list := dartReleaseListURL
		if token != "" {
			list += "&pageToken=" + url.QueryEscape(token)
		}
		var page struct {
			Prefixes      []string `json:"prefixes"`
			NextPageToken string   `json:"nextPageToken"`
		}
		if err := getJSON(ctx, list, &page); err != nil {
			return nil, fmt.Errorf("list the Dart SDK releases: %w", err)
		}
		for _, prefix := range page.Prefixes {
			if v := filepath.Base(strings.TrimSuffix(prefix, "/")); stableVersion.MatchString(v) {
				versions = append(versions, v)
			}
		}
		if page.NextPageToken == "" {
			return versions, nil
		}
		token = page.NextPageToken
	}
}

// dartDigest reads the SHA-256 the dart-archive publishes beside an archive.
func dartDigest(ctx context.Context, archive string) (string, error) {
	body, err := get(ctx, archive+".sha256sum")
	if err != nil {
		return "", fmt.Errorf("read the checksum of %s: %w", archive, err)
	}
	fields := strings.Fields(string(body))
	if len(fields) == 0 {
		return "", fmt.Errorf("the checksum of %s is empty", archive)
	}
	return fields[0], nil
}

// flutterRelease is one release of a Flutter release manifest.
type flutterRelease struct {
	Channel  string `json:"channel"`
	Version  string `json:"version"`
	Dart     string `json:"dart_sdk_version"`
	Arch     string `json:"dart_sdk_arch"`
	Archive  string `json:"archive"`
	SHA256   string `json:"sha256"`
	Released string `json:"release_date"`
}

// flutterPlatforms are the manifest and architecture of each platform the
// Flutter team publishes an archive for: Linux on x64 only.
var flutterPlatforms = map[string]struct{ manifest, arch string }{
	"darwin/amd64":  {"macos", "x64"},
	"darwin/arm64":  {"macos", "arm64"},
	"linux/amd64":   {"linux", "x64"},
	"windows/amd64": {"windows", "x64"},
}

func provideFlutter(ctx context.Context, base string, checks []stacks.Toolchain, expected []stacks.Expected, info func(string)) (stacks.Provided, error) {
	platform, ok := flutterPlatforms[hostPlatform]
	if !ok {
		return stacks.Provided{}, fmt.Errorf("no Flutter SDK is published for %s: put flutter on PATH", hostPlatform)
	}
	suffix := "-" + platform.manifest + "-" + platform.arch
	executable := func(dir string) string {
		name := "flutter"
		if strings.HasPrefix(hostPlatform, "windows/") {
			name += ".bat"
		}
		return filepath.Join(dir, "flutter", "bin", name)
	}
	// A release must pass every check: its version the Flutter one, its
	// bundled Dart the others.
	admitted := func(version, dart string) bool {
		if !admits(checks[0], expected[0], version) {
			return false
		}
		for i := 1; i < len(checks); i++ {
			if !admits(checks[i], expected[i], dart) {
				return false
			}
		}
		return true
	}
	if dir, ok := installed(base, "flutter-", suffix, func(version string) bool {
		dart, _ := os.ReadFile(filepath.Join(base, "flutter-"+version+suffix, "flutter", "bin", "cache", "dart-sdk", "version")) //nolint:gosec // the extension's own install
		return admitted(version, strings.TrimSpace(string(dart)))
	}); ok {
		return stacks.Provided{Executable: executable(dir)}, nil
	}
	var manifest struct {
		BaseURL  string           `json:"base_url"`
		Releases []flutterRelease `json:"releases"`
	}
	if err := getJSON(ctx, flutterReleasesURL+"/releases_"+platform.manifest+".json", &manifest); err != nil {
		return stacks.Provided{}, fmt.Errorf("read the Flutter releases: %w", err)
	}
	var chosen *flutterRelease
	for i, r := range manifest.Releases {
		arch := r.Arch
		if arch == "" {
			arch = "x64"
		}
		if r.Channel != "stable" || arch != platform.arch || !stableVersion.MatchString(r.Version) || !admitted(r.Version, r.Dart) {
			continue
		}
		if chosen == nil || stacks.CompareVersions(r.Version, chosen.Version) > 0 {
			chosen = &manifest.Releases[i]
		}
	}
	if chosen == nil {
		return stacks.Provided{}, fmt.Errorf("no stable Flutter SDK is in %s with a Dart in %s", describe(expected[0]), describe(expected[1:]...))
	}
	dir := filepath.Join(base, "flutter-"+chosen.Version+suffix)
	info("installing the Flutter SDK " + chosen.Version + " (Dart " + chosen.Dart + ") under " + dir)
	if err := installFlutter(ctx, manifest.BaseURL+"/"+chosen.Archive, chosen.SHA256, dir, executable); err != nil {
		return stacks.Provided{}, fmt.Errorf("install the Flutter SDK %s: %w", chosen.Version, err)
	}
	return stacks.Provided{Executable: executable(dir)}, nil
}

// installFlutter downloads a Flutter archive, checks its SHA-256, unpacks it
// with the host's tar beside dir, then moves it in place.
func installFlutter(ctx context.Context, archive, digest, dir string, executable func(string) string) error {
	staging, err := os.MkdirTemp(filepath.Dir(dir), "download-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	file := filepath.Join(staging, filepath.Base(archive))
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	pin := pinnedarchive.Pin{URL: archive, SHA256: digest, Format: pinnedarchive.Zip}
	limits := pinnedarchive.Limits{ArchiveBytes: 4 << 30, ExtractedBytes: 12 << 30, Entries: 400_000}
	if err := pinnedarchive.Download(ctx, pin, file, pinnedarchive.Options{Client: &http.Client{Timeout: downloadTimeout}, Limits: limits}); err != nil {
		return err
	}
	unpacked := filepath.Join(staging, "sdk")
	if err := os.MkdirAll(unpacked, 0o755); err != nil {
		return err
	}
	// The SDK belongs to the user who installs it, never to the archive's
	// owner: a root install would leave git refusing the Flutter checkout.
	cmd := exec.CommandContext(ctx, tarCommand, "--no-same-owner", "-xf", file, "-C", unpacked) //nolint:gosec // the host's tar on an archive whose SHA-256 was checked
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("unpack %s: %w", filepath.Base(archive), err)
	}
	if _, err := os.Stat(executable(unpacked)); err != nil {
		return fmt.Errorf("%s holds no flutter/bin: %w", filepath.Base(archive), err)
	}
	if err := os.WriteFile(filepath.Join(unpacked, installedMarker), nil, 0o644); err != nil { //nolint:gosec // a marker in the extension's own folder
		return err
	}
	_ = os.RemoveAll(dir)
	return robustio.Rename(unpacked, dir)
}

// installed returns the newest finished install under base, named
// prefix+version+suffix, whose version passes.
func installed(base, prefix, suffix string, pass func(version string) bool) (string, bool) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", false
	}
	var versions []string
	for _, e := range entries {
		v, ok := strings.CutPrefix(e.Name(), prefix)
		if !ok || !e.IsDir() {
			continue
		}
		if v, ok = strings.CutSuffix(v, suffix); !ok || !stableVersion.MatchString(v) {
			continue
		}
		if _, err := os.Stat(filepath.Join(base, e.Name(), installedMarker)); err == nil {
			versions = append(versions, v)
		}
	}
	v, ok := newest(versions, pass)
	if !ok {
		return "", false
	}
	return filepath.Join(base, prefix+v+suffix), true
}

// newest returns the highest version that passes.
func newest(versions []string, pass func(string) bool) (string, bool) {
	sorted := slices.Clone(versions)
	slices.SortFunc(sorted, func(a, b string) int { return stacks.CompareVersions(b, a) })
	for _, v := range sorted {
		if pass(v) {
			return v, true
		}
	}
	return "", false
}

func describe(expected ...stacks.Expected) string {
	var parts []string
	for _, e := range expected {
		if e.Version == "" {
			parts = append(parts, "any version")
			continue
		}
		parts = append(parts, e.Version+" ("+e.Source+")")
	}
	if len(parts) == 0 {
		return "any version"
	}
	return strings.Join(parts, " and ")
}

func get(ctx context.Context, address string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, manifestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: HTTP %d", address, resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 64<<20))
}

func getJSON(ctx context.Context, address string, v any) error {
	body, err := get(ctx, address)
	if err != nil {
		return err
	}
	return json.Unmarshal(body, v)
}

// errNoDart is the error of the test tools without a dart to activate them.
var errNoDart = errors.New("dart is not on the task's PATH")
