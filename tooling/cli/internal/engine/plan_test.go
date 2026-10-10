package engine

import (
	"errors"
	"strings"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

func TestMissingRegistryExtensions(t *testing.T) {
	t.Parallel()
	loaded := []*extension.ExtensionDescription{
		{Name: "@putnami/go"},
		{Name: "@putnami/typescript"},
	}

	tests := []struct {
		name string
		cfg  *wsproto.Config
		ext  []*extension.ExtensionDescription
		want []string
	}{
		{name: "nil config", cfg: nil, want: nil},
		{
			name: "all loaded",
			cfg: &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
				"@putnami/go": "", "@putnami/typescript": "",
			}}},
			ext:  loaded,
			want: nil,
		},
		{
			name: "registry extension missing",
			cfg: &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
				"@putnami/go": "", "@putnami/ci": "",
			}}},
			ext:  loaded,
			want: []string{"@putnami/ci"},
		},
		{
			name: "missing but disabled is ignored",
			cfg: &wsproto.Config{
				Extensions: wsproto.ExtensionsConfig{List: map[string]string{"@putnami/ci": ""}},
				Disable:    &wsproto.DisableConfig{Extensions: []string{"@putnami/ci"}},
			},
			ext:  loaded,
			want: nil,
		},
		{
			name: "local refs are not registry refs",
			cfg: &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
				"/go/extension": "", "./local": "",
			}}},
			ext:  nil,
			want: nil,
		},
		{
			name: "missing reported sorted",
			cfg: &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
				"@putnami/typescript": "", "@putnami/ci": "", "@putnami/cloud": "",
			}}},
			ext:  loaded,
			want: []string{"@putnami/ci", "@putnami/cloud"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := missingRegistryExtensions(tc.cfg, tc.ext)
			if len(got) != len(tc.want) {
				t.Fatalf("missingRegistryExtensions = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("missingRegistryExtensions = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestSelectedCommandsWithoutJobs(t *testing.T) {
	t.Parallel()
	// A loaded extension declaring "build" and "test" (but no project-matched
	// jobs in `planned` unless we put them there).
	goExt := &extension.ExtensionDescription{
		Name: "@putnami/go",
		Jobs: map[string]*extension.JobDefinition{
			"build": {Name: "build", ExtensionName: "@putnami/go"},
			"test":  {Name: "test", ExtensionName: "@putnami/go"},
		},
	}
	buildJob := &jobs.ScheduledJob{JobDef: &extension.JobDefinition{CommandName: "build"}}

	tests := []struct {
		name     string
		commands []string
		planned  []*jobs.ScheduledJob
		ext      []*extension.ExtensionDescription
		want     []string
	}{
		{
			name:     "selected command with no provider and no jobs is starved",
			commands: []string{"build", "deploy"},
			planned:  []*jobs.ScheduledJob{buildJob},
			ext:      []*extension.ExtensionDescription{goExt},
			want:     []string{"deploy"},
		},
		{
			name:     "all selected commands produced jobs",
			commands: []string{"build"},
			planned:  []*jobs.ScheduledJob{buildJob},
			ext:      []*extension.ExtensionDescription{goExt},
			want:     nil,
		},
		{
			name: "command a loaded extension declares is a legit no-op even with zero jobs",
			// "test" is selected and produced no jobs, but a loaded extension
			// declares it — this is a project that simply doesn't activate test,
			// not a missing provider, so it must not be flagged.
			commands: []string{"test"},
			planned:  []*jobs.ScheduledJob{buildJob},
			ext:      []*extension.ExtensionDescription{goExt},
			want:     nil,
		},
		{
			name:     "unselected missing-provider command is ignored",
			commands: []string{"build"},
			planned:  []*jobs.ScheduledJob{buildJob},
			ext:      []*extension.ExtensionDescription{goExt},
			want:     nil,
		},
		{
			name:     "multiple starved commands are sorted and deduped",
			commands: []string{"deploy", "package", "deploy"},
			planned:  []*jobs.ScheduledJob{buildJob},
			ext:      []*extension.ExtensionDescription{goExt},
			want:     []string{"deploy", "package"},
		},
		{
			name: "expanded pipeline step counts toward its base command",
			// A "build~transpile" step rolls up to the "build" command via
			// CommandName(), so build is considered served even without a loaded
			// extension declaring it.
			commands: []string{"build"},
			planned:  []*jobs.ScheduledJob{{JobDef: &extension.JobDefinition{Name: "build~transpile"}}},
			ext:      nil,
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := selectedCommandsWithoutJobs(tc.commands, tc.planned, tc.ext)
			if len(got) != len(tc.want) {
				t.Fatalf("selectedCommandsWithoutJobs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("selectedCommandsWithoutJobs = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// buildPlanGuardFixture drives BuildPlan with one project that lists
// @putnami/go and one loaded @putnami/go extension declaring a no-activation
// "build" job, so the project always matches build but never deploy. The
// returned plan is a partial plan (build present, deploy absent), exercising
// the per-required-extension guard rather than the zero-jobs guard.
//
// skipped, when given, is discovery's skip-record set — what the guard reads to
// tell "not installed" from "installed but not loadable".
func buildPlanGuardFixture(t *testing.T, commands []string, cfg *wsproto.Config, skipped ...extension.SkippedExtension) ([]*jobs.ScheduledJob, int) {
	t.Helper()
	wsRoot := t.TempDir()
	proj := &workspace.Project{
		ID:         "/proj",
		Name:       "proj",
		Path:       "proj",
		Extensions: []string{"@putnami/go"},
	}
	ws := workspace.NewWorkspace(wsRoot, nil, []*workspace.Project{proj})
	goExt := &extension.ExtensionDescription{
		Name: "@putnami/go",
		Jobs: map[string]*extension.JobDefinition{
			"build": {Name: "build", ExtensionName: "@putnami/go", Command: "/bin/true"},
		},
	}
	req := &Request{WorkspaceRoot: wsRoot, Config: cfg, Commands: commands}
	return buildPlan(req, ws, []*workspace.Project{proj},
		[]*extension.ExtensionDescription{goExt},
		&extension.DiscoveryResult{Extensions: []*extension.ExtensionDescription{goExt}, Skipped: skipped})
}

// TestBuildPlan_PartialPlanMissingSelectedCommandExtensionErrors is the core
// missing-extension case: build jobs planned, but the selected "deploy" command's
// configured registry extension (@putnami/cloud) failed to load, so deploy
// silently produced nothing. The run must fail rather than report success.
func TestBuildPlan_PartialPlanMissingSelectedCommandExtensionErrors(t *testing.T) {
	t.Parallel()
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@putnami/go":    "",
		"@putnami/cloud": "", // configured but not loaded → missing
	}}}
	planned, code := buildPlanGuardFixture(t, []string{"build", "deploy"}, cfg)
	if code != ExitError {
		t.Fatalf("buildPlan code = %d, want ExitError (%d); planned=%d", code, ExitError, len(planned))
	}
	if planned != nil {
		t.Fatalf("planned = %v, want nil when the guard trips", planned)
	}
}

// belowContractSkip is the skip record the loader produces for an extension
// that is installed but stamped below the current CLI contract — the shape the
// require-v3 flip made common. Its Reason already names the
// remedy; the guard's job is to let the user see it.
func belowContractSkip(name string) extension.SkippedExtension {
	return extension.SkippedExtension{
		Ref:  name,
		Name: name,
		Path: "/ws/.putnami/extensions/" + name,
		Reason: errors.New("extension manifest putnami.extension.json declares CLI contract 2 but this putnami requires 3: " +
			"re-package the extension with putnami 3 (or run `putnami extensions update` to pull a build that has been)"),
	}
}

// TestBuildPlan_MissingExtensionGuardSurfacesSkipReason pins the B6r/F3 repair:
// when a missing extension has a discovery skip record, the guard prints the
// record's Reason WITHOUT --debug. Before the repair the only advice printed
// was `putnami install`, which for a below-contract extension re-installs the
// same unloadable bytes; the reason (and the `putnami extensions update` it
// names) was debug-only.
func TestBuildPlan_MissingExtensionGuardSurfacesSkipReason(t *testing.T) {
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@putnami/go":    "",
		"@putnami/cloud": "",
	}}}

	var code int
	stderr := captureStderr(t, func() {
		_, code = buildPlanGuardFixture(t, []string{"build", "deploy"}, cfg, belowContractSkip("@putnami/cloud"))
	})
	if code != ExitError {
		t.Fatalf("buildPlan code = %d, want ExitError (%d)", code, ExitError)
	}
	if !strings.Contains(stderr, "@putnami/cloud was found but not loaded") {
		t.Errorf("guard did not attribute the miss to the skip record:\n%s", stderr)
	}
	if !strings.Contains(stderr, "putnami extensions update") {
		t.Errorf("guard did not surface the skip reason's remedy:\n%s", stderr)
	}
	// The install hint is the WRONG advice here: every missing extension was
	// explained, so it must not be printed at all.
	if strings.Contains(stderr, "run `putnami install`") {
		t.Errorf("guard still advised `putnami install` for an explained skip:\n%s", stderr)
	}
}

// TestBuildPlan_MissingExtensionGuardKeepsInstallHintWhenUnexplained is the
// other arm: an extension with no skip record is genuinely absent, and
// `putnami install` is still the remedy.
func TestBuildPlan_MissingExtensionGuardKeepsInstallHintWhenUnexplained(t *testing.T) {
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@putnami/go":    "",
		"@putnami/cloud": "",
	}}}

	var code int
	stderr := captureStderr(t, func() {
		_, code = buildPlanGuardFixture(t, []string{"build", "deploy"}, cfg)
	})
	if code != ExitError {
		t.Fatalf("buildPlan code = %d, want ExitError (%d)", code, ExitError)
	}
	if !strings.Contains(stderr, "run `putnami install`") {
		t.Errorf("an unexplained miss must keep the install hint:\n%s", stderr)
	}
	if strings.Contains(stderr, "was found but not loaded") {
		t.Errorf("no skip record was given, so nothing may be attributed:\n%s", stderr)
	}
}

