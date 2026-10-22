package toolchain

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
	wsproto "go.putnami.dev/protocol/workspace"
)

// bunResolveRequest asks for the Bun pin of a workspace on platform, with the
// committed entry locked when it is not nil.
func bunResolveRequest(platform string, locked *wsproto.LockToolchainEntry, offline bool) wsproto.ToolchainResolveRequest {
	request := wsproto.ToolchainResolveRequest{
		Version:    wsproto.ToolchainResolveProtocolVersion,
		Extension:  "@putnami/typescript",
		Identities: []string{bunLock},
		Platform:   platform,
		Offline:    offline,
	}
	if locked != nil {
		request.Locked = map[string]wsproto.LockToolchainEntry{bunLock: *locked}
	}
	return request
}

// answerBun resolves request against the workspace at root and returns the
// Bun resolution, after checking the answer the way the CLI checks it.
func answerBun(t *testing.T, root string, request wsproto.ToolchainResolveRequest) wsproto.ToolchainResolution {
	t.Helper()
	result := checkedAnswer(t, request, ResolveToolchains(context.Background(), root, "", request))
	resolution, ok := result.Toolchains[bunLock]
	if !ok || len(result.Toolchains) != 1 {
		t.Fatalf("toolchains = %+v, want one resolution for %q", result.Toolchains, bunLock)
	}
	return resolution
}

// checkedAnswer attributes result to the asked extension, as the serving
// command does, and fails the test when the CLI would reject it.
func checkedAnswer(t *testing.T, request wsproto.ToolchainResolveRequest, result wsproto.ToolchainResolveResult) wsproto.ToolchainResolveResult {
	t.Helper()
	result.Extension = request.Extension
	diags := append(wsproto.ValidateToolchainResolveResult(&result), wsproto.ValidateToolchainResolveAnswer(&request, &result)...)
	if diag.HasErrors(diags) {
		t.Fatalf("the answer breaks the toolchain-resolve contract:\n%s", diag.ErrorText(diags))
	}
	return result
}

func wantPin(t *testing.T, resolution wsproto.ToolchainResolution) wsproto.LockToolchainEntry {
	t.Helper()
	if resolution.Pin == nil {
		t.Fatalf("resolution = problem %+v, want a pin", resolution.Problem)
	}
	return *resolution.Pin
}

func wantProblem(t *testing.T, resolution wsproto.ToolchainResolution, contains ...string) wsproto.ToolchainProblem {
	t.Helper()
	if resolution.Problem == nil {
		t.Fatalf("resolution = pin %+v, want a problem", resolution.Pin)
	}
	text := resolution.Problem.Message + "\n" + resolution.Problem.Remedy
	for _, want := range contains {
		if !strings.Contains(text, want) {
			t.Errorf("problem %q does not mention %q", text, want)
		}
	}
	return *resolution.Problem
}

