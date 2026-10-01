package main

import (
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/exec"
	"go.putnami.dev/sdk/extension/jsonl"
	"go.putnami.dev/sdk/extension/memberprobe"
	"go.putnami.dev/sdk/extension/recorded"
)

const (
	probeTarballPath = "/npm/@test/pkg/-/pkg-1.2.3-r42.tgz"
	probeNPMToken    = "npm-probe-test-token"
)

// stagedTarball is the tarball the fake pack writes for the staged package.
var stagedTarball = []byte("immutable npm tarball bytes")

func recordedNPMResponse(t *testing.T, name string) recorded.Response {
	t.Helper()
	return recorded.HTTP(t, filepath.Join("testdata", "recorded", "npm-registry", name))
}

func recordedNPMCommand(t *testing.T, name string) *exec.Result {
	t.Helper()
	return replay(recorded.Command(t, filepath.Join("testdata", "recorded", "npm-cli", name)))
}

func replay(exchange recorded.Exchange) *exec.Result {
	return &exec.Result{
		Stdout: string(exchange.Stdout()), Stderr: string(exchange.Stderr()),
		ExitCode: exchange.ExitCode(), Success: exchange.ExitCode() == 0,
	}
}

// servedTarball answers as an npm registry does for a version it holds: the
// tarball bytes. The success is built here because the recorded one is a
// package.
func servedTarball(body []byte) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/octet-stream")
		_, _ = w.Write(body)
	})
}

// npmDryRunProbes runs the publisher's dry run and returns the member probes it
// emitted, read through the strict reader. The task must succeed whatever the
// registry answered, and a dry run is never publication evidence.
func npmDryRunProbes(t *testing.T, ctx *pctx.Context) []*extproto.MemberProbe {
	t.Helper()
	ctx.Params["dry-run"] = json.RawMessage(`true`)
	var status string
	var err error
	events := captureEvents(t, func() {
		status, _, err = runPublishNpm(ctx, jsonl.New(), nil)
	})
	if err != nil || status != "OK" {
		t.Fatalf("dry run = (%q, %v), want OK whatever the registry answered", status, err)
	}
	var probes []*extproto.MemberProbe
	for _, event := range events {
		switch event["kind"] {
		case extproto.PublishedMemberEventKind:
			t.Fatalf("dry run emitted publication evidence: %+v", event)
		case "published":
			if event["dryRun"] != true {
				t.Fatalf("dry run emitted publication proof: %+v", event)
			}
		case extproto.MemberProbeEventKind:
			probe, err := memberprobe.Decode(event)
			if err != nil {
				t.Fatalf("member-probe event %+v does not pass the strict reader: %v", event, err)
			}
			probes = append(probes, probe)
		}
	}
	return probes
}

func oneNPMProbe(t *testing.T, ctx *pctx.Context) *extproto.MemberProbe {
	t.Helper()
	probes := npmDryRunProbes(t, ctx)
	if len(probes) != 1 {
		t.Fatalf("dry run emitted %d member probes, want exactly one", len(probes))
	}
	return probes[0]
}

// managedProbeSeams replaces what a managed dry run reaches outside the
// process: the credential seam and the bun that packs the staged package. It
// returns the number of packs. Any other child process fails the test: a
// managed probe runs no npm.
func managedProbeSeams(t *testing.T, token string, packErr error) (packs *int, tokenHosts *[]string) {
	t.Helper()
	originalRun, originalToken, originalBun, originalGOOS := npmExecRun, npmResolveRegistryToken, resolveBunBin, npmGOOS
	t.Cleanup(func() {
		npmExecRun, npmResolveRegistryToken, resolveBunBin, npmGOOS = originalRun, originalToken, originalBun, originalGOOS
	})
	packs, tokenHosts = new(int), new([]string)
	npmGOOS = "linux"
	npmResolveRegistryToken = func(host string) (string, string) {
		*tokenHosts = append(*tokenHosts, host)
		if token == "" {
			return "", "sign in first"
		}
		return token, ""
	}
	resolveBunBin = func() (string, error) {
		if packErr != nil {
			return "", packErr
		}
		return managedTestBun, nil
	}
	npmExecRun = func(name string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if name != managedTestBun || len(args) < 2 || args[0] != "pm" || args[1] != "pack" {
			t.Fatalf("managed dry run ran %s %v, want only the local bun pack", name, args)
		}
		*packs++
		if err := os.WriteFile(filepath.Join(flagValue(args, "--destination"), "artifact.tgz"), stagedTarball, 0o644); err != nil {
			t.Fatalf("stage fake tarball: %v", err)
		}
		return &exec.Result{Success: true}, nil
	}
	return packs, tokenHosts
}