// TestBuildPlan_ZeroJobsGuardSurfacesSkipReason covers the other guard site: a
// run that planned NOTHING takes an earlier return, and it must explain
// itself the same way.
func TestBuildPlan_ZeroJobsGuardSurfacesSkipReason(t *testing.T) {
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@putnami/go":    "",
		"@putnami/cloud": "",
	}}}

	var planned []*jobs.ScheduledJob
	var code int
	stderr := captureStderr(t, func() {
		planned, code = buildPlanGuardFixture(t, []string{"deploy"}, cfg, belowContractSkip("@putnami/cloud"))
	})
	if code != ExitError || len(planned) != 0 {
		t.Fatalf("buildPlan = %d jobs, code %d; want 0 jobs and ExitError (%d)", len(planned), code, ExitError)
	}
	if !strings.Contains(stderr, "putnami extensions update") {
		t.Errorf("zero-jobs guard did not surface the skip reason:\n%s", stderr)
	}
}

// TestBuildPlan_PartialPlanMissingUnselectedCommandExtensionSucceeds proves the
// guard does not fire for a missing extension whose command was not selected:
// running only "build" while @putnami/cloud (deploy) is missing is a healthy run.
func TestBuildPlan_PartialPlanMissingUnselectedCommandExtensionSucceeds(t *testing.T) {
	t.Parallel()
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@putnami/go":    "",
		"@putnami/cloud": "",
	}}}
	planned, code := buildPlanGuardFixture(t, []string{"build"}, cfg)
	if code != ExitSuccess {
		t.Fatalf("buildPlan code = %d, want ExitSuccess (%d)", code, ExitSuccess)
	}
	if len(planned) == 0 {
		t.Fatal("expected build jobs to be planned")
	}
}

