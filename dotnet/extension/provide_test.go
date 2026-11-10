package dotnet

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/putnamihome"
	"go.putnami.dev/tooling/stacks"
	"go.putnami.dev/tooling/stacks/stackstest"
)

// sdkArchive is a tar.gz that holds a dotnet that prints version for
// --version and logs every other call to log.
func sdkArchive(t *testing.T, version, log string) []byte {
	t.Helper()
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo " + version + "; exit 0; fi\necho \"$PWD: installed dotnet $*\" >> \"" + log + "\"\n"
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []struct {
		name, body string
		mode       int64
	}{{"dotnet", script, 0o755}, {"sdk/" + version + "/dotnet.dll", "", 0o644}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// runtimeArchive is a tar.gz that holds a dotnet host and the .NET and
// ASP.NET Core shared frameworks of a version, as Microsoft's ASP.NET Core
// runtime archive does.
func runtimeArchive(t *testing.T, version string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for _, f := range []string{"dotnet", "shared/Microsoft.NETCore.App/" + version + "/System.dll", "shared/Microsoft.AspNetCore.App/" + version + "/Microsoft.AspNetCore.dll"} {
		if err := tw.WriteHeader(&tar.Header{Name: f, Mode: 0o755, Size: 0, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// releaseServer serves release metadata for the channels 8.0, 9.0 and 10.0
// and an archive per SDK, and counts the archive downloads. corrupt names an
// SDK whose stated hash is wrong.
func releaseServer(t *testing.T, log, corrupt string) *atomic.Int32 {
	t.Helper()
	channels := map[string][]string{
		"8.0":  {"8.0.100", "8.0.400", "8.0.404", "8.0.410", "8.0.500", "8.0.600-preview.1"},
		"9.0":  {"9.0.100", "9.0.300"},
		"10.0": {"10.0.100", "10.0.103"},
	}
	archives := map[string][]byte{}
	var downloads atomic.Int32
	mux := http.NewServeMux()
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	type file struct {
		Name string `json:"name"`
		RID  string `json:"rid"`
		URL  string `json:"url"`
		Hash string `json:"hash"`
	}
	// An out-of-support channel still gets its runtime.
	phases := map[string]string{"8.0": "eol", "9.0": "maintenance", "10.0": "active"}
	runtimeFile := func(name, version string, data []byte) map[string]any {
		archives[name+"-"+version] = data
		sum := sha512.Sum512(data)
		return map[string]any{"version": version, "files": []file{{Name: name + "-linux-x64.tar.gz", RID: "linux-x64", URL: srv.URL + "/sdk/" + name + "-" + version + ".tar.gz", Hash: hex.EncodeToString(sum[:])}}}
	}
	var index []map[string]string
	table := shippedReleases{Channels: map[string]channelReleases{}}
	for ch, versions := range channels {
		index = append(index, map[string]string{"channel-version": ch, "support-phase": phases[ch], "releases.json": ch})
		var releases []map[string]any
		rt := ch + ".7"
		releases = append(releases, map[string]any{
			"aspnetcore-runtime": runtimeFile("aspnetcore-runtime", rt, runtimeArchive(t, rt)),
			"sdks":               []map[string]any{},
		})
		for _, v := range versions {
			archives[v] = sdkArchive(t, v, log)
			sum := sha512.Sum512(archives[v])
			hash := hex.EncodeToString(sum[:])
			if v == corrupt {
				hash = strings.Repeat("0", 128)
			}
			releases = append(releases, map[string]any{"sdks": []map[string]any{{"version": v, "files": []file{
				{Name: "dotnet-sdk-linux-x64-composite.tar.gz", RID: "linux-x64", URL: srv.URL + "/missing", Hash: hash},
				{Name: "dotnet-sdk-linux-x64.tar.gz", RID: "linux-x64", URL: srv.URL + "/sdk/" + v + ".tar.gz", Hash: hash},
				{Name: "dotnet-sdk-linux-x64.zip", RID: "linux-x64", URL: srv.URL + "/sdk/" + v + ".zip", Hash: hash},
			}}}})
		}
		body, _ := json.Marshal(map[string]any{"channel-version": ch, "releases": releases})
		var decoded channelReleases
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatal(err)
		}
		table.Channels[ch] = decoded
	}
	indexBody, _ := json.Marshal(map[string]any{"releases-index": index})
	if err := json.Unmarshal(indexBody, &table.releasesIndex); err != nil {
		t.Fatal(err)
	}
	mux.HandleFunc("/sdk/", func(w http.ResponseWriter, r *http.Request) {
		v := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/sdk/"), ".tar.gz")
		data, ok := archives[v]
		if !ok {
			http.NotFound(w, r)
			return
		}
		downloads.Add(1)
		_, _ = w.Write(data)
	})
	oldTable, oldPlatform, oldClient, oldMusl := shipped, hostPlatform, httpClient, hostMusl
	t.Cleanup(func() { shipped, hostPlatform, httpClient, hostMusl = oldTable, oldPlatform, oldClient, oldMusl })
	hostMusl = func() bool { return false }
	shipped, hostPlatform, httpClient = table, "linux/amd64", srv.Client()
	t.Setenv(putnamihome.Env, t.TempDir())
	// The release server stands in for the network, so the run may install.
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	return &downloads
}

func TestTheSDKToInstallFollowsTheRollForwardPolicy(t *testing.T) {
	spectest.Proves(t, feature, "provides-the-sdk", "a-task-installs-the-sdk-global-json-accepts-when-the-machine-has-none")
	releaseServer(t, filepath.Join(t.TempDir(), "log"), "")
	root := t.TempDir()
	for stated, cases := range map[string]map[string]string{
		"8.0.400": {
			"": "8.0.400", "patch": "8.0.400", "disable": "8.0.400", "latestPatch": "8.0.410",
			"feature": "8.0.410", "latestFeature": "8.0.500", "minor": "8.0.410", "latestMinor": "8.0.500",
			"major": "8.0.410", "latestMajor": "10.0.103",
		},
		// A stated SDK that is not published rolls to the latest of its band.
		"8.0.402": {"": "8.0.410", "feature": "8.0.410"},
		// A stated band that is not published rolls to the latest patch of the next one.
		"8.0.300": {"feature": "8.0.410", "latestFeature": "8.0.500"},
	} {
		for policy, want := range cases {
			doc := `{"sdk": {"version": "` + stated + `"}}`
			if policy != "" {
				doc = `{"sdk": {"version": "` + stated + `", "rollForward": "` + policy + `"}}`
			}
			stackstest.WriteFile(t, root, "global.json", doc)
			plan, err := planInstall(stacks.Expected{Version: stated, Source: "global.json", Root: root}, "linux-x64")
			if err != nil || plan.sdk.Version != want {
				t.Errorf("%s with rollForward %q: %s %v, want %s", stated, policy, plan.sdk.Version, err, want)
			}
		}
	}
	plan, err := planInstall(stacks.Expected{}, "linux-x64")
	if err != nil || plan.sdk.Version != "10.0.103" {
		t.Fatalf("with no global.json, the latest of the default channel: %s %v", plan.sdk.Version, err)
	}
	// An SDK carries its own runtime only: the runtimes of every channel from
	// 8.0 up to it come beside it.
	if got := strings.Join(plan.runtimeVersions(), " "); !sameWords(got, "10.0.7 8.0.7 9.0.7") {
		t.Errorf("runtimes = %s", got)
	}
	stackstest.WriteFile(t, root, "global.json", `{"sdk": {"version": "7.0.100", "rollForward": "disable"}}`)
	if _, err := planInstall(stacks.Expected{Version: "7.0.100", Source: "global.json", Root: root}, "linux-x64"); err == nil {
		t.Errorf("an SDK no channel lists is an error")
	}
}

func sameWords(a, b string) bool {
	x, y := strings.Fields(a), strings.Fields(b)
	slices.Sort(x)
	slices.Sort(y)
	return slices.Equal(x, y)
}

func TestProvideInstallsTheSDKWithItsRuntimesOnceAndChecksItsHash(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	downloads := releaseServer(t, log, "9.0.300")
	root := t.TempDir()
	stackstest.WriteFile(t, root, "global.json", `{"sdk": {"version": "8.0.404"}}`)
	expected := stacks.Expected{Version: "8.0.404", Source: "global.json", Root: root}
	var infos []string
	provided, err := Provide(context.Background(), expected, func(s string) { infos = append(infos, s) })
	if err != nil {
		t.Fatal(err)
	}
	home := os.Getenv(putnamihome.Env)
	dir := filepath.Join(home, "tools", "dotnet", "sdk-8.0.404_linux-x64")
	if provided.Executable != filepath.Join(dir, "dotnet") || !strings.Contains(strings.Join(provided.Env, " "), "DOTNET_ROOT="+dir) {
		t.Errorf("provided = %+v", provided)
	}
	for _, f := range []string{"sdk/8.0.404/dotnet.dll", "shared/Microsoft.NETCore.App/8.0.7", "shared/Microsoft.AspNetCore.App/8.0.7"} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("the installation holds no %s: %v", f, err)
		}
	}
	if downloads.Load() != 2 {
		t.Errorf("downloads = %d, want the SDK and the ASP.NET Core runtime", downloads.Load())
	}

	// An installed SDK the statement accepts is reused without the pinned
	// metadata.
	shipped = shippedReleases{}
	stackstest.WriteFile(t, root, "global.json", `{"sdk": {"version": "8.0.400"}}`)
	if again, err := Provide(context.Background(), stacks.Expected{Version: "8.0.400", Source: "global.json", Root: root}, func(string) {}); err != nil || again.Executable != provided.Executable || len(infos) != 1 {
		t.Errorf("a second call reuses the SDK: %+v %v, infos %v", again, err, infos)
	}
	// An interrupted installation leaves no staging folder behind.
	if err := os.MkdirAll(filepath.Join(home, "tools", "dotnet", "download-123"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Provide(context.Background(), stacks.Expected{Version: "8.0.400", Source: "global.json", Root: root}, func(string) {}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, "tools", "dotnet", "download-123")); err == nil {
		t.Errorf("a stale staging folder is kept")
	}
}

func TestAnArchiveWhoseHashDiffersIsRefused(t *testing.T) {
	releaseServer(t, filepath.Join(t.TempDir(), "log"), "9.0.300")
	root := t.TempDir()
	stackstest.WriteFile(t, root, "global.json", `{"sdk": {"version": "9.0.300", "rollForward": "disable"}}`)
	_, err := Provide(context.Background(), stacks.Expected{Version: "9.0.300", Source: "global.json", Root: root}, func(string) {})
	if err == nil || !strings.Contains(err.Error(), "SHA-512") {
		t.Errorf("an archive whose hash differs is refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv(putnamihome.Env), "tools", "dotnet", "sdk-9.0.300_linux-x64")); err == nil {
		t.Errorf("a refused archive leaves nothing installed")
	}
}

func TestAMuslLinuxGetsItsOwnArchives(t *testing.T) {
	oldPlatform, oldMusl := hostPlatform, hostMusl
	t.Cleanup(func() { hostPlatform, hostMusl = oldPlatform, oldMusl })
	hostPlatform, hostMusl = "linux/arm64", func() bool { return true }
	if rid, ok := hostRuntimeIdentifier(); !ok || rid != "linux-musl-arm64" {
		t.Errorf("rid = %s %v", rid, ok)
	}
	hostMusl = func() bool { return false }
	if rid, _ := hostRuntimeIdentifier(); rid != "linux-arm64" {
		t.Errorf("rid = %s", rid)
	}
}

func TestARedirectAwayFromHTTPSIsRefused(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "http://example.test/sdk.tar.gz", nil)
	if err := httpsOnly(req, nil); err == nil {
		t.Errorf("a redirect to HTTP is followed")
	}
}

func TestATaskRunsTheInstalledSDKWhenTheMachineOneIsRefused(t *testing.T) {
	// The machine's SDK is outside the policy: the task installs 8.0.404.
	stubDotnet(t, "6.0.100", trxPass)
	log := filepath.Join(t.TempDir(), "log")
	releaseServer(t, log, "")
	root := fixture(t)
	stackstest.WriteFile(t, root, "global.json", `{"sdk": {"version": "8.0.404"}}`)
	if status, _, _, err := runJob(t, root, "src/Api", stacks.TaskBuild); status != "OK" || err != nil {
		t.Fatalf("build: %s %v", status, err)
	}
	ran, _ := os.ReadFile(log)
	if want := fmt.Sprintf("installed dotnet build %s", filepath.Join(root, "src/Api/Api.csproj")); !strings.Contains(string(ran), want) {
		t.Errorf("the installed SDK ran the step:\n%s\nwant %q", ran, want)
	}
}

func TestAnInstalledSDKIsPickedAsThePublishedOneWouldBe(t *testing.T) {
	base, root := t.TempDir(), t.TempDir()
	install := func(versions ...string) {
		entries, _ := os.ReadDir(base)
		for _, e := range entries {
			_ = os.RemoveAll(filepath.Join(base, e.Name()))
		}
		for _, v := range versions {
			stackstest.WriteFile(t, base, sdkFolder(v, "linux-x64")+"/"+installedMarker, "")
		}
		// Another platform's install is never reused.
		stackstest.WriteFile(t, base, sdkFolder("8.0.499", "linux-musl-x64")+"/"+installedMarker, "")
	}
	for _, c := range []struct {
		policy    string
		installed []string
		want      string
	}{
		{"major", []string{"8.0.410", "10.0.103"}, "8.0.410"},
		{"feature", []string{"8.0.410", "8.0.500"}, "8.0.410"},
		{"", []string{"8.0.400", "8.0.410"}, "8.0.400"},
		{"latestFeature", []string{"8.0.410", "8.0.600-preview.1"}, "8.0.410"},
		{"latestMajor", []string{"8.0.410", "10.0.103"}, "10.0.103"},
	} {
		install(c.installed...)
		doc := `{"sdk": {"version": "8.0.400"}}`
		if c.policy != "" {
			doc = `{"sdk": {"version": "8.0.400", "rollForward": "` + c.policy + `"}}`
		}
		stackstest.WriteFile(t, root, "global.json", doc)
		if got, ok := installedSDK(base, stacks.Expected{Version: "8.0.400", Source: "global.json", Root: root}, "linux-x64"); !ok || got != c.want {
			t.Errorf("rollForward %q over %v: %s, want %s", c.policy, c.installed, got, c.want)
		}
	}
	install("10.0.103", "10.0.200-preview.1")
	if got, _ := installedSDK(base, stacks.Expected{}, "linux-x64"); got != "10.0.103" {
		t.Errorf("with nothing stated, no preview: %s", got)
	}
}

// elfWithInterpreter writes a minimal 64-bit ELF file whose only program
// header names interpreter, and answers its path.
func elfWithInterpreter(t *testing.T, interpreter string) string {
	t.Helper()
	interp := append([]byte(interpreter), 0)
	const ehsize, phsize = 64, 56
	var b bytes.Buffer
	b.Write([]byte{0x7f, 'E', 'L', 'F', 2, 1, 1, 0})
	b.Write(make([]byte, 8))
	le := func(v uint64, n int) {
		for i := 0; i < n; i++ {
			b.WriteByte(byte(v >> (8 * i)))
		}
	}
	le(2, 2)             // e_type: executable
	le(62, 2)            // e_machine: x86-64
	le(1, 4)             // e_version
	le(0, 8)             // e_entry
	le(ehsize, 8)        // e_phoff
	le(0, 8)             // e_shoff
	le(0, 4)             // e_flags
	le(ehsize, 2)        // e_ehsize
	le(phsize, 2)        // e_phentsize
	le(1, 2)             // e_phnum
	le(64, 2)            // e_shentsize
	le(0, 2)             // e_shnum
	le(0, 2)             // e_shstrndx
	le(3, 4)             // p_type: PT_INTERP
	le(4, 4)             // p_flags
	le(ehsize+phsize, 8) // p_offset
	le(0, 8)             // p_vaddr
	le(0, 8)             // p_paddr
	le(uint64(len(interp)), 8)
	le(uint64(len(interp)), 8)
	le(1, 8)
	b.Write(interp)
	path := filepath.Join(t.TempDir(), "sh")
	if err := os.WriteFile(path, b.Bytes(), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMuslIsTheCLibraryTheShellRunsOn(t *testing.T) {
	for interpreter, want := range map[string]bool{
		// Alpine, with or without gcompat's glibc loader beside musl's.
		"/lib/ld-musl-x86_64.so.1":  true,
		"/lib/ld-musl-aarch64.so.1": true,
		// A glibc system, with or without the musl package.
		"/lib64/ld-linux-x86-64.so.2": false,
		"/lib/ld-linux-aarch64.so.1":  false,
	} {
		if got, known := muslProgram(elfWithInterpreter(t, interpreter)); !known || got != want {
			t.Errorf("interpreter %s: musl = %v, want %v", interpreter, got, want)
		}
	}
	notELF := filepath.Join(t.TempDir(), "sh")
	stackstest.WriteFile(t, filepath.Dir(notELF), "sh", "#!/bin/true\n")
	for _, path := range []string{notELF, filepath.Join(t.TempDir(), "none")} {
		if musl, known := muslProgram(path); musl || known {
			t.Errorf("%s says nothing of the C library: %v %v", path, musl, known)
		}
	}
}

func TestTheShippedMetadataPinsAnArchiveOfEveryPlatformForEveryRelease(t *testing.T) {
	table := mustDecodeReleases(sdkReleasesJSON)
	if len(table.Channels) == 0 || len(table.releasesIndex.Channels) != len(table.Channels) {
		t.Fatalf("index %d channels, metadata %d", len(table.releasesIndex.Channels), len(table.Channels))
	}
	for _, ch := range table.releasesIndex.Channels {
		releases, ok := table.Channels[ch.Version]
		if !ok || ch.ReleasesJSON != ch.Version {
			t.Errorf("channel %s: %+v", ch.Version, ch)
			continue
		}
		for _, r := range releases.Releases {
			for _, p := range append(slices.Clone(r.SDKs), deref(r.SDK), deref(r.ASPNET)) {
				for _, f := range p.Files {
					if len(f.Hash) != 128 || !strings.HasPrefix(f.URL, "https://") {
						t.Errorf("%s %s: %+v", ch.Version, p.Version, f)
					}
				}
			}
		}
	}
}

func deref(p *published) published {
	if p == nil {
		return published{}
	}
	return *p
}
