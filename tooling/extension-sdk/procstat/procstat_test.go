package procstat

import (
	"os"
	"os/exec"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

func TestMain(m *testing.M) {
	// The test binary is its own child: PROCSTAT_CHILD makes it burn a little
	// CPU and exit, so the tests need no program of the host.
	if os.Getenv("PROCSTAT_CHILD") == "1" {
		sum := 0
		for i := 0; i < 20_000_000; i++ {
			sum += i % 7
		}
		if sum < 0 {
			os.Exit(2)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func child(t *testing.T, name string) *exec.Cmd {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable)
	cmd.Env = append(os.Environ(), "PROCSTAT_CHILD=1")
	// argv[0] only: the ledger classifies a process by its program, cmd.Path,
	// which is the test binary, so every child here counts as other unless a
	// kind is given.
	cmd.Args[0] = name
	return cmd
}

func TestTheLedgerCountsEachWrapperByKind(t *testing.T) {
	before := Take()

	if err := Run(child(t, "worker")); err != nil {
		t.Fatal(err)
	}
	if _, err := Output(child(t, "worker"), Control); err != nil {
		t.Fatal(err)
	}
	if _, err := CombinedOutput(child(t, "worker"), CLI); err != nil {
		t.Fatal(err)
	}
	started := child(t, "worker")
	if err := Start(started, Batch); err != nil {
		t.Fatal(err)
	}
	if err := Wait(started, Batch); err != nil {
		t.Fatal(err)
	}
	if err := Run(exec.Command("/nonexistent/program")); err == nil {
		t.Fatal("a missing program started")
	}

	after := Take()
	for kind, want := range map[Kind]int64{Other: 1, Control: 1, CLI: 1, Batch: 1} {
		delta := after.Of(kind).Starts - before.Of(kind).Starts
		if delta != want {
			t.Errorf("%s starts = %d, want %d (a process that never started is not counted)", kind, delta, want)
		}
		if after.Of(kind).Measured-before.Of(kind).Measured != want {
			t.Errorf("%s measured = %d, want %d", kind, after.Of(kind).Measured-before.Of(kind).Measured, want)
		}
	}
}

func TestGitCallsCountBySubcommand(t *testing.T) {
	before := Take()
	Started(Git, []string{"git", "-C", "/repo", "-c", "core.fsmonitor=false", "rev-parse", "--git-common-dir"})
	Started(Git, []string{"/usr/bin/git", "--no-pager", "ls-files", "-z"})
	Started(Git, []string{"git", "--version"})
	after := Take()
	for subcommand, want := range map[string]int64{"rev-parse": 1, "ls-files": 1, "--version": 1} {
		if got := after.Git[subcommand] - before.Git[subcommand]; got != want {
			t.Errorf("git %s = %d, want %d", subcommand, got, want)
		}
	}
	if got := after.Of(Git).Starts - before.Of(Git).Starts; got != 3 {
		t.Errorf("git starts = %d, want 3", got)
	}
}

func TestClassifyNamesGitTheCLIAndOther(t *testing.T) {
	for program, want := range map[string]Kind{
		"git":                    Git,
		"/usr/bin/git":           Git,
		`C:\Git\cmd\git.exe`:     Git,
		"/usr/local/bin/putnami": CLI,
		"./putnamiw":             CLI,
		"go":                     Other,
		"/usr/bin/gitk":          Other,
	} {
		if got := Classify(program); got != want {
			t.Errorf("Classify(%q) = %s, want %s", program, got, want)
		}
	}
	if got := GitSubcommand(nil); got != "git" {
		t.Errorf("GitSubcommand(nil) = %q, want git", got)
	}
}

func TestGitSubcommandSkipsTheGlobalOptionsAndNeverNamesNothing(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"git", "-C", "/repo", "rev-parse", "HEAD"}, "rev-parse"},
		{[]string{"git", "-c", "core.x=y", "--git-dir=/g", "ls-files"}, "ls-files"},
		{[]string{"git", "--attr-source", "HEAD", "check-attr", "-a", "f"}, "check-attr"},
		{[]string{"git", "--super-prefix", "x", "diff"}, "diff"},
		{[]string{"git", "--no-pager", "log"}, "log"},
		{[]string{"git", "--version"}, "--version"},
		{[]string{"git", ""}, "git"},
		{[]string{"git", "-C", "/repo", ""}, "git"},
	} {
		if got := GitSubcommand(tc.argv); got != tc.want {
			t.Errorf("GitSubcommand(%q) = %q, want %q", tc.argv, got, tc.want)
		}
	}
}

