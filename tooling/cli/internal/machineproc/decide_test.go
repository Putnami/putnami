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