func managedDryRunCtx(t *testing.T, registry string) *pctx.Context {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"releaseSetPlan": oneMemberNPMPlan(t, "@test/pkg")}
	stageNPMFixture(t, dir)
	withRegistries(t, ctx, fmt.Sprintf(`{"npm":{"publish":%q}}`, registry))
	return ctx
}

// assertOneTarballRead fails unless the registry saw exactly one request, the
// GET of the version's tarball, carrying wantAuthorization.
func assertOneTarballRead(t *testing.T, server *recorded.Server, wantAuthorization string) {
	t.Helper()
	requests := server.Requests()
	if len(requests) != 1 {
		t.Fatalf("the dry run sent %d registry requests, want exactly one", len(requests))
	}
	request := requests[0]
	if request.Method != http.MethodGet {
		t.Fatalf("the dry run sent %s %s, want GET: a probe never writes", request.Method, request.URL.Path)
	}
	if request.URL.Path != probeTarballPath {
		t.Fatalf("the dry run asked %s, want %s", request.URL.Path, probeTarballPath)
	}
	if got := request.Header.Get("Authorization"); got != wantAuthorization {
		t.Fatalf("Authorization = %q, want %q: a probe sends a credential only when one resolved", got, wantAuthorization)
	}
}

// The managed npm dry run answers the four states from what the registry said,
// names the member, the registry and the version in every verdict, and asks
// with one GET and no npm child process. Every 404 is a response the production
// registry sent.
func TestManagedNPMDryRunProbesTheRegistry(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "dry-run-registry-probe", "managed-npm-probe-is-read-only")
	stagedDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(stagedTarball))
	otherDigest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte("another tarball")))
	cases := []struct {
		name      string
		recording string
		served    []byte
		anonymous bool
		packErr   error

		wantState    string
		wantLocal    string
		wantRegistry string
		wantReason   string
		wantPacks    int
	}{
		{name: "a version the registry does not hold is absent", recording: "tarball-version-not-found.404.http", wantState: extproto.MemberProbeAbsent},
		{name: "a package the registry does not know is absent", recording: "tarball-package-not-found.404.http", wantState: extproto.MemberProbeAbsent},
		{
			name: "an anonymous 404 is absent and says it was anonymous", recording: "tarball-version-not-found.404.http",
			anonymous: true, wantState: extproto.MemberProbeAbsent,
		},
		{
			name: "the same tarball is identical", served: stagedTarball,
			wantState: extproto.MemberProbeIdentical, wantLocal: stagedDigest, wantRegistry: stagedDigest, wantPacks: 1,
		},
		{
			name: "the same tarball read without a credential is identical", served: stagedTarball, anonymous: true,
			wantState: extproto.MemberProbeIdentical, wantLocal: stagedDigest, wantRegistry: stagedDigest, wantPacks: 1,
		},
		{
			name: "another tarball is a conflict", served: []byte("another tarball"),
			wantState: extproto.MemberProbeConflict, wantLocal: stagedDigest, wantRegistry: otherDigest,
			wantReason: "another digest", wantPacks: 1,
		},
		{
			name: "a held version the dry run cannot pack is a conflict", served: stagedTarball, packErr: errors.New("no bun on this host"),
			wantState: extproto.MemberProbeConflict, wantRegistry: stagedDigest,
			wantReason: "could not be packed to compare",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			token := probeNPMToken
			if tc.anonymous {
				token = ""
			}
			packs, tokenHosts := managedProbeSeams(t, token, tc.packErr)
			var server *recorded.Server
			if tc.recording != "" {
				server = recorded.NewServer(t, nil, recordedNPMResponse(t, tc.recording))
			} else {
				server = recorded.NewServer(t, servedTarball(tc.served))
			}
			registry := server.URL + "/npm"

			probe := oneNPMProbe(t, managedDryRunCtx(t, registry))

			if probe.State != tc.wantState {
				t.Fatalf("state = %q (%s), want %q", probe.State, probe.Reason, tc.wantState)
			}
			if probe.Ecosystem != "npm" || probe.Coordinate != "@test/pkg" || probe.Version != "1.2.3-r42" || probe.Registry != registry {
				t.Fatalf("probe = %+v, want the member, the registry and the version", probe)
			}
			if probe.ArtifactDigest != tc.wantLocal || probe.RegistryDigest != tc.wantRegistry || probe.Anonymous != tc.anonymous {
				t.Fatalf("probe = %+v, want local %q, registry %q, anonymous %t", probe, tc.wantLocal, tc.wantRegistry, tc.anonymous)
			}
			if !strings.Contains(probe.Reason, tc.wantReason) || (tc.wantReason == "" && probe.Reason != "") {
				t.Fatalf("reason = %q, want %q", probe.Reason, tc.wantReason)
			}
			if *packs != tc.wantPacks {
				t.Fatalf("the dry run packed %d times, want %d: it packs only to compare a held version", *packs, tc.wantPacks)
			}
			if len(*tokenHosts) != 1 || (*tokenHosts)[0] != strings.TrimPrefix(server.URL, "http://") {
				t.Fatalf("credential seam asked about %v, want the registry host once", *tokenHosts)
			}
			wantAuthorization := ""
			if token != "" {
				wantAuthorization = "Bearer " + token
			}
			assertOneTarballRead(t, server, wantAuthorization)
		})
	}
}

