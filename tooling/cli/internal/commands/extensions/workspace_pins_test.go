package extensions

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/hometest"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

var (
	extensionPinOps = artifactOps{workspacePins: extensionWorkspacePins, pinLiteral: extensionPinLiteral, configMap: shared.BuildExtensionMap, plural: "extensions"}
	templatePinOps  = artifactOps{workspacePins: templateWorkspacePins, pinLiteral: templatePinLiteral, configMap: shared.BuildTemplateMap, plural: "templates"}
)

// pinReleases returns the release each name pins in config.
func pinReleases(t *testing.T, ops artifactOps, config string) map[string]string {
	t.Helper()
	pins, err := workspacePinsOf([]byte(config), ops)
	if err != nil {
		t.Fatal(err)
	}
	releases := map[string]string{}
	for name, pin := range pins {
		releases[name] = pin.release
	}
	return releases
}

// rewrite returns config with the moved releases written to their pins.
func rewrite(t *testing.T, ops artifactOps, config string, moved map[string]string) string {
	t.Helper()
	got, err := rewritePins([]byte(config), ops, moved)
	if err != nil {
		t.Fatal(err)
	}
	return string(got)
}

// Only an exact release is a pin: ranges, channels, "latest", unpinned
// entries and local paths are not.
func TestExtensionWorkspacePins_OnlyExactReleases(t *testing.T) {
	config := `{"extensions": {
		"@a/exact": "1.2.3",
		"@a/prerelease": "0.0.0-20260926154701-255b7e227",
		"@a/caret": "^1.0.0",
		"@a/range": ">=1.0.0 <2.0.0",
		"@a/latest": "latest",
		"@a/channel": "canary",
		"@a/unpinned": "",
		"./local": "1.0.0",
		"/abs/local": ""
	}}`
	want := map[string]string{"@a/exact": "1.2.3", "@a/prerelease": "0.0.0-20260926154701-255b7e227"}
	if got := pinReleases(t, extensionPinOps, config); !maps.Equal(got, want) {
		t.Errorf("pins = %v, want %v", got, want)
	}
}

func TestExtensionWorkspacePins_ListFormPinsNothing(t *testing.T) {
	if got := pinReleases(t, extensionPinOps, `{"extensions": ["@a/one", "@a/two"]}`); len(got) != 0 {
		t.Errorf("list form pins = %v, want none", got)
	}
}

// A workspace file the config reader cannot decode pins nothing.
func TestWorkspacePins_UndecodableFilePinsNothing(t *testing.T) {
	if got := pinReleases(t, extensionPinOps, `{"extensions": {"@a/x": "1.0.0", "@a/y": 2}}`); len(got) != 0 {
		t.Errorf("extension pins = %v, want none", got)
	}
	if got := pinReleases(t, templatePinOps, `{"templates": ["@a/x:1.0.0", 5]}`); len(got) != 0 {
		t.Errorf("template pins = %v, want none", got)
	}
}

// The last entry for a name declares it, so a later range entry means the
// template is not pinned.
func TestTemplateWorkspacePins_LastEntryWins(t *testing.T) {
	config := `{"templates": ["@a/x:1.0.0", "@a/x:^1", "@a/y:^1", "@a/y:2.0.0"]}`
	want := map[string]string{"@a/y": "2.0.0"}
	if got := pinReleases(t, templatePinOps, config); !maps.Equal(got, want) {
		t.Errorf("pins = %v, want %v", got, want)
	}
}

func TestTemplateWorkspacePins_OnlyExactReleases(t *testing.T) {
	config := `{"templates": ["@a/exact:1.2.3", "@a/caret:^1.0.0", "@a/bare", "@a/latest:latest"]}`
	want := map[string]string{"@a/exact": "1.2.3"}
	if got := pinReleases(t, templatePinOps, config); !maps.Equal(got, want) {
		t.Errorf("pins = %v, want %v", got, want)
	}
}

