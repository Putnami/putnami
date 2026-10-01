package migrate

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/commands/shared"
	"go.putnami.dev/tooling/cli/internal/commands/sharedtest"
	"go.putnami.dev/tooling/cli/internal/launch"
	"go.putnami.dev/tooling/cli/internal/layout"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// `putnami migrate vnext` is the explicit path to the current lock format AND
// its test harness: these tests prove migration is exercised rather than merely
// declared.
//
// DUAL-READ, CONVERTED at B6c: a v1 lock is now refused by every reader except
// this command's (lockfile.ReadMigratableLockFile), so the cases that start
// from one prove two things at once — that ordinary access hard-errors, and
// that the migration still converts past it. The compatibility matrix they lean
// on lives in internal/lockfile/lockfile_v2_test.go.
//
// DELETION DEFERRED past B7a, which deleted the rest of the bridge.
// This command IS the bridge for v1 locks, and no RELEASED build has carried it
// yet: consumer repositories still hold v1 locks, so removing the conversion
// before a published CLI can perform it would strand them on a lock no released
// build can load or migrate. The gate is the first released build carrying this
// command, not a slice number.

// v2ManifestJSON is a manifest with no `declares` block: a v2 task contract.
const v2ManifestJSON = `{
  "name": "@putnami/go",
  "version": "2.0.0",
  "cliContract": 4,
  "commands": { "build": { "run": [{ "id": "build", "task": "build-exec" }] } },
  "tasks": { "build-exec": { "kind": "command", "command": "echo", "args": ["built"] } }
}`

// v3ManifestJSON carries a `declares` block, which is how a manifest
// self-identifies as task contract v3.
const v3ManifestJSON = `{
  "name": "@putnami/typescript",
  "version": "3.1.0",
  "cliContract": 4,
  "commands": { "build": { "run": [{ "id": "build", "task": "build-exec" }] } },
  "tasks": {
    "build-exec": {
      "kind": "command",
      "command": "echo",
      "args": ["built"],
      "declares": {
        "outputs": { "dist": { "kind": "directory", "root": "project", "path": "dist" } }
      }
    }
  }
}`

// seedInstalledExtension writes a manifest where the migration probes for it:
// the stable symlink directory an install materializes.
func seedInstalledExtension(t *testing.T, root, name, manifest string) {
	t.Helper()
	dir := layout.StableDir(root, layout.Extensions, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest for %s: %v", name, err)
	}
}

// seedV1Lock writes a v1 lock pinning the given extensions. The version is set
// explicitly because NewLockFile now defaults to v2 (slice B6c): a v1 lock is
// something a workspace ARRIVES with, not something this CLI creates.
func seedV1Lock(t *testing.T, root string, extensions map[string]lockfile.LockEntry) {
	t.Helper()
	lf := lockfile.NewLockFile()
	lf.Version = lockfile.FormatVersionV1
	for name, entry := range extensions {
		lf.SetExtension(name, entry)
	}
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}
	if _, err := lockfile.ReadLockFile(root); err == nil {
		t.Fatal("the seeded lock must be v1, which an ordinary reader refuses")
	}
}

func readLock(t *testing.T, root string) *lockfile.LockFile {
	t.Helper()
	lf, err := lockfile.ReadLockFile(root)
	if err != nil {
		t.Fatalf("ReadLockFile: %v", err)
	}
	if lf == nil {
		t.Fatal("no lock file")
	}
	return lf
}

// readMigratableLock is readLock for a lock that has not been migrated yet: the
// only reader allowed below the format floor is the migration's own.
func readMigratableLock(t *testing.T, root string) *lockfile.LockFile {
	t.Helper()
	lf, err := lockfile.ReadMigratableLockFile(root)
	if err != nil {
		t.Fatalf("ReadMigratableLockFile: %v", err)
	}
	if lf == nil {
		t.Fatal("no lock file")
	}
	return lf
}

