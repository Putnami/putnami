package hostenv

import (
	"go.putnami.dev/protocol/features/spectest"

	"slices"
	"strings"
	"testing"
)

// The variable this was reported against, plus the rest of the Knative block a
// guard could equally well key on, must be scrubbed.
func TestPlatformIdentityVars_CoversKnativeIdentity(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "host-identity-scrub", "the-list-covers-the-managed-runtime-identity-variables")
	for _, name := range []string{"K_SERVICE", "K_REVISION", "K_CONFIGURATION"} {
		if !IsPlatformIdentity(name) {
			t.Errorf("IsPlatformIdentity(%q) = false, want true", name)
		}
	}
}

// Criterion 2: identity is scrubbed, capability is not. Every name here is one
// an integration test, the build toolchain or the harness itself needs, and
// removing any of them would break a legitimate test rather than fix one.
func TestPlatformIdentityVars_ExcludesCapabilityBearingVars(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "host-identity-scrub", "credential-and-endpoint-bearing-variables-are-excluded")
	preserved := []string{
		// Credentials and ADC — explicitly out of scope.
		"GOOGLE_APPLICATION_CREDENTIALS",
		"AWS_ACCESS_KEY_ID",
		"AWS_SECRET_ACCESS_KEY",
		"AWS_SESSION_TOKEN",
		// Selectors and endpoints a client SDK needs to address an API.
		"AWS_REGION",
		"AWS_DEFAULT_REGION",
		"GOOGLE_CLOUD_PROJECT",
		"GCLOUD_PROJECT",
		"KUBERNETES_SERVICE_HOST",
		"KUBERNETES_SERVICE_PORT",
		// Harness contract: the test binding credential, the color flag, the
		// app environment, the CI marker, and the toolchain env the Go and
		// Python test paths depend on.
		"DATABASE_TEST_BINDINGS",
		"FORCE_COLOR",
		"APP_ENV",
		"CI",
		"CGO_ENABLED",
		"GOWORK",
		"GOFLAGS",
		"PYTHONPATH",
		"PATH",
		"HOME",
		"PORT",
		// Nothing PUTNAMI_* is ever a host platform signal.
		"PUTNAMI_WORKSPACE",
		"PUTNAMI_WORKING_DIR",
	}
	for _, name := range preserved {
		if IsPlatformIdentity(name) {
			t.Errorf("IsPlatformIdentity(%q) = true, want false: it carries a capability a test may need", name)
		}
	}
}

// The list is a reviewable document: sorted, duplicate-free, and free of empty
// or malformed names. A diff to it must read as one added line, not a reshuffle.
func TestPlatformIdentityVars_IsSortedAndUnique(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "host-identity-scrub", "the-list-is-sorted-and-unique")
	vars := PlatformIdentityVars()
	if len(vars) == 0 {
		t.Fatal("PlatformIdentityVars() is empty")
	}
	if !slices.IsSorted(vars) {
		t.Errorf("PlatformIdentityVars() is not sorted: %v", vars)
	}
	seen := map[string]bool{}
	for _, name := range vars {
		if seen[name] {
			t.Errorf("duplicate entry %q", name)
		}
		seen[name] = true
		if name == "" || strings.ContainsAny(name, "= ") {
			t.Errorf("malformed variable name %q", name)
		}
	}
}

func TestPlatformIdentityVars_ReturnsIndependentCopy(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "host-identity-scrub", "the-list-is-returned-as-an-independent-copy")
	first := PlatformIdentityVars()
	if len(first) == 0 {
		t.Fatal("PlatformIdentityVars() is empty")
	}
	first[0] = "MUTATED"
	if second := PlatformIdentityVars(); second[0] == "MUTATED" {
		t.Error("PlatformIdentityVars() shares backing storage with its caller")
	}
}

func TestIsPlatformIdentity_IsCaseSensitive(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "host-identity-scrub", "matching-is-case-sensitive")
	if IsPlatformIdentity("k_service") {
		t.Error(`IsPlatformIdentity("k_service") = true, want false: matching is exact`)
	}
}

