package cli

import (
	"errors"
	"reflect"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
)

func TestParseStandardFlags_OutputModes(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantCtx  string
		wantMode protocolcli.OutputMode
		wantJob  []string
	}{
		{
			name:     "output space form",
			args:     []string{"--putnamiContext", "/tmp/ctx.json", "--output", "json"},
			wantCtx:  "/tmp/ctx.json",
			wantMode: protocolcli.OutputJSON,
			wantJob:  nil,
		},
		{
			name:     "output equals form",
			args:     []string{"--output=jsonl"},
			wantMode: protocolcli.OutputJSONL,
		},
		{
			name:     "json shorthand",
			args:     []string{"--json"},
			wantMode: protocolcli.OutputJSON,
		},
		{
			name:     "no output flag defaults to auto",
			args:     []string{"--putnamiContext", "/c"},
			wantCtx:  "/c",
			wantMode: protocolcli.OutputAuto,
		},
		{
			name:     "job args flow through unchanged",
			args:     []string{"--putnamiContext", "/c", "--target", "build", "pkg"},
			wantCtx:  "/c",
			wantMode: protocolcli.OutputAuto,
			wantJob:  []string{"--target", "build", "pkg"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parseStandardFlags(c.args)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.contextFile != c.wantCtx {
				t.Errorf("contextFile = %q, want %q", got.contextFile, c.wantCtx)
			}
			if got.mode != c.wantMode {
				t.Errorf("mode = %q, want %q", got.mode, c.wantMode)
			}
			if !reflect.DeepEqual(got.jobArgs, c.wantJob) {
				t.Errorf("jobArgs = %v, want %v", got.jobArgs, c.wantJob)
			}
		})
	}
}

func TestParseStandardFlags_UsageErrors(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"json conflicts with output", []string{"--json", "--output", "jsonl"}},
		{"unknown output mode", []string{"--output", "bogus"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseStandardFlags(c.args)
			if err == nil {
				t.Fatal("expected a usage error, got nil")
			}
			if !errors.Is(err, protocolcli.ErrUsage) {
				t.Errorf("error is not ErrUsage-classified: %v", err)
			}
		})
	}
}

func TestExitCodeFor(t *testing.T) {
	cases := []struct {
		name   string
		status string
		err    error
		want   int
	}{
		{"ok success", "OK", nil, protocolcli.ExitSuccess},
		{"skip success", "SKIP", nil, protocolcli.ExitSuccess},
		{"failed no error", "FAILED", nil, protocolcli.ExitFailure},
		{"unclassified error", "FAILED", errors.New("boom"), protocolcli.ExitFailure},
		{"auth error", "FAILED", protocolcli.Authf("token expired"), protocolcli.ExitAuth},
		{"api error wins over ok status", "OK", protocolcli.APIf("registry 500"), protocolcli.ExitAPI},
		{"usage error", "FAILED", protocolcli.Usagef("bad flag"), protocolcli.ExitUsage},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := exitCodeFor(c.status, c.err); got != c.want {
				t.Errorf("exitCodeFor(%q, %v) = %d, want %d", c.status, c.err, got, c.want)
			}
		})
	}
}