func TestTheReportCarriesTheProcessOwnCPUApartFromItsChildren(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "a-run-reports-its-orchestration-cpu", "an-sdk-job-reports-its-own-cpu-apart-from-its-children")
	if err := Run(child(t, "worker")); err != nil {
		t.Fatal(err)
	}
	report := Take().Report()
	self, measured := SelfCPU()
	children, _ := ChildrenCPU()
	if !measured {
		if report.SelfCPUMs != nil {
			t.Fatalf("selfCpuMs = %d on a platform that does not measure it", *report.SelfCPUMs)
		}
		t.Skip("this platform measures no process CPU")
	}
	if report.SelfCPUMs == nil {
		t.Fatal("no selfCpuMs where the platform measures it")
	}
	if *report.SelfCPUMs > self.Milliseconds() {
		t.Errorf("selfCpuMs = %d, above the process's own %d ms", *report.SelfCPUMs, self.Milliseconds())
	}
	var other int64
	for _, start := range report.Starts {
		if start.Kind == string(Other) && start.CPUMs != nil {
			other = *start.CPUMs
		}
	}
	if other == 0 || other > children.Milliseconds()+1 {
		t.Errorf("children cpu in report = %d ms, waited-for children = %d ms", other, children.Milliseconds())
	}
}

func TestAbsorbSumsTheReportsOfJobProcesses(t *testing.T) {
	before := TakeReported()
	self, six, nine := int64(40), int64(6), int64(900)
	Absorb(&runtimeproto.ProcessReport{
		SelfCPUMs: &self,
		Starts:    []runtimeproto.ProcessStarts{{Kind: "git", Count: 2, CPUMs: &six}, {Kind: "other", Count: 1, CPUMs: &nine}},
		Git:       []runtimeproto.GitCalls{{Subcommand: "rev-parse", Count: 2}},
	})
	Absorb(&runtimeproto.ProcessReport{SelfCPUMs: &self, Starts: []runtimeproto.ProcessStarts{{Kind: "git", Count: 1}}, Git: []runtimeproto.GitCalls{{Subcommand: "rev-parse", Count: 1}}})
	Absorb(nil)
	after := TakeReported()
	if got := after.Of(Git).Starts - before.Of(Git).Starts; got != 3 {
		t.Errorf("git starts = %d, want 3", got)
	}
	if got := after.Of(Other).CPU - before.Of(Other).CPU; got.Milliseconds() != 900 {
		t.Errorf("other cpu = %s, want 900ms", got)
	}
	if got := after.Of(Git).Measured - before.Of(Git).Measured; got != 1 {
		t.Errorf("git measured = %d, want 1", got)
	}
	if got := after.Of(Other).Measured - before.Of(Other).Measured; got != 1 {
		t.Errorf("other measured = %d, want 1", got)
	}
	if got := after.Git["rev-parse"] - before.Git["rev-parse"]; got != 3 {
		t.Errorf("rev-parse = %d, want 3", got)
	}
	if got := (after.SelfCPU - before.SelfCPU).Milliseconds(); got != 80 {
		t.Errorf("self cpu = %d ms, want 80", got)
	}
	if after.Reports-before.Reports != 2 || after.Unreported-before.Unreported != 1 {
		t.Errorf("reports = %d, unreported = %d; want 2 and 1", after.Reports-before.Reports, after.Unreported-before.Unreported)
	}
}
