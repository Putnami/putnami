package scheduler

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cache "go.putnami.dev/protocol/cache"
	extensionproto "go.putnami.dev/protocol/extension"
	hostrpcproto "go.putnami.dev/protocol/extension/hostrpc"
	registryproto "go.putnami.dev/protocol/registry"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/sdk/extension/registrycred"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// countSeamCalls replaces the engine's seam call with one that answers
// outcome and counts the hosts it was asked for.
func countSeamCalls(t *testing.T, outcome registrycred.Outcome) *sync.Map {
	t.Helper()
	calls := &sync.Map{}
	previous := materializeNativeCredential
	t.Cleanup(func() { materializeNativeCredential = previous })
	materializeNativeCredential = func(_ context.Context, executable, _ string, host string, env []string) registrycred.Outcome {
		if executable == "" {
			t.Errorf("the seam was called without the engine's executable")
		}
		if envHasKey(env, extensionproto.NativeRegistryCredentialsEnv) || !envHasKey(env, "PATH") {
			t.Errorf("the seam's child environment is not the engine's, stripped")
		}
		counter, _ := calls.LoadOrStore(host, new(atomic.Int64))
		counter.(*atomic.Int64).Add(1)
		return outcome
	}
	return calls
}

func seamCalls(calls *sync.Map, host string) int64 {
	counter, ok := calls.Load(host)
	if !ok {
		return 0
	}
	return counter.(*atomic.Int64).Load()
}

// credentialJob is a job whose task declares the Go origin of a project that
// declares registries.
func credentialJob(registries string) *ScheduledJob {
	project := &workspace.Project{Name: "lib"}
	if registries != "" {
		_ = json.Unmarshal([]byte(registries), &project.Registries)
	}
	return &ScheduledJob{
		Project: project,
		JobDef: &extension.JobDefinition{
			Name:                     "build",
			NativeRegistryCredential: &extensionproto.TaskRegistryCredential{Registry: "go", Endpoint: "origin"},
		},
	}
}

func answerOf(t *testing.T, entry, host string) extensionproto.NativeRegistryCredential {
	t.Helper()
	value, ok := strings.CutPrefix(entry, extensionproto.NativeRegistryCredentialsEnv+"=")
	if !ok {
		t.Fatalf("entry %q does not set %s", entry, extensionproto.NativeRegistryCredentialsEnv)
	}
	credentials, err := extensionproto.ParseNativeRegistryCredentials(value)
	if err != nil {
		t.Fatal(err)
	}
	answer, ok := credentials[host]
	if !ok || len(credentials) != 1 {
		t.Fatalf("entry %q names %v, want %s alone", entry, credentials, host)
	}
	return answer
}

const goOrigin = `{"go": {"origin": "https://Go.Putnami.Dev/", "proxy": ["direct"]}}`

func TestNativeCredentialEnv_MaterializesEachHostOnceForTheWindow(t *testing.T) {
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	// An outer run's answers never reach the child.
	t.Setenv(extensionproto.NativeRegistryCredentialsEnv, "{}")
	calls := countSeamCalls(t, registrycred.Outcome{Kind: registrycred.KindMaterialized, Level: "debug", Message: "refreshed"})
	credentials := newNativeCredentials(t.TempDir())
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	credentials.now = func() time.Time { return now }
	ctx := withNativeCredentials(context.Background(), credentials)

	var wg sync.WaitGroup
	entries := make([]string, 16)
	for i := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entries[i] = nativeCredentialEnv(ctx, credentialJob(goOrigin))
		}()
	}
	wg.Wait()
	if got := seamCalls(calls, "go.putnami.dev"); got != 1 {
		t.Fatalf("16 jobs called the seam %d times, want once", got)
	}
	for _, entry := range entries {
		answer := answerOf(t, entry, "go.putnami.dev")
		if answer.Outcome != extensionproto.NativeCredentialMaterialized || answer.Message != "refreshed" {
			t.Fatalf("answer = %+v, want the materialized outcome and its line", answer)
		}
		if want := now.Add(registryproto.NativeCredentialValidity).Format(time.RFC3339); answer.ValidUntil != want {
			t.Fatalf("validUntil = %s, want %s", answer.ValidUntil, want)
		}
	}

	now = now.Add(registryproto.NativeCredentialValidity - time.Second)
	_ = nativeCredentialEnv(ctx, credentialJob(goOrigin))
	if got := seamCalls(calls, "go.putnami.dev"); got != 1 {
		t.Fatalf("a job inside the window called the seam again (%d calls)", got)
	}

	// The answer expires: the next jobs renew it once, together.
	now = now.Add(time.Second)
	for i := range entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entries[i] = nativeCredentialEnv(ctx, credentialJob(goOrigin))
		}()
	}
	wg.Wait()
	if got := seamCalls(calls, "go.putnami.dev"); got != 2 {
		t.Fatalf("an expired answer was renewed %d times, want once", got-1)
	}
	if answer := answerOf(t, entries[0], "go.putnami.dev"); answer.ValidUntil != now.Add(registryproto.NativeCredentialValidity).Format(time.RFC3339) {
		t.Fatalf("renewed validUntil = %s", answer.ValidUntil)
	}
}