// A dry run with no network access must not pass in silence: a registry that
// refuses the connection is an unverified verdict whose reason says so.
func TestManagedNPMDryRunReportsAnUnreachableRegistry(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry := "http://" + listener.Addr().String() + "/npm"
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	packs, _ := managedProbeSeams(t, probeNPMToken, nil)

	probe := oneNPMProbe(t, managedDryRunCtx(t, registry))

	if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, "the registry could not be reached") {
		t.Fatalf("probe = %+v, want unverified with a reason that names the unreachable registry", probe)
	}
	if probe.Coordinate != "@test/pkg" || probe.Version != "1.2.3-r42" || probe.Registry != registry {
		t.Fatalf("probe = %+v, want the member, the registry and the version", probe)
	}
	if strings.Contains(probe.Reason, probeTarballPath) || strings.Contains(probe.Reason, probeNPMToken) {
		t.Fatalf("reason %q repeats the request URL or the credential", probe.Reason)
	}
	if *packs != 0 {
		t.Fatalf("the dry run packed %d times for a registry that never answered", *packs)
	}
}

// Under a native publication run the probe is sent to the private broker, with
// the credential of the broker host, and still names the declared registry: the
// broker is a route, not the registry a member is published to.
func TestManagedNPMDryRunProbesThroughThePrivateBroker(t *testing.T) {
	_, tokenHosts := managedProbeSeams(t, probeNPMToken, nil)
	broker := recorded.NewServer(t, nil, recordedNPMResponse(t, "tarball-version-not-found.404.http"))
	t.Setenv(privateNPMRegistryURLEnv, broker.URL+"/npm")

	probe := oneNPMProbe(t, managedDryRunCtx(t, "https://npm.example.test/"))

	if probe.State != extproto.MemberProbeAbsent || probe.Registry != "https://npm.example.test" {
		t.Fatalf("probe = %+v, want absent at the declared registry", probe)
	}
	if len(*tokenHosts) != 1 || (*tokenHosts)[0] != strings.TrimPrefix(broker.URL, "http://") {
		t.Fatalf("credential seam asked about %v, want the broker host only", *tokenHosts)
	}
	assertOneTarballRead(t, broker, "Bearer "+probeNPMToken)
}