// bunWorkspace returns a workspace root whose package.json is manifest, or
// that has none when manifest is "".
func bunWorkspace(t *testing.T, manifest string) string {
	t.Helper()
	root := t.TempDir()
	if manifest != "" {
		if err := os.WriteFile(filepath.Join(root, "package.json"), []byte(manifest), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// noBunReleaseLookup fails the test when an answer reads release metadata.
func noBunReleaseLookup(t *testing.T) {
	t.Helper()
	previous := bunReleaseAPIURL
	t.Cleanup(func() { bunReleaseAPIURL = previous })
	bunReleaseAPIURL = func(version string) string {
		t.Errorf("the answer looked up Bun %s; it must answer offline", version)
		return "http://127.0.0.1:1/unexpected"
	}
}

// bunReleaseServer serves GitHub release documents through handler and points
// the lookup at it.
func bunReleaseServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	previous := bunReleaseAPIURL
	t.Cleanup(func() { bunReleaseAPIURL = previous })
	bunReleaseAPIURL = func(version string) string {
		return server.URL + "/repos/oven-sh/bun/releases/tags/bun-v" + version
	}
}

// vendorPin is the version 5 pin of a release published at the vendor's
// release downloads with integrities.
func vendorPin(version string, integrities map[string]string) wsproto.LockToolchainEntry {
	pin := wsproto.LockToolchainEntry{
		Version:     version,
		Archives:    map[string]wsproto.LockToolchainArchive{},
		Integrities: maps.Clone(integrities),
	}
	for platform := range integrities {
		target := bunTargets[platform]
		executable := "bun-" + target + "/bun"
		if strings.HasPrefix(platform, "windows/") {
			executable += ".exe"
		}
		pin.Archives[platform] = wsproto.LockToolchainArchive{
			URL:        "https://github.com/oven-sh/bun/releases/download/bun-v" + version + "/bun-" + target + ".zip",
			Format:     wsproto.LockArchiveFormatZip,
			Executable: executable,
		}
	}
	return pin
}

func digestsFor(seed string) map[string]string {
	digests := map[string]string{}
	for platform := range bunTargets {
		digests[platform] = sha256Hex([]byte(seed + platform))
	}
	return digests
}

// The declaration is packageManager's bun@<version>, normalized as the CLI
// normalizes it; a missing manifest or another manager declares none, and a
// manifest that cannot be parsed is an error, not an absent declaration.
func TestReadDeclaredBunVersion_NormalizesLikeTheCLI(t *testing.T) {
	for _, tc := range []struct {
		name, manifest, want string
	}{
		{"exact", `{"packageManager":"bun@1.3.2"}`, "1.3.2"},
		{"leading v", `{"packageManager":"bun@v1.3.2"}`, "1.3.2"},
		{"major.minor", `{"packageManager":"bun@1.3"}`, "1.3.0"},
		{"padded", `{"packageManager":"  bun@ 1.3.2 "}`, "1.3.2"},
		{"another manager", `{"packageManager":"pnpm@9.1.0"}`, ""},
		{"no version", `{"packageManager":"bun@"}`, ""},
		{"no packageManager", `{"name":"repo"}`, ""},
		{"no manifest", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readDeclaredBunVersion(bunWorkspace(t, tc.manifest))
			if err != nil || got != tc.want {
				t.Fatalf("readDeclaredBunVersion = (%q, %v), want (%q, nil)", got, err, tc.want)
			}
		})
	}

	root := bunWorkspace(t, `{"packageManager":`)
	if _, err := readDeclaredBunVersion(root); err == nil {
		t.Fatal("a package.json that does not parse declares nothing; want an error")
	}
	if got := declaredBunVersion(root); got != "" {
		t.Fatalf("declaredBunVersion = %q, want the installer to read no declaration", got)
	}
	noBunReleaseLookup(t)
	wantProblem(t, answerBun(t, root, bunResolveRequest("linux/amd64", nil, false)), "package.json")
}

// A committed version 5 pin of the declared release is the answer as it is,
// with no lookup, even online.
func TestResolveBun_KeepsACommittedVersion5PinAsItIs(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-answers-its-toolchain-pin",
		"a-committed-version-5-pin-of-the-release-is-kept-as-committed")
	noBunReleaseLookup(t)
	committed := vendorPin("1.3.0", digestsFor("1.3.0"))
	committed.Archives["linux/amd64"] = wsproto.LockToolchainArchive{
		URL: "https://mirror.example/bun/linux.zip", Format: wsproto.LockArchiveFormatZip, Executable: "bun-linux-x64/bun",
	}
	root := bunWorkspace(t, `{"packageManager":"bun@v1.3"}`)

	got := wantPin(t, answerBun(t, root, bunResolveRequest("linux/amd64", &committed, false)))
	if !reflect.DeepEqual(got, committed) {
		t.Fatalf("pin = %+v, want the committed entry %+v", got, committed)
	}
}

