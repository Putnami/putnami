//go:build !windows

package runcredential

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"

	extensionproto "go.putnami.dev/protocol/extension"
	"golang.org/x/sys/unix"
)

// The tests below run the CLI's side of the protocol in a child of this test
// binary, started the way a hosted runner starts the CLI: the credential on
// descriptor 3 (exec.Cmd.ExtraFiles) and the flag in its arguments. The child
// reports what it holds as a digest, never as the bearer.
const (
	helperRoleEnv  = "RUNCREDENTIAL_HELPER_ROLE"
	helperArgsEnv  = "RUNCREDENTIAL_HELPER_ARGS"
	helperRun      = "-test.run=^TestRunCredentialHelper$"
	helperPrefix   = "runcredential: "
	helperNoReport = "-"
	// helperMarkEnv set to "1" makes the exec roles record repository code,
	// named helperMarkReason, before they call Exec.
	helperMarkEnv    = "RUNCREDENTIAL_HELPER_MARK"
	helperMarkReason = "hook hooks.commands.build.before"
)

func digest(bearer string) string {
	sum := sha256.Sum256([]byte(bearer))
	return hex.EncodeToString(sum[:])
}

// descriptorState reports whether fd is open in this process.
func descriptorState(fd int) string {
	if _, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0); errors.Is(err, unix.EBADF) {
		return "closed"
	}
	return "open"
}

// report prints what this process holds after Capture returned err.
func report(err error, fd int) {
	sum := helperNoReport
	if bearer, ok := Current(); ok {
		sum = digest(bearer)
	}
	_, token := os.LookupEnv(CacheTokenEnv)
	_, cloudToken := os.LookupEnv(CloudTokenEnv)
	offline, declared := os.LookupEnv(extensionproto.OfflineDependenciesEnv)
	if !declared {
		offline = helperNoReport
	}
	fields := []string{
		"err=" + strconv.Quote(fmt.Sprint(err)),
		"hosted=" + strconv.FormatBool(Hosted()),
		"digest=" + sum,
		"descriptor=" + descriptorState(fd),
		"fd=" + strconv.Itoa(fd),
		"cache-token=" + strconv.FormatBool(token),
		"cloud-token=" + strconv.FormatBool(cloudToken),
		"offline=" + strconv.Quote(offline),
		"dumpable=" + dumpableState(),
		"args=" + strconv.Quote(strings.Join(os.Args[1:], " ")),
	}
	fmt.Println(helperPrefix + strings.Join(fields, " "))
}

// flagDescriptor returns the descriptor Flag names in args, or -1.
func flagDescriptor(args []string) int {
	values, _ := flagValues(args)
	if len(values) != 1 {
		return -1
	}
	fd, err := strconv.Atoi(values[0])
	if err != nil {
		return -1
	}
	return fd
}

// TestRunCredentialHelper is not a test: it is one role of the child
// processes the tests below start, and it returns at once unless a test
// started it as one.
func TestRunCredentialHelper(t *testing.T) {
	role := os.Getenv(helperRoleEnv)
	if role == "" {
		return
	}
	var args []string
	if raw := os.Getenv(helperArgsEnv); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			t.Fatal(err)
		}
	}
	switch role {
	case "capture":
		err := Capture(args)
		report(err, flagDescriptor(args))
	case "guard":
		err := Capture(args)
		guardErr := Guard(os.Getenv("RUNCREDENTIAL_HELPER_PROVIDERS") == "1")
		report(errors.Join(err, guardErr), flagDescriptor(args))
	case "exec", "exec-none":
		if role == "exec" {
			if err := Capture(args); err != nil {
				report(err, flagDescriptor(args))
				return
			}
		}
		next := []string{os.Args[0], helperRun, "--"}
		if os.Getenv("RUNCREDENTIAL_HELPER_KEEP_FLAG") == "1" {
			next = append(next, "build", Flag, "3")
		}
		// syscall.Exec keeps the first of two entries of one name.
		var env []string
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, helperRoleEnv+"=") && !strings.HasPrefix(entry, helperArgsEnv+"=") {
				env = append(env, entry)
			}
		}
		if os.Getenv(helperMarkEnv) == "1" {
			MarkRepositoryCodeStarted(helperMarkReason)
		}
		err := Exec(os.Args[0], next, append(env, helperRoleEnv+"=next"))
		report(fmt.Errorf("exec returned: %w", err), -1)
	case "next":
		// The arguments of this image are the ones Exec handed it.
		own := os.Args[1:]
		err := Capture(own)
		report(err, flagDescriptor(own))
	}
}

type helperResult struct {
	fields map[string]string
	stdout string
	stderr string
}

