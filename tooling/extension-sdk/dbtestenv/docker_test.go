package dbtestenv

import (
	"go.putnami.dev/protocol/features/spectest"

	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"
)

// The PROVIDER conformance suite: resource identity, session reuse, concurrent
// name races, stale-container recovery and credential confinement, all proven
// against a fake docker seam so no case needs a daemon.
//
// Migrated from the CLI's internal/testinfra/docker_test.go. The cases are
// unchanged on purpose: they are the behavior the move promised to preserve
// exactly, and moving them without changing them is what makes that checkable.

// --- fake docker seam -----------------------------------------------------

// fakeDocker records every docker invocation's argv and replies from a
// scripted responder, so the pure provisioning logic (argv, reuse, reaping,
// redaction) is asserted without a live daemon.
type fakeDocker struct {
	calls       [][]string
	hadDeadline []bool
	respond     func(args []string) (string, error)
}

func (f *fakeDocker) run(ctx context.Context, args ...string) (string, error) {
	f.calls = append(f.calls, append([]string(nil), args...))
	_, bounded := ctx.Deadline()
	f.hadDeadline = append(f.hadDeadline, bounded)
	if f.respond != nil {
		return f.respond(args)
	}
	return "", nil
}

// verb reports the docker subcommand of a recorded call (args[0]).
func verb(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}

// hasSub reports whether the joined argv contains sub as a substring.
func hasSub(args []string, sub string) bool {
	return strings.Contains(strings.Join(args, "\x00"), sub)
}

// findCall returns the first recorded call whose verb is v, or nil.
func findCall(calls [][]string, v string) []string {
	for _, c := range calls {
		if verb(c) == v {
			return c
		}
	}
	return nil
}

// okProbe is a readiness probe that always succeeds immediately.
func okProbe(context.Context, runner, string, string, string) error { return nil }

// testProvisioner wires a Provisioner to the fake docker and a canned probe
// with stable, distinctive credentials (the password shares no substring with
// any container name, label, filter, or the image digest).
func testProvisioner(fd *fakeDocker, probe probeFunc) *Provisioner {
	return &Provisioner{
		run:             fd.run,
		probe:           probe,
		dockerAvailable: func() bool { return true },
		cfg: serverConfig{
			Image:    postgresImagePinned,
			User:     "putnami",
			Database: "putnami_test",
		},
		password:         "local-dev-secret",
		readyTimeout:     time.Second,
		provisionTimeout: 30 * time.Second,
	}
}

func testDigest() string {
	return (serverConfig{
		Image:    postgresImagePinned,
		User:     "putnami",
		Database: "putnami_test",
	}).digest()
}

// --- digest determinism ---------------------------------------------------

// The reuse digest must be stable across runs for identical config and must move
// when any server-affecting field changes; otherwise reuse and reaping would key
// on shifting identities.
func TestDigestDeterministicAndSensitive(t *testing.T) {
	base := serverConfig{Image: postgresImagePinned, User: "putnami", Database: "db"}
	same := serverConfig{Image: postgresImagePinned, User: "putnami", Database: "db"}
	if base.digest() != same.digest() {
		t.Fatal("digest is not stable for identical config")
	}
	changed := base
	changed.Image = "postgres:17@sha256:" + strings.Repeat("a", 64)
	if base.digest() == changed.digest() {
		t.Error("digest did not change when the image changed")
	}
	changed = base
	changed.Database = "other"
	if base.digest() == changed.digest() {
		t.Error("digest did not change when the database changed")
	}
	// Hex only: no credential substring can ever appear in a digest.
	if strings.ContainsAny(base.digest(), "ghijklmnopqrstuvwxyz-_") {
		t.Errorf("digest %q is not pure hex", base.digest())
	}
}

// --- provision: start a fresh container -----------------------------------

