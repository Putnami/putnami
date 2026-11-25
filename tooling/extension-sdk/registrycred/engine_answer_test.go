package registrycred

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	extensionproto "go.putnami.dev/protocol/extension"
	registryproto "go.putnami.dev/protocol/registry"
)

// engineHands sets the variable the engine hands a job, naming host with
// outcome until validUntil.
func engineHands(t *testing.T, host string, outcome Outcome, validUntil time.Time) {
	t.Helper()
	answer, ok := EngineAnswer(outcome, validUntil)
	if !ok {
		t.Fatalf("EngineAnswer(%+v) reports nothing", outcome)
	}
	t.Setenv(extensionproto.NativeRegistryCredentialsEnv,
		extensionproto.NativeRegistryCredentials{host: answer}.Encode())
}

// started reports whether the fake CLI recorded an argv.
func started(t *testing.T, recorded string) bool {
	t.Helper()
	_, err := os.Stat(recorded)
	return err == nil
}

func TestEnsureNativeCredential_StartsNoCLIWhileTheEngineAnswerHolds(t *testing.T) {
	recorded := filepath.Join(t.TempDir(), "argv")
	fakeMaterializeCLI(t, fakeCLI{WordsFile: recorded})
	engineHands(t, "go.putnami.dev", Outcome{Kind: KindMaterialized, Level: "debug", Message: "refreshed the registry credential for go.putnami.dev"},
		time.Now().Add(time.Minute))

	outcome := EnsureNativeCredential(context.Background(), t.TempDir(), "go.putnami.dev")
	if outcome.Kind != KindMaterialized || outcome.Level != "debug" || outcome.Message == "" {
		t.Fatalf("outcome = %+v, want the engine's materialized answer with its line", outcome)
	}
	if started(t, recorded) {
		t.Fatal("the job started the CLI although the engine's answer holds")
	}
	if again := EnsureNativeCredential(context.Background(), t.TempDir(), "go.putnami.dev"); again.Message != "" {
		t.Errorf("second outcome = %+v, want its line logged once per process", again)
	}
}

func TestEnsureNativeCredential_HoldsTheEngineWarning(t *testing.T) {
	recorded := filepath.Join(t.TempDir(), "argv")
	fakeMaterializeCLI(t, fakeCLI{WordsFile: recorded})
	warning := "not signed in for go.putnami.dev; " + signInHint
	engineHands(t, "go.putnami.dev", Outcome{Kind: KindNotSignedIn, Level: "warn", Message: warning}, time.Now().Add(time.Minute))

	outcome := EnsureNativeCredential(context.Background(), "", "go.putnami.dev")
	if outcome.Kind != KindNotSignedIn || outcome.Level != "warn" || outcome.Message != warning {
		t.Fatalf("outcome = %+v, want the engine's warning", outcome)
	}
	if started(t, recorded) {
		t.Fatal("the job started the CLI although the engine's answer holds")
	}
}

func TestEnsureNativeCredential_CallsTheSeamWhenTheEngineAnswerDoesNotHold(t *testing.T) {
	for name, hand := range map[string]func(t *testing.T){
		"expired": func(t *testing.T) {
			engineHands(t, "go.putnami.dev", Outcome{Kind: KindMaterialized}, time.Now().Add(-time.Second))
		},
		"another host": func(t *testing.T) {
			engineHands(t, "other.example.com", Outcome{Kind: KindMaterialized}, time.Now().Add(time.Minute))
		},
		"unreadable": func(t *testing.T) {
			t.Setenv(extensionproto.NativeRegistryCredentialsEnv, "go.putnami.dev")
		},
		"unknown outcome": func(t *testing.T) {
			t.Setenv(extensionproto.NativeRegistryCredentialsEnv,
				`{"go.putnami.dev":{"validUntil":"`+time.Now().Add(time.Minute).UTC().Format(time.RFC3339)+`","outcome":"later"}}`)
		},
		"absent": func(t *testing.T) {},
	} {
		t.Run(name, func(t *testing.T) {
			recorded := filepath.Join(t.TempDir(), "argv")
			fakeMaterializeCLI(t, fakeCLI{WordsFile: recorded})
			hand(t)
			if outcome := EnsureNativeCredential(context.Background(), "", "go.putnami.dev"); outcome.Kind != KindMaterialized {
				t.Fatalf("Kind = %v, want KindMaterialized from the job's own call", outcome.Kind)
			}
			if !started(t, recorded) {
				t.Fatal("the job started no CLI although no engine answer holds")
			}
		})
	}
}

func TestMaterializeNativeCredential_CallsTheSeamForARunEveryTime(t *testing.T) {
	recorded := filepath.Join(t.TempDir(), "argv")
	fakeMaterializeCLI(t, fakeCLI{WordsFile: recorded})
	executable, _ := resolveCloudCLIExecutable()
	want := registryproto.SeamParentCommand + " " + registryproto.SeamSubcommand + " --" + registryproto.SeamHostFlag +
		" go.putnami.dev --" + registryproto.SeamMaterializeFlag + " --" + registryproto.SeamForRunFlag
	for i := range 2 {
		if outcome := MaterializeNativeCredential(context.Background(), executable, "", " Go.Putnami.Dev ", nil); outcome.Kind != KindMaterialized {
			t.Fatalf("call %d: Kind = %v, want KindMaterialized", i, outcome.Kind)
		}
		argv, err := os.ReadFile(recorded)
		if err != nil {
			t.Fatalf("call %d started no CLI: the engine renews an answer that no longer holds", i)
		}
		if string(argv) != want {
			t.Fatalf("argv = %q, want %q", argv, want)
		}
		_ = os.Remove(recorded)
	}
}

// A cloud that predates --for-run refuses it as a usage error.
func TestMaterializeNativeCredential_ACloudWithoutTheRunWindowIsUnsupported(t *testing.T) {
	fakeMaterializeCLI(t, fakeCLI{Stderr: "putnami: flag --for-run is not declared by `cloud registry-token`\n  Accepted: --host, --materialize\n", Exit: 2})
	executable, _ := resolveCloudCLIExecutable()
	if outcome := MaterializeNativeCredential(context.Background(), executable, "", "go.putnami.dev", nil); outcome.Kind != KindUnsupported {
		t.Fatalf("Kind = %v, want KindUnsupported", outcome.Kind)
	}
}

func TestMaterializeNativeCredential_HostedJobAsksNothing(t *testing.T) {
	recorded := filepath.Join(t.TempDir(), "argv")
	fakeMaterializeCLI(t, fakeCLI{WordsFile: recorded})
	t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
	executable, _ := resolveCloudCLIExecutable()
	if outcome := MaterializeNativeCredential(context.Background(), executable, "", "go.putnami.dev", nil); outcome.Kind != KindSkipped {
		t.Fatalf("Kind = %v, want KindSkipped on a hosted run", outcome.Kind)
	}
	if started(t, recorded) {
		t.Fatal("a hosted run started the credential CLI")
	}
}

func TestEngineAnswer_NamesNoSkippedHost(t *testing.T) {
	if _, ok := EngineAnswer(Outcome{Kind: KindSkipped}, time.Now()); ok {
		t.Fatal("EngineAnswer reports a skipped host")
	}
	for kind := range outcomeNames {
		answer, ok := EngineAnswer(Outcome{Kind: kind}, time.Now().Add(time.Minute))
		if !ok || answer.Outcome == "" {
			t.Fatalf("kind %v has no wire name", kind)
		}
	}
}
