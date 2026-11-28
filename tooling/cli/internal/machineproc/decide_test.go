package machineproc

import (
	"runtime"
	"testing"

	"go.putnami.dev/tooling/cli/internal/runcredential"
)

func TestDecideTurnsTheMachineProcessOffWhereARunMustBeItsOwn(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no machine process on this platform")
	}
	for _, tc := range []struct {
		name    string
		env     map[string]string
		setting string
		use     bool
		reason  string
	}{
		{name: "the default", use: true},
		{name: "a job's CLI", env: map[string]string{"PUTNAMI_JOB_NAME": "test~test"}, reason: "nested"},
		{name: "CI", env: map[string]string{"CI": "true"}, reason: "ci"},
		{name: "Azure Pipelines", env: map[string]string{"TF_BUILD": "True"}, reason: "ci"},
		{name: "Jenkins", env: map[string]string{"JENKINS_URL": "http://ci"}, reason: "ci"},
		{name: "CI=false", env: map[string]string{"CI": "false"}, use: true},
		{name: "the variable off", env: map[string]string{Env: "off"}, reason: "env"},
		{name: "the setting off", setting: SettingOff, reason: "setting"},
		{name: "the variable on over the setting off", env: map[string]string{Env: "on"}, setting: SettingOff, use: true},
		{name: "the variable on in CI", env: map[string]string{Env: "on", "CI": "1"}, reason: "ci"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(func(name string) string { return tc.env[name] }, tc.setting, true)
			if got.Use != tc.use || got.Reason != tc.reason {
				t.Errorf("Decide = %+v, want use=%v reason=%q", got, tc.use, tc.reason)
			}
		})
	}
}

func TestAWorkerNeverServesTheCommandsItRuns(t *testing.T) {
	inWorker = true
	defer func() { inWorker = false }()
	if d := Decide(func(string) string { return "" }, "", true); d.Use || d.Reason != "nested" {
		t.Errorf("Decide in a worker = %+v, want nested", d)
	}
}

func TestTheRuntimesStartupVariablesAreComparedAndNoOthers(t *testing.T) {
	a := runtimeEnv([]string{"GOPATH=/a", "GOFLAGS=-mod=mod", "PATH=/bin", "GOGC=100"})
	b := runtimeEnv([]string{"GOPATH=/b", "GOGC=100"})
	if a != b {
		t.Errorf("tool variables changed the comparison: %q against %q", a, b)
	}
	if runtimeEnv([]string{"GOMAXPROCS=2"}) == runtimeEnv(nil) {
		t.Error("GOMAXPROCS does not change the comparison")
	}
}

// The machine process is on by default on Linux and macOS (CLI ADR 0116
// §12): a command that neither PUTNAMI_MACHINE nor the user's setting turns
// off goes through it. It stays off on a platform without one, in a nested
// run, in a hosted run and in CI, whatever the variable or the setting says,
// and the variable or the setting turns it off.
func TestTheMachineProcessIsOnByDefaultAndOffWhereARunMustBeItsOwn(t *testing.T) {
	if !DefaultOn {
		t.Fatal("DefaultOn is false: the machine process is on by default on Linux and macOS (CLI ADR 0116 §12)")
	}
	env := func(vars map[string]string) func(string) string {
		return func(name string) string { return vars[name] }
	}
	on := map[string]string{Env: "on"}
	for _, tc := range []struct {
		name     string
		platform string
		worker   bool
		hosted   bool
		env      map[string]string
		setting  string
		use      bool
		reason   string
	}{
		{name: "neither says, on Linux", platform: "linux", use: true},
		{name: "neither says, on macOS", platform: "darwin", use: true},
		{name: "the setting on", platform: "linux", setting: SettingOn, use: true},
		{name: "a platform without one", platform: "windows", env: on, setting: SettingOn, reason: "platform"},
		{name: "a job's CLI", platform: "linux", env: map[string]string{jobEnv: "test~test", Env: "on"}, setting: SettingOn, reason: "nested"},
		{name: "a worker's own command", platform: "linux", worker: true, env: on, setting: SettingOn, reason: "nested"},
		{name: "a hosted run", platform: "linux", hosted: true, env: on, setting: SettingOn, reason: "hosted"},
		{name: "CI", platform: "linux", env: map[string]string{"CI": "true", Env: "on"}, setting: SettingOn, reason: "ci"},
		{name: "the variable off", platform: "linux", env: map[string]string{Env: "off"}, setting: SettingOn, reason: "env"},
		{name: "the setting off", platform: "linux", setting: SettingOff, reason: "setting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			savedPlatform, savedHosted := platform, hosted
			platform, inWorker, hosted = tc.platform, tc.worker, func() bool { return tc.hosted }
			defer func() { platform, inWorker, hosted = savedPlatform, false, savedHosted }()
			if got := Decide(env(tc.env), tc.setting, DefaultOn); got.Use != tc.use || got.Reason != tc.reason {
				t.Errorf("Decide = %+v, want use=%v reason=%q", got, tc.use, tc.reason)
			}
		})
	}
}

// A hosted run is the process that captured a run credential: Decide reads
// runcredential's own answer, so the credential a hosted runner hands the CLI
// keeps it in its own process (CLI ADR 0116 §12).
func TestAHostedRunIsTheOneThatHoldsARunCredential(t *testing.T) {
	if !supported() {
		t.Skip("no machine process on this platform")
	}
	restore := runcredential.SetForTest("bearer")
	defer restore()
	if d := Decide(func(string) string { return "" }, SettingOn, DefaultOn); d.Use || d.Reason != "hosted" {
		t.Errorf("Decide with a run credential = %+v, want hosted", d)
	}
}