// A duplicate entry that declares a range is never turned into a pin.
func TestRewriteWorkspacePins_LeavesDuplicateRanges(t *testing.T) {
	got := rewrite(t, templatePinOps, `{"templates": ["@a/y:^1", "@a/y:2.0.0"]}`, map[string]string{"@a/y": "3.0.0"})
	if want := `{"templates": ["@a/y:^1", "@a/y:3.0.0"]}`; got != want {
		t.Errorf("templates rewrite = %s, want %s", got, want)
	}
	got = rewrite(t, extensionPinOps, `{"extensions": {"@a/x": "^1", "@a/x": "1.0.0"}}`, map[string]string{"@a/x": "2.0.0"})
	if want := `{"extensions": {"@a/x": "^1", "@a/x": "2.0.0"}}`; got != want {
		t.Errorf("extensions rewrite = %s, want %s", got, want)
	}
}

// When a name is declared more than once, the rewrite changes only the
// declaration the config reader keeps, and reads its release as the pin.
func TestRewriteWorkspacePins_RewritesOnlyTheEffectiveDeclaration(t *testing.T) {
	tests := []struct {
		name, config, want, pin string
		ops                     artifactOps
	}{
		{
			name:   "duplicate template entries",
			ops:    templatePinOps,
			config: `{"templates": ["@a/x:1.0.0", "@a/z:1.0.0", "@a/x:2.0.0"]}`,
			want:   `{"templates": ["@a/x:1.0.0", "@a/z:1.0.0", "@a/x:3.0.0"]}`,
			pin:    "2.0.0",
		},
		{
			name:   "a repeated identical template entry is dropped",
			ops:    templatePinOps,
			config: `{"templates": ["@a/x:1.0.0", "@a/x:2.0.0", "@a/x:1.0.0"]}`,
			want:   `{"templates": ["@a/x:1.0.0", "@a/x:3.0.0", "@a/x:1.0.0"]}`,
			pin:    "2.0.0",
		},
		{
			name:   "duplicate extension keys",
			ops:    extensionPinOps,
			config: `{"extensions": {"@a/x": "1.0.0", "@a/x": "2.0.0"}}`,
			want:   `{"extensions": {"@a/x": "1.0.0", "@a/x": "3.0.0"}}`,
			pin:    "2.0.0",
		},
		{
			name:   "a repeated member",
			ops:    extensionPinOps,
			config: `{"extensions": {"@a/x": "1.0.0"}, "extensions": {"@a/x": "2.0.0"}}`,
			want:   `{"extensions": {"@a/x": "1.0.0"}, "extensions": {"@a/x": "3.0.0"}}`,
			pin:    "2.0.0",
		},
		{
			name:   "a member spelled with other capitals",
			ops:    extensionPinOps,
			config: `{"extensions": {"@a/x": "1.0.0"}, "Extensions": {"@a/x": "2.0.0"}}`,
			want:   `{"extensions": {"@a/x": "1.0.0"}, "Extensions": {"@a/x": "3.0.0"}}`,
			pin:    "2.0.0",
		},
		{
			name:   "a capitalized templates member",
			ops:    templatePinOps,
			config: `{"Templates": ["@a/x:2.0.0"]}`,
			want:   `{"Templates": ["@a/x:3.0.0"]}`,
			pin:    "2.0.0",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pinReleases(t, tc.ops, tc.config)["@a/x"]; got != tc.pin {
				t.Errorf("pin = %q, want %q", got, tc.pin)
			}
			if got := rewrite(t, tc.ops, tc.config, map[string]string{"@a/x": "3.0.0"}); got != tc.want {
				t.Errorf("rewrite = %s, want %s", got, tc.want)
			}
		})
	}
}

// A rewrite that duplicates an earlier entry leaves another constraint
// effective, so rendering fails before the lock is written.
func TestRenderWorkspacePins_FailsWhenTheRewriteLeavesAnotherConstraint(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceFile(t, ws, `{"templates": ["@a/x:3.0.0", "@a/x:^1", "@a/x:1.0.0"]}`)
	_, err := renderWorkspacePins(ws, templatePinOps, map[string]string{"@a/x": "3.0.0"})
	if err == nil {
		t.Fatal("render succeeded, want the pin that cannot move")
	}
	for _, want := range []string{"across putnami.workspace.json and the global putnami config", "@a/x would declare ^1, not 3.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("render error %q does not contain %q", err, want)
		}
	}
}

