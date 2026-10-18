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
