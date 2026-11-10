package dotnet

import (
	"context"
	"crypto/sha512"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"

	"go.putnami.dev/sdk/extension/pinnedarchive"
	"go.putnami.dev/sdk/extension/robustio"
	"go.putnami.dev/tooling/stacks"
)

// ProvisionedEnv names the dotnet a task installed for its steps.
const ProvisionedEnv = "PUTNAMI_DOTNET_EXECUTABLE"

const (
	// defaultChannel is the channel whose latest SDK a repository gets when
	// no global.json states one.
	defaultChannel = "10.0"
	// downloadTimeout bounds the download of an archive.
	downloadTimeout = 20 * time.Minute
	// installedMarker is the file of an SDK whose installation finished.
	installedMarker = ".putnami-installed"
	// sdkPrefix starts the folder of an installed SDK, then its version.
	sdkPrefix = "sdk-"
	// minRuntimeChannel is the oldest channel whose runtime an installation
	// adds: the oldest one the extension pins an image for.
	minRuntimeChannel = "8.0"
	// maxMetadataBytes bounds a release metadata document.
	maxMetadataBytes = 32 << 20
	// maxArchiveBytes bounds an archive.
	maxArchiveBytes = 2 << 30
)

// hostPlatform is the platform the extension runs on. Tests name another.
var hostPlatform = runtime.GOOS + "/" + runtime.GOARCH

// hostMusl reports whether the host's C library is musl: the one /bin/sh
// runs on, else, when /bin/sh names no loader, whether musl's loader is there.
// Tests set it.
var hostMusl = func() bool {
	if musl, known := muslProgram("/bin/sh"); known {
		return musl
	}
	loaders, _ := filepath.Glob("/lib/ld-musl-*")
	return len(loaders) > 0
}

// muslProgram reports whether the program at path runs on musl: its ELF
// interpreter is musl's loader. That is the C library the system runs,
// whatever compatibility loaders it also carries. known is false when the
// program cannot be read or names no interpreter, as a static one.
func muslProgram(path string) (musl, known bool) {
	f, err := elf.Open(path)
	if err != nil {
		return false, false
	}
	defer func() { _ = f.Close() }()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		interpreter, err := io.ReadAll(io.LimitReader(p.Open(), 4096))
		if err != nil {
			return false, false
		}
		return strings.Contains(filepath.Base(strings.TrimRight(string(interpreter), "\x00")), "ld-musl-"), true
	}
	return false, false
}

// runtimeIdentifiers are the .NET names of the platforms the extension
// installs an SDK on.
var runtimeIdentifiers = map[string]string{
	"darwin/amd64":  "osx-x64",
	"darwin/arm64":  "osx-arm64",
	"linux/amd64":   "linux-x64",
	"linux/arm64":   "linux-arm64",
	"windows/amd64": "win-x64",
	"windows/arm64": "win-arm64",
}

// hostRuntimeIdentifier is the .NET name of the host: a musl Linux has its
// own archives.
func hostRuntimeIdentifier() (string, bool) {
	rid, ok := runtimeIdentifiers[hostPlatform]
	if ok && strings.HasPrefix(rid, "linux-") && hostMusl() {
		rid = "linux-musl-" + strings.TrimPrefix(rid, "linux-")
	}
	return rid, ok
}

// metadataBase is where Microsoft publishes the release metadata of every
// channel. Tests point it at a local server.
var metadataBase = "https://builds.dotnet.microsoft.com/dotnet/release-metadata"

// httpClient fetches the metadata and the archives, and follows no redirect
// away from HTTPS.
var httpClient = &http.Client{CheckRedirect: httpsOnly}

func httpsOnly(req *http.Request, via []*http.Request) error {
	if req.URL.Scheme != "https" {
		return fmt.Errorf("a redirect to %s leaves HTTPS", req.URL)
	}
	if len(via) >= 10 {
		return errors.New("too many redirects")
	}
	return nil
}

// releasesIndex is the part of releases-index.json the extension reads.
type releasesIndex struct {
	Channels []struct {
		Version      string `json:"channel-version"`
		ReleasesJSON string `json:"releases.json"`
	} `json:"releases-index"`
}

// channelReleases is the part of a channel's releases.json the extension
// reads. Its releases come newest first.
type channelReleases struct {
	Releases []struct {
		SDK    *published  `json:"sdk"`
		SDKs   []published `json:"sdks"`
		ASPNET *published  `json:"aspnetcore-runtime"`
	} `json:"releases"`
}

// published is one SDK or runtime and its archives.
type published struct {
	Version string        `json:"version"`
	Files   []archiveFile `json:"files"`
}

// archiveFile is one archive of a release.
type archiveFile struct {
	Name string `json:"name"`
	RID  string `json:"rid"`
	URL  string `json:"url"`
	Hash string `json:"hash"`
}