// A workspace file that is JSON null declares nothing, as for the config
// reader, and the update runs.
func TestUpdateArtifacts_NullWorkspaceFile(t *testing.T) {
	ws := t.TempDir()
	path := writeWorkspaceFile(t, ws, "null\n")
	cfg := &wsproto.Config{}
	cfg.Extensions.List = map[string]string{"@a/one": "^1"}

	runPinUpdate(t, ws, cfg, newPinOps(extensionOps(ws), "1.5.0"), ArtifactUpdateOptions{}, "")

	if got := readFile(t, path); got != "null\n" {
		t.Errorf("%s = %q, want it untouched", wsproto.WorkspaceConfigFilename, got)
	}
}

// When the global putnami config repeats a workspace template entry, the
// config reader drops that entry, and a range the workspace declares stays
// effective: nothing is pinned, so nothing is rewritten.
func TestUpdateArtifacts_KeepsARangeTheGlobalDuplicateLeavesEffective(t *testing.T) {
	ws := t.TempDir()
	const config = `{"templates": ["@a/x:^1", "@a/x:1.0.0"]}`
	path := writeWorkspaceFile(t, ws, config)
	home := hometest.Temp(t)
	global := filepath.Join(home, wsproto.GlobalConfigDir, wsproto.GlobalConfigFilename)
	if err := os.MkdirAll(filepath.Dir(global), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(global, []byte(`{"templates": ["@a/x:1.0.0"]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	stdout, _ := runPinUpdate(t, ws, wsproto.Load(ws), newPinOps(templateOps(ws), "1.5.0"), ArtifactUpdateOptions{}, "")

	if got := readFile(t, path); got != config {
		t.Errorf("%s =\n%s\nwant it untouched", wsproto.WorkspaceConfigFilename, got)
	}
	if strings.Contains(stdout, "pin") {
		t.Errorf("a range was reported as a pin move:\n%s", stdout)
	}
}

// A failed workspace write lists each pin to set by hand.
func TestWriteWorkspacePins_FailureListsThePins(t *testing.T) {
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeWorkspacePins(notADir, []byte("{}"), map[string]string{"@a/x": "1.0.0"}, map[string]string{"@a/x": "2.0.0"})
	if err == nil {
		t.Fatal("writeWorkspacePins succeeded under a regular file")
	}
	for _, want := range []string{"putnami.lock.json already records the new releases", "@a/x 1.0.0 → 2.0.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not contain %q", err, want)
		}
	}
}

// A pin moves only to another release: the "0.0.0" fallback of a registry
// answer without a version, and another spelling of the same release, are not
// moves.
func TestMovesWorkspacePin(t *testing.T) {
	pins := map[string]string{"@a/one": "1.0.0", "@a/v": "v1.2.3", "@a/short": "1.2"}
	for _, tc := range []struct {
		name, declared, version string
		want                    bool
	}{
		{"@a/one", "1.0.0", "2.0.0", true},
		{"@a/one", "1.0.0", "1.0.0", false},
		{"@a/one", "1.0.0", "0.0.0", false},
		{"@a/v", "v1.2.3", "1.2.3", false},
		{"@a/v", "v1.2.3", "1.2.4", true},
		{"@a/short", "1.2", "1.2.0", false},
		{"@a/other", "1.0.0", "2.0.0", false},
		// The config reader keeps a range for the name: nothing is pinned.
		{"@a/one", "^1", "1.5.0", false},
	} {
		if got := movesWorkspacePin(pins, tc.name, tc.declared, tc.version); got != tc.want {
			t.Errorf("movesWorkspacePin(%s, %s, %s) = %v, want %v", tc.name, tc.declared, tc.version, got, tc.want)
		}
	}
}

// The rewrite changes the moved pin's bytes and nothing else: layout, number
// spelling, key order inside arrays and a same-named key elsewhere all stay.
func TestRewriteExtensionWorkspacePins_KeepsEveryOtherByte(t *testing.T) {
	const config = "{\n\t\"name\": \"ws\",\n\t\"includes\": [\"apps\", \"libs\"],\n" +
		"\t\"options\": {\"@a/one\": {\"@a/one\": \"1.0.0\", \"ratio\": 1.0, \"rules\": [{\"z\": 1, \"a\": 2}]}},\n" +
		"\t\"extensions\": {\"@a/one\": \"1.0.0\", \"@a/two\":\"1.0.0\", \"./local\": \"\"}\n}\n"
	got := rewrite(t, extensionPinOps, config, map[string]string{"@a/one": "2.0.0"})
	want := strings.Replace(config, "\"extensions\": {\"@a/one\": \"1.0.0\"", "\"extensions\": {\"@a/one\": \"2.0.0\"", 1)
	if got != want {
		t.Errorf("rewrite =\n%s\nwant\n%s", got, want)
	}
}

// newPinOps returns the real ops of a kind with an install that always
// resolves version, so a test exercises the real pin wiring without a
// registry.
func newPinOps(ops artifactOps, version string) artifactOps {
	ops.install = func(context.Context, string, string, *lockfile.LockEntry) (*artifactInstallOutcome, error) {
		return &artifactInstallOutcome{Version: version}, nil
	}
	ops.resolve = func(context.Context, string, string) (*artifactInstallOutcome, error) {
		return &artifactInstallOutcome{Version: version}, nil
	}
	ops.resolvePlatformIntegrity = nil
	return ops
}

// writeWorkspaceFile writes the workspace config and points the user home at
// an empty directory, so wsproto.Load reads no global putnami config.
func writeWorkspaceFile(t *testing.T, ws, config string) string {
	t.Helper()
	hometest.Temp(t)
	path := filepath.Join(ws, wsproto.WorkspaceConfigFilename)
	if err := os.WriteFile(path, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// runPinUpdate runs updateArtifacts against the workspace file and returns
// stdout and stderr.
func runPinUpdate(t *testing.T, ws string, cfg *wsproto.Config, ops artifactOps, opts ArtifactUpdateOptions, outputFormat string) (string, string) {
	t.Helper()
	var stdout string
	var runErr error
	stderr := sharedtest.CaptureStderr(t, func() {
		stdout, runErr = sharedtest.CaptureStdout(t, func() error {
			return updateArtifacts(context.Background(), ws, cfg, nil, ops, opts, outputFormat)
		})
	})
	if runErr != nil {
		t.Fatalf("updateArtifacts: %v", runErr)
	}
	return stdout, stderr
}

// A template update rewrites its "name:release" entry in place.
func TestUpdateArtifacts_MovesTemplateWorkspacePin(t *testing.T) {
	ws := t.TempDir()
	const config = `{
  "name": "ws",
  "templates": ["@a/bare", "@a/pinned:1.0.0"]
}
`
	path := writeWorkspaceFile(t, ws, config)
	ops := newPinOps(templateOps(ws), "2.0.0")

	runPinUpdate(t, ws, wsproto.Load(ws), ops, ArtifactUpdateOptions{ConstraintOverride: "2.0.0"}, "")

	if got, want := readFile(t, path), strings.Replace(config, "@a/pinned:1.0.0", "@a/pinned:2.0.0", 1); got != want {
		t.Errorf("%s =\n%s\nwant\n%s", wsproto.WorkspaceConfigFilename, got, want)
	}
}

// An update that moves nothing leaves the workspace file byte for byte.
func TestUpdateArtifacts_NoMoveLeavesWorkspaceFileUntouched(t *testing.T) {
	ws := t.TempDir()
	const config = "{\"name\": \"ws\", \"extensions\": {\"@a/one\": \"v1.0.0\"}}\n"
	path := writeWorkspaceFile(t, ws, config)
	ops := newPinOps(extensionOps(ws), "1.0.0")

	stdout, _ := runPinUpdate(t, ws, wsproto.Load(ws), ops, ArtifactUpdateOptions{}, "")

	if got := readFile(t, path); got != config {
		t.Errorf("a no-op update rewrote %s:\n%s", wsproto.WorkspaceConfigFilename, got)
	}
	if strings.Contains(stdout, "pin") {
		t.Errorf("a no-op update reported a pin move:\n%s", stdout)
	}
}

// A pin behind a lock that already records the new release is realigned: the
// lock is up to date, the pin moves, and the summary counts it.
func TestUpdateArtifacts_RealignsAPinTheLockAlreadyMoved(t *testing.T) {
	ws := t.TempDir()
	path := writeWorkspaceFile(t, ws, "{\"extensions\": {\"@a/one\": \"1.0.0\"}}\n")
	lf := lockfile.NewLockFile()
	lf.SetExtension("@a/one", lockfile.LockEntry{Version: "2.0.0"})
	if err := lockfile.WriteLockFile(ws, lf); err != nil {
		t.Fatal(err)
	}
	ops := newPinOps(extensionOps(ws), "2.0.0")

	stdout, _ := runPinUpdate(t, ws, wsproto.Load(ws), ops, ArtifactUpdateOptions{ConstraintOverride: "2.0.0"}, "")

	if got := readFile(t, path); got != "{\"extensions\": {\"@a/one\": \"2.0.0\"}}\n" {
		t.Errorf("%s = %s, want the pin at 2.0.0", wsproto.WorkspaceConfigFilename, got)
	}
	if !strings.Contains(stdout, "0 updated, 1 pins moved in putnami.workspace.json") {
		t.Errorf("summary does not count the pin move:\n%s", stdout)
	}
}

// The jsonl events name the pin they replace, on the real run and the preview.
func TestUpdateArtifacts_JSONLNamesTheReplacedPin(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		ws := t.TempDir()
		writeWorkspaceFile(t, ws, "{\"extensions\": {\"@a/one\": \"1.0.0\"}}\n")
		ops := newPinOps(extensionOps(ws), "2.0.0")

		stdout, _ := runPinUpdate(t, ws, wsproto.Load(ws), ops, ArtifactUpdateOptions{ConstraintOverride: "2.0.0", DryRun: dryRun}, "jsonl")

		var event artifactActionEntry
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &event); err != nil {
			t.Fatalf("dryRun=%v: parse %q: %v", dryRun, stdout, err)
		}
		if event.ReplacesPin != "1.0.0" || event.Version != "2.0.0" {
			t.Errorf("dryRun=%v: event = %+v, want replacesPin 1.0.0 and version 2.0.0", dryRun, event)
		}
	}
}

// A pin only the global putnami config declares is not rewritten, and the
// update says so.
func TestUpdateArtifacts_WarnsAboutAGlobalPin(t *testing.T) {
	ws := t.TempDir()
	const config = "{\"name\": \"ws\"}\n"
	path := writeWorkspaceFile(t, ws, config)
	cfg := &wsproto.Config{}
	cfg.Extensions.List = map[string]string{"@a/global": "1.0.0"}
	ops := newPinOps(extensionOps(ws), "2.0.0")

	_, stderr := runPinUpdate(t, ws, cfg, ops, ArtifactUpdateOptions{ConstraintOverride: "2.0.0"}, "")

	if got := readFile(t, path); got != config {
		t.Errorf("a global pin was written to %s:\n%s", wsproto.WorkspaceConfigFilename, got)
	}
	if !strings.Contains(stderr, "@a/global is pinned to 1.0.0 outside putnami.workspace.json (in the global putnami config); the lock now records 2.0.0") {
		t.Errorf("no warning about the global pin: %q", stderr)
	}

	_, stderr = runPinUpdate(t, ws, cfg, ops, ArtifactUpdateOptions{ConstraintOverride: "2.0.0", DryRun: true}, "")
	if !strings.Contains(stderr, "the update would record 2.0.0") {
		t.Errorf("dry-run warning does not use the conditional: %q", stderr)
	}

	for _, dryRun := range []bool{false, true} {
		stdout, _ := runPinUpdate(t, ws, cfg, ops, ArtifactUpdateOptions{ConstraintOverride: "2.0.0", DryRun: dryRun}, "jsonl")
		var event artifactActionEntry
		if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &event); err != nil {
			t.Fatalf("dryRun=%v: parse %q: %v", dryRun, stdout, err)
		}
		if event.GlobalPin != "1.0.0" || event.ReplacesPin != "" {
			t.Errorf("dryRun=%v: event = %+v, want globalPin 1.0.0 and no replacesPin", dryRun, event)
		}
	}
}
