package runcredential

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"

	"go.putnami.dev/tooling/cli/internal/cmderr"
)

const testBearer = "run-credential-bearer-7f3a"

// unreachable fails the test when capture denies inspection or reads a
// descriptor it should have refused first.
func unreachable(t *testing.T) (func() error, func(int) ([]byte, error)) {
	t.Helper()
	deny := func() error {
		t.Error("capture denied inspection before refusing the flag")
		return nil
	}
	read := func(fd int) ([]byte, error) {
		t.Errorf("capture read descriptor %d before refusing the flag", fd)
		return nil, nil
	}
	return deny, read
}

func TestCaptureWithoutTheFlagDoesNothing(t *testing.T) {
	t.Parallel()
	deny, read := unreachable(t)
	for _, args := range [][]string{nil, {"build", "--projects", "api"}, {"test", "--", "-run", "X"}, {"--credential-fdx", "3"}} {
		c, err := capture(args, "linux", deny, read)
		if c != nil || err != nil {
			t.Errorf("capture(%q) = %v, %v; want nothing", args, c, err)
		}
	}
}

func TestCaptureRefusesAMalformedFlag(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args []string
		want string
	}{
		"twice":        {[]string{"build", "--credential-fd", "3", "--credential-fd=4"}, "--credential-fd is given 2 times"},
		"no value":     {[]string{"build", "--credential-fd"}, "flag --credential-fd requires a value"},
		"not a number": {[]string{"build", "--credential-fd", "three"}, `invalid --credential-fd value "three"`},
		"empty":        {[]string{"build", "--credential-fd="}, `invalid --credential-fd value ""`},
		"negative":     {[]string{"build", "--credential-fd=-1"}, `invalid --credential-fd value "-1"`},
		"stdin":        {[]string{"build", "--credential-fd", "0"}, "invalid --credential-fd value 0: descriptors 0, 1 and 2 are the standard streams"},
		"stderr":       {[]string{"build", "--credential-fd=2"}, "invalid --credential-fd value 2"},
	}
	for name, tc := range cases {
		deny, read := unreachable(t)
		c, err := capture(tc.args, "darwin", deny, read)
		if c != nil || err == nil {
			t.Errorf("%s: capture = %v, %v; want a usage error", name, c, err)
			continue
		}
		if !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("%s: %v is not a usage error", name, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q, want it to contain %q", name, err, tc.want)
		}
	}
}

func TestCaptureRefusesTheFlagOnWindows(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{{"build", "--credential-fd", "3"}, {"build", "--credential-fd"}, {"build", "--credential-fd=3", "--credential-fd=4"}} {
		deny, read := unreachable(t)
		c, err := capture(args, "windows", deny, read)
		if c != nil || err == nil || !errors.Is(err, cmderr.ErrUsage) {
			t.Fatalf("capture(%q) on windows = %v, %v; want a usage error", args, c, err)
		}
		if err.Error() != "--credential-fd is not supported on Windows" {
			t.Errorf("capture(%q) on windows: %q", args, err)
		}
	}
}

func TestCaptureDeniesInspectionBeforeItReads(t *testing.T) {
	t.Parallel()
	var steps []string
	deny := func() error { steps = append(steps, "deny"); return nil }
	read := func(fd int) ([]byte, error) {
		steps = append(steps, fmt.Sprintf("read %d", fd))
		return []byte(testBearer + "\n"), nil
	}
	c, err := capture([]string{"build", "--", "--credential-fd", "7"}, "linux", deny, read)
	if err != nil {
		t.Fatal(err)
	}
	if c.bearer != testBearer {
		t.Errorf("bearer = %q, want %q", c.bearer, testBearer)
	}
	if want := []string{"deny", "read 7"}; !reflect.DeepEqual(steps, want) {
		t.Errorf("steps = %q, want %q", steps, want)
	}
}