// A committed version 3 or 4 pin, with integrities and a source, becomes a
// version 5 pin offline: each archive is <source>bun-<target>.zip, as the
// installer downloads it, and a platform Bun publishes no archive for is
// dropped. Nothing declared keeps whatever the lock pins.
func TestResolveBun_ConvertsACommittedVersion4PinOffline(t *testing.T) {
	noBunReleaseLookup(t)
	integrities := digestsFor("1.2.9")
	integrities["linux/riscv64"] = sha256Hex([]byte("riscv"))
	committed := wsproto.LockToolchainEntry{
		Version:     "1.2.9",
		Integrities: integrities,
		Source:      "https://mirror.example/bun/1.2.9",
	}

	for _, manifest := range []string{"", `{"name":"repo"}`, `{"packageManager":"pnpm@9.1.0"}`, `{"packageManager":"bun@1.2.9"}`} {
		got := wantPin(t, answerBun(t, bunWorkspace(t, manifest), bunResolveRequest("windows/arm64", &committed, true)))
		want := wsproto.LockToolchainEntry{
			Version:     "1.2.9",
			Archives:    map[string]wsproto.LockToolchainArchive{},
			Integrities: map[string]string{},
		}
		for platform, target := range bunTargets {
			executable := "bun-" + target + "/bun"
			if strings.HasPrefix(platform, "windows/") {
				executable += ".exe"
			}
			want.Archives[platform] = wsproto.LockToolchainArchive{
				URL:        "https://mirror.example/bun/1.2.9/bun-" + target + ".zip",
				Format:     wsproto.LockArchiveFormatZip,
				Executable: executable,
			}
			want.Integrities[platform] = integrities[platform]
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("package.json %q: pin = %+v, want %+v", manifest, got, want)
		}
	}
}

// A declared default release the lock does not pin is pinned from the digests
// the extension ships, offline, at the vendor's release downloads.
func TestResolveBun_PinsTheDeclaredDefaultReleaseOffline(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-answers-its-toolchain-pin",
		"the-default-release-is-pinned-offline-from-the-shipped-digests")
	noBunReleaseLookup(t)
	committed := wsproto.LockToolchainEntry{Version: "1.2.9", Integrities: digestsFor("1.2.9"), Source: bunReleaseSource("1.2.9")}
	root := bunWorkspace(t, `{"packageManager":"bun@`+DefaultBunVersion+`"}`)

	for _, locked := range []*wsproto.LockToolchainEntry{nil, &committed} {
		got := wantPin(t, answerBun(t, root, bunResolveRequest("darwin/arm64", locked, true)))
		if want := vendorPin(DefaultBunVersion, defaultBunIntegrities); !reflect.DeepEqual(got, want) {
			t.Fatalf("pin = %+v, want %+v", got, want)
		}
	}
}

// A declared release that is not the default one, and that the lock does not
// pin, is pinned from the SHA-256 digests its GitHub release lists for the six
// archives; the other assets are ignored.
func TestResolveBun_LooksUpADeclaredRelease(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-answers-its-toolchain-pin",
		"the-release-is-the-package-manager-declaration")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-answers-its-toolchain-pin",
		"a-declared-release-is-pinned-from-its-github-release-digests")
	digests := digestsFor("1.3.2")
	var mu sync.Mutex
	var paths, agents []string
	bunReleaseServer(t, func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		agents = append(agents, r.Header.Get("User-Agent"))
		mu.Unlock()
		assets := make([]string, 0, len(bunTargets)+2)
		for platform, target := range bunTargets {
			assets = append(assets, fmt.Sprintf(`{"name":"bun-%s.zip","digest":"sha256:%s"}`, target, digests[platform]))
		}
		assets = append(assets,
			`{"name":"bun-linux-x64-baseline.zip","digest":"sha256:`+sha256Hex([]byte("baseline"))+`"}`,
			`{"name":"SHASUMS256.txt","digest":"sha256:`+sha256Hex([]byte("sums"))+`"}`)
		_, _ = fmt.Fprintf(w, `{"tag_name":"bun-v1.3.2","assets":[%s]}`, strings.Join(assets, ","))
	})
	committed := wsproto.LockToolchainEntry{Version: "1.2.9", Integrities: digestsFor("1.2.9"), Source: bunReleaseSource("1.2.9")}
	request := bunResolveRequest("linux/arm64", &committed, false)
	root := bunWorkspace(t, `{"packageManager":"bun@1.3.2"}`)

	result := checkedAnswer(t, request, ResolveToolchains(context.Background(), root, "putnami-cli/test", request))
	got := wantPin(t, result.Toolchains[bunLock])
	if want := vendorPin("1.3.2", digests); !reflect.DeepEqual(got, want) {
		t.Fatalf("pin = %+v, want %+v", got, want)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(paths) != 1 || paths[0] != "/repos/oven-sh/bun/releases/tags/bun-v1.3.2" || agents[0] != "putnami-cli/test" {
		t.Fatalf("requests = %v with user agents %v, want one for the bun-v1.3.2 release", paths, agents)
	}
}