func TestProvisionStartsWhenNoneRunning(t *testing.T) {
	digest := testDigest()
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "ps":
			return "", nil // nothing running, nothing to reap
		case "run":
			return "newcontainerid", nil
		case "port":
			return "127.0.0.1:49153", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)

	conn, gotDigest, err := p.Provision()
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if gotDigest != digest {
		t.Errorf("reported digest = %q, want %q", gotDigest, digest)
	}
	if conn.Host != "127.0.0.1" || conn.Port != 49153 {
		t.Errorf("connection endpoint = %s:%d, want 127.0.0.1:49153", conn.Host, conn.Port)
	}
	if conn.User != "putnami" || conn.Password != "local-dev-secret" || conn.Database != "putnami_test" {
		t.Errorf("connection creds not carried through: %+v", conn)
	}

	run := findCall(fd.calls, "run")
	if run == nil {
		t.Fatal("no `docker run` call was made for a fresh provision")
	}
	if !hasSub(run, "--label\x00"+labelKey+"="+digest) {
		t.Errorf("run call missing digest label: %v", run)
	}
	if !hasSub(run, "--name\x00"+p.ContainerName(digest)) {
		t.Errorf("run call missing deterministic name: %v", run)
	}
	if !hasSub(run, "--publish\x00127.0.0.1::"+containerPort) {
		t.Errorf("run call did not publish to loopback: %v", run)
	}
}

// --- provision: reuse a running container ---------------------------------

// A still-running container matching the current digest must be reused — no
// second `docker run` — which is the session-reuse invariant.
func TestProvisionReusesRunningContainer(t *testing.T) {
	digest := testDigest()
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		if verb(args) == "ps" {
			// The running-only filter for the current digest finds one.
			if hasSub(args, "status=running") && hasSub(args, "label="+labelKey+"="+digest) {
				return "runningid", nil
			}
			return "", nil
		}
		if verb(args) == "port" {
			return "127.0.0.1:55000", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, postgresReady)

	conn, _, err := p.Provision()
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if conn.Port != 55000 {
		t.Errorf("reused connection port = %d, want 55000", conn.Port)
	}
	if findCall(fd.calls, "run") != nil {
		t.Error("a `docker run` was issued despite a reusable running container — session reuse failed")
	}
	if got := findCall(fd.calls, "exec"); len(got) < 2 || got[1] != "runningid" {
		t.Errorf("reused readiness target = %v, want container ID runningid", got)
	}
}

// --- provision: concurrent name race falls back to reuse ------------------

// When `docker run` fails (a concurrent run won the deterministic-name race),
// the loser must reuse the winner's container rather than error out.
func TestProvisionNameRaceReuses(t *testing.T) {
	digest := testDigest()
	seenRun := false
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "ps":
			// No running match until after the run attempt (the race winner appears).
			if seenRun && hasSub(args, "status=running") && hasSub(args, "label="+labelKey+"="+digest) {
				return "raceid", nil
			}
			return "", nil
		case "run":
			seenRun = true
			return "", context.DeadlineExceeded // simulate `name already in use`
		case "port":
			return "127.0.0.1:60000", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, postgresReady)

	conn, _, err := p.Provision()
	if err != nil {
		t.Fatalf("Provision after name race: %v", err)
	}
	if conn.Port != 60000 {
		t.Errorf("raced connection port = %d, want 60000 (winner's container)", conn.Port)
	}
	if got := findCall(fd.calls, "exec"); len(got) < 2 || got[1] != "raceid" {
		t.Errorf("raced readiness target = %v, want winner container ID raceid", got)
	}
}

// --- start: never force-removes a concurrent winner by name ---------------