// TestMigrateVNextCheck_ReportsPendingMigration pins the CI gate: a v1 lock
// needs migrating, the check says so without writing, and the failure carries
// the usage exit code plus the command that fixes it.
func TestMigrateVNextCheck_ReportsPendingMigration(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "explicit-v4-migration", "vnext-check-and-apply-are-explicit-and-idempotent")
	root := t.TempDir()
	seedInstalledExtension(t, root, "@putnami/typescript", v3ManifestJSON)
	seedInstalledExtension(t, root, "@putnami/go", v2ManifestJSON)
	seedV1Lock(t, root, map[string]lockfile.LockEntry{
		"@putnami/typescript": {Version: "3.1.0"},
		"@putnami/go":         {Version: "2.0.0"},
	})
	before, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
	if err != nil {
		t.Fatalf("read seeded lock: %v", err)
	}

	out, runErr := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "") })
	if runErr == nil {
		t.Fatal("migrate vnext --check on a v1 lock must fail so CI can gate on it")
	}
	if !errors.Is(runErr, protocolcli.ErrInvalidConfig) {
		t.Errorf("check error class = %v, want ErrInvalidConfig", runErr)
	}
	if code := protocolcli.ExitCodeForError(runErr); code != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, protocolcli.ExitUsage)
	}
	if next := protocolcli.SuggestedNext(runErr); next != "putnami migrate vnext --apply" {
		t.Errorf("suggested next = %q, want the apply command", next)
	}

	report, ok := shared.ResultData(runErr).(VNextReport)
	if !ok {
		t.Fatalf("check failure carried no VNextReport (data = %#v)", shared.ResultData(runErr))
	}
	if report.Outcome != VNextOutcomeMigrate {
		t.Errorf("outcome = %q, want %q", report.Outcome, VNextOutcomeMigrate)
	}
	if report.LockVersion != lockfile.FormatVersionV1 || report.TargetVersion != lockfile.FormatVersionV4 {
		t.Errorf("versions = %d → %d, want 1 → 4", report.LockVersion, report.TargetVersion)
	}
	assertChanges(t, report.Changes, []VNextChange{
		{Kind: VNextChangeLockVersion, From: 1, To: 4},
		{Kind: VNextChangeTaskContract, Extension: "@putnami/go", From: 0, To: 2},
		{Kind: VNextChangeTaskContract, Extension: "@putnami/typescript", From: 0, To: 3},
	})

	if !strings.Contains(out, "Pending 3 change(s)") {
		t.Errorf("human output does not summarize the pending changes:\n%s", out)
	}

	after, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
	if err != nil {
		t.Fatalf("re-read lock: %v", err)
	}
	if string(after) != string(before) {
		t.Error("--check wrote to the lock file; it must be read-only")
	}
}

// TestMigrateVNextApply_ConvertsAndIsIdempotent is the migration's core
// contract: apply records exactly what the check planned, a re-check is clean,
// and a second apply changes nothing on disk.
func TestMigrateVNextApply_ConvertsAndIsIdempotent(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "explicit-v4-migration", "vnext-check-and-apply-are-explicit-and-idempotent")
	root := t.TempDir()
	seedInstalledExtension(t, root, "@putnami/typescript", v3ManifestJSON)
	seedInstalledExtension(t, root, "@putnami/go", v2ManifestJSON)
	seedV1Lock(t, root, map[string]lockfile.LockEntry{
		"@putnami/typescript": {Version: "3.1.0", ManifestHash: "tsmanifest"},
		"@putnami/go":         {Version: "2.0.0"},
	})

	if _, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, true, "") }); err != nil {
		t.Fatalf("migrate vnext --apply: %v", err)
	}

	migrated := readLock(t, root)
	if migrated.Version != lockfile.FormatVersionV4 {
		t.Fatalf("lock version after apply = %d, want %d", migrated.Version, lockfile.FormatVersionV4)
	}
	if migrated.AgentArtifacts == nil || migrated.Toolchains == nil {
		t.Fatalf("current-format dimensions were not initialized: %+v", migrated)
	}
	ts, _ := migrated.GetExtension("@putnami/typescript")
	if ts.TaskContract != 3 {
		t.Errorf("typescript task contract = %d, want 3", ts.TaskContract)
	}
	if ts.Version != "3.1.0" || ts.ManifestHash != "tsmanifest" {
		t.Errorf("apply changed a pin it must not touch: %+v", ts)
	}
	if goEntry, _ := migrated.GetExtension("@putnami/go"); goEntry.TaskContract != 2 {
		t.Errorf("go task contract = %d, want 2", goEntry.TaskContract)
	}

	firstBytes, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
	if err != nil {
		t.Fatalf("read migrated lock: %v", err)
	}

	// Idempotent: a re-check is clean and exits 0.
	out, checkErr := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "") })
	if checkErr != nil {
		t.Errorf("re-check after apply = %v, want nil (exit 0)", checkErr)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("clean check does not say so:\n%s", out)
	}

	// Idempotent: a second apply produces the same bytes.
	if _, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, true, "") }); err != nil {
		t.Fatalf("second migrate vnext --apply: %v", err)
	}
	secondBytes, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
	if err != nil {
		t.Fatalf("read lock after second apply: %v", err)
	}
	if string(secondBytes) != string(firstBytes) {
		t.Errorf("second apply rewrote the lock.\n--- first ---\n%s\n--- second ---\n%s", firstBytes, secondBytes)
	}
}