// TestBuildPlan_PartialPlanDisabledExtensionSucceeds proves a disabled extension
// is never treated as missing: deploy legitimately produces no jobs.
func TestBuildPlan_PartialPlanDisabledExtensionSucceeds(t *testing.T) {
	t.Parallel()
	cfg := &wsproto.Config{
		Extensions: wsproto.ExtensionsConfig{List: map[string]string{
			"@putnami/go":    "",
			"@putnami/cloud": "",
		}},
		Disable: &wsproto.DisableConfig{Extensions: []string{"@putnami/cloud"}},
	}
	planned, code := buildPlanGuardFixture(t, []string{"build", "deploy"}, cfg)
	if code != ExitSuccess {
		t.Fatalf("buildPlan code = %d, want ExitSuccess (%d)", code, ExitSuccess)
	}
	if len(planned) == 0 {
		t.Fatal("expected build jobs to be planned")
	}
}

// TestBuildPlan_PartialPlanMissingLocalRefSucceeds proves a missing local path
// ref never trips the guard — local extensions load under their manifest name,
// so an unresolved "./local-ext" is not a registry-ref install failure.
func TestBuildPlan_PartialPlanMissingLocalRefSucceeds(t *testing.T) {
	t.Parallel()
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@putnami/go": "",
		"./local-ext": "", // configured local ref, not loaded — must be ignored
	}}}
	planned, code := buildPlanGuardFixture(t, []string{"build", "deploy"}, cfg)
	if code != ExitSuccess {
		t.Fatalf("buildPlan code = %d, want ExitSuccess (%d)", code, ExitSuccess)
	}
	if len(planned) == 0 {
		t.Fatal("expected build jobs to be planned")
	}
}