// Provide returns a dotnet whose SDK global.json accepts, or one of the
// default channel when nothing states one: an SDK an earlier call installed
// under the Putnami home, with no network, else one it installs there from
// the release metadata, with the runtimes of every channel from 8.0 up to its
// own.
func Provide(ctx context.Context, expected stacks.Expected, info func(string)) (stacks.Provided, error) {
	rid, ok := hostRuntimeIdentifier()
	if !ok {
		return stacks.Provided{}, fmt.Errorf("the extension installs no .NET SDK on %s", hostPlatform)
	}
	base, release, err := stacks.LockedToolsHome("dotnet")
	if err != nil {
		return stacks.Provided{}, err
	}
	defer release()
	if version, ok := installedSDK(base, expected, rid); ok {
		return provided(filepath.Join(base, sdkFolder(version, rid))), nil
	}
	plan, err := planInstall(ctx, expected, rid)
	if err != nil {
		return stacks.Provided{}, err
	}
	dir := filepath.Join(base, sdkFolder(plan.sdk.Version, rid))
	info(fmt.Sprintf("installing the .NET SDK %s for %s, with the runtimes %s", plan.sdk.Version, rid, strings.Join(plan.runtimeVersions(), ", ")))
	if err := install(ctx, base, plan, dir); err != nil {
		return stacks.Provided{}, fmt.Errorf("install the .NET SDK %s: %w", plan.sdk.Version, err)
	}
	return provided(dir), nil
}

// provided is the dotnet of the SDK installed at dir.
func provided(dir string) stacks.Provided {
	return stacks.Provided{
		Executable: filepath.Join(dir, "dotnet"+stacks.ExecutableSuffix(hostPlatform)),
		Env: []string{
			"DOTNET_ROOT=" + dir,
			"DOTNET_MULTILEVEL_LOOKUP=0",
			"DOTNET_CLI_TELEMETRY_OPTOUT=1",
			"DOTNET_NOLOGO=1",
			"DOTNET_SKIP_FIRST_TIME_EXPERIENCE=1",
		},
	}
}

// sdkFolder is the folder of an SDK installed for a runtime identifier.
func sdkFolder(version, rid string) string {
	return sdkPrefix + version + "_" + rid
}

// installedSDK is the version of the installed SDK of the platform to reuse:
// the one pickSDK picks among those installed, as among those published.
func installedSDK(base string, expected stacks.Expected, rid string) (string, bool) {
	entries, err := os.ReadDir(base)
	if err != nil {
		return "", false
	}
	var versions []string
	for _, e := range entries {
		name, ok := strings.CutSuffix(e.Name(), "_"+rid)
		version, prefixed := strings.CutPrefix(name, sdkPrefix)
		if !ok || !prefixed || !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(base, e.Name(), installedMarker)); err != nil {
			continue
		}
		if eligible(version, expected) {
			versions = append(versions, version)
		}
	}
	if len(versions) == 0 {
		return "", false
	}
	return pickSDK(versions, expected), true
}

// eligible reports whether an SDK version may serve the expected one: one
// MatchSDK accepts, or with nothing stated one of the default channel; a
// prerelease only when the stated version is one.
func eligible(version string, expected stacks.Expected) bool {
	v, ok := parseSDK(version)
	if !ok {
		return false
	}
	want, stated := parseSDK(expected.Version)
	if v.prerelease != "" && !(stated && want.prerelease != "") {
		return false
	}
	if !stated {
		return fmt.Sprintf("%d.%d", v.major, v.minor) == defaultChannel
	}
	return MatchSDK(expected.Root, expected, version)
}

// pickSDK picks among eligible versions the one the .NET host would: with
// nothing stated, the highest; the stated one for disable, patch or no
// policy (else, for patch, the latest of its feature band); the latest patch
// of the lowest feature band for feature, minor and major; the highest for
// the latest policies.
func pickSDK(versions []string, expected stacks.Expected) string {
	sorted := slices.Clone(versions)
	slices.SortFunc(sorted, func(a, b string) int {
		va, _ := parseSDK(a)
		vb, _ := parseSDK(b)
		switch {
		case va.less(vb):
			return -1
		case vb.less(va):
			return 1
		}
		return 0
	})
	highest := sorted[len(sorted)-1]
	if _, stated := parseSDK(expected.Version); !stated {
		return highest
	}
	policy := rollForward(expected.Root, expected.Source)
	if strings.HasPrefix(policy, "latest") && policy != "latestpatch" {
		return highest
	}
	if policy == "disable" || policy == "" || policy == "patch" {
		if slices.Contains(sorted, expected.Version) {
			return expected.Version
		}
	}
	// The lowest feature band holds the stated band when it is there: its
	// latest patch is the one the host rolls to.
	lowest, _ := parseSDK(sorted[0])
	band := sorted[0]
	for _, version := range sorted {
		v, _ := parseSDK(version)
		if v.major == lowest.major && v.minor == lowest.minor && v.patch/100 == lowest.patch/100 {
			band = version
		}
	}
	return band
}

