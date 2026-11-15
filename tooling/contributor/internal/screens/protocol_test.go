package screens

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/protocol/screenshots"
	"go.putnami.dev/protocol/screenshots/schematest"
)

const protocolReq = "the-manifest-is-a-protocol-document"

// TestTheWrittenManifestValidatesAgainstThePublishedSchema runs the command
// on a change that touches a page both sides hold and adds another, then
// judges the bytes it wrote with the published schema and with the protocol
// decoder.
func TestTheWrittenManifestValidatesAgainstThePublishedSchema(t *testing.T) {
	t.Parallel()
	dir := webRepo(t, func(dir string) {
		inv := sampleWeb(t)
		inv.Routes = append(inv.Routes, webInventory(t, "exact /about src/app/about/page.tsx").Routes[0])
		write(t, dir, "apps/web/schema/http-routes.json", inventoryJSON(t, inv))
		write(t, dir, "apps/web/src/app/about/page.tsx", "export default 3\n")
		write(t, dir, "apps/web/src/app/tasks/page.tsx", "export default 2\n")
	})
	capturer := &fakeCapturer{redirects: map[string]string{"http://127.0.0.1:3000/about": "https://accounts.example.test/sign-in?next=%2Fabout"}}
	got := runCommand(t, dir, capturer, serverArgs...)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	data, err := os.ReadFile(filepath.Join(dir, ".putnami", "screenshots", "apps-web", screenshots.ManifestFile))
	if err != nil {
		t.Fatal(err)
	}
	if refusal := schematest.Check(t, data); refusal != nil {
		t.Fatalf("the published schema refuses the manifest the command wrote: %v\n%s", refusal, data)
	}
	manifest, diags := screenshots.Decode(data)
	if manifest == nil {
		t.Fatalf("the protocol decoder refuses the manifest the command wrote: %v", diags)
	}
	if len(manifest.Pages) != 2 || len(manifest.Captures) != 12 {
		t.Fatalf("%d pages, %d captures, want a page of both sides and a page of one", len(manifest.Pages), len(manifest.Captures))
	}
	// The command writes the bytes the protocol serializes for the manifest.
	again, err := screenshots.Marshal(*manifest)
	if err != nil || string(again) != string(data) {
		t.Fatalf("the manifest is not the protocol's serialization: %v", err)
	}
	spectest.Proves(t, screensFeature, protocolReq, "the-written-manifest-validates-against-the-published-schema")
}

// TestTheCapturesAreTheProtocolSets pins the viewports and the schemes the
// command captures to the closed sets of the protocol.
func TestTheCapturesAreTheProtocolSets(t *testing.T) {
	want := []Viewport{
		{Name: "desktop", Width: 1440, Height: 900, Scale: 1},
		{Name: "mobile", Width: 390, Height: 844, Scale: 2, Mobile: true},
	}
	if !reflect.DeepEqual(Viewports, want) {
		t.Fatalf("Viewports = %+v", Viewports)
	}
	if !reflect.DeepEqual(Schemes, screenshots.Schemes()) || SideBefore != screenshots.SideBefore || SideAfter != screenshots.SideAfter {
		t.Fatalf("Schemes = %v", Schemes)
	}
}

