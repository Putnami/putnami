package runcredential

import (
	"reflect"
	"slices"
	"testing"

	protocolcli "go.putnami.dev/protocol/cli"
	extensionproto "go.putnami.dev/protocol/extension"
)

// Without a run credential a child's environment is the one it was given,
// byte for byte: an inherited offline signal and a cache token pass through
// as they do today.
func TestChildEnvWithoutARunCredentialIsUnchanged(t *testing.T) {
	restore := SetForTest("")
	t.Cleanup(restore)
	env := []string{
		"PATH=/bin",
		CacheTokenEnv + "=cache-token",
		extensionproto.OfflineDependenciesEnv + "=1",
		"LAST=x",
	}
	for _, fetches := range []bool{false, true} {
		got := ChildEnv(slices.Clone(env), fetches)
		if !reflect.DeepEqual(got, env) {
			t.Errorf("ChildEnv(fetches=%v) = %q, want %q unchanged", fetches, got, env)
		}
	}
}

// A hosted run removes the cache, cloud and reporter tokens from every child,
// sets the offline signal last so that no earlier entry overrides it, and gives
// the fetch no offline signal at all.
func TestChildEnvOnAHostedRun(t *testing.T) {
	restore := SetForTest(testBearer)
	t.Cleanup(restore)
	offline := extensionproto.OfflineDependenciesEnv
	cases := map[string]struct {
		env     []string
		fetches bool
		want    []string
	}{
		"a job": {
			env:  []string{"PATH=/bin", CacheTokenEnv + "=cache-token", CloudTokenEnv + "=cloud-token", "LAST=x"},
			want: []string{"PATH=/bin", "LAST=x", offline + "=1"},
		},
		"a job that declares the reporter tokens": {
			env: []string{"PATH=/bin", protocolcli.SessionReporterTokenEnv + "=session-token",
				protocolcli.LogReporterTokenEnv + "=log-token", "LAST=x"},
			want: []string{"PATH=/bin", "LAST=x", offline + "=1"},
		},
		"a job whose own entry says online": {
			env:  []string{"PATH=/bin", offline + "=0", "LAST=x"},
			want: []string{"PATH=/bin", "LAST=x", offline + "=1"},
		},
		"the fetch": {
			env:     []string{"PATH=/bin", offline + "=1", CacheTokenEnv + "=cache-token", CloudTokenEnv + "=cloud-token", "LAST=x"},
			fetches: true,
			want:    []string{"PATH=/bin", "LAST=x"},
		},
		"the fetch with nothing to remove": {
			env:     []string{"PATH=/bin"},
			fetches: true,
			want:    []string{"PATH=/bin"},
		},
	}
	for name, tc := range cases {
		given := slices.Clone(tc.env)
		got := ChildEnv(given, tc.fetches)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: ChildEnv = %q, want %q", name, got, tc.want)
		}
		if !reflect.DeepEqual(given, tc.env) {
			t.Errorf("%s: ChildEnv modified its argument: %q", name, given)
		}
	}
}
