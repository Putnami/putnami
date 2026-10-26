package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.putnami.dev/protocol/features/spectest"
	"go.putnami.dev/tooling/cli/internal/cmderr"
	"go.putnami.dev/tooling/cli/internal/commandmeta"
)

// TestServeEnvFlagReadsStdinOnly pins --serve-env on the surfaces a global flag
// reaches: the parser binds the one value it accepts, "-", and refuses any
// other as a usage error; the catalog lists it as a job-only Execution flag;
// and the flag never becomes a job argument or a command parameter, so it
// never moves a cache key.
func TestServeEnvFlagReadsStdinOnly(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/supervised-serve-run", "serve-env-reaches-only-serve-steps", "the-flag-is-execution-only")

	parsed := ParseArgs([]string{"serve", "--serve-env", "-", "--port", "0"}, nil, nil)
	if parsed.Err != nil {
		t.Fatalf("--serve-env - was refused: %v", parsed.Err)
	}
	if parsed.Global.ServeEnv != "-" {
		t.Fatalf("Global.ServeEnv = %q, want -", parsed.Global.ServeEnv)
	}
	for _, arg := range parsed.RawJobArgs {
		if strings.Contains(arg, "serve-env") {
			t.Errorf("--serve-env leaked into the job arguments: %v", parsed.RawJobArgs)
		}
	}
	if _, leaked := buildCommandParams(parsed.RawJobArgs)["serve-env"]; leaked {
		t.Errorf("--serve-env became a command parameter: %v", buildCommandParams(parsed.RawJobArgs))
	}
	if ParseArgs([]string{"serve", "--serve-env=-"}, nil, nil).Global.ServeEnv != "-" {
		t.Error("--serve-env=- did not bind")
	}

	for _, value := range []string{"env.json", "/dev/stdin", "--"} {
		parsed := ParseArgs([]string{"serve", "--serve-env=" + value}, nil, nil)
		if parsed.Err == nil || !errors.Is(parsed.Err, cmderr.ErrUsage) {
			t.Errorf("--serve-env=%s: err = %v, want a usage error", value, parsed.Err)
		}
	}

	var found *commandmeta.GlobalFlag
	for _, flag := range commandmeta.GlobalFlags() {
		if flag.Long == "--serve-env" {
			found = &flag
			break
		}
	}
	if found == nil {
		t.Fatal("--serve-env is absent from the global-flag catalog")
	}
	if found.Type != commandmeta.FlagValue || !found.JobOnly || found.Category != "Execution" || !slices.Equal(found.Values, []string{"-"}) {
		t.Errorf("--serve-env catalog row = %+v, want a job-only Execution value flag accepting only -", *found)
	}
	if ParseArgs([]string{"serve"}, nil, nil).Global.ServeEnv != "" {
		t.Error("--serve-env defaulted to on")
	}
}

// TestReadServeEnvDecodesOneObjectOfStrings pins the document: one JSON object
// whose members become KEY=value entries sorted by name, an empty object an
// empty environment.
func TestReadServeEnvDecodesOneObjectOfStrings(t *testing.T) {
	t.Parallel()
	env, err := readServeEnv(strings.NewReader(`{"PORT":"0","CONFIG_DATA":"{\"db\":{\"password\":\"p=w\"}}","EMPTY":""}` + "\n"))
	if err != nil {
		t.Fatalf("readServeEnv: %v", err)
	}
	want := []string{`CONFIG_DATA={"db":{"password":"p=w"}}`, "EMPTY=", "PORT=0"}
	if !slices.Equal(env, want) {
		t.Errorf("env = %q, want %q", env, want)
	}
	if env, err := readServeEnv(strings.NewReader(" {} ")); err != nil || len(env) != 0 {
		t.Errorf("an empty object = %q, %v; want an empty environment", env, err)
	}
}

