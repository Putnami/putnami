package skipguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// testFile wraps body in a test function of a file that imports testing plus
// the packages the guards use.
func testFile(body string) []byte {
	return []byte(`package sample

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

var _, _, _ = os.Getenv, exec.LookPath, runtime.GOOS

func TestSample(t *testing.T) {
` + body + `
}
`)
}

func TestScanFileRefusedForms(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"unconditional Skip":                       {`t.Skip("later")`, "t.Skip runs unconditionally"},
		"unconditional Skipf":                      {`t.Skipf("later: %d", 1)`, "t.Skipf runs unconditionally"},
		"unconditional SkipNow":                    {`t.SkipNow()`, "t.SkipNow runs unconditionally"},
		"after an early return":                    {"if runtime.GOOS == \"linux\" {\n\t\treturn\n\t}\n\tt.Skip(\"not linux\")", "runs unconditionally"},
		"inside a loop":                            {"for range 1 {\n\t\tt.Skip(\"loop\")\n\t}", "runs unconditionally"},
		"inside a subtest":                         {"t.Run(\"sub\", func(t *testing.T) {\n\t\tt.Skip(\"sub\")\n\t})", "runs unconditionally"},
		"reason names flakiness":                   {"if testing.Short() {\n\t\tt.Skip(\"flaky on slow disks\")\n\t}", "flaky or fails in CI"},
		"reason names CI":                          {"if testing.Short() {\n\t\tt.Skip(\"fails on CI\")\n\t}", "flaky or fails in CI"},
		"reason names flakiness in a constant sum": {`t.Skip("known " + "flake")`, "flaky or fails in CI"},
		"guard reads the CI variable":              {"if os.Getenv(\"CI\") != \"\" {\n\t\tt.Skip(\"not here\")\n\t}", "CI or flakiness condition"},
		"guard names CI":                           {"isCI := true\n\tif isCI {\n\t\tt.Skip(\"not here\")\n\t}", "CI or flakiness condition"},
		"guard names flakiness":                    {"flakyHost := true\n\tif flakyHost {\n\t\tt.Skip(\"not here\")\n\t}", "CI or flakiness condition"},
		"switch on the CI variable":                {"switch os.Getenv(\"GITHUB_ACTIONS\") {\n\tcase \"true\":\n\t\tt.Skip(\"not here\")\n\t}", "CI or flakiness condition"},
		"guard names a CI constant":                {"const EnvCI = \"CI\"\n\tif os.Getenv(EnvCI) != \"\" {\n\t\tt.Skip(\"not here\")\n\t}", "CI or flakiness condition (os.Getenv(EnvCI) != \"\")"},
		"guard names a CI variable":                {"CI := true\n\tif CI {\n\t\tt.Skip(\"not here\")\n\t}", "CI or flakiness condition (CI)"},
		"guard names a CI service variable":        {"GITHUB_ACTIONS := true\n\tif GITHUB_ACTIONS {\n\t\tt.Skip(\"not here\")\n\t}", "CI or flakiness condition (GITHUB_ACTIONS)"},
		"else branch of a platform check":          {"if runtime.GOOS == \"linux\" {\n\t\tt.Log(\"linux\")\n\t} else if isFlaky := true; isFlaky {\n\t\tt.Skip(\"not here\")\n\t}", "CI or flakiness condition (isFlaky)"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			findings := ScanFile("sample_test.go", testFile(tc.body))
			if len(findings) != 1 {
				t.Fatalf("findings = %+v, want one", findings)
			}
			got := findings[0]
			if got.Severity != "error" || !strings.Contains(got.Message, tc.want) {
				t.Fatalf("finding = %+v, want an error containing %q", got, tc.want)
			}
		})
	}
}

