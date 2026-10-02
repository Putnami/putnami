package memberprobe

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	extproto "go.putnami.dev/protocol/extension"
	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/sdk/extension/recorded"
)

const (
	archiveLocalDigest = "sha256:926e5e7d82a5c98207475a2515515097b2d415cbcd16eed5a901c4483b037016"
	archiveOtherDigest = "sha256:7d78d59654b702b8852725acf694a1391b8e184b3547808ac08af81d5bcaa308"
	archiveTestToken   = "pkt_archive_probe_test_token"
)

func recordedPutResponse(t *testing.T, name string) recorded.Response {
	t.Helper()
	return recorded.HTTP(t, filepath.Join("testdata", "recorded", "put-registry", name))
}

// heldArchive answers as the put registry does for a version it holds: no body
// worth transferring, and the two headers it sends on that answer. The success
// is built here because the recorded one declares tens of megabytes.
func heldArchive(digest, resolvedVersion string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("content-type", "application/gzip")
		if digest != "" {
			w.Header().Set("x-integrity", digest)
		}
		w.Header().Set("x-resolved-version", resolvedVersion)
		w.WriteHeader(http.StatusOK)
	})
}

// The put probe answers the four states from what the registry said, names the
// member, the registry and the version in every verdict, and asks with HEAD
// only. Every refusal is a response the production registry sent.
func TestProbeArchiveReportsWhatTheRegistryHolds(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "dry-run-member-probe", "put-archive-probe-is-read-only")
	const version = "1.4.0"
	cases := []struct {
		name string
		// answer is the registry's response: a recording, or a built success.
		recording string
		held      http.Handler
		digest    string
		token     string

		wantState     string
		wantRegistry  string
		wantReason    string
		wantAnonymous bool
	}{
		{
			name: "a version the registry does not hold is absent", recording: "archive-version-not-found.404.http",
			digest: archiveLocalDigest, token: archiveTestToken, wantState: extproto.MemberProbeAbsent,
		},
		{
			name: "a package the registry does not know is absent", recording: "archive-package-not-found.404.http",
			digest: archiveLocalDigest, token: archiveTestToken, wantState: extproto.MemberProbeAbsent,
		},
		{
			name: "an anonymous 404 is absent and says it was anonymous", recording: "archive-private-anonymous.404.http",
			digest: archiveLocalDigest, wantState: extproto.MemberProbeAbsent, wantAnonymous: true,
		},
		{
			name: "the same digest is identical", held: heldArchive(archiveLocalDigest, version),
			digest: archiveLocalDigest, token: archiveTestToken,
			wantState: extproto.MemberProbeIdentical, wantRegistry: archiveLocalDigest,
		},
		{
			name: "another digest is a conflict", held: heldArchive(archiveOtherDigest, version),
			digest: archiveLocalDigest, token: archiveTestToken,
			wantState: extproto.MemberProbeConflict, wantRegistry: archiveOtherDigest, wantReason: "another digest",
		},
		{
			name: "a held version with no local archive is a conflict", held: heldArchive(archiveOtherDigest, version),
			token:     archiveTestToken,
			wantState: extproto.MemberProbeConflict, wantRegistry: archiveOtherDigest, wantReason: "built no artifact to compare",
		},
		{
			name: "a held version without an advertised digest is unverified", held: heldArchive("", version),
			digest: archiveLocalDigest, token: archiveTestToken,
			wantState: extproto.MemberProbeUnverified, wantReason: "advertised no digest",
		},
		{
			name: "an answer for another version is unverified", held: heldArchive(archiveLocalDigest, "1.5.0"),
			digest: archiveLocalDigest, token: archiveTestToken,
			wantState: extproto.MemberProbeUnverified, wantReason: "resolved version 1.5.0, not 1.4.0",
		},
		{
			name: "an anonymous 401 is unverified and asks for a credential", recording: "archive-anonymous.401.http",
			digest: archiveLocalDigest, wantState: extproto.MemberProbeUnverified,
			wantReason: "401 Unauthorized to a request without a credential", wantAnonymous: true,
		},
		{
			name: "a refused credential is unverified", recording: "archive-token-for-another-registry.403.http",
			digest: archiveLocalDigest, token: archiveTestToken,
			wantState: extproto.MemberProbeUnverified, wantReason: "refused the credential with 403 Forbidden",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var server *recorded.Server
			if tc.held != nil {
				server = recorded.NewServer(t, tc.held)
			} else {
				server = recorded.NewServer(t, nil, recordedPutResponse(t, tc.recording))
			}

			probe := ProbeArchive(context.Background(), Archive{
				Registry: server.URL, Coordinate: "acme/widget", Version: version,
				OS: "linux", Arch: "amd64", Digest: tc.digest, Token: tc.token,
			})

			if probe.State != tc.wantState {
				t.Fatalf("state = %q (%s), want %q", probe.State, probe.Reason, tc.wantState)
			}
			if probe.Ecosystem != PutEcosystem || probe.Coordinate != "acme/widget" || probe.Version != version ||
				probe.Platform != "linux/amd64" || probe.Registry != server.URL {
				t.Fatalf("probe = %+v, want the member, the platform, the registry and the version", probe)
			}
			if probe.ArtifactDigest != tc.digest || probe.RegistryDigest != tc.wantRegistry || probe.Anonymous != tc.wantAnonymous {
				t.Fatalf("probe = %+v, want local %q, registry %q, anonymous %t", probe, tc.digest, tc.wantRegistry, tc.wantAnonymous)
			}
			if !strings.Contains(probe.Reason, tc.wantReason) {
				t.Fatalf("reason = %q, want it to contain %q", probe.Reason, tc.wantReason)
			}
			if strings.Contains(probe.Reason, archiveTestToken) {
				t.Fatalf("reason %q carries the credential", probe.Reason)
			}
			if diagnostics := extproto.ValidateMemberProbe(&probe); len(diagnostics) != 0 {
				t.Fatalf("probe %+v is not a valid member-probe: %v", probe, diagnostics)
			}

			requests := server.Requests()
			if len(requests) != 1 {
				t.Fatalf("the probe sent %d requests, want exactly one", len(requests))
			}
			request := requests[0]
			if request.Method != http.MethodHead {
				t.Fatalf("the probe sent %s, want HEAD: it must never write and never download the archive", request.Method)
			}
			wantQuery := url.Values{"channel": {version}, "os": {"linux"}, "arch": {"amd64"}}.Encode()
			if request.URL.Path != "/acme/widget/download" || request.URL.Query().Encode() != wantQuery {
				t.Fatalf("the probe asked %s?%s, want /acme/widget/download?%s", request.URL.Path, request.URL.RawQuery, wantQuery)
			}
			wantAuthorization := ""
			if tc.token != "" {
				wantAuthorization = "Bearer " + tc.token
			}
			if got := request.Header.Get("Authorization"); got != wantAuthorization {
				t.Fatalf("Authorization = %q, want %q: a probe sends a credential only when one resolved", got, wantAuthorization)
			}
		})
	}
}

