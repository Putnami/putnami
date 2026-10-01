//go:build linux

package runcredential

import (
	"strconv"
	"testing"

	"golang.org/x/sys/unix"
)

// A process that captured the run credential is non-dumpable on Linux.
const (
	dumpableAfterCapture = "0"
	dumpableUntouched    = "1"
)

func dumpableState() string {
	value, err := unix.PrctlRetInt(unix.PR_GET_DUMPABLE, 0, 0, 0, 0)
	if err != nil {
		return err.Error()
	}
	return strconv.Itoa(value)
}

func TestGuardMarksTheEngineNonDumpable(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args    []string
		content []byte
		env     string
		want    string
	}{
		"off":       {args: []string{"build"}, env: "RUNCREDENTIAL_HELPER_PROVIDERS=0", want: dumpableUntouched},
		"hosted":    {args: []string{"build", "--credential-fd", "3"}, content: []byte(testBearer), env: "RUNCREDENTIAL_HELPER_PROVIDERS=0", want: dumpableAfterCapture},
		"providers": {args: []string{"build"}, env: "RUNCREDENTIAL_HELPER_PROVIDERS=1", want: dumpableAfterCapture},
	}
	for name, tc := range cases {
		result := runHelper(t, "guard", tc.args, tc.content, tc.env)
		if result.fields["err"] != "<nil>" || result.fields["dumpable"] != tc.want {
			t.Errorf("%s: err %q, dumpable %q; want no error and %s", name, result.fields["err"], result.fields["dumpable"], tc.want)
		}
	}
	if got := dumpableState(); got != dumpableUntouched {
		t.Errorf("the test process is not dumpable: %s", got)
	}
}
