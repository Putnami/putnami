package distributioncli

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The corruption this file guards against, observed on a developer machine on
// 2026-09-02: ~/.npmrc held 61 bytes that were not npm config at all — they were
// the tail of a registries.json token recipe:
//
//	"command": [
//	"putnami",
//	"cloud",
//	"token",
//	"--for",
//	"npm"
//	]
//	}
//
// Nothing "copied registries.json into .npmrc". The mechanism is two ordinary
// functions composing badly:
//
//  1. writeNpmrcAuth wrote `//<host>/:_authToken=<token>` with a token that
//     carried embedded newlines (a token source that printed a JSON object
//     instead of a bearer). Only the FIRST physical line carries the
//     `_authToken=` prefix; the rest land in the file as free-standing lines.
//  2. removeNpmrcAuth later filtered by that same line prefix, so it removed
//     exactly one line — the one with the prefix — and left the continuation
//     lines behind. Every later run preserved them.
//
// Both halves are now closed: the write is refused, and the residue is scrubbed.
const corruptedNpmrcArtifact = `"command": [
"putnami",
"cloud",
"token",
"--for",
"npm"
]
}
`

// tokenSourceJSON is the value that, written as an _authToken, produces the
// artifact above: json.MarshalIndent(TokenSource{...}, "", "").
const tokenSourceJSON = `{
"command": [
"putnami",
"cloud",
"token",
"--for",
"npm"
]
}`

