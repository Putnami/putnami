// Per-composition database isolation on a server this package started.
//
// A test environment binds a project's datasources to the server's own
// database and lets the language runtime isolate each suite. A composition
// (`putnami compose`) is not a test suite: it serves several workloads at once,
// each applying its own migrations at boot, and two compositions of the same
// workload may run side by side. Each needs its OWN physical database, created
// before the workload starts and dropped when the composition ends.
//
// The CLI carries no PostgreSQL driver and adds none, so the statements run
// through the `psql` the pinned postgres image already ships, invoked with
// `docker exec` — the same docker seam every other call here goes through. The
// statements carry identifiers only: the superuser password is never part of
// the argv, because psql inside the container authenticates over the local
// socket.
package dbtestenv

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// maxIdentifierBytes is PostgreSQL's identifier limit (NAMEDATALEN - 1). A
// longer name is silently truncated by the server, which would make two
// distinct names collide; it is refused here instead.
const maxIdentifierBytes = 63

// DatabaseIsolator is implemented by providers that can give one composition
// its own physical databases. The provided server does not implement it.
type DatabaseIsolator interface {
	// CreateDatabase creates the named database on the server identified by
	// digest. It fails when the database already exists.
	CreateDatabase(ctx context.Context, digest, name string) error
	// DropDatabase drops the named database, terminating the connections still
	// open on it. Dropping a database that does not exist succeeds.
	DropDatabase(ctx context.Context, digest, name string) error
	// ListDatabases returns the names of the databases whose name starts with
	// prefix, sorted. The prefix is matched literally.
	ListDatabases(ctx context.Context, digest, prefix string) ([]string, error)
}

var _ DatabaseIsolator = (*Provisioner)(nil)

// CreateDatabase creates name inside the container provisioned for digest.
func (p *Provisioner) CreateDatabase(ctx context.Context, digest, name string) error {
	if err := validateIdentifier(name); err != nil {
		return err
	}
	if _, err := p.psql(ctx, digest, "CREATE DATABASE "+pgQuoteIdent(name)); err != nil {
		return fmt.Errorf("create database %s: %w", name, err)
	}
	return nil
}

// DropDatabase drops name inside the container provisioned for digest. WITH
// (FORCE) terminates the sessions a stopped workload may still hold, so a
// composition torn down right after its members exit does not fail on a
// connection the server has not noticed is gone yet.
func (p *Provisioner) DropDatabase(ctx context.Context, digest, name string) error {
	if err := validateIdentifier(name); err != nil {
		return err
	}
	if _, err := p.psql(ctx, digest, "DROP DATABASE IF EXISTS "+pgQuoteIdent(name)+" WITH (FORCE)"); err != nil {
		return fmt.Errorf("drop database %s: %w", name, err)
	}
	return nil
}

// ListDatabases lists the databases of the container provisioned for digest
// whose name starts with prefix.
func (p *Provisioner) ListDatabases(ctx context.Context, digest, prefix string) ([]string, error) {
	pattern := escapeLikePattern(prefix) + "%"
	query := "SELECT datname FROM pg_database WHERE datname LIKE " + pgQuoteLiteral(pattern) +
		` ESCAPE '\' ORDER BY datname`
	out, err := p.psql(ctx, digest, query)
	if err != nil {
		return nil, fmt.Errorf("list databases: %w", err)
	}
	return nonEmptyLines(out), nil
}

// psql runs one statement in the container provisioned for digest. -At prints
// unaligned tuples only, one per line, and ON_ERROR_STOP turns a failed
// statement into a non-zero exit instead of a warning on stderr.
func (p *Provisioner) psql(ctx context.Context, digest, statement string) (string, error) {
	if digest == "" {
		return "", errors.New("no server digest: provision the server first")
	}
	return p.docker(ctx, dockerRunTimeout,
		"exec", p.ContainerName(digest),
		"psql", "-U", p.cfg.User, "-d", p.cfg.Database, "-v", "ON_ERROR_STOP=1", "-Atc", statement)
}

// validateIdentifier refuses the names quoting cannot make safe or exact: an
// empty name, a NUL byte, and a name the server would truncate.
func validateIdentifier(name string) error {
	switch {
	case name == "":
		return errors.New("database name is empty")
	case strings.ContainsRune(name, 0):
		return errors.New("database name contains a NUL byte")
	case len(name) > maxIdentifierBytes:
		return fmt.Errorf("database name %s is %d bytes, above the %d-byte identifier limit", name, len(name), maxIdentifierBytes)
	}
	return nil
}

// pgQuoteIdent quotes an identifier: every double quote is doubled and the
// result is wrapped in double quotes, so any byte sequence names exactly
// itself.
func pgQuoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// pgQuoteLiteral quotes a string literal under standard_conforming_strings
// (the default since PostgreSQL 9.1): every single quote is doubled.
func pgQuoteLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// escapeLikePattern escapes the LIKE metacharacters so the prefix matches
// literally. `_` is the common case: every composition database name carries
// several.
func escapeLikePattern(value string) string {
	return strings.NewReplacer(`\`, `\\`, `_`, `\_`, `%`, `\%`).Replace(value)
}