// A concurrent provision that already won the deterministic-name race owns a
// RUNNING container under that name. start() must never force-remove by name, or
// it would yank that running winner out from under its own caller — leaving the
// loser (and, worse, the winner) to dial a dead port. start() issues a non-force
// `docker rm` (which docker refuses on a running container) and then falls back
// to reusing the winner when `docker run` name-conflicts.
func TestStartNeverForceRemovesRunningWinnerByName(t *testing.T) {
	digest := testDigest()
	var rmCalls [][]string
	winnerRunning := false
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "ps":
			// The initial reuse probe misses (the loser has not observed the
			// winner yet); once the winner holds the name, the running-digest
			// filter finds it so start()'s fallback can reuse it.
			if winnerRunning && hasSub(args, "status=running") && hasSub(args, "label="+labelKey+"="+digest) {
				return "winnerid", nil
			}
			return "", nil
		case "rm":
			rmCalls = append(rmCalls, append([]string(nil), args...))
			// A non-force rm against the running winner is refused by docker; a
			// force rm would (wrongly) succeed and delete it.
			if !slices.Contains(args, "-f") {
				return "", context.DeadlineExceeded // docker: cannot remove a running container
			}
			return "", nil
		case "run":
			// The winner already holds the deterministic name: run conflicts.
			winnerRunning = true
			return "", context.DeadlineExceeded // docker: name already in use
		case "port":
			return "127.0.0.1:60000", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)

	conn, _, err := p.Provision()
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if conn.Port != 60000 {
		t.Errorf("did not reuse the running winner: port = %d, want 60000", conn.Port)
	}
	if len(rmCalls) == 0 {
		t.Fatal("start issued no `docker rm`; expected a non-force stopped-leftover cleanup")
	}
	for _, c := range rmCalls {
		// An exact-element check (not hasSub) — "-f" can appear inside an unrelated
		// argument such as a hex container name.
		if slices.Contains(c, "-f") {
			t.Errorf("start force-removed by name (%v) — could delete a concurrent running winner and orphan its port", c)
		}
	}
}

// --- reaping: only non-matching digests are removed -----------------------

func TestReapStaleRemovesOnlyNonMatching(t *testing.T) {
	keep := testDigest()
	var removed []string
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "ps":
			// The digest-scoped filter matches only "bbb"; the key-only filter
			// (no value) lists every workspace-labeled container.
			if hasSub(args, "label="+labelKey+"="+keep) {
				return "bbb", nil
			}
			if hasSub(args, "label="+labelKey) {
				return "aaa\nbbb\nccc", nil
			}
			return "", nil
		case "rm":
			removed = append(removed, args[2:]...) // args = rm -f <ids...>
			return "", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)

	if err := p.reapStale(context.Background(), keep); err != nil {
		t.Fatalf("reapStale: %v", err)
	}
	want := "aaa,ccc"
	if got := strings.Join(removed, ","); got != want {
		t.Errorf("reaped %q, want %q (only non-matching digests, matching kept)", got, want)
	}
}

// Stale-container RECOVERY: a killed run leaves containers behind that --rm can
// never reap, and a Provision must remove the mismatched ones BEFORE it reuses
// or starts anything. Ordering is the property: reaping after provisioning would
// leave a run racing its own predecessor's leftovers.
func TestProvisionReapsStaleBeforeReuse(t *testing.T) {
	keep := testDigest()
	var order []string
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "ps":
			if hasSub(args, "status=running") {
				order = append(order, "reuse-probe")
				return "liveid", nil
			}
			if hasSub(args, "label="+labelKey+"="+keep) {
				return "liveid", nil
			}
			return "orphanid\nliveid", nil
		case "rm":
			order = append(order, "reap")
			return "", nil
		case "port":
			return "127.0.0.1:49999", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)

	if _, _, err := p.Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if len(order) < 2 || order[0] != "reap" {
		t.Fatalf("call order = %v, want the stale reap before the reuse probe", order)
	}
	if findCall(fd.calls, "run") != nil {
		t.Error("started a container despite a reusable one surviving the reap")
	}
}

// --- teardown: reaps every workspace-labeled container --------------------

func TestTeardownAllReapsEverything(t *testing.T) {
	var removed []string
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "ps":
			return "x\ny", nil
		case "rm":
			removed = append(removed, args[2:]...)
			return "", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)

	if err := p.TeardownAll(); err != nil {
		t.Fatalf("TeardownAll: %v", err)
	}
	if got := strings.Join(removed, ","); got != "x,y" {
		t.Errorf("teardown removed %q, want x,y", got)
	}
}