// TestAManifestTheProtocolRefusesIsNotWrittenOrRead fails a run whose
// manifest the contract does not allow before it writes or removes a file,
// and reads no manifest the protocol decoder refuses.
func TestAManifestTheProtocolRefusesIsNotWrittenOrRead(t *testing.T) {
	t.Parallel()
	dir := webRepo(t, func(dir string) { write(t, dir, "apps/web/src/app/tasks/page.tsx", "export default 2\n") })
	out := filepath.Join(dir, ".putnami", "screenshots", "apps-web")
	file := filepath.Join(out, ManifestFile)
	if first := runCommand(t, dir, nil, serverArgs...); first.code != 0 {
		t.Fatalf("exit %d: %s", first.code, first.stderr)
	}
	before, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}

	// A page that ends outside http and https: the protocol refuses its url.
	capturer := &fakeCapturer{redirects: map[string]string{"http://127.0.0.1:3000/tasks": "about:blank"}}
	got := runCommand(t, dir, capturer, serverArgs...)
	if got.code != 1 || !strings.Contains(got.stderr, "the screenshots manifest is not valid") ||
		!strings.Contains(got.stderr, `captures[4].url: url "about:blank" is no absolute http or https URL (screenshots.invalid_url)`) {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	after, err := os.ReadFile(file)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the refused run changed the manifest of the run before it: %v", err)
	}
	if kept, err := ReadManifest(file); err != nil || len(Check(kept, out, kept.HeadSHA, "apps/web", kept.Pages)) != 0 {
		t.Fatalf("the refused run changed the captures of the run before it: %v", err)
	}

	// A capture past the bound the protocol publishes fails the run.
	got = runCommand(t, dir, &fakeCapturer{pad: screenshots.MaxCaptureBytes + 1}, serverArgs...)
	if got.code != 1 || !strings.Contains(got.stderr, "before /tasks (desktop light): the capture is 67108865 bytes, more than the 67108864 a capture may be") {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}

	// The reader refuses what the decoder refuses, and names the rule.
	for name, c := range map[string]struct{ old, new, want string }{
		"an unknown member":  {`"project":`, `"tool": "x", "project":`, `unknown field "tool" (screenshots.unknown_field)`},
		"a missing capture":  {`"scheme": "dark"`, `"scheme": "light"`, "(screenshots.missing_capture)"},
		"an abbreviated sha": {`"headSHA": "`, `"headSHA": "abc`, "(screenshots.invalid_sha)"},
		"another version":    {`"protocolVersion": 1`, `"protocolVersion": 2`, "protocolVersion 2 is not supported (want 1) (screenshots.invalid_protocol_version)"},
	} {
		edited := filepath.Join(t.TempDir(), ManifestFile)
		if err := os.WriteFile(edited, []byte(strings.Replace(string(before), c.old, c.new, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadManifest(edited); err == nil || !strings.Contains(err.Error(), "not a screenshots manifest") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if _, err := WriteManifest(t.TempDir(), Manifest{ProtocolVersion: 1, Project: "apps/web"}); err == nil {
		t.Error("WriteManifest wrote a manifest without its commits")
	}
	spectest.Proves(t, screensFeature, protocolReq, "a-manifest-the-protocol-refuses-is-not-read")
}

// TestARunRemovesTheFilesOfAPreviousManifestTheContractRefuses replaces the
// captures a manifest lists even when the decoder would refuse it, such as
// one cut short of a capture, and keeps the files it does not list.
func TestARunRemovesTheFilesOfAPreviousManifestTheContractRefuses(t *testing.T) {
	t.Parallel()
	dir := webRepo(t, func(dir string) { write(t, dir, "apps/web/src/app/tasks/page.tsx", "export default 2\n") })
	out := filepath.Join(dir, "shots")
	write(t, out, "old-capture.png", "old\n")
	write(t, out, "mine.png", "mine\n")
	write(t, out, ManifestFile, `{"protocolVersion": 1, "project": "apps/web", "baseSHA": "old", "headSHA": "old", "pages": [],
		"captures": [{"file": "old-capture.png", "route": "/gone", "side": "after", "viewport": "desktop", "scheme": "light",
		"sha256": "", "status": 200, "url": ""}, {"file": "../mine.png", "route": "/gone", "side": "after", "viewport": "desktop",
		"scheme": "dark", "sha256": "", "status": 200, "url": ""}]}`)
	if got := runCommand(t, dir, nil, append([]string{"--out", "shots"}, serverArgs...)...); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if _, err := os.Stat(filepath.Join(out, "old-capture.png")); !os.IsNotExist(err) {
		t.Errorf("the capture of the previous manifest is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Join(out, "mine.png")); err != nil {
		t.Errorf("a file the previous manifest does not list was removed: %v", err)
	}
	if _, err := ReadManifest(filepath.Join(out, ManifestFile)); err != nil {
		t.Errorf("the new manifest does not read: %v", err)
	}
}