func TestNpmrcCorruptionReproducesAndIsRefused(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home}
	npmrc := filepath.Join(home, ".npmrc")

	// Reproduce the historical damage with the OLD write behavior, byte for
	// byte, so the guard below is pinned to the real artifact rather than to a
	// guess about it.
	legacyLine := "//npm.putnami.dev/:_authToken=" + tokenSourceJSON
	if err := os.WriteFile(npmrc, []byte(legacyLine+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeNpmrcAuth(env, "npm.putnami.dev"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after, err := os.ReadFile(npmrc)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read npmrc: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("residue survived the scrub: %q", after)
	}

	// The write side now refuses the value outright rather than spilling it.
	if err := writeNpmrcAuth(env, "npm.putnami.dev", tokenSourceJSON); err == nil {
		t.Fatal("writeNpmrcAuth accepted a multi-line token")
	} else if !strings.Contains(err.Error(), "multi-line") {
		t.Fatalf("error = %v, want a multi-line refusal", err)
	}
	if data, err := os.ReadFile(npmrc); err == nil && len(data) > 0 {
		t.Fatalf("a refused write must touch nothing, npmrc = %q", data)
	}
}

// TestNpmrcResidueIsScrubbedFromAnAlreadyDamagedFile is the repair path for the
// machines that already carry the artifact: the anchor line is long gone, so the
// scrub cannot key off it and must recognize the residue for what it is.
func TestNpmrcResidueIsScrubbedFromAnAlreadyDamagedFile(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home}
	npmrc := filepath.Join(home, ".npmrc")
	// A file that is ONLY residue is repaired whole (see repairResidue); mixing
	// in real settings is covered by the anchored test below, so this one keeps
	// the pure-orphan shape the machine actually had.
	seed := corruptedNpmrcArtifact
	if err := os.WriteFile(npmrc, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeNpmrcAuth(env, "npm.putnami.dev"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	data, err := os.ReadFile(npmrc)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read npmrc: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("an all-residue file must be emptied, got %q", data)
	}
}

// TestNpmrcRepairIsAnchoredAndNeverTouchesUnrelatedLines is the fence around the
// repair. It may only remove what a damaged entry left behind: the lines
// immediately under a removed `_authToken`, or a file that is nothing but
// residue. A JSON-looking line the user put somewhere else — an ini section, a
// value in braces — is not ours to delete.
func TestNpmrcRepairIsAnchoredAndNeverTouchesUnrelatedLines(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home}
	npmrc := filepath.Join(home, ".npmrc")

	seed := "[some-ini-section]\n" +
		"//npm.putnami.dev/:_authToken=" + tokenSourceJSON + "\n" +
		"strict-ssl=true\n" +
		"}orphan-brace-the-user-wrote\n"
	if err := os.WriteFile(npmrc, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removeNpmrcAuth(env, "npm.putnami.dev"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	got := readFile(t, npmrc)

	// The damaged entry and its continuation lines are gone.
	if strings.Contains(got, "_authToken") || strings.Contains(got, `"putnami",`) {
		t.Fatalf("the damaged entry survived: %q", got)
	}
	// Everything the user owns survives, including lines that LOOK like residue
	// but do not follow a removed entry.
	for _, keep := range []string{"[some-ini-section]", "strict-ssl=true", "}orphan-brace-the-user-wrote"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("the repair deleted an unrelated line %q: %q", keep, got)
		}
	}
}

// TestNetrcRepairIsAnchored mirrors it for the Go credential file.
func TestNetrcRepairIsAnchored(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home}
	netrc := filepath.Join(home, ".netrc")
	seed := "machine go.putnami.dev login _token password " + tokenSourceJSON + "\n" +
		"machine other.example login u password p\n" +
		"}not-ours\n"
	if err := os.WriteFile(netrc, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (gomodWriter{}).Teardown(env, "go.putnami.dev"); err != nil {
		t.Fatalf("teardown: %v", err)
	}
	got := readFile(t, netrc)
	if strings.Contains(got, "go.putnami.dev") || strings.Contains(got, `"putnami",`) {
		t.Fatalf("the damaged entry survived: %q", got)
	}
	for _, keep := range []string{"machine other.example login u password p", "}not-ours"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("the repair deleted an unrelated line %q: %q", keep, got)
		}
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func TestCredentialWritersRefuseMultiLineTokens(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home}
	multiline := "header\nsecond-line"
	for name, write := range map[string]func() error{
		"npm":   func() error { return writeNpmrcAuth(env, "npm.putnami.dev", multiline) },
		"gomod": func() error { return gomodWriter{}.Setup(env, "go.putnami.dev", multiline) },
		"oci":   func() error { return writeDockerCredential(env, "oci.putnami.dev", multiline) },
		"put":   func() error { return putWriter{}.Setup(env, "put.putnami.dev", multiline) },
		"lease": func() error {
			return materializeRegistryLease(env, RegistryEndpoint{Registry: RegistryNPM, Host: "npm.putnami.dev"}, multiline)
		},
		"empty":  func() error { return writeNpmrcAuth(env, "npm.putnami.dev", "") },
		"carrge": func() error { return gomodWriter{}.Setup(env, "go.putnami.dev", "a\rb") },
	} {
		if err := write(); err == nil {
			t.Fatalf("%s writer accepted an unusable credential", name)
		}
	}
	// Nothing may have been created by any refused write.
	for _, path := range []string{".npmrc", ".netrc", filepath.Join(".docker", "config.json")} {
		if _, err := os.Stat(filepath.Join(home, path)); !os.IsNotExist(err) {
			t.Fatalf("refused write created %s: %v", path, err)
		}
	}
}

// TestNetrcSetupRepairsItsOwnDamagedEntry pins the anchored repair on the write
// path: replacing the Putnami entry also clears the continuation lines that
// entry spilled, and leaves every other record — including a JSON-looking line
// the user owns — exactly where it was.
func TestNetrcSetupRepairsItsOwnDamagedEntry(t *testing.T) {
	home := t.TempDir()
	env := map[string]string{"HOME": home}
	netrc := filepath.Join(home, ".netrc")
	seed := "machine go.putnami.dev login _token password " + tokenSourceJSON + "\n" +
		"machine other.example login u password p\n"
	if err := os.WriteFile(netrc, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := (gomodWriter{}).Setup(env, "go.putnami.dev", "bearer-value"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	got := readFile(t, netrc)
	if strings.Contains(got, `"putnami",`) || strings.Contains(got, `"command": [`) {
		t.Fatalf("the damaged entry's continuation lines survived: %q", got)
	}
	if !strings.Contains(got, "machine other.example login u password p") {
		t.Fatalf("the repair removed a real netrc entry: %q", got)
	}
	if !strings.Contains(got, "machine go.putnami.dev login _token password bearer-value") {
		t.Fatalf("setup did not write the credential: %q", got)
	}
	if strings.Count(got, "machine go.putnami.dev") != 1 {
		t.Fatalf("setup must replace, not duplicate: %q", got)
	}
}

// TestMaterializeWritesNativeCredentialsToHomeNotPutnamiHome pins the fix for a
// 2026-09-03 regression: the framework CLI runs every extension with
// PUTNAMI_HOME=~/.putnami, and materializeRegistryLease used to resolve
// ~/.npmrc, ~/.netrc, and the Docker config through that same variable
// (homeRoot preferred PUTNAMI_HOME over HOME). `putnami cloud token --for npm
// --materialize` then wrote a working credential to ~/.putnami/.npmrc — a file
// bun and go never read — while the real ~/.npmrc stayed untouched and the
// login silently did nothing from the native tool's point of view.
//
// HOME and PUTNAMI_HOME are pinned to two DIFFERENT temp dirs so a writer that
// still consulted PUTNAMI_HOME for a native file fails this test instead of
// accidentally passing because the two roots happened to coincide.
func TestMaterializeWritesNativeCredentialsToHomeNotPutnamiHome(t *testing.T) {
	home := t.TempDir()
	putnamiHome := t.TempDir()
	env := map[string]string{"HOME": home, "PUTNAMI_HOME": putnamiHome}

	npmEndpoint := RegistryEndpoint{Registry: RegistryNPM, Host: "npm.putnami.dev", URL: "https://npm.putnami.dev"}
	if err := materializeRegistryLease(env, npmEndpoint, "npm-bearer"); err != nil {
		t.Fatalf("materialize npm: %v", err)
	}
	npmrc := readFile(t, filepath.Join(home, ".npmrc"))
	if !strings.Contains(npmrc, "//npm.putnami.dev/:_authToken=npm-bearer") {
		t.Fatalf("npm credential missing from $HOME/.npmrc: %q", npmrc)
	}
	if _, err := os.Stat(filepath.Join(putnamiHome, ".npmrc")); !os.IsNotExist(err) {
		t.Fatalf("npm credential leaked into PUTNAMI_HOME (err = %v)", err)
	}

	gomodEndpoint := RegistryEndpoint{Registry: RegistryGomod, Host: "go.putnami.dev", URL: "https://go.putnami.dev"}
	if err := materializeRegistryLease(env, gomodEndpoint, "go-bearer"); err != nil {
		t.Fatalf("materialize go: %v", err)
	}
	netrc := readFile(t, filepath.Join(home, ".netrc"))
	if !strings.Contains(netrc, "machine go.putnami.dev login _token password go-bearer") {
		t.Fatalf("go credential missing from $HOME/.netrc: %q", netrc)
	}
	if _, err := os.Stat(filepath.Join(putnamiHome, ".netrc")); !os.IsNotExist(err) {
		t.Fatalf("go credential leaked into PUTNAMI_HOME (err = %v)", err)
	}

	ociEndpoint := RegistryEndpoint{Registry: RegistryOCI, Host: "oci.putnami.dev", URL: "https://oci.putnami.dev"}
	if err := materializeRegistryLease(env, ociEndpoint, "oci-bearer"); err != nil {
		t.Fatalf("materialize oci: %v", err)
	}
	dockerConfig := readFile(t, filepath.Join(home, ".docker", "config.json"))
	var cfg struct {
		Auths map[string]struct {
			Auth string `json:"auth"`
		} `json:"auths"`
	}
	if err := json.Unmarshal([]byte(dockerConfig), &cfg); err != nil {
		t.Fatalf("parse docker config: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(cfg.Auths["oci.putnami.dev"].Auth)
	if err != nil || string(decoded) != "_token:oci-bearer" {
		t.Fatalf("docker credential = %q (err = %v), want _token:oci-bearer", decoded, err)
	}
	if _, err := os.Stat(filepath.Join(putnamiHome, ".docker")); !os.IsNotExist(err) {
		t.Fatalf("docker config leaked into PUTNAMI_HOME (err = %v)", err)
	}

	// registries.json is Putnami-owned state and belongs under PUTNAMI_HOME —
	// unlike the three native files above, its location must NOT move to HOME.
	if _, err := WriteRegistryTokenRecipes(nil, env); err != nil {
		t.Fatalf("write recipes: %v", err)
	}
	statePath := registriesStatePath(env)
	if !strings.HasPrefix(statePath, putnamiHome+string(filepath.Separator)) {
		t.Fatalf("registries.json path %q is not under PUTNAMI_HOME %q", statePath, putnamiHome)
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatalf("registries.json missing at %s: %v", statePath, err)
	}
	if _, err := os.Stat(filepath.Join(home, "registries.json")); !os.IsNotExist(err) {
		t.Fatalf("registries.json leaked into HOME (err = %v)", err)
	}
}

// TestNetrcPathFollowsTheGoCommand pins the file the gomod writer uses to the
// one the go command reads (cmd/go/internal/auth): on Windows _netrc wins when
// it exists, .netrc is used when only it exists, and a new file is _netrc,
// which every Go release reads there. Elsewhere it is always .netrc.
func TestNetrcPathFollowsTheGoCommand(t *testing.T) {
	cases := []struct {
		name     string
		goos     string
		existing []string
		want     string
	}{
		{"windows without a netrc", "windows", nil, "_netrc"},
		{"windows with only .netrc", "windows", []string{".netrc"}, ".netrc"},
		{"windows with only _netrc", "windows", []string{"_netrc"}, "_netrc"},
		{"windows with both", "windows", []string{".netrc", "_netrc"}, "_netrc"},
		{"linux without a netrc", "linux", nil, ".netrc"},
		{"linux with _netrc", "linux", []string{"_netrc"}, ".netrc"},
		{"darwin with both", "darwin", []string{".netrc", "_netrc"}, ".netrc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			for _, name := range tc.existing {
				if err := os.WriteFile(filepath.Join(home, name), []byte("machine other login x password y\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			env := map[string]string{"HOME": home, "USERPROFILE": home}
			if got, want := netrcPathFor(env, tc.goos), filepath.Join(home, tc.want); got != want {
				t.Fatalf("netrcPathFor = %q, want %q", got, want)
			}
		})
	}
}

// TestNetrcPathHonorsNETRC pins the go command's NETRC override on every OS,
// and proves the gomod writer and the token reader both use that file.
func TestNetrcPathHonorsNETRC(t *testing.T) {
	home := t.TempDir()
	custom := filepath.Join(t.TempDir(), "go-netrc")
	env := map[string]string{"HOME": home, "USERPROFILE": home, "NETRC": custom}
	for _, goos := range []string{"windows", "linux", "darwin"} {
		if got := netrcPathFor(env, goos); got != custom {
			t.Fatalf("netrcPathFor on %s = %q, want NETRC %q", goos, got, custom)
		}
	}
	if err := (gomodWriter{}).Setup(env, "go.putnami.dev", "go-bearer"); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if got := readFile(t, custom); !strings.Contains(got, "machine go.putnami.dev login _token password go-bearer") {
		t.Fatalf("credential missing from NETRC: %q", got)
	}
	token, err := readNetrcToken(env, "go.putnami.dev")
	if err != nil || token != "go-bearer" {
		t.Fatalf("readNetrcToken = %q, %v; want the credential written to NETRC", token, err)
	}
	for _, name := range []string{".netrc", "_netrc"} {
		if _, err := os.Stat(filepath.Join(home, name)); !os.IsNotExist(err) {
			t.Fatalf("%s written in the home directory despite NETRC (err = %v)", name, err)
		}
	}
}

// TestNativeHomeRootUsesUSERPROFILEOnWindows pins decision D-W7 for the native
// credential files: npm, go and Docker find them under %USERPROFILE% on
// Windows, so a HOME exported there by a POSIX shell must not redirect them.
func TestNativeHomeRootUsesUSERPROFILEOnWindows(t *testing.T) {
	env := map[string]string{"HOME": "/posix-home", "USERPROFILE": `C:\Users\dev`}
	if got := nativeHomeRootFor(env, "windows"); got != `C:\Users\dev` {
		t.Fatalf("nativeHomeRootFor on windows = %q, want USERPROFILE", got)
	}
	for _, goos := range []string{"linux", "darwin"} {
		if got := nativeHomeRootFor(env, goos); got != "/posix-home" {
			t.Fatalf("nativeHomeRootFor on %s = %q, want HOME", goos, got)
		}
	}
}