// A registry that refuses an anonymous read is unverified, and the reason says
// to sign in. The refusal is the one npm.putnami.dev sends.
func TestManagedNPMProbeReportsARefusedAnonymousRead(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "dry-run-registry-probe", "npm-refusal-is-unverified")
	managedProbeSeams(t, "", nil)
	server := recorded.NewServer(t, nil, recordedNPMResponse(t, "anonymous-request.401.http"))
	registry := server.URL + "/npm"

	probe := oneNPMProbe(t, managedDryRunCtx(t, registry))

	if probe.State != extproto.MemberProbeUnverified || !probe.Anonymous ||
		!strings.Contains(probe.Reason, `401 Unauthorized: {"error":"authentication required"} to a request without a credential; sign in`) {
		t.Fatalf("probe = %+v, want an anonymous unverified verdict with the registry's refusal and the remedy", probe)
	}
	assertOneTarballRead(t, server, "")
}

// A registry answer that states neither a held version nor an absent one is
// unverified, and the reason carries neither the body's echo of the credential
// nor an unbounded body. The redirect is not followed: the bearer reaches one
// recipient.
func TestManagedNPMProbeRefusesWhatIsNotAnAnswer(t *testing.T) {
	var redirected int
	target := recorded.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected++ }))
	subject := memberprobe.Subject{Ecosystem: "npm", Coordinate: "@test/pkg", Version: "1.2.3-r42", Registry: "https://npm.example.test"}
	for name, handler := range map[string]http.Handler{
		"302 Found": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, target.URL, http.StatusFound)
		}),
		"502 Bad Gateway": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
			_, _ = fmt.Fprintf(w, "upstream said %s %s", r.Header.Get("Authorization"), strings.Repeat("x", 8192))
		}),
	} {
		t.Run(name, func(t *testing.T) {
			server := recorded.NewServer(t, handler)

			digest, verdict := readNPMTarballDigest(subject, server.URL, probeNPMToken)

			if verdict == nil || digest != "" || verdict.State != extproto.MemberProbeUnverified || !strings.Contains(verdict.Reason, name) {
				t.Fatalf("verdict = %+v (digest %q), want unverified naming %s", verdict, digest, name)
			}
			if strings.Contains(verdict.Reason, probeNPMToken) || len(verdict.Reason) > 500 {
				t.Fatalf("reason %q carries the credential or is unbounded", verdict.Reason)
			}
			if diagnostics := extproto.ValidateMemberProbe(verdict); len(diagnostics) != 0 {
				t.Fatalf("probe %+v is not a valid member-probe: %v", verdict, diagnostics)
			}
		})
	}
	if redirected != 0 {
		t.Fatalf("the probe followed %d redirects", redirected)
	}
}

// npmCLI replays npm for an unmanaged dry run: the recorded or built answer of
// `npm view`, the recorded `npm config get`, and a pack that writes the staged
// tarball. It records every argv, which is what the tests assert on.
type npmCLI struct {
	view *exec.Result
	// viewErr makes npm impossible to start.
	viewErr error
	argv    [][]string
}