// A dry run with no network access must not pass in silence: a registry that
// cannot be reached is an unverified verdict whose reason says so.
func TestProbeArchiveReportsAnUnreachableRegistry(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	registry := "http://" + listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}

	probe := ProbeArchive(context.Background(), Archive{
		Registry: registry, Coordinate: "acme/widget", Version: "1.4.0", OS: "linux", Arch: "amd64", Digest: archiveLocalDigest,
	})

	if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, "the registry could not be reached") {
		t.Fatalf("probe = %+v, want unverified with a reason that names the unreachable registry", probe)
	}
	if strings.Contains(probe.Reason, "/acme/widget/download") {
		t.Fatalf("reason %q repeats the request URL; the probe already names the registry", probe.Reason)
	}
	if diagnostics := extproto.ValidateMemberProbe(&probe); len(diagnostics) != 0 {
		t.Fatalf("probe %+v is not a valid member-probe: %v", probe, diagnostics)
	}
}

// A redirect is an answer, not a route: the bearer bound to the registry origin
// never reaches the redirect target, and the probe reports the status it got.
func TestProbeArchiveFollowsNoRedirect(t *testing.T) {
	target := recorded.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the probe followed a redirect")
	}))
	registry := recorded.NewServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/acme/widget/download", http.StatusFound)
	}))

	probe := ProbeArchive(context.Background(), Archive{
		Registry: registry.URL, Coordinate: "acme/widget", Version: "1.4.0",
		OS: "linux", Arch: "amd64", Digest: archiveLocalDigest, Token: archiveTestToken,
	})

	if probe.State != extproto.MemberProbeUnverified || !strings.Contains(probe.Reason, "302 Found") {
		t.Fatalf("probe = %+v, want unverified with the redirect status", probe)
	}
	if requests := registry.Requests(); len(requests) != 1 || requests[0].Method != http.MethodHead {
		t.Fatalf("the probe sent %d requests to the registry, want one HEAD", len(requests))
	}
	if requests := target.Requests(); len(requests) != 0 {
		t.Fatalf("the probe sent %d requests to the redirect target", len(requests))
	}
}