// installPlan is what an installation downloads: the SDK archive and the
// runtime archives.
type installPlan struct {
	sdk      published
	archive  archiveFile
	runtimes []published
	archives []archiveFile
}

func (p installPlan) runtimeVersions() []string {
	var out []string
	for _, r := range p.runtimes {
		out = append(out, r.Version)
	}
	return out
}

// planInstall reads the release metadata and plans the SDK chooseSDK picks
// and the latest ASP.NET Core runtime, which holds the .NET runtime, of every
// channel from 8.0 up to the SDK's.
func planInstall(ctx context.Context, expected stacks.Expected, rid string) (installPlan, error) {
	var index releasesIndex
	if err := fetchJSON(ctx, metadataBase+"/releases-index.json", &index); err != nil {
		return installPlan{}, err
	}
	channels := map[string]channelReleases{}
	load := func(version, url string) (channelReleases, error) {
		if c, ok := channels[version]; ok {
			return c, nil
		}
		var c channelReleases
		if err := fetchJSON(ctx, url, &c); err != nil {
			return channelReleases{}, err
		}
		channels[version] = c
		return c, nil
	}
	chosen, err := chooseSDK(index, load, expected)
	if err != nil {
		return installPlan{}, err
	}
	plan := installPlan{sdk: chosen}
	if plan.archive, err = archiveOf(chosen, "dotnet-sdk", rid); err != nil {
		return installPlan{}, err
	}
	top, _ := parseSDK(chosen.Version)
	for _, ch := range index.Channels {
		var major, minor int
		if _, err := fmt.Sscanf(ch.Version, "%d.%d", &major, &minor); err != nil {
			continue
		}
		var minMajor, minMinor int
		_, _ = fmt.Sscanf(minRuntimeChannel, "%d.%d", &minMajor, &minMinor)
		if major < minMajor || major == minMajor && minor < minMinor || major > top.major || major == top.major && minor > top.minor {
			continue
		}
		releases, err := load(ch.Version, ch.ReleasesJSON)
		if err != nil {
			return installPlan{}, err
		}
		// The ASP.NET Core runtime archive holds the .NET runtime too.
		for _, r := range releases.Releases {
			if r.ASPNET == nil {
				continue
			}
			a, err := archiveOf(*r.ASPNET, "aspnetcore-runtime", rid)
			if err != nil {
				return installPlan{}, err
			}
			plan.runtimes = append(plan.runtimes, *r.ASPNET)
			plan.archives = append(plan.archives, a)
			break
		}
	}
	return plan, nil
}

// chooseSDK picks, among the published SDKs of the channels the policy
// reaches, the one pickSDK picks.
func chooseSDK(index releasesIndex, load func(version, url string) (channelReleases, error), expected stacks.Expected) (published, error) {
	want, stated := parseSDK(expected.Version)
	policy := ""
	if stated {
		policy = rollForward(expected.Root, expected.Source)
	}
	byVersion := map[string]published{}
	var versions []string
	for _, ch := range index.Channels {
		if !channelReached(ch.Version, want, stated, policy) {
			continue
		}
		releases, err := load(ch.Version, ch.ReleasesJSON)
		if err != nil {
			return published{}, err
		}
		for _, r := range releases.Releases {
			sdks := r.SDKs
			if len(sdks) == 0 && r.SDK != nil {
				sdks = []published{*r.SDK}
			}
			for _, s := range sdks {
				if _, seen := byVersion[s.Version]; seen || !eligible(s.Version, expected) {
					continue
				}
				byVersion[s.Version] = s
				versions = append(versions, s.Version)
			}
		}
	}
	if len(versions) == 0 {
		if !stated {
			return published{}, fmt.Errorf("the release metadata lists no SDK of the %s channel", defaultChannel)
		}
		return published{}, fmt.Errorf("the release metadata lists no SDK that %s accepts for version %s", expected.Source, expected.Version)
	}
	return byVersion[pickSDK(versions, expected)], nil
}

// channelReached reports whether a channel ("8.0") may hold an SDK the policy
// accepts: with no stated version, the default channel only.
func channelReached(channel string, want sdk, stated bool, policy string) bool {
	if !stated {
		return channel == defaultChannel
	}
	var major, minor int
	if _, err := fmt.Sscanf(channel, "%d.%d", &major, &minor); err != nil {
		return false
	}
	switch policy {
	case "major", "latestmajor":
		return major > want.major || major == want.major && minor >= want.minor
	case "minor", "latestminor":
		return major == want.major && minor >= want.minor
	}
	return major == want.major && minor == want.minor
}

