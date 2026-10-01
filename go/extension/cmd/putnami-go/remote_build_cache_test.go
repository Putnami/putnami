package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.putnami.dev/go/extension/internal/toolchain"
	cache "go.putnami.dev/protocol/cache"
	"go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/cli"
	pctx "go.putnami.dev/sdk/extension/context"
	"go.putnami.dev/sdk/extension/jsonl"
)

// goCacheProgFor builds a job environment for a provider-backed run and returns
// the GOCACHEPROG the toolchain decided on. The home is set under both names
// os.UserHomeDir reads, HOME on Unix and USERPROFILE on Windows.
func goCacheProgFor(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	env := toolchain.GoCommandEnv([]string{
		"HOME=" + home,
		"USERPROFILE=" + home,
		cache.ObjectCacheSocketEnv + "=/tmp/exchange/objects.sock",
	}, "")
	for _, entry := range env {
		if value, ok := strings.CutPrefix(entry, "GOCACHEPROG="); ok {
			return value
		}
	}
	return ""
}

// runWrapped runs a job through the option wrapper with one parameter bag, in
// the raw JSON form a job context carries.
func runWrapped(t *testing.T, params map[string]string) {
	t.Helper()
	bag := job.Params{}
	for key, value := range params {
		bag[key] = json.RawMessage(value)
	}
	t.Cleanup(func() { toolchain.UseRemoteBuildCacheOption(true) })
	wrapped := withRemoteBuildCacheOption(func(*pctx.Context, *jsonl.Emitter, []string) (string, map[string]any, error) {
		return "OK", nil, nil
	})
	if _, _, err := wrapped(&pctx.Context{Params: bag}, jsonl.New(), nil); err != nil {
		t.Fatalf("job: %v", err)
	}
}

// TestTheOptionReachesTheEnvironmentBuilder is the plumbing this option needs
// and nothing more: the dispatch table records it, and the environment builder
// a dozen call sites share reads it back. Without this seam the option would
// have to be threaded through every one of those call sites, including the ones
// inside toolchain resolution that have no job context at all.
func TestTheOptionReachesTheEnvironmentBuilder(t *testing.T) {
	runWrapped(t, map[string]string{"remote-build-cache": "false"})
	if got := goCacheProgFor(t); got != "" {
		t.Fatalf("GOCACHEPROG = %q after a job turned the option off, want unset", got)
	}

	runWrapped(t, map[string]string{"remote-build-cache": "true"})
	if got := goCacheProgFor(t); !strings.HasSuffix(got, " "+toolchain.GoCacheProgSubcommand) {
		t.Fatalf("GOCACHEPROG = %q after a job turned the option on, want the helper", got)
	}
}

// TestTheOptionDefaultsOn pins the default for a job whose command does not
// declare the flag: the presence of a provider socket is then the only switch.
func TestTheOptionDefaultsOn(t *testing.T) {
	runWrapped(t, map[string]string{"remote-build-cache": "false"})
	runWrapped(t, map[string]string{})
	if got := goCacheProgFor(t); !strings.HasSuffix(got, " "+toolchain.GoCacheProgSubcommand) {
		t.Fatalf("GOCACHEPROG = %q with the option undeclared, want the helper", got)
	}
}

// TestTheOptionAcceptsTheCamelCaseAlias covers the spelling parameter
// resolution produces for a dashed flag.
func TestTheOptionAcceptsTheCamelCaseAlias(t *testing.T) {
	runWrapped(t, map[string]string{"remoteBuildCache": "false"})
	if got := goCacheProgFor(t); got != "" {
		t.Fatalf("GOCACHEPROG = %q for the camel-case spelling, want unset", got)
	}
}

// TestRunEntrypointServesTheCacheProg pins the hidden entry point. The go
// command starts this binary with one argument and expects the capability
// message immediately; falling through to job dispatch would leave it waiting
// for a helper that never announces itself, and the go command turns that into
// a failed build.
func TestRunEntrypointServesTheCacheProg(t *testing.T) {
	// The helper reads the go command's requests from stdin, so the test hands
	// it a closed pipe: an immediate end of stream is the shutdown the go
	// command performs when it stops without a close request.
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close pipe: %v", err)
	}
	previous := os.Stdin
	os.Stdin = reader
	t.Cleanup(func() {
		os.Stdin = previous
		_ = reader.Close()
	})

	var out bytes.Buffer
	dispatched := false
	err = runEntrypoint(
		[]string{toolchain.GoCacheProgSubcommand},
		&out,
		func(key string) string {
			if key == "PUTNAMI_GO_CACHE_DIR" {
				return t.TempDir()
			}
			return ""
		},
		func(map[string]cli.JobFunc) { dispatched = true },
	)
	if err != nil {
		t.Fatalf("runEntrypoint(gocacheprog): %v", err)
	}
	if dispatched {
		t.Fatal("the cache helper must not enter normal command dispatch")
	}

	line, err := bufio.NewReader(&out).ReadBytes('\n')
	if err != nil {
		t.Fatalf("read the capability message: %v (%q)", err, out.String())
	}
	var announced struct {
		ID            int64
		KnownCommands []string
	}
	if err := json.Unmarshal(line, &announced); err != nil {
		t.Fatalf("decode the capability message %q: %v", line, err)
	}
	if announced.ID != 0 {
		t.Fatalf("capability message ID = %d, want 0", announced.ID)
	}
	for _, want := range []string{"get", "put", "close"} {
		if !containsString(announced.KnownCommands, want) {
			t.Errorf("command %q is not advertised (%v)", want, announced.KnownCommands)
		}
	}
}