// The release document is GitHub's, at the tag the vendor names a release by.
func TestBunReleaseAPIURL_IsTheVendorRelease(t *testing.T) {
	if got, want := bunReleaseAPIURL("1.3.2"), "https://api.github.com/repos/oven-sh/bun/releases/tags/bun-v1.3.2"; got != want {
		t.Fatalf("bunReleaseAPIURL = %q, want %q", got, want)
	}
}

// Offline, a declared release that is neither pinned nor the default one has
// no digests to pin, and the remedy is an install with network access.
func TestResolveBun_OfflineCannotPinAnUnpinnedRelease(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-answers-its-toolchain-pin",
		"offline-an-unpinned-declared-release-is-a-problem-naming-putnami-install")
	noBunReleaseLookup(t)
	committed := vendorPin("1.2.9", digestsFor("1.2.9"))
	root := bunWorkspace(t, `{"packageManager":"bun@1.3.2"}`)

	for _, locked := range []*wsproto.LockToolchainEntry{nil, &committed} {
		problem := wantProblem(t, answerBun(t, root, bunResolveRequest("linux/amd64", locked, true)), "1.3.2", "offline")
		if !strings.Contains(problem.Remedy, "putnami install") || !strings.Contains(problem.Remedy, "network") {
			t.Fatalf("remedy = %q, want `putnami install` with network access", problem.Remedy)
		}
	}
}