// A registry URL that carries a credential, or that would send a bearer in the
// clear, is refused before any request, and the refusal does not repeat the
// credential.
func TestProbeArchiveRefusesAnUnsafeRegistryBeforeAnyRequest(t *testing.T) {
	sentinel := recorded.NewServer(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("the probe sent a request to a registry URL it must refuse")
	}))
	host := strings.TrimPrefix(sentinel.URL, "http://")
	for name, archive := range map[string]Archive{
		"user information": {Registry: "http://user:hunter2@" + host, Coordinate: "acme/widget", Version: "1.4.0", OS: "linux", Arch: "amd64"},
		"query string":     {Registry: sentinel.URL + "?token=hunter2", Coordinate: "acme/widget", Version: "1.4.0", OS: "linux", Arch: "amd64"},
		"remote http":      {Registry: "http://registry.example.test", Coordinate: "acme/widget", Version: "1.4.0", OS: "linux", Arch: "amd64"},
		"no scheme":        {Registry: host, Coordinate: "acme/widget", Version: "1.4.0", OS: "linux", Arch: "amd64"},
		"bad coordinate":   {Registry: sentinel.URL, Coordinate: "widget", Version: "1.4.0", OS: "linux", Arch: "amd64"},
		"no platform":      {Registry: sentinel.URL, Coordinate: "acme/widget", Version: "1.4.0"},
	} {
		t.Run(name, func(t *testing.T) {
			archive.Token = archiveTestToken
			probe := ProbeArchive(context.Background(), archive)
			if probe.State != extproto.MemberProbeUnverified || probe.Reason == "" {
				t.Fatalf("probe = %+v, want unverified with a reason", probe)
			}
			if strings.Contains(probe.Reason, "hunter2") || strings.Contains(probe.Reason, archiveTestToken) {
				t.Fatalf("reason %q repeats a credential", probe.Reason)
			}
		})
	}
	if requests := sentinel.Requests(); len(requests) != 0 {
		t.Fatalf("the probe sent %d requests to refused registries", len(requests))
	}
}

func TestAdvertisedArchiveDigestAcceptsTheRegistrySpellings(t *testing.T) {
	const hex = "926e5e7d82a5c98207475a2515515097b2d415cbcd16eed5a901c4483b037016"
	for name, tc := range map[string]struct {
		header http.Header
		want   string
	}{
		"x-integrity":        {http.Header{"X-Integrity": {"sha256:" + hex}}, "sha256:" + hex},
		"upper case":         {http.Header{"X-Integrity": {"SHA256:" + strings.ToUpper(hex)}}, "sha256:" + hex},
		"raw hex":            {http.Header{"X-Integrity": {hex}}, "sha256:" + hex},
		"digest header":      {http.Header{"Digest": {"sha-512=abc, sha-256=" + hex}}, "sha256:" + hex},
		"integrity wins":     {http.Header{"X-Integrity": {"sha256:" + hex}, "Digest": {"sha-256=" + strings.Repeat("0", 64)}}, "sha256:" + hex},
		"none":               {http.Header{}, ""},
		"not a sha256":       {http.Header{"X-Integrity": {"sha512-abc"}}, ""},
		"not hex":            {http.Header{"X-Integrity": {"sha256:" + strings.Repeat("z", 64)}}, ""},
		"base64 is not read": {http.Header{"Digest": {"sha-256=:" + strings.Repeat("A", 43) + "=:"}}, ""},
	} {
		if got := advertisedArchiveDigest(tc.header); got != tc.want {
			t.Errorf("%s: advertisedArchiveDigest() = %q, want %q", name, got, tc.want)
		}
	}
}