func (c *npmCLI) install(t *testing.T) {
	t.Helper()
	originalRun, originalToken, originalGOOS := npmExecRun, npmResolveRegistryToken, npmGOOS
	t.Cleanup(func() { npmExecRun, npmResolveRegistryToken, npmGOOS = originalRun, originalToken, originalGOOS })
	npmGOOS = "linux"
	npmResolveRegistryToken = func(string) (string, string) { return "", "" }
	configAnswer := recordedNPMCommand(t, "config-get-registry")
	npmExecRun = func(name string, args []string, _ ...exec.Option) (*exec.Result, error) {
		if name != "npm" || len(args) == 0 {
			t.Fatalf("unmanaged dry run ran %s %v, want npm", name, args)
		}
		c.argv = append(c.argv, args)
		switch args[0] {
		case "view":
			return c.view, c.viewErr
		case "config":
			return configAnswer, nil
		case "pack":
			if err := os.WriteFile(filepath.Join(flagValue(args, "--pack-destination"), "test-pkg-1.2.3-r42.tgz"), stagedTarball, 0o644); err != nil {
				t.Fatalf("stage fake tarball: %v", err)
			}
			return &exec.Result{Success: true, Stdout: "[]"}, nil
		default:
			t.Fatalf("unmanaged dry run ran npm %v: a probe only reads", args)
			return nil, nil
		}
	}
}

// builtView is an `npm view` answer for a held version, carrying integrity.
func builtView(integrity string) *exec.Result {
	body, _ := json.Marshal(map[string]string{"integrity": integrity, "tarball": "https://registry.npmjs.org/@test/pkg/-/pkg-1.2.3-r42.tgz"})
	return &exec.Result{Success: true, Stdout: string(body) + "\n"}
}

func unmanagedDryRunCtx(t *testing.T) *pctx.Context {
	t.Helper()
	ctx, dir := makeTestCtx(t)
	ctx.Params = pctx.Params{"npm": json.RawMessage(`true`)}
	stageNPMFixture(t, dir)
	return ctx
}