// A lookup that fails is a problem that carries the failure.
func TestResolveBun_AFailedLookupIsAProblem(t *testing.T) {
	root := bunWorkspace(t, `{"packageManager":"bun@1.3.2"}`)
	for _, tc := range []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"not found", func(w http.ResponseWriter, r *http.Request) { http.NotFound(w, r) }, "404"},
		{"not JSON", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("<html>")) }, "decode"},
		{"no digest", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"assets":[{"name":"bun-linux-x64.zip"}]}`))
		}, "no Bun archive with a SHA-256 digest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bunReleaseServer(t, tc.handler)
			wantProblem(t, answerBun(t, root, bunResolveRequest("linux/amd64", nil, false)), "1.3.2", tc.want)
		})
	}
}

// A declaration that names no published release is a problem, and no lookup
// is made for it.
func TestResolveBun_ADeclarationThatIsNoReleaseIsAProblem(t *testing.T) {
	noBunReleaseLookup(t)
	for _, declared := range []string{"latest", "^1.3.0", "1.3.2+sha512.abc"} {
		root := bunWorkspace(t, `{"packageManager":"bun@`+declared+`"}`)
		wantProblem(t, answerBun(t, root, bunResolveRequest("linux/amd64", nil, false)), declared, "bun@MAJOR.MINOR.PATCH")
	}
}

// Nothing declared and nothing pinned pins the default release offline: the
// release workspace-install declares for such a workspace.
func TestResolveBun_PinsTheDefaultReleaseWhenNothingIsDeclared(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-answers-its-toolchain-pin",
		"nothing-declared-pins-the-default-release")
	spectest.Proves(t, "typescript/typescript-project-toolchain", "bun-answers-its-toolchain-pin",
		"the-default-release-is-pinned-offline-from-the-shipped-digests")
	noBunReleaseLookup(t)
	for _, manifest := range []string{"", `{"name":"repo"}`, `{"packageManager":"npm@10.0.0"}`} {
		got := wantPin(t, answerBun(t, bunWorkspace(t, manifest), bunResolveRequest("linux/amd64", nil, false)))
		if want := vendorPin(DefaultBunVersion, defaultBunIntegrities); !reflect.DeepEqual(got, want) {
			t.Fatalf("package.json %q: pin = %+v, want %+v", manifest, got, want)
		}
	}
}

// A pin that has no archive for the host platform is answered as a problem.
func TestResolveBun_APinThatDoesNotServeTheHostIsAProblem(t *testing.T) {
	noBunReleaseLookup(t)
	wantProblem(t, answerBun(t, bunWorkspace(t, ""), bunResolveRequest("freebsd/amd64", nil, false)),
		DefaultBunVersion, "freebsd/amd64")

	committed := vendorPin("1.3.2", digestsFor("1.3.2"))
	delete(committed.Archives, "linux/arm64")
	delete(committed.Integrities, "linux/arm64")
	wantProblem(t, answerBun(t, bunWorkspace(t, `{"packageManager":"bun@1.3.2"}`), bunResolveRequest("linux/arm64", &committed, false)),
		"1.3.2", WorkspaceLockFile, "linux/arm64")
}

// A committed entry that converts to no valid version 5 pin, such as a source
// with credentials that an archive URL cannot carry, is a problem rather than
// an answer the CLI would reject.
func TestResolveBun_AnInvalidConversionIsAProblem(t *testing.T) {
	noBunReleaseLookup(t)
	committed := wsproto.LockToolchainEntry{
		Version:     "1.2.9",
		Integrities: digestsFor("1.2.9"),
		Source:      "https://user:secret@mirror.example/bun/",
	}
	problem := wantProblem(t, answerBun(t, bunWorkspace(t, ""), bunResolveRequest("linux/amd64", &committed, true)),
		"1.2.9", "toolchains.bun")
	if strings.Contains(problem.Message, "secret") {
		t.Fatalf("problem %q repeats the credentials of the source", problem.Message)
	}
}

// The extension answers for bun only; another asked identity is a problem.
func TestResolveToolchains_AnswersAProblemForAnotherIdentity(t *testing.T) {
	noBunReleaseLookup(t)
	request := bunResolveRequest("linux/amd64", nil, true)
	request.Identities = []string{bunLock, "go"}
	result := checkedAnswer(t, request, ResolveToolchains(context.Background(), bunWorkspace(t, ""), "", request))
	wantPin(t, result.Toolchains[bunLock])
	wantProblem(t, result.Toolchains["go"], `"go"`)
}

// A problem holds to the contract's bound on its message and remedy, and
// repeats no credentials of a URL.
func TestToolchainProblem_BoundsItsText(t *testing.T) {
	long := strings.Repeat("é", maxProblemBytes)
	problem := toolchainProblem(long, "bad\xffbyte").Problem
	if len(problem.Message) > maxProblemBytes || !strings.HasSuffix(problem.Message, "...") {
		t.Fatalf("message is %d bytes, want at most %d ending in ...", len(problem.Message), maxProblemBytes)
	}
	if problem.Remedy != "bad\uFFFDbyte" {
		t.Fatalf("remedy = %q, want valid UTF-8", problem.Remedy)
	}

	problem = toolchainProblem(`fetch "https://user:secret@mirror.example/bun.zip"`, "see HTTP://token@host/x").Problem
	if problem.Message != `fetch "https://mirror.example/bun.zip"` || problem.Remedy != "see HTTP://host/x" {
		t.Fatalf("problem = %+v, want the URLs without their credentials", problem)
	}
}