func TestScanFileAllowedGuards(t *testing.T) {
	cases := map[string]string{
		"platform check":            "if runtime.GOOS == \"windows\" {\n\t\tt.Skip(\"no umask on Windows\")\n\t}",
		"missing binary":            "if _, err := exec.LookPath(\"docker\"); err != nil {\n\t\tt.Skipf(\"docker is not installed: %v\", err)\n\t}",
		"missing variable":          "if os.Getenv(\"DATABASE_URL\") == \"\" {\n\t\tt.Skip(\"no database binding\")\n\t}",
		"short mode":                "if testing.Short() {\n\t\tt.Skip(\"slow\")\n\t}",
		"else branch":               "if runtime.GOOS == \"linux\" {\n\t\treturn\n\t} else {\n\t\tt.SkipNow()\n\t}",
		"switch case":               "switch runtime.GOOS {\n\tcase \"plan9\":\n\t\tt.Skip(\"unsupported\")\n\t}",
		"select case":               "ch := make(chan int)\n\tclose(ch)\n\tselect {\n\tcase <-ch:\n\t\tt.Skip(\"closed\")\n\t}",
		"guarded subtest":           "if testing.Short() {\n\t\tt.Run(\"sub\", func(t *testing.T) {\n\t\t\tt.Skip(\"slow\")\n\t\t})\n\t}",
		"reason names circuit":      "if testing.Short() {\n\t\tt.Skip(\"circuit breaker test is slow\")\n\t}",
		"else branch of a CI check": "if os.Getenv(\"CI\") != \"\" {\n\t\tt.Log(\"ci\")\n\t} else if _, err := exec.LookPath(\"docker\"); err != nil {\n\t\tt.Skip(\"no docker\")\n\t}",
		"name ending in ci":         "magic := true\n\tif magic {\n\t\tt.Skip(\"not here\")\n\t}",
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			if findings := ScanFile("sample_test.go", testFile(body)); len(findings) != 0 {
				t.Fatalf("findings = %+v, want none", findings)
			}
		})
	}
}

func TestScanFileReceivers(t *testing.T) {
	src := []byte(`package sample

import (
	"encoding/xml"
	tst "testing"
)

type suite struct{}

func (suite) T() *tst.T { return nil }

func skipAlways(tb tst.TB) { tb.Skip("helper") }

func BenchmarkSample(b *tst.B) { b.Skip("bench") }

func FuzzSample(f *tst.F) { f.Skip("fuzz") }

func (s suite) TestSuite() { s.T().Skip("suite") }

func TestDecoder(t *tst.T) {
	var decoder *xml.Decoder
	_ = decoder.Skip()
	t.Run("shadowed", func(t *xml.Decoder) { _ = t.Skip() })
}
`)
	findings := ScanFile("sample_test.go", src)
	lines := make([]int, 0, len(findings))
	for _, finding := range findings {
		lines = append(lines, finding.Line)
	}
	want := []int{12, 14, 16, 18}
	if len(lines) != len(want) {
		t.Fatalf("finding lines = %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("finding lines = %v, want %v", lines, want)
		}
	}
}

func TestScanFileBuildConstraints(t *testing.T) {
	body := testFile(`t.Skip("Windows has no process umask")`)
	if findings := ScanFile("umask_windows_test.go", body); len(findings) != 0 {
		t.Fatalf("GOOS suffix: findings = %+v, want none", findings)
	}
	if findings := ScanFile("umask_windows_amd64_test.go", body); len(findings) != 0 {
		t.Fatalf("GOOS_GOARCH suffix: findings = %+v, want none", findings)
	}
	for _, line := range []string{"//go:build windows", "//go:build !windows && !plan9", "//go:build unix", "//go:build cgo", "// +build linux", "//go:build linux && integration"} {
		constrained := append([]byte(line+"\n\n"), body...)
		if findings := ScanFile("sample_test.go", constrained); len(findings) != 0 {
			t.Fatalf("%s: findings = %+v, want none", line, findings)
		}
	}
	ignored := append([]byte("//go:build ignore\n\n"), body...)
	if findings := ScanFile("sample_test.go", ignored); len(findings) != 0 {
		t.Fatalf("//go:build ignore is never compiled: findings = %+v, want none", findings)
	}
	for _, line := range []string{"//go:build integration", "//go:build go1.22", "//go:build !race", "//go:build integration || linux"} {
		tagged := append([]byte(line+"\n\n"), body...)
		if findings := ScanFile("sample_test.go", tagged); len(findings) != 1 {
			t.Fatalf("%s names no platform: findings = %+v, want one", line, findings)
		}
	}
	if findings := ScanFile("windows_test.go", body); len(findings) != 1 {
		t.Fatalf("a bare windows_test.go is not constrained: findings = %+v, want one", findings)
	}
	flaky := testFile(`t.Skip("flaky on Windows")`)
	if findings := ScanFile("umask_windows_test.go", flaky); len(findings) != 1 {
		t.Fatalf("a constrained file still refuses a flaky reason: findings = %+v", findings)
	}
}

