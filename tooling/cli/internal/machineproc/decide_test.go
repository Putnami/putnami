package machineproc

import (
	"runtime"
	"testing"
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

// The machine process ships off until the owner decides its default on
// measurements: a command that neither PUTNAMI_MACHINE nor the user's setting
// turns on runs in its own process, and either one turns it on (CLI ADR 0116
// §12).
func TestTheMachineProcessIsOffUntilTheUserTurnsItOn(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("no machine process on this platform")
	}
	if DefaultOn {
		t.Fatal("DefaultOn is true: the machine process is off by default (CLI ADR 0116 §12)")
	}
	none := func(string) string { return "" }
	on := func(name string) string {
		if name == Env {
			return "on"
		}
		return ""
	}
	for _, tc := range []struct {
		name    string
		getenv  func(string) string
		setting string
		use     bool
		reason  string
	}{
		{name: "neither says", getenv: none, reason: "default"},
		{name: "the setting on", getenv: none, setting: SettingOn, use: true},
		{name: "the variable on", getenv: on, use: true},
	} {
		if got := Decide(tc.getenv, tc.setting, DefaultOn); got.Use != tc.use || got.Reason != tc.reason {
			t.Errorf("%s: Decide = %+v, want use=%v reason=%q", tc.name, got, tc.use, tc.reason)
		}
	}
}