// TestBuildPlan_NarrowedPlanningScopeIsNotAMissingExtension pins both guards
// to discovery. A release-set publish that changes no member plans only the
// providers of the verification commands, with their publish jobs removed, so
// an extension that declares no command leaves the planning scope while it is
// loaded. The guard must not report it "not installed", and a publish that
// plans nothing on purpose must not fail the run.
func TestBuildPlan_NarrowedPlanningScopeIsNotAMissingExtension(t *testing.T) {
	wsRoot := t.TempDir()
	proj := &workspace.Project{
		ID:         "/proj",
		Name:       "proj",
		Path:       "proj",
		Extensions: []string{"@putnami/go"},
	}
	ws := workspace.NewWorkspace(wsRoot, nil, []*workspace.Project{proj})
	goExt := &extension.ExtensionDescription{
		Name: "@putnami/go",
		Jobs: map[string]*extension.JobDefinition{
			"build":   {Name: "build", ExtensionName: "@putnami/go", Command: "/bin/true"},
			"publish": {Name: "publish", ExtensionName: "@putnami/go", Command: "/bin/true"},
		},
	}
	commandless := &extension.ExtensionDescription{Name: "@putnami/contributor"}
	// The planning scope of a publish that changes no member: go keeps build
	// and loses publish, and the command-less extension is gone.
	verifier := *goExt
	verifier.Jobs = map[string]*extension.JobDefinition{"build": goExt.Jobs["build"]}
	cfg := &wsproto.Config{Extensions: wsproto.ExtensionsConfig{List: map[string]string{
		"@putnami/go":          "",
		"@putnami/contributor": "",
	}}}
	plan := func(loaded ...*extension.ExtensionDescription) ([]*jobs.ScheduledJob, int, string) {
		req := &Request{WorkspaceRoot: wsRoot, Config: cfg, Commands: []string{"build", "publish"}}
		var planned []*jobs.ScheduledJob
		var code int
		stderr := captureStderr(t, func() {
			planned, code = buildPlan(req, ws, []*workspace.Project{proj},
				[]*extension.ExtensionDescription{&verifier},
				&extension.DiscoveryResult{Extensions: loaded})
		})
		return planned, code, stderr
	}

	planned, code, stderr := plan(goExt, commandless)
	if code != ExitSuccess {
		t.Fatalf("buildPlan code = %d, want ExitSuccess (%d); stderr:\n%s", code, ExitSuccess, stderr)
	}
	if strings.Contains(stderr, "required extensions") {
		t.Errorf("guard reported a loaded extension as missing:\n%s", stderr)
	}
	if len(planned) == 0 {
		t.Fatal("expected build jobs to be planned")
	}
	for _, job := range planned {
		if job.CommandName() != "build" {
			t.Errorf("planned %s, want build jobs only", job.CommandName())
		}
	}

	// The same plan while discovery did NOT load the extension is the same
	// failure, and it still fails.
	if _, code, stderr := plan(goExt); code != ExitError ||
		!strings.Contains(stderr, "required extensions are not installed: @putnami/contributor") {
		t.Fatalf("buildPlan code = %d, want ExitError (%d) naming @putnami/contributor; stderr:\n%s", code, ExitError, stderr)
	}
}