// TestReadServeEnvRefusesWithoutQuotingAValue pins every refusal as a usage
// error whose message never quotes a value, which may be a secret.
func TestReadServeEnvRefusesWithoutQuotingAValue(t *testing.T) {
	t.Parallel()
	const secret = "s3cr3t-value"
	cases := map[string]string{
		"an array":                `["` + secret + `"]`,
		"a string":                `"` + secret + `"`,
		"a number value":          `{"PORT":4310}`,
		"an object value":         `{"CONFIG":{"password":"` + secret + `"}}`,
		"a null value":            `{"TOKEN":null}`,
		"an empty name":           `{"":"` + secret + `"}`,
		"a name holding =":        `{"A=B":"` + secret + `"}`,
		"a name holding NUL":      `{"A\u0000B":"` + secret + `"}`,
		"a value holding NUL":     `{"TOKEN":"` + secret + `\u0000"}`,
		"the PUTNAMI_ namespace":  `{"PUTNAMI_PARENT_SESSION":"` + secret + `"}`,
		"a lower-case PUTNAMI_":   `{"putnami_host_rpc":"` + secret + `"}`,
		"a repeated variable":     `{"TOKEN":"a","TOKEN":"` + secret + `"}`,
		"trailing content":        `{"TOKEN":"a"} {"TOKEN":"` + secret + `"}`,
		"invalid JSON":            `{"TOKEN":"` + secret + `",}`,
		"a truncated document":    `{"TOKEN":"` + secret,
		"an empty document":       ``,
		"a document over the cap": `{"TOKEN":"` + strings.Repeat("x", maxServeEnvBytes) + `"}`,
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			env, err := readServeEnv(strings.NewReader(document))
			if err == nil {
				t.Fatalf("readServeEnv accepted %s: %q", name, env)
			}
			if !errors.Is(err, cmderr.ErrUsage) {
				t.Errorf("err = %v, want a usage error", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Errorf("the refusal quotes a value: %v", err)
			}
			if !strings.HasPrefix(err.Error(), "--serve-env -: ") {
				t.Errorf("the refusal does not name the flag: %v", err)
			}
		})
	}
	if _, err := readServeEnv(nil); err == nil {
		t.Error("readServeEnv accepted a nil stdin")
	}
}

// TestReadServeEnvStdinRefusesACharacterDevice pins the refusal of a stdin a
// read would block on: a character device, such as a terminal, is refused as a
// usage error before any read, and a pipe or a file is read.
func TestReadServeEnvStdinRefusesACharacterDevice(t *testing.T) {
	t.Parallel()
	spectest.Proves(t, "cli/supervised-serve-run", "a-manifest-env-conflict-is-refused", "a-terminal-stdin-is-refused")

	device, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer func() { _ = device.Close() }()
	if info, err := device.Stat(); err != nil || info.Mode()&os.ModeCharDevice == 0 {
		t.Skipf("%s is no character device on this platform", os.DevNull)
	}
	_, err = readServeEnvStdin(device)
	if err == nil || !errors.Is(err, cmderr.ErrUsage) || !strings.Contains(err.Error(), "--serve-env -: stdin is a character device") {
		t.Errorf("a character-device stdin: err = %v, want the usage error that names it", err)
	}

	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer func() { _ = reader.Close() }()
	go func() {
		_, _ = writer.WriteString(`{"PORT":"0"}`)
		_ = writer.Close()
	}()
	if env, err := readServeEnvStdin(reader); err != nil || !slices.Equal(env, []string{"PORT=0"}) {
		t.Errorf("a piped stdin = %q, %v; want PORT=0", env, err)
	}

	path := filepath.Join(t.TempDir(), "env.json")
	if err := os.WriteFile(path, []byte(`{"PORT":"0"}`), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() { _ = file.Close() }()
	if env, err := readServeEnvStdin(file); err != nil || !slices.Equal(env, []string{"PORT=0"}) {
		t.Errorf("a file stdin = %q, %v; want PORT=0", env, err)
	}
	if _, err := readServeEnvStdin(nil); err == nil || !errors.Is(err, cmderr.ErrUsage) {
		t.Errorf("a nil stdin: err = %v, want a usage error", err)
	}
}

// TestServeEnvIsRefusedOnAnExtensionSubcommand pins the alias path's refusal:
// a structured extension subcommand would drop the environment, so it refuses
// the flag before it resolves or runs anything.
func TestServeEnvIsRefusedOnAnExtensionSubcommand(t *testing.T) {
	t.Parallel()
	parsed := &ParsedArgs{Commands: []string{"cloud"}, Subcommand: "deploy", Global: GlobalFlags{ServeEnv: "-"}}
	var stdout, stderr bytes.Buffer
	code := (&App{}).runExtensionStructuredCommand(context.Background(), parsed, nil, "", nil,
		strings.NewReader(`{"PORT":"0"}`), &stdout, &stderr)
	if code != ExitUsage {
		t.Errorf("exit = %d, want %d", code, ExitUsage)
	}
	if !strings.Contains(stderr.String(), "--serve-env is refused on `putnami cloud deploy`") {
		t.Errorf("stderr = %q, want the refusal naming the subcommand", stderr.String())
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want nothing", stdout.String())
	}
}