// The unmanaged npm dry run asks through npm, whose configuration owns the
// registry and the credential there. It runs `npm view` once, packs
// with `npm pack` only to compare a held version, and never runs a command
// that writes. Every refusal is what npm printed against a real registry.
func TestUnmanagedNPMDryRunAsksThroughNPM(t *testing.T) {
	spectest.Proves(t, "typescript/typescript-project-toolchain", "dry-run-registry-probe", "unmanaged-npm-probe-asks-through-npm")
	stagedDigest := fmt.Sprintf("sha256:%x", sha256.Sum256(stagedTarball))
	staged512 := sha512.Sum512(stagedTarball)
	staged256 := sha256.Sum256(stagedTarball)
	cases := []struct {
		name string
		view func(t *testing.T) *exec.Result

		wantState    string
		wantLocal    string
		wantRegistry string
		wantReason   string
		wantPack     bool
	}{
		{
			name:      "a version the registry does not hold is absent",
			view:      func(t *testing.T) *exec.Result { return recordedNPMCommand(t, "view-version-not-found") },
			wantState: extproto.MemberProbeAbsent,
		},
		{
			name:      "a package the registry does not know is absent",
			view:      func(t *testing.T) *exec.Result { return recordedNPMCommand(t, "view-package-not-found") },
			wantState: extproto.MemberProbeAbsent,
		},
		{
			name:      "an npm that prints nothing for a missing version is absent",
			view:      func(*testing.T) *exec.Result { return &exec.Result{Success: true} },
			wantState: extproto.MemberProbeAbsent,
		},
		{
			name: "the same sha512 integrity is identical",
			view: func(*testing.T) *exec.Result {
				return builtView("sha512-" + base64.StdEncoding.EncodeToString(staged512[:]))
			},
			wantState: extproto.MemberProbeIdentical, wantLocal: stagedDigest, wantRegistry: stagedDigest, wantPack: true,
		},
		{
			name: "the same sha256 integrity is identical",
			view: func(*testing.T) *exec.Result {
				return builtView("sha256-" + base64.StdEncoding.EncodeToString(staged256[:]))
			},
			wantState: extproto.MemberProbeIdentical, wantLocal: stagedDigest, wantRegistry: stagedDigest, wantPack: true,
		},
		{
			name:      "another sha512 integrity is a conflict",
			view:      func(t *testing.T) *exec.Result { return recordedNPMCommand(t, "view-version-held") },
			wantState: extproto.MemberProbeConflict, wantLocal: stagedDigest, wantReason: "reuses the existing version without comparing", wantPack: true,
		},
		{
			name:      "another sha256 integrity is a conflict that names the registry digest",
			view:      func(t *testing.T) *exec.Result { return recordedNPMCommand(t, "view-private-registry") },
			wantState: extproto.MemberProbeConflict, wantLocal: stagedDigest,
			wantRegistry: "sha256:c84acb73640240b3be4bca3a67c5cb9c5b1039c260f79aff9dd33bdab518d2be",
			wantReason:   "with other bytes than the packed package", wantPack: true,
		},
		{
			name:      "an integrity the probe cannot compare is unverified",
			view:      func(*testing.T) *exec.Result { return builtView("sha1-2jmj7l5rSw0yVb/vlWAYkK/YBwk=") },
			wantState: extproto.MemberProbeUnverified, wantLocal: stagedDigest, wantReason: "advertised no sha512 or sha256 integrity", wantPack: true,
		},
		{
			name:      "a registry npm cannot reach is unverified",
			view:      func(t *testing.T) *exec.Result { return recordedNPMCommand(t, "view-registry-unreachable") },
			wantState: extproto.MemberProbeUnverified, wantReason: "the registry could not be reached: npm reports ECONNREFUSED",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cli := &npmCLI{view: tc.view(t)}
			cli.install(t)

			probe := oneNPMProbe(t, unmanagedDryRunCtx(t))

			if probe.State != tc.wantState {
				t.Fatalf("state = %q (%s), want %q", probe.State, probe.Reason, tc.wantState)
			}
			// The registry is the one the recorded `npm config get` names.
			if probe.Ecosystem != "npm" || probe.Coordinate != "@test/pkg" || probe.Version != "1.2.3-r42" || probe.Registry != "https://registry.npmjs.org" {
				t.Fatalf("probe = %+v, want the member, the registry and the version", probe)
			}
			if probe.ArtifactDigest != tc.wantLocal || probe.RegistryDigest != tc.wantRegistry || probe.Anonymous {
				t.Fatalf("probe = %+v, want local %q, registry %q, and no claim about the credential npm sent", probe, tc.wantLocal, tc.wantRegistry)
			}
			if !strings.Contains(probe.Reason, tc.wantReason) || (tc.wantReason == "" && probe.Reason != "") {
				t.Fatalf("reason = %q, want %q", probe.Reason, tc.wantReason)
			}

			want := [][]string{
				{"config", "get", "@test:registry", "registry"},
				{"view", "@test/pkg@1.2.3-r42", "dist", "--json", "--fetch-retries", "0"},
			}
			if tc.wantPack {
				want = append(want, []string{"pack", "--json", "--ignore-scripts", "--pack-destination"})
			}
			if len(cli.argv) != len(want) {
				t.Fatalf("npm ran %v, want %d read-only commands", cli.argv, len(want))
			}
			for i, args := range want {
				if strings.Join(cli.argv[i][:len(args)], " ") != strings.Join(args, " ") {
					t.Fatalf("npm command %d = %v, want %v", i, cli.argv[i], args)
				}
			}
		})
	}
}

// An unmanaged dry run on a host where npm cannot start must not pass in
// silence either.
func TestUnmanagedNPMDryRunReportsAnNPMThatCannotRun(t *testing.T) {
	cli := &npmCLI{viewErr: errors.New(`exec: "npm": executable file not found in $PATH`)}
	cli.install(t)

	probe := oneNPMProbe(t, unmanagedDryRunCtx(t))

	if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, "npm could not be run") {
		t.Fatalf("probe = %+v, want unverified because npm could not be run", probe)
	}
}

