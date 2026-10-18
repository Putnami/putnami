package doctor

import (
	"bytes"
	"errors"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
)

func linuxReader(content string, cpus int) LoadReader {
	return LoadReader{OS: "linux", NumCPU: func() int { return cpus },
		ReadFile: func(string) ([]byte, error) { return []byte(content), nil }}
}

func TestMachineLoad_RatioOrUnknown(t *testing.T) {
	spectest.Proves(t, "cli/diagnostics-results", "doctor-machine", "machine-load-is-a-ratio-or-unknown")
	cases := []struct {
		name   string
		reader LoadReader
		want   string
	}{
		{"linux ratio", linuxReader("2.00 1.00 0.50 1/100 42\n", 4), "0.50"},
		{"darwin ratio", LoadReader{OS: "darwin", NumCPU: func() int { return 2 },
			Command: func(string, ...string) ([]byte, error) { return []byte("{ 1.00 0.80 0.60 }\n"), nil }}, "0.50"},
		{"unreadable file", LoadReader{OS: "linux", NumCPU: func() int { return 4 },
			ReadFile: func(string) ([]byte, error) { return nil, errors.New("no") }}, "unknown"},
		{"no cpus", linuxReader("1.00 1 1 1/1 1", 0), "unknown"},
		{"negative load", linuxReader("-1.00 1 1 1/1 1", 4), "unknown"},
		{"unsupported os", LoadReader{OS: "plan9", NumCPU: func() int { return 4 }}, "unknown"},
		{"missing cpu func", LoadReader{OS: "linux"}, "unknown"},
	}
	for _, c := range cases {
		if got := MachineLoad(c.reader).String(); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestWriteMachineLoad_Formats(t *testing.T) {
	known := MachineLoad(linuxReader("2.00 1 1 1/1 1", 4))
	var human, structured, unknown bytes.Buffer
	_ = WriteMachineLoad(&human, "", known)
	_ = WriteMachineLoad(&structured, "json", known)
	_ = WriteMachineLoad(&unknown, "json", LoadSample{})
	if human.String() != "0.50\n" {
		t.Errorf("human = %q", human.String())
	}
	if structured.String() != `{"known":true,"oneMinute":2,"cpus":4,"ratio":0.5}`+"\n" {
		t.Errorf("structured = %q", structured.String())
	}
	if unknown.String() != `{"known":false}`+"\n" {
		t.Errorf("unknown = %q", unknown.String())
	}
}

func TestMachineLoadKnownAndUnknown(t *testing.T) {
	read := LoadReader{OS: "linux", NumCPU: func() int { return 4 }, ReadFile: func(string) ([]byte, error) { return []byte("2.00 1.00 0.50 1/4 100"), nil }}
	sample := MachineLoad(read)
	if !sample.Known || sample.CPUs != 4 || sample.Ratio != 0.5 || sample.String() != "0.50" {
		t.Fatalf("sample %+v", sample)
	}
	read.OS = "windows"
	if sample := MachineLoad(read); sample.Known || sample.String() != "unknown" {
		t.Fatalf("unsupported host %+v", sample)
	}
	read.OS = "linux"
	read.ReadFile = func(string) ([]byte, error) { return []byte("invalid"), nil }
	if MachineLoad(read).Known {
		t.Fatal("invalid load was reported as known")
	}
	read = LoadReader{OS: "darwin", NumCPU: func() int { return 4 }, Command: func(name string, args ...string) ([]byte, error) {
		if name != "/usr/sbin/sysctl" || len(args) != 2 || args[0] != "-n" || args[1] != "vm.loadavg" {
			t.Fatalf("load reader used PATH-selected or unexpected command: %q %v", name, args)
		}
		return []byte("{ 2.00 1.00 0.50 }"), nil
	}}
	if sample := MachineLoad(read); !sample.Known || sample.Ratio != 0.5 {
		t.Fatalf("absolute Darwin load probe failed: %+v", sample)
	}
	for _, platform := range []string{"freebsd", "openbsd", "netbsd"} {
		read.OS = platform
		if sample := MachineLoad(read); sample.Known {
			t.Fatalf("unverified %s path was treated as trustworthy: %+v", platform, sample)
		}
	}
}