// TestReportMissingExtensions_HeadlineTellsTheTwoFailuresApart pins the guard's
// headline: "not installed" and "did not load" have opposite remedies, and the
// mixed case must keep both.
func TestReportMissingExtensions_HeadlineTellsTheTwoFailuresApart(t *testing.T) {
	tests := []struct {
		name        string
		missing     []string
		skipped     []extension.SkippedExtension
		wantHeadl   string
		wantInstall bool
	}{
		{
			name:        "all explained",
			missing:     []string{"@putnami/cloud"},
			skipped:     []extension.SkippedExtension{belowContractSkip("@putnami/cloud")},
			wantHeadl:   "required extensions did not load",
			wantInstall: false,
		},
		{
			name:        "none explained",
			missing:     []string{"@putnami/cloud"},
			wantHeadl:   "required extensions are not installed",
			wantInstall: true,
		},
		{
			name:        "mixed",
			missing:     []string{"@putnami/cloud", "@putnami/other"},
			skipped:     []extension.SkippedExtension{belowContractSkip("@putnami/cloud")},
			wantHeadl:   "required extensions are not installed",
			wantInstall: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := captureStderr(t, func() { reportMissingExtensions(tc.missing, tc.skipped, "") })
			if !strings.Contains(out, tc.wantHeadl) {
				t.Errorf("headline missing %q:\n%s", tc.wantHeadl, out)
			}
			if got := strings.Contains(out, "run `putnami install`"); got != tc.wantInstall {
				t.Errorf("install hint present = %v, want %v:\n%s", got, tc.wantInstall, out)
			}
		})
	}
}

// TestReportUnservedSDDCommands_NamesTheExtensionOnlyWhenNothingServesIt pins
// the courtesy hint added when the four SDD commands leave the CLI.
//
// The three cases are the three states a user can be in, and the hint is right
// in exactly one of them: the command is one that moved into an extension and
// no loaded extension declares it. Firing when the extension IS loaded would
// tell a user to install what they already have — the failure mode that makes
// people stop reading hints — and firing on a command the extension never
// served would send them after the wrong dependency.
func TestReportUnservedSDDCommands_NamesTheExtensionOnlyWhenNothingServesIt(t *testing.T) {
	sddExt := &extension.ExtensionDescription{
		Name: "@putnami/sdd",
		Jobs: map[string]*extension.JobDefinition{
			"features": {Name: "features", ExtensionName: "@putnami/sdd"},
		},
	}

	tests := []struct {
		name     string
		commands []string
		ext      []*extension.ExtensionDescription
		want     string
	}{
		{
			name:     "no extension serves features",
			commands: []string{"features"},
			want:     "putnami: features " + sddExtensionHintRemedy + "\n",
		},
		{
			name:     "every SDD command is named once, sorted, in one line",
			commands: []string{"specs", "features", "features", "architecture", "contracts"},
			want:     "putnami: architecture, contracts, features, specs " + sddExtensionHintRemedy + "\n",
		},
		{
			name:     "a loaded extension serving the command silences the hint",
			commands: []string{"features"},
			ext:      []*extension.ExtensionDescription{sddExt},
			want:     "",
		},
		{
			name:     "a command @putnami/sdd never served gets no hint",
			commands: []string{"deploy"},
			want:     "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := captureStderr(t, func() { reportUnservedSDDCommands(tc.commands, tc.ext) })
			if got != tc.want {
				t.Errorf("hint =\n%q\nwant\n%q", got, tc.want)
			}
		})
	}
}

// TestSDDExtensionHintNamesNoExtensionAndStaysActionable keeps the engine
// from naming an extension or a path of one repository, while the remedy
// still cites the published reference that names the extension, and says
// where an extension is declared and how it is installed.
func TestSDDExtensionHintNamesNoExtensionAndStaysActionable(t *testing.T) {
	t.Parallel()
	for _, banned := range []string{"@putnami/", "sdd", "/tooling/"} {
		if strings.Contains(sddExtensionHintRemedy, banned) {
			t.Errorf("the remedy names %q:\n%s", banned, sddExtensionHintRemedy)
		}
	}
	for _, want := range []string{"moved from the core CLI to an extension", commandmeta.CommandsThatLeftTheCoreURL + " names that extension", `"extensions"`, "putnami.workspace.json", "`putnami install`"} {
		if !strings.Contains(sddExtensionHintRemedy, want) {
			t.Errorf("the remedy does not name %q:\n%s", want, sddExtensionHintRemedy)
		}
	}
}