func TestScanFileReviewedExceptions(t *testing.T) {
	above := ScanFile("sample_test.go", testFile("//putnami:allow-skip the harness drives this test itself\n\tt.Skip(\"driven elsewhere\")"))
	if len(above) != 1 || above[0].Severity != "warning" ||
		!strings.Contains(above[0].Message, "reviewed exception: t.Skip stays because the harness drives this test itself") {
		t.Fatalf("exception above: findings = %+v", above)
	}

	sameLine := ScanFile("sample_test.go", testFile(`t.Skip("fails on CI") // putnami:allow-skip: CI has no GPU`))
	if len(sameLine) != 1 || sameLine[0].Severity != "warning" || !strings.Contains(sameLine[0].Message, "because CI has no GPU") {
		t.Fatalf("exception on the same line: findings = %+v", sameLine)
	}

	noReason := ScanFile("sample_test.go", testFile("//putnami:allow-skip\n\tt.Skip(\"later\")"))
	if len(noReason) != 1 || noReason[0].Severity != "error" || !strings.Contains(noReason[0].Message, "needs a reason") {
		t.Fatalf("exception without a reason: findings = %+v", noReason)
	}

	other := ScanFile("sample_test.go", testFile("//putnami:allow-skipping later\n\tt.Skip(\"later\")"))
	if len(other) != 1 || other[0].Severity != "error" {
		t.Fatalf("another directive is not an exception: findings = %+v", other)
	}

	farAbove := ScanFile("sample_test.go", testFile("//putnami:allow-skip too far\n\n\tt.Skip(\"later\")"))
	if len(farAbove) != 1 || farAbove[0].Severity != "error" {
		t.Fatalf("an exception two lines above does not apply: findings = %+v", farAbove)
	}

	trailing := ScanFile("sample_test.go", testFile("t.Skip(\"a\") // putnami:allow-skip reviewed\n\tt.Skip(\"b\")"))
	if len(trailing) != 2 || trailing[0].Severity != "warning" || trailing[1].Severity != "error" {
		t.Fatalf("a trailing exception covers its own line only: findings = %+v", trailing)
	}
}

func TestScanFileIgnoresFilesItCannotRead(t *testing.T) {
	if findings := ScanFile("broken_test.go", []byte("package sample\nfunc (")); findings != nil {
		t.Fatalf("findings = %+v, want none for a file that does not parse", findings)
	}
	noTesting := []byte("package sample\n\ntype T struct{}\n\nfunc (T) Skip() {}\n\nfunc use(t T) { t.Skip() }\n")
	if findings := ScanFile("sample_test.go", noTesting); len(findings) != 0 {
		t.Fatalf("findings = %+v, want none without the testing package", findings)
	}
}

func TestScanProject(t *testing.T) {
	root := t.TempDir()
	unguarded := testFile(`t.Skip("later")`)
	write := func(rel string, content []byte) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", []byte("module sample\n"))
	write("b_test.go", unguarded)
	write("pkg/a_test.go", unguarded)
	write("pkg/code.go", unguarded)
	write("testdata/x_test.go", unguarded)
	write("vendor/v/x_test.go", unguarded)
	write(".hidden/x_test.go", unguarded)
	write("_scratch/x_test.go", unguarded)
	write("nested/go.mod", []byte("module nested\n"))
	write("nested/x_test.go", unguarded)
	write("project/putnami.json", []byte("{}"))
	write("project/x_test.go", unguarded)

	findings, err := ScanProject(root)
	if err != nil {
		t.Fatal(err)
	}
	files := make([]string, 0, len(findings))
	for _, finding := range findings {
		rel, _ := filepath.Rel(root, finding.File)
		files = append(files, filepath.ToSlash(rel))
	}
	if strings.Join(files, ",") != "b_test.go,pkg/a_test.go" {
		t.Fatalf("scanned files = %v, want b_test.go and pkg/a_test.go", files)
	}

	if _, err := ScanProject(filepath.Join(root, "missing")); err == nil {
		t.Fatal("a missing project root must be an error")
	}
}

func TestScanProjectSkipsDirectoriesGitIgnores(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root := t.TempDir()
	unguarded := testFile(`t.Skip("later")`)
	for rel, content := range map[string][]byte{
		".gitignore":        []byte("build/\n"),
		"go.mod":            []byte("module sample\n"),
		"a_test.go":         unguarded,
		"build/gen_test.go": unguarded,
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, content, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command("git", "init", "-q")
	cmd.Dir = root
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}

	findings, err := ScanProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || filepath.Base(findings[0].File) != "a_test.go" {
		t.Fatalf("findings = %+v, want a_test.go only", findings)
	}
}
