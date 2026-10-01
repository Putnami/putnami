package releaseset

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.putnami.dev/sdk/extension/ownerperm"
)

// fakeProviderEnv makes the test binary a fake release-set provider: TestMain
// runs the fake the variable names instead of the tests. A test sets it in the
// client's environment and points the client at the test binary, so the fake
// runs wherever the tests run, with no shell.
const fakeProviderEnv = "PUTNAMI_RELEASESET_FAKE_PROVIDER"

// The fakes a test can select. Each reads the FAKE_* variables its test sets.
const (
	// fakeProtocol records its argv, the request file's path, access and
	// bytes, then answers as FAKE_SLEEP, FAKE_SECRET, FAKE_STDERR,
	// FAKE_FAILURE, FAKE_OVERFLOW, FAKE_CONTAMINATION and FAKE_RESPONSE say.
	fakeProtocol = "protocol"
	// fakePublished records the published images file's path and bytes.
	fakePublished = "published"
	// fakeAmbient records the published images file's path alone.
	fakeAmbient = "ambient"
	// fakeMembers records the member evidence file's path and bytes.
	fakeMembers = "members"
)

func TestMain(m *testing.M) {
	if fake := os.Getenv(fakeProviderEnv); fake != "" {
		os.Exit(runFakeProvider(fake, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// privateRequestAccess is what requestAccess reports for a private request
// file: its mode on Unix, and on Windows, where the mode bits do not keep
// other users out, an access list that admits the owner alone.
var privateRequestAccess = func() string {
	if runtime.GOOS == "windows" {
		return "owner-only"
	}
	return "600"
}()

// requestAccess describes who may read path, as privateRequestAccess does.
func requestAccess(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return err.Error()
	}
	if runtime.GOOS != "windows" {
		return strconv.FormatUint(uint64(info.Mode().Perm()), 8)
	}
	private, err := ownerperm.OwnerOnly(path)
	switch {
	case err != nil:
		return err.Error()
	case private:
		return "owner-only"
	default:
		return "shared"
	}
}

func runFakeProvider(fake string, args []string) int {
	switch fake {
	case fakeProtocol:
		return runProtocolFake(args)
	case fakePublished:
		recordEvidence(os.Getenv(PublishedImagesFileEnv)+"\n", os.Getenv("FAKE_PATH_OBSERVATION"), true)
	case fakeAmbient:
		recordEvidence(os.Getenv(PublishedImagesFileEnv)+"\n", os.Getenv("FAKE_OBSERVATION"), false)
	case fakeMembers:
		recordEvidence(os.Getenv(MemberEvidenceFileEnv), os.Getenv("FAKE_PATH_OBSERVATION"), true)
	default:
		fmt.Fprintf(os.Stderr, "unknown fake provider %q\n", fake)
		return 125
	}
	fmt.Println(os.Getenv("FAKE_RESPONSE"))
	return 0
}

// recordEvidence writes path to pathObservation and, when copyFile is set,
// the file that path names to FAKE_OBSERVATION. A failed write leaves the
// observation missing, which the test reports.
func recordEvidence(path, pathObservation string, copyFile bool) {
	_ = os.WriteFile(pathObservation, []byte(path), 0o600)
	if !copyFile {
		return
	}
	if data, err := os.ReadFile(strings.TrimSuffix(path, "\n")); err == nil {
		_ = os.WriteFile(os.Getenv("FAKE_OBSERVATION"), data, 0o600)
	}
}

func runProtocolFake(args []string) int {
	for len(args) < 5 {
		args = append(args, "")
	}
	requestPath := args[4]
	var observation bytes.Buffer
	fmt.Fprintf(&observation, "%s|%s|%s|%s\n%s\n%s\n", args[0], args[1], args[2], args[3], requestPath, requestAccess(requestPath))
	if request, err := os.Open(requestPath); err == nil {
		_, _ = io.Copy(&observation, io.LimitReader(request, 4<<20))
		_ = request.Close()
	}
	observation.WriteString("\n")
	_ = os.WriteFile(os.Getenv("FAKE_OBSERVATION"), observation.Bytes(), 0o600)

	if seconds := os.Getenv("FAKE_SLEEP"); seconds != "" {
		n, _ := strconv.Atoi(seconds)
		time.Sleep(time.Duration(n) * time.Second)
	}
	for _, name := range []string{"FAKE_SECRET", "FAKE_STDERR"} {
		if value := os.Getenv(name); value != "" {
			fmt.Fprintln(os.Stderr, value)
		}
	}
	if os.Getenv("FAKE_FAILURE") != "" {
		return 17
	}
	switch os.Getenv("FAKE_OVERFLOW") {
	case "stdout":
		_, _ = os.Stdout.Write(make([]byte, 4194306))
		fmt.Println()
		return 0
	case "stderr":
		_, _ = os.Stderr.Write(make([]byte, 65537))
		return 0
	}
	if contamination := os.Getenv("FAKE_CONTAMINATION"); contamination != "" {
		fmt.Println(contamination)
	}
	fmt.Println(os.Getenv("FAKE_RESPONSE"))
	return 0
}

// fakeProviderExecutable is the test binary, which runs as the fake an
// environment's fakeProviderEnv selects.
func fakeProviderExecutable(t *testing.T) string {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return executable
}
