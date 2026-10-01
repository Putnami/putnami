package jobs

import (
	"reflect"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	runtimeproto "go.putnami.dev/protocol/runtime"
)

// A provider launched before CaptureProcessCapabilities gets none of the
// capability transport capture takes. The cloud token goes with it only when
// AFTER or a release-plan callback opts in; a token-only environment keeps it,
// as every job's does.
func TestProviderEnvironmentHoldsNoUncapturedCapability(t *testing.T) {
	t.Parallel()
	token := extensionproto.CloudTokenEnv + "=cloud-token"
	transport := []string{
		InternalReleaseSetProviderCapabilityEnv + "={}",
		runtimeproto.ReleaseSetPublishedImagesFileEnv + "=/tmp/images",
		runtimeproto.ReleaseSetMembersFileEnv + "=/tmp/members",
	}
	for _, c := range []struct {
		name string
		env  []string
		want []string
	}{
		{
			name: "after opts in",
			env:  append([]string{"PATH=/bin", token, extensionproto.CloudCapabilityAfterEnv + "=[\"publish\"]"}, transport...),
			want: []string{"PATH=/bin"},
		},
		{
			name: "a release-plan callback opts in",
			env:  []string{"PATH=/bin", token, InternalReleasePlanCallbackEnv + "=callback"},
			want: []string{"PATH=/bin"},
		},
		{
			name: "token only",
			env:  []string{"PATH=/bin", token, extensionproto.CloudCapabilityAfterEnv + "="},
			want: []string{"PATH=/bin", token},
		},
		{
			name: "after capture",
			env:  []string{"PATH=/bin"},
			want: []string{"PATH=/bin"},
		},
	} {
		if got := withoutUncapturedCapabilities(c.env); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: withoutUncapturedCapabilities = %q, want %q", c.name, got, c.want)
		}
	}
}