func TestCaptureFailuresNameTheDescriptorAndNotTheBytes(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		deny func() error
		read func(int) ([]byte, error)
		want string
	}{
		"deny fails": {
			deny: func() error { return errors.New("prctl refused") },
			read: func(int) ([]byte, error) { return []byte(testBearer), nil },
			want: "--credential-fd 5: prctl refused",
		},
		"read fails": {
			deny: func() error { return nil },
			read: func(int) ([]byte, error) { return nil, errors.New("descriptor 5 is not open") },
			want: "--credential-fd 5: descriptor 5 is not open",
		},
		"whitespace": {
			deny: func() error { return nil },
			read: func(int) ([]byte, error) { return []byte(testBearer + " " + testBearer), nil },
			want: "--credential-fd 5: the credential is empty, holds whitespace, or is not UTF-8",
		},
		"overflow": {
			deny: func() error { return nil },
			read: func(int) ([]byte, error) { return []byte(testBearer + strings.Repeat("x", MaxBytes)), nil },
			want: "--credential-fd 5: the credential exceeds 16384 bytes",
		},
	}
	for name, tc := range cases {
		c, err := capture([]string{"build", "--credential-fd=5"}, "linux", tc.deny, tc.read)
		if c != nil || err == nil || !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("%s: capture = %v, %v; want a usage error", name, c, err)
			continue
		}
		if err.Error() != tc.want {
			t.Errorf("%s: error %q, want %q", name, err, tc.want)
		}
		if strings.Contains(err.Error(), testBearer) {
			t.Errorf("%s: the error shows the credential: %q", name, err)
		}
	}
}

func TestParseBearer(t *testing.T) {
	t.Parallel()
	full := strings.Repeat("a", MaxBytes)
	cases := []struct {
		data string
		want string
		ok   bool
	}{
		{"tok", "tok", true},
		{"tok\n", "tok", true},
		{"tok\r\n", "tok", true},
		{full, full, true},
		{full[:MaxBytes-1] + "\n", full[:MaxBytes-1], true},
		{full + "\n", "", false},
		{full + "a", "", false},
		{"tok\n\n", "", false},
		{"tok\r", "", false},
		{" tok", "", false},
		{"tok tok", "", false},
		{"to\tk", "", false},
		{"", "", false},
		{"\n", "", false},
		{"\r\n", "", false},
	}
	for _, tc := range cases {
		got, err := parseBearer([]byte(tc.data))
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("parseBearer(%d bytes %.12q) = %.12q, %v; want %.12q, ok %v", len(tc.data), tc.data, got, err, tc.want, tc.ok)
		}
	}
}

func TestDescriptor(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]int{"3": 3, "10": 10, "+4": 4, "03": 3} {
		if got, err := Descriptor(value); err != nil || got != want {
			t.Errorf("Descriptor(%q) = %d, %v; want %d", value, got, err, want)
		}
	}
	for _, value := range []string{"", "x", "3.0", "-3", "0", "1", "2", "99999999999999999999"} {
		if _, err := Descriptor(value); err == nil || !errors.Is(err, cmderr.ErrUsage) {
			t.Errorf("Descriptor(%q) = %v, want a usage error", value, err)
		}
	}
}

func TestACredentialNeverFormatsItsBearer(t *testing.T) {
	t.Parallel()
	c := credential{bearer: testBearer}
	for _, verb := range []string{"%v", "%+v", "%#v", "%s", "%q", "%x", "%X", "%d", "%10.3v"} {
		for _, value := range []any{c, &c, struct{ C credential }{c}, []credential{c}} {
			got := fmt.Sprintf(verb, value)
			if strings.Contains(got, testBearer) || strings.Contains(got, fmt.Sprintf("%x", testBearer)) {
				t.Errorf("Sprintf(%q, %T) = %q shows the bearer", verb, value, got)
			}
			if !strings.Contains(got, Redacted) {
				t.Errorf("Sprintf(%q, %T) = %q, want %s", verb, value, got, Redacted)
			}
		}
	}
}

func TestSetForTestHoldsAndRestores(t *testing.T) {
	restore := SetForTest(testBearer)
	bearer, ok := Current()
	if !ok || bearer != testBearer || !Hosted() {
		t.Fatalf("after SetForTest: Current() = %q, %v; Hosted() = %v", bearer, ok, Hosted())
	}
	// A process that holds a credential does not capture another one.
	if err := Capture([]string{"build", "--credential-fd", "nonsense"}); err != nil {
		t.Errorf("Capture while holding a credential = %v, want nil", err)
	}
	clearHeld := SetForTest("")
	if _, ok := Current(); ok || Hosted() {
		t.Error("SetForTest(\"\") left a credential held")
	}
	clearHeld()
	restore()
	if _, ok := Current(); ok || Hosted() {
		t.Error("restore left a credential held")
	}
}