// A v2 or v3 lock is already readable, but it still cannot carry an
// agentArtifacts pin. Migration must be explicit, preserve every existing pin,
// initialize the v4 dimensions, and remain byte-idempotent.
func TestMigrateVNext_V2AndV3ProjectToV4WithoutChangingPins(t *testing.T) {
	spectest.Proves(t, "cli/agent-workflow-lifecycle", "explicit-v4-migration", "vnext-check-and-apply-are-explicit-and-idempotent")
	for _, version := range []int{lockfile.FormatVersionV2, lockfile.FormatVersionV3} {
		t.Run(fmt.Sprintf("v%d", version), func(t *testing.T) {
			root := t.TempDir()
			lf := &lockfile.LockFile{
				Version: version,
				CLI: &lockfile.LockEntry{
					Version:     "0.9.0",
					Integrities: map[string]string{"linux/amd64": "cli-digest"},
					Source:      "https://registry.example/cli",
				},
				Extensions: map[string]lockfile.LockEntry{
					"@putnami/go": {
						Version:      "1.2.3",
						Integrities:  map[string]string{"linux/amd64": "extension-digest"},
						ManifestHash: "extension-manifest",
						TaskContract: 3,
					},
				},
				Templates: map[string]lockfile.LockEntry{
					"go-server": {Version: "2.0.0", Integrity: "template-digest"},
				},
			}
			if version >= lockfile.FormatVersionV3 {
				lf.Toolchains = map[string]lockfile.LockEntry{
					"go": {Version: "1.25.7", Source: "https://registry.example/go"},
				}
			}
			if err := lockfile.WriteLockFile(root, lf); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
			if err != nil {
				t.Fatal(err)
			}

			_, checkErr := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "") })
			if checkErr == nil {
				t.Fatalf("v%d must report an explicit migration to v4", version)
			}
			report, ok := shared.ResultData(checkErr).(VNextReport)
			if !ok {
				t.Fatalf("check carried no report: %#v", shared.ResultData(checkErr))
			}
			assertChanges(t, report.Changes, []VNextChange{{
				Kind: VNextChangeLockVersion, From: version, To: lockfile.FormatVersionV4,
			}})
			afterCheck, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, afterCheck) {
				t.Fatal("--check changed the legacy lock")
			}

			if _, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, true, "") }); err != nil {
				t.Fatalf("apply: %v", err)
			}
			migrated := readLock(t, root)
			if migrated.Version != lockfile.FormatVersionV4 || migrated.AgentArtifacts == nil || migrated.Toolchains == nil {
				t.Fatalf("migrated dimensions = %+v", migrated)
			}
			if got, _ := migrated.GetExtension("@putnami/go"); got.Version != "1.2.3" || got.ManifestHash != "extension-manifest" || got.TaskContract != 3 {
				t.Errorf("extension pin changed: %+v", got)
			}
			if got, _ := migrated.GetTemplate("go-server"); got.Version != "2.0.0" || got.Integrity != "template-digest" {
				t.Errorf("template pin changed: %+v", got)
			}
			if version == lockfile.FormatVersionV3 {
				if got, ok := migrated.Toolchains["go"]; !ok || got.Version != "1.25.7" {
					t.Errorf("toolchain pin changed: %+v", migrated.Toolchains)
				}
			}

			first, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, true, "") }); err != nil {
				t.Fatalf("second apply: %v", err)
			}
			second, err := os.ReadFile(filepath.Join(root, lockfile.LockFilename))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(first, second) {
				t.Fatal("second apply changed migrated lock bytes")
			}
		})
	}
}