// A declared registry is named as declared, without asking npm for it, and the
// credential the seam yields for its host is the only one the probe adds.
func TestUnmanagedNPMDryRunNamesTheDeclaredRegistry(t *testing.T) {
	cli := &npmCLI{view: recordedNPMCommand(t, "view-version-not-found")}
	cli.install(t)
	var tokenHosts []string
	npmResolveRegistryToken = func(host string) (string, string) {
		tokenHosts = append(tokenHosts, host)
		return probeNPMToken, ""
	}
	ctx := unmanagedDryRunCtx(t)
	withRegistries(t, ctx, `{"npm":{"publish":"https://npm.example.test/scoped/"}}`)

	probe := oneNPMProbe(t, ctx)

	if probe.State != extproto.MemberProbeAbsent || probe.Registry != "https://npm.example.test/scoped" {
		t.Fatalf("probe = %+v, want absent at the declared registry", probe)
	}
	if len(tokenHosts) != 1 || tokenHosts[0] != "npm.example.test" {
		t.Fatalf("credential seam asked about %v, want the declared host once", tokenHosts)
	}
	if len(cli.argv) != 1 || cli.argv[0][0] != "view" {
		t.Fatalf("npm ran %v, want the one view", cli.argv)
	}
}

func TestCompareNPMIntegrityReadsTheStrongestAlgorithm(t *testing.T) {
	local256 := sha256.Sum256(stagedTarball)
	local512 := sha512.Sum512(stagedTarball)
	other256 := sha256.Sum256([]byte("another tarball"))
	localDigest := fmt.Sprintf("sha256:%x", local256)
	otherDigest := fmt.Sprintf("sha256:%x", other256)
	localIntegrity := "sha512-" + base64.StdEncoding.EncodeToString(local512[:])
	local256Entry := "sha256-" + base64.StdEncoding.EncodeToString(local256[:])
	other256Entry := "sha256-" + base64.StdEncoding.EncodeToString(other256[:])
	other512Entry := "sha512-" + base64.StdEncoding.EncodeToString(make([]byte, sha512.Size))
	for name, tc := range map[string]struct {
		integrity      string
		wantSame       bool
		wantRegistry   string
		wantComparable bool
	}{
		"equal sha512":                     {localIntegrity, true, localDigest, true},
		"equal sha512 among several":       {"sha1-abc " + other512Entry + " " + localIntegrity, true, localDigest, true},
		"other sha512":                     {other512Entry, false, "", true},
		"other sha512 names a sha256":      {other512Entry + " " + other256Entry, false, otherDigest, true},
		"sha512 decides over an equal 256": {other512Entry + " " + local256Entry, false, "", true},
		"equal sha256":                     {local256Entry, true, localDigest, true},
		"other sha256":                     {other256Entry, false, otherDigest, true},
		"sha1 only":                        {"sha1-2jmj7l5rSw0yVb/vlWAYkK/YBwk=", false, "", false},
		"malformed sha256":                 {"sha256-not-base64", false, "", false},
		"nothing":                          {"", false, "", false},
	} {
		same, registryDigest, comparable := compareNPMIntegrity(tc.integrity, localDigest, localIntegrity)
		if same != tc.wantSame || registryDigest != tc.wantRegistry || comparable != tc.wantComparable {
			t.Errorf("%s: compareNPMIntegrity() = (%t, %q, %t), want (%t, %q, %t)",
				name, same, registryDigest, comparable, tc.wantSame, tc.wantRegistry, tc.wantComparable)
		}
	}
}

func TestNPMFailureReasonSaysWhatToDo(t *testing.T) {
	for code, want := range map[string]string{
		"ENOTFOUND": "the registry could not be reached: npm reports ENOTFOUND",
		"E401":      "sign in to npm or supply the registry token",
		"ENEEDAUTH": "sign in to npm or supply the registry token",
		"E500":      "npm view failed with E500: Internal Server Error",
		"":          "npm view failed without an error code",
	} {
		if got := npmFailureReason(code, "Internal Server Error"); !strings.Contains(got, want) {
			t.Errorf("npmFailureReason(%q) = %q, want it to contain %q", code, got, want)
		}
	}
	if got := npmFailureReason("EUSAGE", ""); got != "npm view failed with EUSAGE" {
		t.Errorf("npmFailureReason without a summary = %q", got)
	}
}