func TestDropCredentialVariables(t *testing.T) {
	for _, name := range credentialVariables {
		const value = "credential-value-91c2"
		t.Setenv(name, value)
		var stderr bytes.Buffer
		dropCredentialVariables(&stderr)
		if _, present := os.LookupEnv(name); present {
			t.Errorf("%s is still set", name)
		}
		want := "putnami: --credential-fd: removed " + name + " from the environment; the run credential replaces it\n"
		if stderr.String() != want {
			t.Errorf("stderr = %q, want %q", stderr.String(), want)
		}

		t.Setenv(name, "")
		stderr.Reset()
		dropCredentialVariables(&stderr)
		if _, present := os.LookupEnv(name); present || stderr.Len() != 0 {
			t.Errorf("an empty %s: present %v, stderr %q; want it removed silently", name, present, stderr.String())
		}

		stderr.Reset()
		dropCredentialVariables(&stderr)
		if stderr.Len() != 0 {
			t.Errorf("an absent %s: stderr %q, want nothing", name, stderr.String())
		}
	}
}

func TestGuardDeniesInspectionOnlyWithCredentials(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		hosted, providers, want bool
	}{
		{false, false, false},
		{true, false, true},
		{false, true, true},
		{true, true, true},
	} {
		denied := false
		if err := guard(tc.hosted, tc.providers, func() error { denied = true; return nil }); err != nil {
			t.Fatal(err)
		}
		if denied != tc.want {
			t.Errorf("guard(hosted %v, providers %v) denied inspection: %v, want %v", tc.hosted, tc.providers, denied, tc.want)
		}
	}
	err := guard(true, false, func() error { return errors.New("prctl refused") })
	if err == nil || err.Error() != "protect the credentials of this process: prctl refused" {
		t.Errorf("guard with a failing deny = %v", err)
	}
}

func TestWithDescriptorNamesTheNewDescriptor(t *testing.T) {
	t.Parallel()
	cases := []struct {
		argv, want []string
	}{
		{[]string{"putnami", "build", "--credential-fd", "3", "--projects", "api"}, []string{"putnami", "build", "--credential-fd", "9", "--projects", "api"}},
		{[]string{"putnami", "build", "--credential-fd=3"}, []string{"putnami", "build", "--credential-fd=9"}},
		{[]string{"putnami", "test", "--", "--credential-fd", "3"}, []string{"putnami", "test", "--", "--credential-fd", "9"}},
		{[]string{"putnami", "upgrade", "--extensions"}, []string{"putnami", "upgrade", "--extensions", "--credential-fd=9"}},
		{[]string{"putnami", "build", "--credential-fd"}, []string{"putnami", "build", "--credential-fd=9"}},
		{[]string{"--credential-fd"}, []string{"--credential-fd", "--credential-fd=9"}},
	}
	for _, tc := range cases {
		original := append([]string(nil), tc.argv...)
		got := withDescriptor(tc.argv, 9)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("withDescriptor(%q) = %q, want %q", tc.argv, got, tc.want)
		}
		if !reflect.DeepEqual(tc.argv, original) {
			t.Errorf("withDescriptor modified its argument: %q", tc.argv)
		}
	}
}

func TestWithoutFlagDropsEveryOccurrenceAndItsValue(t *testing.T) {
	t.Parallel()
	cases := []struct {
		args, want []string
	}{
		{nil, nil},
		{[]string{"--credential-fd", "3"}, nil},
		{[]string{"--credential-fd=3"}, nil},
		{[]string{"build", "--credential-fd", "3", "--projects", "api"}, []string{"build", "--projects", "api"}},
		{[]string{"--credential-fd"}, nil},
		{[]string{"test"}, []string{"test"}},
	}
	for _, tc := range cases {
		if got := WithoutFlag(tc.args); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("WithoutFlag(%q) = %q, want %q", tc.args, got, tc.want)
		}
	}
}