func TestScrubPlatformIdentity(t *testing.T) {
	t.Run("removes every platform identity entry", func(t *testing.T) {
		env := []string{
			"K_SERVICE=ci-worker",
			"K_REVISION=ci-worker-00042-abc",
			"K_CONFIGURATION=ci-worker",
			"GAE_ENV=standard",
			"AWS_LAMBDA_FUNCTION_NAME=fn",
			"_HANDLER=index.handler",
			"PATH=/usr/bin",
		}
		got := ScrubPlatformIdentity(env)
		if want := []string{"PATH=/usr/bin"}; !slices.Equal(got, want) {
			t.Errorf("ScrubPlatformIdentity() = %v, want %v", got, want)
		}
	})

	t.Run("preserves everything else byte for byte", func(t *testing.T) {
		env := []string{
			"K_SERVICE=ci-worker",
			"FORCE_COLOR=1",
			`DATABASE_TEST_BINDINGS={"protocolVersion":1,"databases":{}}`,
			"APP_ENV=test",
			"CGO_ENABLED=1",
			"GOWORK=/ws/go.work",
			"PYTHONPATH=/ws/pkg:/ws",
			"GOOGLE_APPLICATION_CREDENTIALS=/secrets/adc.json",
			"GOOGLE_CLOUD_PROJECT=my-project",
			"AWS_ACCESS_KEY_ID=AKIA",
			"PUTNAMI_WORKSPACE=/ws",
			"PORT=8080",
		}
		got := ScrubPlatformIdentity(env)
		if want := env[1:]; !slices.Equal(got, want) {
			t.Errorf("ScrubPlatformIdentity() = %v, want %v", got, want)
		}
	})

	t.Run("is a no-op when no platform variable is present", func(t *testing.T) {
		env := []string{"PATH=/usr/bin", "HOME=/home/dev", "CI=true"}
		got := ScrubPlatformIdentity(env)
		if !slices.Equal(got, env) {
			t.Errorf("ScrubPlatformIdentity() = %v, want %v", got, env)
		}
	})

	t.Run("keeps a value that itself contains an equals sign", func(t *testing.T) {
		env := []string{"GOFLAGS=-ldflags=-s -w", "K_SERVICE=ci"}
		got := ScrubPlatformIdentity(env)
		if want := []string{"GOFLAGS=-ldflags=-s -w"}; !slices.Equal(got, want) {
			t.Errorf("ScrubPlatformIdentity() = %v, want %v", got, want)
		}
	})

	t.Run("does not match a name that is only a prefix", func(t *testing.T) {
		env := []string{"K_SERVICE_ACCOUNT=svc@example.com", "MY_K_SERVICE=x"}
		if got := ScrubPlatformIdentity(env); !slices.Equal(got, env) {
			t.Errorf("ScrubPlatformIdentity() = %v, want %v", got, env)
		}
	})

	t.Run("passes through an entry that is not an assignment", func(t *testing.T) {
		env := []string{"NOT_AN_ASSIGNMENT", "K_SERVICE=ci"}
		got := ScrubPlatformIdentity(env)
		if want := []string{"NOT_AN_ASSIGNMENT"}; !slices.Equal(got, want) {
			t.Errorf("ScrubPlatformIdentity() = %v, want %v", got, want)
		}
	})

	t.Run("tolerates an empty environment", func(t *testing.T) {
		if got := ScrubPlatformIdentity(nil); got != nil {
			t.Errorf("ScrubPlatformIdentity(nil) = %v, want nil", got)
		}
		if got := ScrubPlatformIdentity([]string{}); got != nil {
			t.Errorf("ScrubPlatformIdentity([]) = %v, want nil", got)
		}
	})

	t.Run("does not alias its input", func(t *testing.T) {
		env := []string{"PATH=/usr/bin", "K_SERVICE=ci"}
		got := ScrubPlatformIdentity(env)
		got[0] = "PATH=/mutated"
		if env[0] != "PATH=/usr/bin" {
			t.Errorf("input mutated to %q", env[0])
		}
	})
}