// archiveOf is the archive of a release named <prefix>-<rid>: a tar.gz, or a
// zip on Windows. A release lists other archives for the same platform, such
// as a composite runtime or a targeting pack.
func archiveOf(p published, prefix, rid string) (archiveFile, error) {
	suffix := ".tar.gz"
	if strings.HasPrefix(rid, "win-") {
		suffix = ".zip"
	}
	name := prefix + "-" + rid + suffix
	for _, f := range p.Files {
		if f.RID == rid && f.Name == name && f.URL != "" {
			return f, nil
		}
	}
	return archiveFile{}, fmt.Errorf("%s has no %s archive", p.Version, name)
}

func archiveFormat(name string) pinnedarchive.Format {
	if strings.HasSuffix(name, ".zip") {
		return pinnedarchive.Zip
	}
	return pinnedarchive.TarGz
}

// install downloads and checks the planned archives, extracts the SDK, adds
// the shared frameworks of each runtime beside it, and moves the result to
// dir, through a staging folder under base.
func install(ctx context.Context, base string, plan installPlan, dir string) error {
	staging, err := os.MkdirTemp(base, stacks.StagingPattern)
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(staging) }()
	extract := func(a archiveFile, to string) error {
		ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
		defer cancel()
		archive := filepath.Join(staging, "archive")
		if err := download(ctx, a.URL, archive, strings.ToLower(strings.TrimSpace(a.Hash))); err != nil {
			return err
		}
		defer func() { _ = os.Remove(archive) }()
		return pinnedarchive.Extract(ctx, archive, archiveFormat(a.Name), to, pinnedarchive.DefaultLimits())
	}
	root := filepath.Join(staging, "sdk")
	if err := extract(plan.archive, root); err != nil {
		return err
	}
	for i, a := range plan.archives {
		rt := filepath.Join(staging, fmt.Sprintf("runtime-%d", i))
		if err := extract(a, rt); err != nil {
			return err
		}
		if err := mergeShared(rt, root); err != nil {
			return err
		}
	}
	if err := os.WriteFile(filepath.Join(root, installedMarker), nil, 0o644); err != nil { //nolint:gosec // a marker file
		return err
	}
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	return robustio.Rename(root, dir)
}

// mergeShared moves the shared framework versions of an extracted runtime
// into the SDK's root, keeping any the root already holds.
func mergeShared(runtimeRoot, root string) error {
	frameworks, err := os.ReadDir(filepath.Join(runtimeRoot, "shared"))
	if err != nil {
		return fmt.Errorf("the runtime archive holds no shared framework: %w", err)
	}
	for _, f := range frameworks {
		versions, err := os.ReadDir(filepath.Join(runtimeRoot, "shared", f.Name()))
		if err != nil {
			return err
		}
		for _, v := range versions {
			to := filepath.Join(root, "shared", f.Name(), v.Name())
			if _, err := os.Stat(to); err == nil {
				continue
			}
			if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
				return err
			}
			if err := robustio.Rename(filepath.Join(runtimeRoot, "shared", f.Name(), v.Name()), to); err != nil {
				return err
			}
		}
	}
	return nil
}

// download writes the body at url to path and checks its SHA-512.
func download(ctx context.Context, url, path, want string) error {
	if len(want) != sha512.Size*2 {
		return fmt.Errorf("the release metadata states no SHA-512 for %s", url)
	}
	body, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	f, err := os.Create(path) //nolint:gosec // a staging file the extension names
	if err != nil {
		return err
	}
	h := sha512.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(body, maxArchiveBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("download %s: %w", url, err)
	}
	if n > maxArchiveBytes {
		return fmt.Errorf("download %s: the archive exceeds %d bytes", url, int64(maxArchiveBytes))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("download %s: SHA-512 %s, the release metadata states %s", url, got, want)
	}
	return nil
}

func fetchJSON(ctx context.Context, url string, v any) error {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	body, err := get(ctx, url)
	if err != nil {
		return err
	}
	defer func() { _ = body.Close() }()
	data, err := io.ReadAll(io.LimitReader(body, maxMetadataBytes+1))
	if err != nil {
		return fmt.Errorf("read %s: %w", url, err)
	}
	if len(data) > maxMetadataBytes {
		return fmt.Errorf("read %s: the document exceeds %d bytes", url, maxMetadataBytes)
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("read %s: %w", url, err)
	}
	return nil
}

func get(ctx context.Context, url string) (io.ReadCloser, error) {
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("%s is no HTTPS URL", url)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("get %s: %s", url, resp.Status)
	}
	return resp.Body, nil
}
