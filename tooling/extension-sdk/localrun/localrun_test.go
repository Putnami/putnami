package localrun

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// probeEnv makes the test binary print, one per line, each of HostedRunVars
// it still holds, instead of running the tests.
const probeEnv = "PUTNAMI_LOCALRUN_TEST_PROBE"

func TestMain(m *testing.M) {
	if os.Getenv(probeEnv) != "" {
		for _, name := range HostedRunVars() {
			if value, set := os.LookupEnv(name); set {
				_, _ = os.Stdout.WriteString(name + "=" + value + "\n")
			}
		}
		_, _ = os.Stdout.WriteString(startedEnv + "=" + os.Getenv(startedEnv) + "\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// probe starts the test binary again with the hosted run's job environment,
// and StartedEntry when started, and returns what it printed.
func probe(t *testing.T, started bool) string {
	t.Helper()
	cmd := exec.Command(os.Args[0])
	var env []string
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, startedEnv+"=") {
			env = append(env, entry)
		}
	}
	env = append(env, probeEnv+"=1", extensionproto.OfflineDependenciesEnv+"=1", extensionproto.JobCredentialFDEnv+"=7")
	if started {
		env = append(env, StartedEntry)
	}
	cmd.Env = env
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	return string(out)
}

func TestATestBinaryOfAHostedRunStartsWithoutTheHostedRunVariables(t *testing.T) {
	if got, want := probe(t, false), startedEnv+"=1\n"; got != want {
		t.Errorf("a test binary started by a hosted run's test job holds\n%swant\n%s", got, want)
	}
}

func TestAProcessStartedByATestKeepsTheVariablesTheTestSet(t *testing.T) {
	want := extensionproto.JobCredentialFDEnv + "=7\n" + extensionproto.OfflineDependenciesEnv + "=1\n" + startedEnv + "=1\n"
	if got := probe(t, true); got != want {
		t.Errorf("a test binary started by a test holds\n%swant\n%s", got, want)
	}
}

func TestThisTestBinaryHoldsNoHostedRunVariable(t *testing.T) {
	for _, name := range HostedRunVars() {
		if value, set := os.LookupEnv(name); set {
			t.Errorf("%s=%q reached the tests", name, value)
		}
	}
}

// Every test package of the SDK that links a reader of the hosted-run
// variables imports this package.
func TestTheSDKTestPackagesRunAsALocalRun(t *testing.T) {
	CheckModule(t)
}

func TestCheckModuleNamesATestPackageThatReadsTheInheritedEnvironment(t *testing.T) {
	root := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.test/probe\n\ngo 1.25\n")
	write("reader/reader.go", "package reader\n\nimport \"os\"\n\nfunc Offline() bool { return os.Getenv(\"PUTNAMI_OFFLINE_DEPENDENCIES\") == \"1\" }\n")
	write("reader/reader_test.go", "package reader\n\nimport \"testing\"\n\nfunc TestOffline(t *testing.T) { _ = Offline() }\n")
	write("caller/caller.go", "package caller\n\nimport \"example.test/probe/reader\"\n\nfunc Offline() bool { return reader.Offline() }\n")
	write("caller/caller_test.go", "package caller_test\n\nimport (\n\t\"testing\"\n\n\t\"example.test/probe/caller\"\n)\n\nfunc TestOffline(t *testing.T) { _ = caller.Offline() }\n")
	write("plain/plain.go", "package plain\n\nfunc One() int { return 1 }\n")
	write("plain/plain_test.go", "package plain\n\nimport \"testing\"\n\nfunc TestOne(t *testing.T) { _ = One() }\n")
	t.Setenv("GOWORK", "off")
	t.Setenv("GOFLAGS", "-mod=mod")

	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	problems, err := checkModule(resolved)
	if err != nil {
		t.Fatalf("checkModule: %v", err)
	}
	if len(problems) != 2 ||
		!strings.HasPrefix(problems[0], "the tests of example.test/probe/caller link example.test/probe/reader,") ||
		!strings.HasPrefix(problems[1], "the tests of example.test/probe/reader link example.test/probe/reader,") {
		t.Errorf("problems = %q, want the caller and the reader test packages, and not the plain one", problems)
	}
}