// runHelper starts this test binary in role, with args, content on descriptor
// 3 when content is not nil, and env on top of an environment that carries no
// cache token and no cloud token.
func runHelper(t *testing.T, role string, args []string, content []byte, env ...string) helperResult {
	t.Helper()
	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], helperRun)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, CacheTokenEnv+"=") && !strings.HasPrefix(entry, CloudTokenEnv+"=") {
			cmd.Env = append(cmd.Env, entry)
		}
	}
	cmd.Env = append(cmd.Env, helperRoleEnv+"="+role, helperArgsEnv+"="+string(encoded))
	cmd.Env = append(cmd.Env, env...)
	if content != nil {
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			_, _ = w.Write(content)
			_ = w.Close()
		}()
		defer func() { _ = r.Close() }()
		cmd.ExtraFiles = []*os.File{r}
	}
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("run the %s helper: %v\nstdout:\n%s\nstderr:\n%s", role, err, stdout.String(), stderr.String())
	}
	result := helperResult{fields: map[string]string{}, stdout: stdout.String(), stderr: stderr.String()}
	for line := range strings.Lines(result.stdout) {
		fields, ok := strings.CutPrefix(strings.TrimSpace(line), helperPrefix)
		if !ok {
			continue
		}
		for len(fields) > 0 {
			key, rest, _ := strings.Cut(fields, "=")
			var value string
			if strings.HasPrefix(rest, `"`) {
				quoted, err := strconv.QuotedPrefix(rest)
				if err != nil {
					t.Fatalf("parse %q: %v", line, err)
				}
				value, _ = strconv.Unquote(quoted)
				rest = rest[len(quoted):]
			} else {
				value, rest, _ = strings.Cut(rest, " ")
			}
			result.fields[key] = value
			fields = strings.TrimPrefix(rest, " ")
		}
	}
	if len(result.fields) == 0 {
		t.Fatalf("the %s helper reported nothing\nstdout:\n%s\nstderr:\n%s", role, result.stdout, result.stderr)
	}
	return result
}

func (r helperResult) expect(t *testing.T, want map[string]string) {
	t.Helper()
	for key, value := range want {
		if r.fields[key] != value {
			t.Errorf("%s = %q, want %q\nstdout:\n%s\nstderr:\n%s", key, r.fields[key], value, r.stdout, r.stderr)
		}
	}
}

func (r helperResult) neverShows(t *testing.T, secret string) {
	t.Helper()
	if strings.Contains(r.stdout, secret) || strings.Contains(r.stderr, secret) {
		t.Errorf("the credential reached the output\nstdout:\n%s\nstderr:\n%s", r.stdout, r.stderr)
	}
}

func TestCaptureReadsAndClosesTheDescriptor(t *testing.T) {
	t.Parallel()
	for _, args := range [][]string{
		{"build", "--credential-fd", "3"},
		{"build", "--credential-fd=3", "--projects", "api"},
		{"test", "--", "--credential-fd", "3"},
	} {
		result := runHelper(t, "capture", args, []byte(testBearer+"\n"))
		result.expect(t, map[string]string{
			"err": "<nil>", "hosted": "true", "digest": digest(testBearer),
			"descriptor": "closed", "cache-token": "false", "offline": "1", "dumpable": dumpableAfterCapture,
		})
		result.neverShows(t, testBearer)
	}
}

func TestCaptureWithoutTheFlagLeavesTheProcessAlone(t *testing.T) {
	t.Parallel()
	result := runHelper(t, "capture", []string{"build"}, []byte(testBearer), CacheTokenEnv+"=kept")
	result.expect(t, map[string]string{
		"err": "<nil>", "hosted": "false", "digest": helperNoReport, "cache-token": "true", "offline": helperNoReport, "dumpable": dumpableUntouched,
	})
}

// A process that holds the run credential declares itself offline, whatever
// value it inherited, so the cache keys that read the variable and every
// process it starts see "1".
func TestCaptureDeclaresTheProcessOffline(t *testing.T) {
	t.Parallel()
	for _, inherited := range []string{"", "0", "1"} {
		var env []string
		if inherited != "" {
			env = append(env, extensionproto.OfflineDependenciesEnv+"="+inherited)
		}
		hosted := runHelper(t, "capture", []string{"build", "--credential-fd", "3"}, []byte(testBearer), env...)
		hosted.expect(t, map[string]string{"err": "<nil>", "hosted": "true", "offline": "1"})
		want := inherited
		if want == "" {
			want = helperNoReport
		}
		local := runHelper(t, "capture", []string{"build"}, nil, env...)
		local.expect(t, map[string]string{"err": "<nil>", "hosted": "false", "offline": want})
	}
}