// TestMigrateVNext_IsTheOnlyReaderOfAV1Lock is the converted dual-read case
// (slice B6c). Every other reader refuses a v1 lock with an actionable error;
// this command reads it, converts it, and leaves a file the ordinary readers
// then accept. Without that asymmetry a v1 workspace would be wedged: the fix
// would be refused by the same rule that makes it necessary.
func TestMigrateVNext_IsTheOnlyReaderOfAV1Lock(t *testing.T) {
	root := t.TempDir()
	seedInstalledExtension(t, root, "@putnami/typescript", v3ManifestJSON)
	seedV1Lock(t, root, map[string]lockfile.LockEntry{"@putnami/typescript": {Version: "3.1.0"}})

	// The ordinary reader refuses, and says which command to run.
	_, err := lockfile.ReadLockFile(root)
	var outdated *lockfile.OutdatedVersionError
	if !errors.As(err, &outdated) {
		t.Fatalf("ReadLockFile(v1) = %v, want *OutdatedVersionError", err)
	}
	if !strings.Contains(outdated.Error(), "putnami migrate vnext --apply") {
		t.Errorf("the refusal must name its own fix, got %q", outdated.Error())
	}

	// That command reads the same file and converts it.
	if _, applyErr := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, true, "") }); applyErr != nil {
		t.Fatalf("migrate vnext --apply on a v1 lock: %v", applyErr)
	}
	migrated := readLock(t, root)
	if migrated.Version != lockfile.FormatVersionV4 {
		t.Fatalf("lock version after apply = %d, want %d", migrated.Version, lockfile.FormatVersionV4)
	}
}

// TestMigrateVNext_IsLauncherExempt resolves wave-1 finding F3. Relaunching
// into a workspace-pinned CLI READS the lock to find the pin, and a v1 lock no
// longer parses — so a non-exempt `migrate vnext` would fail on exactly the
// file it exists to convert.
func TestMigrateVNext_IsLauncherExempt(t *testing.T) {
	if !launch.IsExemptInvocation([]string{"migrate", "vnext"}) {
		t.Error("`migrate vnext` must not relaunch: it is the recovery path for an unreadable lock")
	}
	if !launch.IsExemptInvocation([]string{"migrate", "--json", "vnext"}) {
		t.Error("the exemption must survive a leading boolean flag, like `version use`'s does")
	}
	if launch.IsExemptInvocation([]string{"migrate", "something-else"}) {
		t.Error("only `migrate vnext` is exempt; another migration has no claim on the escape hatch")
	}
}

// TestMigrateVNextCheck_CleanLockExitsZero pins the gate's success side: a lock
// already at the current format whose records match installed manifests is clean.
func TestMigrateVNextCheck_CleanLockExitsZero(t *testing.T) {
	root := t.TempDir()
	seedInstalledExtension(t, root, "@putnami/typescript", v3ManifestJSON)

	lf := lockfile.NewLockFile()
	lf.SetExtension("@putnami/typescript", lockfile.LockEntry{Version: "3.1.0", TaskContract: 3})
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatalf("seed current lock: %v", err)
	}

	if _, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "") }); err != nil {
		t.Errorf("clean check = %v, want nil", err)
	}
}

