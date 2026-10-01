package runcredential

import (
	"errors"
	"strings"
	"testing"
)

// A CLI whose environment carries HostedFetchEnv, with any
// value, refuses with ErrInHostedFetch, and names the variable. Without it,
// nothing changes.
func TestRefuseInHostedFetch(t *testing.T) {
	t.Parallel()
	for _, value := range []string{"1", "0", "yes"} {
		err := RefuseInHostedFetch(func(name string) string {
			if name == HostedFetchEnv {
				return value
			}
			return ""
		})
		if !errors.Is(err, ErrInHostedFetch) || !strings.Contains(err.Error(), HostedFetchEnv) {
			t.Errorf("%s=%s: RefuseInHostedFetch = %v, want ErrInHostedFetch naming %s", HostedFetchEnv, value, err, HostedFetchEnv)
		}
	}
	if err := RefuseInHostedFetch(func(string) string { return "" }); err != nil {
		t.Errorf("without %s: RefuseInHostedFetch = %v, want nil", HostedFetchEnv, err)
	}
}