func TestCaptureReplacesTheFrameworkTokens(t *testing.T) {
	t.Parallel()
	const cacheToken = "cache-token-value-5e1d"
	const cloudToken = "cloud-token-value-3b7a"
	result := runHelper(t, "capture", []string{"build", "--credential-fd", "3"}, []byte(testBearer),
		CacheTokenEnv+"="+cacheToken, CloudTokenEnv+"="+cloudToken)
	result.expect(t, map[string]string{"err": "<nil>", "hosted": "true", "cache-token": "false", "cloud-token": "false"})
	for _, name := range []string{CacheTokenEnv, CloudTokenEnv} {
		notice := "putnami: --credential-fd: removed " + name + " from the environment; the run credential replaces it\n"
		if strings.Count(result.stderr, notice) != 1 {
			t.Errorf("stderr = %q, want the %s notice once", result.stderr, name)
		}
	}
	result.neverShows(t, cacheToken)
	result.neverShows(t, cloudToken)
	result.neverShows(t, testBearer)

	local := runHelper(t, "capture", []string{"build"}, nil, CacheTokenEnv+"="+cacheToken, CloudTokenEnv+"="+cloudToken)
	local.expect(t, map[string]string{"err": "<nil>", "hosted": "false", "cache-token": "true", "cloud-token": "true"})
}

func TestCaptureRefusesAnUnusableDescriptor(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		args    []string
		content []byte
		want    string
	}{
		"overflow": {
			args:    []string{"build", "--credential-fd", "3"},
			content: []byte(strings.Repeat(testBearer, MaxBytes/len(testBearer)+1)),
			want:    "--credential-fd 3: the credential exceeds 16384 bytes",
		},
		"whitespace": {
			args:    []string{"build", "--credential-fd", "3"},
			content: []byte(testBearer + " " + testBearer + "\n"),
			want:    "--credential-fd 3: the credential is empty, holds whitespace, or is not UTF-8",
		},
		"empty": {
			args:    []string{"build", "--credential-fd", "3"},
			content: []byte{},
			want:    "--credential-fd 3: the credential is empty, holds whitespace, or is not UTF-8",
		},
		"not open": {
			args: []string{"build", "--credential-fd", "97"},
			want: "--credential-fd 97: descriptor 97 is not open",
		},
		"standard stream": {
			args:    []string{"build", "--credential-fd", "1"},
			content: []byte(testBearer),
			want:    "invalid --credential-fd value 1: descriptors 0, 1 and 2 are the standard streams",
		},
	}
	for name, tc := range cases {
		result := runHelper(t, "capture", tc.args, tc.content)
		if result.fields["err"] != tc.want || result.fields["hosted"] != "false" {
			t.Errorf("%s: err %q, hosted %s; want %q and nothing held", name, result.fields["err"], result.fields["hosted"], tc.want)
		}
		result.neverShows(t, testBearer)
	}
}

func TestExecHandsTheCredentialToTheNextImage(t *testing.T) {
	t.Parallel()
	for name, env := range map[string]string{
		"the flag is appended": "RUNCREDENTIAL_HELPER_KEEP_FLAG=0",
		"the flag is renamed":  "RUNCREDENTIAL_HELPER_KEEP_FLAG=1",
	} {
		result := runHelper(t, "exec", []string{"build", "--credential-fd", "3"}, []byte(testBearer+"\r\n"), env)
		result.expect(t, map[string]string{
			"err": "<nil>", "hosted": "true", "digest": digest(testBearer), "descriptor": "closed",
		})
		fd, err := strconv.Atoi(result.fields["fd"])
		if err != nil || fd < firstDescriptor {
			t.Errorf("%s: the next image read descriptor %q, want 3 or more", name, result.fields["fd"])
		}
		if strings.Count(result.fields["args"], Flag) != 1 {
			t.Errorf("%s: the next image has arguments %q, want the flag once", name, result.fields["args"])
		}
		result.neverShows(t, testBearer)
	}
}

// Once the process started repository code, Exec starts no image that would
// receive the credential: the process stays, and the error names the next
// image and the hook.
func TestExecAfterRepositoryCodeReplacesNothing(t *testing.T) {
	t.Parallel()
	result := runHelper(t, "exec", []string{"build", "--credential-fd", "3"}, []byte(testBearer), helperMarkEnv+"=1")
	refusal := &CustodyError{Holder: "the CLI " + os.Args[0], Reason: helperMarkReason}
	result.expect(t, map[string]string{
		"err": "exec returned: " + refusal.Error(), "hosted": "true", "digest": digest(testBearer), "args": helperRun,
	})
	result.neverShows(t, testBearer)
}

// Without a run credential the record does nothing: Exec replaces the process.
func TestExecWithoutACredentialIgnoresRepositoryCode(t *testing.T) {
	t.Parallel()
	result := runHelper(t, "exec-none", nil, nil, helperMarkEnv+"=1")
	result.expect(t, map[string]string{"err": "<nil>", "hosted": "false", "args": helperRun + " --"})
}