// TestMigrateVNextCheck_StaleRecordIsAChange covers the drift the epic names
// explicitly: a manifest that moved to v3 while the lock still records v2.
func TestMigrateVNextCheck_StaleRecordIsAChange(t *testing.T) {
	root := t.TempDir()
	seedInstalledExtension(t, root, "@putnami/typescript", v3ManifestJSON)

	lf := lockfile.NewLockFile()
	lf.SetExtension("@putnami/typescript", lockfile.LockEntry{Version: "3.1.0", TaskContract: 2})
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatalf("seed current lock: %v", err)
	}

	_, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "") })
	if err == nil {
		t.Fatal("a stale contract record must fail the check")
	}
	report, ok := shared.ResultData(err).(VNextReport)
	if !ok {
		t.Fatalf("no report attached: %#v", shared.ResultData(err))
	}
	assertChanges(t, report.Changes, []VNextChange{
		{Kind: VNextChangeTaskContract, Extension: "@putnami/typescript", From: 2, To: 3},
	})
}

// TestMigrateVNextDrift_UnresolvableManifestIsReportedNotGuessed pins the
// fail-closed half: an extension whose manifest is not installed gets NO
// contract record and NO invented one, and its absence alone does not fail a
// check — installing it is the fix, not migrating the lock.
func TestMigrateVNextDrift_UnresolvableManifestIsReportedNotGuessed(t *testing.T) {
	root := t.TempDir()
	seedInstalledExtension(t, root, "@putnami/broken", `{"name":"@putnami/broken","cliContract":999}`)

	lf := lockfile.NewLockFile()
	lf.SetExtension("@putnami/missing", lockfile.LockEntry{Version: "1.0.0"})
	lf.SetExtension("@putnami/broken", lockfile.LockEntry{Version: "1.0.0"})
	if err := lockfile.WriteLockFile(root, lf); err != nil {
		t.Fatalf("seed current lock: %v", err)
	}

	out, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "") })
	if err != nil {
		t.Fatalf("unreadable manifests must not fail the check: %v", err)
	}
	if !strings.Contains(out, "Not recorded:") {
		t.Errorf("human output does not report the unresolved entries:\n%s", out)
	}

	report := planVNextMigration(root, readLock(t, root))
	if report.Outcome != VNextOutcomeClean {
		t.Errorf("outcome = %q, want %q", report.Outcome, VNextOutcomeClean)
	}
	if len(report.Drift) != 2 {
		t.Fatalf("drift = %+v, want two entries", report.Drift)
	}
	if report.Drift[0].Extension != "@putnami/broken" || report.Drift[0].Reason != VNextDriftManifestUnreadable {
		t.Errorf("drift[0] = %+v, want @putnami/broken manifestUnreadable", report.Drift[0])
	}
	if report.Drift[1].Extension != "@putnami/missing" || report.Drift[1].Reason != VNextDriftManifestUnavailable {
		t.Errorf("drift[1] = %+v, want @putnami/missing manifestUnavailable", report.Drift[1])
	}

	// Applying must not invent a record for either of them.
	if _, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, true, "") }); err != nil {
		t.Fatalf("apply: %v", err)
	}
	after := readLock(t, root)
	for _, name := range []string{"@putnami/missing", "@putnami/broken"} {
		if entry, _ := after.GetExtension(name); entry.TaskContract != 0 {
			t.Errorf("%s got an invented contract record: %d", name, entry.TaskContract)
		}
	}
}

// TestMigrateVNext_NoLockFile keeps the empty case from looking clean: a
// missing lock is a not-found failure naming what to run, not a silent exit 0.
func TestMigrateVNext_NoLockFile(t *testing.T) {
	root := t.TempDir()
	_, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "") })
	if !errors.Is(err, protocolcli.ErrNotFound) {
		t.Fatalf("error = %v, want ErrNotFound", err)
	}
	if code := protocolcli.ExitCodeForError(err); code != protocolcli.ExitUsage {
		t.Errorf("exit code = %d, want %d", code, protocolcli.ExitUsage)
	}
}

