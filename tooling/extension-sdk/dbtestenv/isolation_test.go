package dbtestenv

import (
	"context"
	"errors"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

// psqlCall returns the SQL statement of a recorded `docker exec … psql -Atc
// <sql>` invocation and asserts the argv around it.
func psqlCall(t *testing.T, call []string, container string) string {
	t.Helper()
	want := []string{"exec", container, "psql", "-U", "putnami", "-d", "putnami_test", "-v", "ON_ERROR_STOP=1", "-Atc"}
	if len(call) != len(want)+1 || !slices.Equal(call[:len(want)], want) {
		t.Fatalf("docker argv = %q, want %q followed by one statement", call, want)
	}
	return call[len(want)]
}

func TestDatabaseIsolator_CreateDropAndListRunPsqlInTheProvisionedContainer(t *testing.T) {
	digest := testDigest()
	fd := &fakeDocker{respond: func(args []string) (string, error) {
		if hasSub(args, "SELECT datname") {
			return "compose_ab_x\ncompose_ab_y\n", nil
		}
		return "", nil
	}}
	p := testProvisioner(fd, okProbe)
	ctx := context.Background()
	container := p.ContainerName(digest)

	if err := p.CreateDatabase(ctx, digest, "compose_ab_x"); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if err := p.DropDatabase(ctx, digest, "compose_ab_x"); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
	names, err := p.ListDatabases(ctx, digest, "compose_ab_")
	if err != nil {
		t.Fatalf("ListDatabases: %v", err)
	}
	if len(fd.calls) != 3 {
		t.Fatalf("docker calls = %d, want 3: %q", len(fd.calls), fd.calls)
	}
	if got := psqlCall(t, fd.calls[0], container); got != `CREATE DATABASE "compose_ab_x"` {
		t.Errorf("create statement = %q", got)
	}
	if got := psqlCall(t, fd.calls[1], container); got != `DROP DATABASE IF EXISTS "compose_ab_x" WITH (FORCE)` {
		t.Errorf("drop statement = %q", got)
	}
	if got := psqlCall(t, fd.calls[2], container); got != `SELECT datname FROM pg_database WHERE datname LIKE 'compose\_ab\_%' ESCAPE '\' ORDER BY datname` {
		t.Errorf("list statement = %q", got)
	}
	if !slices.Equal(names, []string{"compose_ab_x", "compose_ab_y"}) {
		t.Errorf("ListDatabases = %q", names)
	}
	for i, bounded := range fd.hadDeadline {
		if !bounded {
			t.Errorf("docker call %d ran without a deadline", i)
		}
	}
	for _, call := range fd.calls {
		if hasSub(call, p.password) {
			t.Errorf("a psql argv carries the superuser password: %q", call)
		}
	}
}

func TestDatabaseIsolator_QuotingNamesExactlyTheGivenIdentifier(t *testing.T) {
	if got := pgQuoteIdent(`we"ird`); got != `"we""ird"` {
		t.Errorf("pgQuoteIdent = %s", got)
	}
	if got := pgQuoteLiteral(`it's`); got != `'it''s'` {
		t.Errorf("pgQuoteLiteral = %s", got)
	}
	if got := escapeLikePattern(`a_b%c\d`); got != `a\_b\%c\\d` {
		t.Errorf("escapeLikePattern = %s", got)
	}

	fd := &fakeDocker{}
	p := testProvisioner(fd, okProbe)
	if err := p.CreateDatabase(context.Background(), testDigest(), `x"; DROP DATABASE postgres; --`); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	statement := fd.calls[0][len(fd.calls[0])-1]
	if statement != `CREATE DATABASE "x""; DROP DATABASE postgres; --"` {
		t.Errorf("an embedded quote escaped its identifier: %s", statement)
	}
}

func TestDatabaseIsolator_RefusesNamesTheServerWouldAlter(t *testing.T) {
	fd := &fakeDocker{}
	p := testProvisioner(fd, okProbe)
	ctx := context.Background()
	for _, name := range []string{"", strings.Repeat("a", maxIdentifierBytes+1), "a\x00b"} {
		if err := p.CreateDatabase(ctx, testDigest(), name); err == nil {
			t.Errorf("CreateDatabase(%q) succeeded", name)
		}
		if err := p.DropDatabase(ctx, testDigest(), name); err == nil {
			t.Errorf("DropDatabase(%q) succeeded", name)
		}
	}
	if _, err := p.ListDatabases(ctx, "", "compose_"); err == nil {
		t.Error("ListDatabases without a digest succeeded")
	}
	if len(fd.calls) != 0 {
		t.Errorf("a refused name still reached docker: %q", fd.calls)
	}
}

func TestDatabaseIsolator_FailureNamesTheDatabase(t *testing.T) {
	fd := &fakeDocker{respond: func([]string) (string, error) { return "", errors.New("exit status 1") }}
	p := testProvisioner(fd, okProbe)
	err := p.CreateDatabase(context.Background(), testDigest(), "compose_ab_x")
	if err == nil || !strings.Contains(err.Error(), "compose_ab_x") {
		t.Fatalf("CreateDatabase error = %v, want one naming the database", err)
	}
}

// TestDatabaseIsolator_AgainstARealServer runs the three statements against the
// pinned image. It needs a docker binary and a daemon; it never runs on CI,
// where this package never starts infrastructure of its own.
func TestDatabaseIsolator_AgainstARealServer(t *testing.T) {
	if testing.Short() || os.Getenv(EnvCI) != "" || !DockerAvailable() {
		//putnami:allow-skip this package never starts infrastructure of its own on CI, so the test needs a contributor's docker daemon
		t.Skip("needs a local docker daemon outside CI")
	}
	p := NewProvisioner()
	_, digest, err := p.Provision()
	if err != nil {
		t.Skipf("docker is on PATH but no server could be provisioned: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := "dbtestenv_isolation_" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "_"))
	if len(name) > maxIdentifierBytes {
		name = name[:maxIdentifierBytes]
	}
	t.Cleanup(func() { _ = p.DropDatabase(context.Background(), digest, name) })

	if err := p.CreateDatabase(ctx, digest, name); err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	listed, err := p.ListDatabases(ctx, digest, name)
	if err != nil || !slices.Contains(listed, name) {
		t.Fatalf("ListDatabases after create = %q, %v", listed, err)
	}
	if err := p.DropDatabase(ctx, digest, name); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
	if err := p.DropDatabase(ctx, digest, name); err != nil {
		t.Fatalf("dropping an absent database must succeed: %v", err)
	}
	listed, err = p.ListDatabases(ctx, digest, name)
	if err != nil || slices.Contains(listed, name) {
		t.Fatalf("ListDatabases after drop = %q, %v", listed, err)
	}
}
