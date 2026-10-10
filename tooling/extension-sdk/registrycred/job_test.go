package registrycred

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
)

func TestReadJobCredential_AbsentVariableHandsNothing(t *testing.T) {
	resetJobCredentialForTest()
	t.Cleanup(resetJobCredentialForTest)

	credential, err := ReadJobCredential()
	if credential != nil || err != nil {
		t.Fatalf("ReadJobCredential() = %v, %v; want nothing", credential, err)
	}
}

func TestReadJobCredential_RefusesADescriptorThatIsNotANumberAboveStdio(t *testing.T) {
	for _, value := range []string{"x", "-1", "0", "2"} {
		t.Run(value, func(t *testing.T) {
			resetJobCredentialForTest()
			t.Cleanup(resetJobCredentialForTest)
			t.Setenv(extensionproto.JobCredentialFDEnv, value)

			_, err := ReadJobCredential()
			if err == nil || !strings.Contains(err.Error(), extensionproto.JobCredentialFDEnv) {
				t.Fatalf("err = %v, want a refusal naming %s", err, extensionproto.JobCredentialFDEnv)
			}
		})
	}
}

func TestEnsureNativeCredential_OfflineDependenciesWritesNothing(t *testing.T) {
	count := filepath.Join(t.TempDir(), "count")
	fakeMaterializeCLI(t, fakeCLI{CountFile: count, Stdout: "pkt_seam_token\n"})
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")

	outcome := EnsureNativeCredential(t.Context(), t.TempDir(), "npm.putnami.dev")
	if outcome.Kind != KindSkipped || outcome.Message != "" {
		t.Fatalf("outcome = %+v, want a silent skip on a hosted run", outcome)
	}
	if token, hint := ResolveToken("npm.putnami.dev"); token != "" || hint != hostedJobHint {
		t.Fatalf("ResolveToken = %q, %q; want no token and the hosted hint", token, hint)
	}
	if _, err := os.Stat(count); !os.IsNotExist(err) {
		t.Fatalf("a job of a hosted run started the credential child: %v", err)
	}
}
