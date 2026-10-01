//go:build unix

package registrycred

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

// handDescriptor writes body to an unlinked file and points the job variable
// at an independent descriptor for it, as the engine's inherited descriptor
// would be. A file, not a pipe: an oversized body would block a pipe write.
// It returns the descriptor number.
func handDescriptor(t *testing.T, body string) int {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "credential")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(body); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(file.Name())
	fd, err := syscall.Dup(int(file.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	resetJobCredentialForTest()
	t.Cleanup(resetJobCredentialForTest)
	t.Setenv(extensionproto.JobCredentialFDEnv, strconv.Itoa(fd))
	return fd
}

func TestReadJobCredential_ReadsOnceClosesTheDescriptorAndClearsTheVariable(t *testing.T) {
	fd := handDescriptor(t, `{"credential":{"bearer":"pkt_job_secret","expiresAt":"2099-01-01T00:00:00Z","hosts":["npm.putnami.dev"]}}`+"\n")

	credential, err := ReadJobCredential()
	if err != nil {
		t.Fatal(err)
	}
	if credential == nil || credential.Bearer != "pkt_job_secret" || strings.Join(credential.Hosts, ",") != "npm.putnami.dev" {
		t.Fatalf("credential = %v, want the handed credential", credential)
	}
	if _, present := os.LookupEnv(extensionproto.JobCredentialFDEnv); present {
		t.Errorf("%s is still in the environment", extensionproto.JobCredentialFDEnv)
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err == nil {
		t.Errorf("descriptor %d is still open after the read", fd)
	}
	again, err := ReadJobCredential()
	if err != nil || again != credential {
		t.Fatalf("second read = %v, %v; want the first answer", again, err)
	}
}

func TestReadJobCredential_AbsenceIsNoCredential(t *testing.T) {
	handDescriptor(t, "{}\n")
	credential, err := ReadJobCredential()
	if credential != nil || err != nil {
		t.Fatalf("ReadJobCredential() = %v, %v; want absence", credential, err)
	}
}

func TestReadJobCredential_RefusesAnInvalidDocumentWithoutEchoingIt(t *testing.T) {
	for name, body := range map[string]string{
		"empty":         "",
		"not json":      "pkt_bare_secret",
		"unknown field": `{"credential":{"bearer":"pkt_bare_secret","expiresAt":"2099-01-01T00:00:00Z","hosts":["a.dev"]},"x":1}`,
		"oversized":     strings.Repeat("a", 64<<10+1),
	} {
		t.Run(name, func(t *testing.T) {
			handDescriptor(t, body)
			_, err := ReadJobCredential()
			if err == nil {
				t.Fatal("an invalid job credential was accepted")
			}
			if strings.Contains(err.Error(), "pkt_bare_secret") {
				t.Fatalf("the error echoes the credential: %v", err)
			}
		})
	}
}

// The engine hands a hosted workspace-fetch "{}" when the run's
// provider holds no read credential. The fetch then starts no credential
// child: neither EnsureNativeCredential nor ResolveToken runs `putnami cloud
// registry-token`, before or after the descriptor is read. Without a handed
// descriptor the same calls run the child.
func TestAHandedDescriptorStartsNoCredentialChild(t *testing.T) {
	for _, tc := range []struct {
		name        string
		hand, read  bool
		wantStarted bool
	}{
		{name: "no descriptor", wantStarted: true},
		{name: "a handed descriptor not read yet", hand: true},
		{name: "a handed descriptor already read", hand: true, read: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count := filepath.Join(t.TempDir(), "count")
			fakeMaterializeCLI(t, fakeCLI{CountFile: count, Stdout: "pkt_seam_token\n"})
			t.Setenv(extensionproto.OfflineDependenciesEnv, "")
			t.Setenv(extensionproto.JobCredentialFDEnv, "")
			if err := os.Unsetenv(extensionproto.JobCredentialFDEnv); err != nil {
				t.Fatal(err)
			}
			resetJobCredentialForTest()
			t.Cleanup(resetJobCredentialForTest)
			if tc.hand {
				handDescriptor(t, "{}\n")
			}
			if tc.read {
				if credential, err := ReadJobCredential(); credential != nil || err != nil {
					t.Fatalf("ReadJobCredential() = %v, %v; want no credential", credential, err)
				}
			}

			outcome := EnsureNativeCredential(t.Context(), t.TempDir(), "npm.putnami.dev")
			token, hint := ResolveToken("npm.putnami.dev")
			withCLI, _ := ResolveTokenWithCLI(t.Context(), "npm.putnami.dev", cloudCLIName)

			_, err := os.Stat(count)
			if started := err == nil; started != tc.wantStarted {
				t.Fatalf("the credential child started: %v, want %v", started, tc.wantStarted)
			}
			if tc.wantStarted {
				return
			}
			if outcome.Kind != KindSkipped || outcome.Message != "" {
				t.Errorf("EnsureNativeCredential = %+v, want a silent skip", outcome)
			}
			if token != "" || withCLI != "" || hint != hostedJobHint {
				t.Errorf("ResolveToken = %q, %q and ResolveTokenWithCLI = %q; want no token and the hosted hint", token, hint, withCLI)
			}
		})
	}
}