func TestNativeCredentialEnv_AFailedAnswerHoldsForTheWindowToo(t *testing.T) {
	t.Setenv(extensionproto.OfflineDependenciesEnv, "")
	calls := countSeamCalls(t, registrycred.Outcome{Kind: registrycred.KindNotSignedIn, Level: "warn", Message: "not signed in"})
	ctx := withNativeCredentials(context.Background(), newNativeCredentials(t.TempDir()))
	first := nativeCredentialEnv(ctx, credentialJob(goOrigin))
	_ = nativeCredentialEnv(ctx, credentialJob(goOrigin))
	if got := seamCalls(calls, "go.putnami.dev"); got != 1 {
		t.Fatalf("a signed-out user was asked %d times, want once", got)
	}
	if answer := answerOf(t, first, "go.putnami.dev"); answer.Outcome != extensionproto.NativeCredentialNotSignedIn || answer.Message != "not signed in" {
		t.Fatalf("answer = %+v, want the warning the job holds", answer)
	}
}

func TestNativeCredentialEnv_CallsNothing(t *testing.T) {
	cases := map[string]struct {
		job     *ScheduledJob
		ctx     func() context.Context
		hosted  bool
		offline bool
		outcome registrycred.Outcome
	}{
		"a task that declares nothing": {job: &ScheduledJob{Project: &workspace.Project{}, JobDef: &extension.JobDefinition{}}},
		"no registries entry":          {job: credentialJob("")},
		"no origin member":             {job: credentialJob(`{"go": {"proxy": ["direct"]}}`)},
		"an empty origin":              {job: credentialJob(`{"go": {"origin": ""}}`)},
		"outside the scheduler":        {job: credentialJob(goOrigin), ctx: context.Background},
		"a hosted run":                 {job: credentialJob(goOrigin), hosted: true},
		"an offline run":               {job: credentialJob(goOrigin), offline: true},
		"a skipped call":               {job: credentialJob(goOrigin), outcome: registrycred.Outcome{Kind: registrycred.KindSkipped}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			outcome := tc.outcome
			if name != "a skipped call" {
				outcome = registrycred.Outcome{Kind: registrycred.KindMaterialized}
			}
			calls := countSeamCalls(t, outcome)
			t.Setenv(extensionproto.OfflineDependenciesEnv, "")
			if tc.offline {
				t.Setenv(extensionproto.OfflineDependenciesEnv, "1")
			}
			if tc.hosted {
				t.Cleanup(runcredential.SetForTest("run-bearer"))
			}
			ctx := withNativeCredentials(context.Background(), newNativeCredentials(t.TempDir()))
			if tc.ctx != nil {
				ctx = tc.ctx()
			}
			if entry := nativeCredentialEnv(ctx, tc.job); entry != "" {
				t.Fatalf("entry = %q, want none", entry)
			}
			wantCalls := int64(0)
			if name == "a skipped call" {
				wantCalls = 1
			}
			if got := seamCalls(calls, "go.putnami.dev"); got != wantCalls {
				t.Fatalf("the seam was called %d times, want %d", got, wantCalls)
			}
		})
	}
}

func TestHostOfEndpoint(t *testing.T) {
	for raw, want := range map[string]string{
		"https://Go.Putnami.Dev/":  "go.putnami.dev",
		"go.putnami.dev":           "go.putnami.dev",
		"https://reg.example:8443": "reg.example",
		"  ":                       "",
	} {
		if got := hostOfEndpoint(raw); got != want {
			t.Errorf("hostOfEndpoint(%q) = %q, want %q", raw, got, want)
		}
	}
}

// A cloud that refuses --for-run wrote nothing that holds for the window, and
// a failed call may succeed for the next job: the engine names neither to its
// jobs, which call the seam themselves, and asks again only after the window.
func TestNativeCredentialEnv_NamesNoHostForAnUnsupportedOrFailedCall(t *testing.T) {
	for _, kind := range []registrycred.Outcome{
		{Kind: registrycred.KindUnsupported, Level: "warn", Message: "this putnami cloud cannot write the credential"},
		{Kind: registrycred.KindFailed, Level: "warn", Message: "timed out"},
	} {
		t.Run(kind.Message, func(t *testing.T) {
			t.Setenv(extensionproto.OfflineDependenciesEnv, "")
			calls := countSeamCalls(t, kind)
			ctx := withNativeCredentials(context.Background(), newNativeCredentials(t.TempDir()))
			for range 3 {
				if entry := nativeCredentialEnv(ctx, credentialJob(goOrigin)); entry != "" {
					t.Fatalf("entry = %q, want none: the jobs call the seam themselves", entry)
				}
			}
			if got := seamCalls(calls, "go.putnami.dev"); got != 1 {
				t.Fatalf("the engine called the seam %d times inside the window, want once", got)
			}
		})
	}
}

func TestNativeCredentialChildEnv_DropsTheFactsOfThisRunsSpawns(t *testing.T) {
	for _, key := range []string{
		runner.BoundRequestEnv, ProvidersEnv, CallerDirEnv, InvocationDirEnv,
		cache.ObjectCacheSocketEnv, cache.CacheTrustEnv,
		hostrpcproto.SocketEnv, hostrpcproto.TokenEnv,
		extensionproto.JobCredentialFDEnv, extensionproto.NativeRegistryCredentialsEnv,
	} {
		env := nativeCredentialChildEnv([]string{"HOME=/home/u", key + "=x", "PUTNAMI_CLOUD_TOKEN=kept"})
		if !slices.Equal(env, []string{"HOME=/home/u", "PUTNAMI_CLOUD_TOKEN=kept"}) {
			t.Errorf("%s: env = %q, want it dropped and the rest kept", key, env)
		}
	}
}