func TestTeardownAllNoContainersNoRemove(t *testing.T) {
	fd := &fakeDocker{respond: func(args []string) (string, error) { return "", nil }}
	p := testProvisioner(fd, okProbe)
	if err := p.TeardownAll(); err != nil {
		t.Fatalf("TeardownAll: %v", err)
	}
	if findCall(fd.calls, "rm") != nil {
		t.Error("teardown issued `docker rm` with no workspace-labeled containers")
	}
}

// --- readiness: bounded, no unbounded wait --------------------------------

func TestProvisionWaitsForPostgresProtocolReadiness(t *testing.T) {
	probeAttempts := 0
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "run":
			return "id", nil
		case "port":
			return "127.0.0.1:49153", nil
		case "exec":
			probeAttempts++
			if probeAttempts == 1 {
				return "", errors.New("postgres is rejecting connections")
			}
			return "127.0.0.1:5432 - accepting connections", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, postgresReady)

	if _, _, err := p.Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if probeAttempts != 2 {
		t.Fatalf("protocol readiness attempts = %d, want 2", probeAttempts)
	}
	want := []string{"exec", p.ContainerName(testDigest()), "pg_isready", "-h", "127.0.0.1", "-U", p.cfg.User, "-d", p.cfg.Database}
	if got := findCall(fd.calls, "exec"); !slices.Equal(got, want) {
		t.Errorf("readiness call = %v, want %v", got, want)
	}
	for i, call := range fd.calls {
		if verb(call) == "exec" && !fd.hadDeadline[i] {
			t.Errorf("readiness call had no context deadline: %v", call)
		}
	}
}

func TestProvisionReadinessTimeoutIsBounded(t *testing.T) {
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "run":
			return "id", nil
		case "port":
			return "127.0.0.1:49153", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, func(context.Context, runner, string, string, string) error {
		return context.DeadlineExceeded // never ready
	})
	p.readyTimeout = 40 * time.Millisecond

	start := time.Now()
	_, _, err := p.Provision()
	if err == nil {
		t.Fatal("Provision returned nil error despite an unready server")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("readiness wait was not bounded: took %s", elapsed)
	}
}

// --- security: credentials never enter names, labels, or filters ----------

// The Postgres password may appear ONLY in a `docker run --env
// POSTGRES_PASSWORD=` argument — never in a container name, label, filter, or
// any other argv. This pins the credential-redaction invariant across the whole
// provisioning lifecycle (reap, start, reuse, port lookup).
func TestCredentialsNeverLeakIntoArgv(t *testing.T) {
	spectest.Proves(t, "tooling/extension-authoring", "credential-confinement", "the-credential-never-reaches-the-provisioner-argv")
	const password = "local-dev-secret"
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		switch verb(args) {
		case "run":
			return "id", nil
		case "port":
			return "127.0.0.1:49153", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)
	if _, _, err := p.Provision(); err != nil {
		t.Fatalf("Provision: %v", err)
	}

	for _, call := range fd.calls {
		for _, arg := range call {
			if strings.Contains(arg, password) && arg != "POSTGRES_PASSWORD="+password {
				t.Errorf("password leaked into a non-env argument %q (call: %v)", arg, call)
			}
		}
	}

	// The deterministic name and label must be pure identity — never a credential.
	digest := testDigest()
	if strings.Contains(p.ContainerName(digest), password) || strings.Contains(digest, password) {
		t.Error("container name or digest embeds the password")
	}
}

// A failed `docker run` must surface the credential-free sentinel, never the
// exec error — the argv it would echo carries POSTGRES_PASSWORD.
func TestStartFailureIsCredentialFree(t *testing.T) {
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		if verb(args) == "run" {
			return "", context.DeadlineExceeded
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)

	_, _, err := p.Provision()
	if err == nil {
		t.Fatal("Provision succeeded despite a failed `docker run`")
	}
	if strings.Contains(err.Error(), "local-dev-secret") {
		t.Errorf("start error leaked the password: %v", err)
	}
	if !errors.Is(err, errStartFailed) {
		t.Errorf("start error = %v, want the credential-free sentinel", err)
	}
}
