package machineproc

import (
	"os"
	"path/filepath"
	"testing"
)

func TestTheCPUShareIsOffByDefault(t *testing.T) {
	h := t.TempDir()
	getenv := func(name string) string {
		if name == "PUTNAMI_HOME" {
			return h
		}
		return ""
	}
	if got := ReadCPUShare(getenv); got != CPUShareOff {
		t.Fatalf("with no setting file, the CPU share is %q, want off", got)
	}
	if err := os.WriteFile(filepath.Join(h, settingFile), []byte(`{"process":"on","cpuShare":"bogus"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadCPUShare(getenv); got != CPUShareOff {
		t.Fatalf("an unknown value reads as %q, want off", got)
	}
}

func TestTheFairCPUShareIsReadAndKeptByTheProcessSetting(t *testing.T) {
	h := t.TempDir()
	getenv := func(name string) string {
		if name == "PUTNAMI_HOME" {
			return h
		}
		return ""
	}
	if err := os.WriteFile(filepath.Join(h, settingFile), []byte(`{"process":"on","cpuShare":"fair"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := ReadCPUShare(getenv); got != CPUShareFair {
		t.Fatalf("the CPU share is %q, want fair", got)
	}
	if err := WriteSetting(getenv, SettingOff); err != nil {
		t.Fatal(err)
	}
	if got := ReadSetting(getenv); got != SettingOff {
		t.Fatalf("the process setting is %q, want off", got)
	}
	if got := ReadCPUShare(getenv); got != CPUShareFair {
		t.Fatalf("turning the machine process off dropped the CPU share: %q", got)
	}
}