func TestExecWithoutACredentialHandsNothingOn(t *testing.T) {
	t.Parallel()
	result := runHelper(t, "exec-none", nil, nil)
	result.expect(t, map[string]string{"err": "<nil>", "hosted": "false", "digest": helperNoReport, "args": helperRun + " --"})
}

// rawPipe returns a close-on-exec pipe whose write end holds content and is
// closed, unless keepWriter is set.
func rawPipe(t *testing.T, content []byte, keepWriter bool) (readFD, writeFD int) {
	t.Helper()
	var p [2]int
	if err := unix.Pipe(p[:]); err != nil {
		t.Fatal(err)
	}
	unix.CloseOnExec(p[0])
	unix.CloseOnExec(p[1])
	if len(content) > 0 {
		if _, err := unix.Write(p[1], content); err != nil {
			t.Fatal(err)
		}
	}
	if !keepWriter {
		_ = unix.Close(p[1])
		p[1] = -1
	}
	return p[0], p[1]
}

func TestReadDescriptorReadsAPipeToItsEnd(t *testing.T) {
	t.Parallel()
	fd, _ := rawPipe(t, []byte(testBearer+"\n"), false)
	data, err := readDescriptor(fd)
	if err != nil || string(data) != testBearer+"\n" {
		t.Fatalf("readDescriptor = %q, %v", data, err)
	}
}

func TestReadDescriptorStopsPastTheBound(t *testing.T) {
	t.Parallel()
	fd, _ := rawPipe(t, []byte(strings.Repeat("a", MaxBytes+100)), false)
	data, err := readDescriptor(fd)
	if err != nil || len(data) != MaxBytes+1 {
		t.Fatalf("readDescriptor = %d bytes, %v; want %d", len(data), err, MaxBytes+1)
	}
}

func TestReadDescriptorWaitsOnANonBlockingPipe(t *testing.T) {
	t.Parallel()
	fd, writer := rawPipe(t, nil, true)
	if err := unix.SetNonblock(fd, true); err != nil {
		t.Fatal(err)
	}
	go func() {
		_, _ = unix.Write(writer, []byte(testBearer))
		_ = unix.Close(writer)
	}()
	data, err := readDescriptor(fd)
	if err != nil || string(data) != testBearer {
		t.Fatalf("readDescriptor = %q, %v", data, err)
	}
}

func TestReadDescriptorReadsARegularFile(t *testing.T) {
	t.Parallel()
	path := t.TempDir() + "/credential"
	if err := os.WriteFile(path, []byte(testBearer), 0o600); err != nil {
		t.Fatal(err)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	data, err := readDescriptor(fd)
	if err != nil || string(data) != testBearer {
		t.Fatalf("readDescriptor = %q, %v", data, err)
	}
}

func TestReadDescriptorLeavesAnotherKindOfDescriptorAlone(t *testing.T) {
	t.Parallel()
	fd, err := unix.Open(t.TempDir(), unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	_, err = readDescriptor(fd)
	want := fmt.Sprintf("descriptor %d is not a pipe, a socket or a regular file", fd)
	if err == nil || err.Error() != want {
		t.Fatalf("readDescriptor(directory) = %v, want %q", err, want)
	}
	if descriptorState(fd) != "open" {
		t.Error("readDescriptor closed a descriptor it refused")
	}
}

func TestReadDescriptorReportsAReadFailure(t *testing.T) {
	t.Parallel()
	reader, writer := rawPipe(t, nil, true)
	defer func() { _ = unix.Close(reader) }()
	defer func() { _ = unix.Close(writer) }()
	// The write end of a pipe is a pipe that cannot be read.
	if _, err := readDescriptor(writer); err == nil || !strings.HasPrefix(err.Error(), fmt.Sprintf("read descriptor %d: ", writer)) {
		t.Fatalf("readDescriptor(write end) = %v, want a read error", err)
	}
}

func TestCredentialPipeHoldsTheBearer(t *testing.T) {
	t.Parallel()
	fd, err := credentialPipe(testBearer)
	if err != nil {
		t.Fatal(err)
	}
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil || flags&unix.FD_CLOEXEC == 0 {
		t.Errorf("the credential pipe is inheritable before the exec: flags %#x, %v", flags, err)
	}
	data, err := readDescriptor(fd)
	if err != nil || string(data) != testBearer {
		t.Fatalf("read back %q, %v", data, err)
	}
}

func TestCredentialPipeRefusesABearerItCannotHold(t *testing.T) {
	t.Parallel()
	_, err := credentialPipe(strings.Repeat("a", 8<<20))
	if err == nil || !strings.Contains(err.Error(), "does not fit in an empty pipe") {
		t.Fatalf("credentialPipe(8 MiB) = %v, want a refusal", err)
	}
	if strings.Contains(err.Error(), "aaaa") {
		t.Errorf("the error shows the bearer: %v", err)
	}
}
