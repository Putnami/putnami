package upload

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	diag "go.putnami.dev/protocol/diagnostic"
	"go.putnami.dev/protocol/features/spectest"
)

const feature = "tooling/observe-upload"

// fakeCLI writes a CLI that records its arguments and environment, copies the
// document it gets, prints a line and exits with code.
func fakeCLI(t *testing.T, code string) (executable, record string) {
	t.Helper()
	record = t.TempDir()
	executable = filepath.Join(t.TempDir(), "putnami")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > '" + record + "/args'\n" +
		"env > '" + record + "/env'\n" +
		"cp \"$4\" '" + record + "/document'\n" +
		"echo reporter says hello\n" +
		"exit " + code + "\n"
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return executable, record
}

func document(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evidence.json")
	if err := os.WriteFile(path, []byte(`{"protocolVersion":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func env(values map[string]string) func(string) string {
	return func(k string) string { return values[k] }
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func codes(list []diag.Diagnostic) []string {
	out := make([]string, 0, len(list))
	for _, d := range list {
		out = append(out, d.Code)
	}
	return out
}

func TestRunHandsTheDocumentToTheSelectedReporter(t *testing.T) {
	spectest.Proves(t, feature, "upload-hands-the-document-to-the-reporter", "upload-runs-the-selected-reporter-with-the-document")
	executable, record := fakeCLI(t, "0")
	path := document(t)
	var out bytes.Buffer
	_, diags := Run(context.Background(), Request{
		Path:   path,
		Getenv: env(map[string]string{SelectorEnv: "@acme/ledger", "PUTNAMI_CLI_EXECUTABLE": executable}),
		Output: &out,
	})
	if len(diags) != 0 {
		t.Fatalf("diagnostics %v", diags)
	}
	if got, want := strings.Fields(read(t, filepath.Join(record, "args"))), []string{"ledger", Subcommand, FileFlag, path, OutputFlag}; !slices.Equal(got, want) {
		t.Fatalf("args %v, want %v", got, want)
	}
	if read(t, filepath.Join(record, "document")) != read(t, path) {
		t.Fatal("the reporter did not get the document")
	}
	environ := read(t, filepath.Join(record, "env"))
	for _, want := range []string{"PUTNAMI_NO_AUTO_INSTALL=1", "PUTNAMI_NO_RELAUNCH=1"} {
		if !strings.Contains(environ, want) {
			t.Fatalf("the reporter's environment lacks %s", want)
		}
	}
	if !strings.Contains(out.String(), "reporter says hello") {
		t.Fatalf("the reporter's output was not forwarded: %q", out.String())
	}
}

// answeringCLI writes a CLI that prints stdout and exits with code.
func answeringCLI(t *testing.T, stdout, code string) string {
	t.Helper()
	executable := filepath.Join(t.TempDir(), "putnami")
	script := "#!/bin/sh\ncat <<'EOF'\n" + stdout + "\nEOF\necho progress >&2\nexit " + code + "\n"
	if err := os.WriteFile(executable, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return executable
}

func resultJSON(status, data string) string {
	return `{"protocolVersion":2,"command":"cloud evidence-report","status":"` + status + `","exitCode":0,"data":` + data + `}`
}

func TestTheReporterAnswersTheReportURL(t *testing.T) {
	spectest.Proves(t, feature, "upload-answers-the-report-url", "the-reporter-answers-the-report-url")
	const report = "https://putnami.app/w/acme/changes/acme%2Fshop%2342"
	var out bytes.Buffer
	url, diags := Run(context.Background(), Request{
		Path:   document(t),
		Getenv: env(map[string]string{"PUTNAMI_CLI_EXECUTABLE": answeringCLI(t, resultJSON("success", `{"run_id":"r1","reportUrl":"`+report+`"}`), "0")}),
		Output: &out,
	})
	if len(diags) != 0 || url != report {
		t.Fatalf("url %q, diagnostics %v", url, diags)
	}
	if strings.Contains(out.String(), "protocolVersion") || !strings.Contains(out.String(), "progress") ||
		!strings.Contains(out.String(), "the evidence reporter @putnami/cloud stored the evidence; the report is at "+report+"\n") {
		t.Fatalf("output %q", out.String())
	}
}

func TestAReporterWithoutAURLStillUploaded(t *testing.T) {
	spectest.Proves(t, feature, "upload-answers-the-report-url", "a-reporter-without-a-url-still-uploaded")
	for name, stdout := range map[string]string{
		"plain text":         "uploaded the evidence",
		"no URL":             resultJSON("success", `{"run_id":"r1"}`),
		"data not an object": resultJSON("success", `"done"`),
		"plain http":         resultJSON("success", `{"reportUrl":"http://putnami.app/r"}`),
		"credentials":        resultJSON("success", `{"reportUrl":"https://u:p@putnami.app/r"}`),
		"relative":           resultJSON("success", `{"reportUrl":"/w/acme/changes/1"}`),
		"Markdown":           resultJSON("success", `{"reportUrl":"https://putnami.app/r)[x](https://evil"}`),
		"protocol version 1": `{"protocolVersion":1,"status":"success","data":{"reportUrl":"https://putnami.app/r"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			url, diags := Run(context.Background(), Request{Path: document(t), Getenv: env(map[string]string{"PUTNAMI_CLI_EXECUTABLE": answeringCLI(t, stdout, "0")}), Output: &out})
			if len(diags) != 0 || url != "" {
				t.Fatalf("url %q, diagnostics %v", url, diags)
			}
			if !strings.HasSuffix(out.String(), "the evidence reporter @putnami/cloud stored the evidence\n") {
				t.Fatalf("output %q", out.String())
			}
			if name == "plain text" && !strings.Contains(out.String(), "uploaded the evidence") {
				t.Fatalf("the reporter's text output was not forwarded: %q", out.String())
			}
		})
	}
	if got := ReportURL("https://putnami.app/" + strings.Repeat("a", maxReportURLBytes)); got != "" {
		t.Fatalf("an oversized URL was kept: %d bytes", len(got))
	}
}

func TestAFailureResultNamesItsReason(t *testing.T) {
	spectest.Proves(t, feature, "upload-answers-the-report-url", "a-failure-result-names-its-reason")
	stdout := `{"protocolVersion":2,"command":"cloud evidence-report","status":"failure","exitCode":4,"error":{"code":"auth","message":"cloud evidence-report needs\nPUTNAMI_CLOUD_TOKEN"}}`
	var out bytes.Buffer
	url, diags := Run(context.Background(), Request{Path: document(t), Getenv: env(map[string]string{"PUTNAMI_CLI_EXECUTABLE": answeringCLI(t, stdout, "4")}), Output: &out})
	if url != "" || len(diags) != 1 || !strings.HasSuffix(diags[0].Message, "refused the upload: cloud evidence-report needs PUTNAMI_CLOUD_TOKEN") {
		t.Fatalf("url %q, diagnostics %+v", url, diags)
	}
	if strings.Contains(out.String(), "protocolVersion") {
		t.Fatalf("the result reached the output: %q", out.String())
	}

	// A reporter that writes a success result, then a failure result, failed:
	// the last result names the reason.
	twice := resultJSON("success", `{"reportUrl":"https://putnami.app/r"}`) + "\n" + stdout
	url, diags = Run(context.Background(), Request{Path: document(t), Getenv: env(map[string]string{"PUTNAMI_CLI_EXECUTABLE": answeringCLI(t, twice, "4")}), Output: &out})
	if url != "" || len(diags) != 1 || !strings.HasSuffix(diags[0].Message, "needs PUTNAMI_CLOUD_TOKEN") {
		t.Fatalf("two results: url %q, diagnostics %+v", url, diags)
	}
}

func TestAnUnsetSelectorPicksPutnamiCloud(t *testing.T) {
	spectest.Proves(t, feature, "upload-hands-the-document-to-the-reporter", "an-unset-selector-picks-putnami-cloud")
	for _, selector := range []string{"", "  "} {
		name, group, err := Reporter(env(map[string]string{SelectorEnv: selector}))
		if err != nil || name != DefaultReporter || group != "cloud" {
			t.Fatalf("selector %q: %s %s %v", selector, name, group, err)
		}
	}
	name, group, err := Reporter(env(map[string]string{SelectorEnv: "ledger"}))
	if err != nil || name != "ledger" || group != "ledger" {
		t.Fatalf("an unscoped name: %s %s %v", name, group, err)
	}
}

func TestCommandUsesPutnamiOnPathWithoutAnAbsoluteExecutable(t *testing.T) {
	spectest.Proves(t, feature, "upload-hands-the-document-to-the-reporter", "upload-runs-the-selected-reporter-with-the-document")
	for _, advertised := range []string{"", "bin/putnami"} {
		executable, args, environ := Command(env(map[string]string{"PUTNAMI_CLI_EXECUTABLE": advertised}), []string{"A=1"}, "cloud", "/tmp/e.json")
		if executable != "putnami" || !slices.Equal(args, []string{"cloud", Subcommand, FileFlag, "/tmp/e.json", OutputFlag}) {
			t.Fatalf("advertised %q: %s %v", advertised, executable, args)
		}
		if !slices.Equal(environ, []string{"A=1", "PUTNAMI_NO_AUTO_INSTALL=1"}) {
			t.Fatalf("advertised %q: environment %v", advertised, environ)
		}
	}
}

func TestAFailedUploadIsAWarning(t *testing.T) {
	spectest.Proves(t, feature, "upload-fails-open", "a-failed-upload-is-a-diagnostic")
	refusing, _ := fakeCLI(t, "3")
	slow := filepath.Join(t.TempDir(), "putnami")
	if err := os.WriteFile(slow, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		values  map[string]string
		timeout time.Duration
		want    string
	}{
		{"a bad selector", map[string]string{SelectorEnv: "Not An Extension"}, 0, "is not an extension name"},
		{"a refusal", map[string]string{"PUTNAMI_CLI_EXECUTABLE": refusing}, 0, "exited with code 3"},
		{"a missing CLI", map[string]string{"PUTNAMI_CLI_EXECUTABLE": filepath.Join(t.TempDir(), "absent")}, 0, "did not start"},
		{"a timeout", map[string]string{"PUTNAMI_CLI_EXECUTABLE": slow}, 200 * time.Millisecond, "ran past 200ms"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var out bytes.Buffer
			url, diags := Run(context.Background(), Request{Path: document(t), Getenv: env(c.values), Output: &out, Timeout: c.timeout})
			if url != "" {
				t.Fatalf("a failed upload answered %q", url)
			}
			if !slices.Equal(codes(diags), []string{CodeUploadFailed}) || diags[0].Severity != diag.Warning || !strings.Contains(diags[0].Message, c.want) {
				t.Fatalf("diagnostics %+v, want one warning with %q", diags, c.want)
			}
		})
	}
	if skipped := Skipped(); !slices.Equal(codes(skipped), []string{CodeUploadFailed}) || skipped[0].Severity != diag.Warning {
		t.Fatalf("skipped %+v", skipped)
	}
}