// TestMigrateVNext_UnreadableLockIsClassified pins the versioned-error path
// through the command: a lock from a newer CLI must surface as an invalid-config
// failure with the version numbers, not as an unclassified crash.
func TestMigrateVNext_UnreadableLockIsClassified(t *testing.T) {
	root := t.TempDir()
	seed := `{"version": 99, "extensions": {}, "templates": {}}`
	if err := os.WriteFile(filepath.Join(root, lockfile.LockFilename), []byte(seed), 0o644); err != nil {
		t.Fatalf("seed lock file: %v", err)
	}

	_, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "") })
	if !errors.Is(err, protocolcli.ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
	var versionErr *lockfile.UnsupportedVersionError
	if !errors.As(err, &versionErr) {
		t.Fatalf("error = %v, want an *UnsupportedVersionError in the chain", err)
	}
}

// TestMigrateVNext_StructuredOutput pins the --output contract: one Result
// envelope per invocation on stdout, success or failure, with the report in
// data — the shape sibling structured commands emit.
func TestMigrateVNext_StructuredOutput(t *testing.T) {
	root := t.TempDir()
	seedInstalledExtension(t, root, "@putnami/typescript", v3ManifestJSON)
	seedV1Lock(t, root, map[string]lockfile.LockEntry{"@putnami/typescript": {Version: "3.1.0"}})

	// Check mode failing: the handler writes nothing and hands the dispatcher a
	// report-carrying error, exactly like `contracts check`.
	out, err := sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, false, "jsonl") })
	if err == nil {
		t.Fatal("expected a pending-migration failure")
	}
	if strings.TrimSpace(out) != "" {
		t.Errorf("check failure wrote to the machine stream instead of leaving it to the dispatcher:\n%s", out)
	}
	if _, ok := shared.ResultData(err).(VNextReport); !ok {
		t.Errorf("failure carries no report for the envelope: %#v", shared.ResultData(err))
	}

	// Apply mode: a single success envelope.
	out, err = sharedtest.CaptureStdout(t, func() error { return MigrateVNext(root, true, "jsonl") })
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 1 {
		t.Fatalf("structured apply wrote %d lines, want exactly 1:\n%s", len(lines), out)
	}
	var envelope struct {
		Command  string      `json:"command"`
		Status   string      `json:"status"`
		ExitCode int         `json:"exitCode"`
		Data     VNextReport `json:"data"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &envelope); err != nil {
		t.Fatalf("parse result envelope %q: %v", lines[0], err)
	}
	if envelope.Command != "migrate vnext" || envelope.Status != "success" || envelope.ExitCode != 0 {
		t.Errorf("envelope = %+v, want a successful `migrate vnext`", envelope)
	}
	if envelope.Data.Outcome != VNextOutcomeApplied {
		t.Errorf("data.outcome = %q, want %q", envelope.Data.Outcome, VNextOutcomeApplied)
	}
	if envelope.Data.LockVersion != lockfile.FormatVersionV1 || envelope.Data.TargetVersion != lockfile.FormatVersionV4 {
		t.Errorf("data versions = %d → %d, want 1 → 4", envelope.Data.LockVersion, envelope.Data.TargetVersion)
	}
}

// TestMigrateVNext_ProbesTheVersionedArtifactDirectory covers the second probe
// location: an install that left no stable symlink still has its manifest under
// the version-qualified artifact directory.
func TestMigrateVNext_ProbesTheVersionedArtifactDirectory(t *testing.T) {
	root := t.TempDir()
	dir := layout.ArtifactDir(root, layout.Extensions, "@putnami/typescript", "3.1.0")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "putnami.extension.json"), []byte(v3ManifestJSON), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	seedV1Lock(t, root, map[string]lockfile.LockEntry{"@putnami/typescript": {Version: "3.1.0"}})

	report := planVNextMigration(root, readMigratableLock(t, root))
	assertChanges(t, report.Changes, []VNextChange{
		{Kind: VNextChangeLockVersion, From: 1, To: 4},
		{Kind: VNextChangeTaskContract, Extension: "@putnami/typescript", From: 0, To: 3},
	})
}

func assertChanges(t *testing.T, got, want []VNextChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("changes = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("changes[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
