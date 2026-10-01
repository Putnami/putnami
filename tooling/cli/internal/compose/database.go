package compose

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"unicode/utf8"

	pdb "go.putnami.dev/protocol/database"
	"go.putnami.dev/sdk/extension/dbtestenv"
)

// databaseProvider is the part of dbtestenv.Provider a composition uses: one
// ready server and its non-secret digest. A provider that also implements
// dbtestenv.DatabaseIsolator gives every datasource its own database.
type databaseProvider interface {
	Provision() (pdb.Connection, string, error)
}

// Isolation values a composition reports.
const (
	// IsolationDatabase: every (member, datasource) has its own physical
	// database, created for this composition and dropped with it.
	IsolationDatabase = "database"
	// IsolationNone: members share the provided server's database, or the
	// composition declares no database at all.
	IsolationNone = "none"
)

// maxDatabaseNameBytes is PostgreSQL's identifier limit.
const maxDatabaseNameBytes = 63

// databasePrefix is the prefix every database of composition id carries.
func databasePrefix(id string) string {
	return "compose_" + id + "_"
}

// databaseNameHashBytes is the hex length of the suffix a truncated name
// carries.
const databaseNameHashBytes = 8

// databaseName is compose_<id>_<slug>_<datasource>, where slug is the project
// id lowercased with every byte outside [a-z0-9] replaced by `_`. A name past
// PostgreSQL's 63-byte identifier limit is cut on a UTF-8 boundary and ends
// with `_` and the first 8 hex digits of the SHA-256 of the full name, so two
// datasources that differ only after the cut still get distinct databases.
func databaseName(id, projectID, datasource string) string {
	name := databasePrefix(id) + projectSlug(projectID) + "_" + datasource
	if len(name) <= maxDatabaseNameBytes {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	suffix := "_" + hex.EncodeToString(sum[:])[:databaseNameHashBytes]
	head := name[:maxDatabaseNameBytes-len(suffix)]
	for len(head) > 0 && !utf8.ValidString(head) {
		head = head[:len(head)-1]
	}
	return head + suffix
}

func projectSlug(projectID string) string {
	lower := strings.ToLower(projectID)
	slug := make([]byte, len(lower))
	for i := 0; i < len(lower); i++ {
		c := lower[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			slug[i] = c
			continue
		}
		slug[i] = '_'
	}
	return string(slug)
}

// databaseSet is the composition's database state: the provider it used, the
// server digest, and the databases it created.
type databaseSet struct {
	id        string
	isolator  dbtestenv.DatabaseIsolator
	digest    string
	isolation string
	created   []string
}

// provisionDatabases gives every datasource of the plan a connection. It
// provisions the server once, through the provider dbtestenv selects for this
// host; on a provider that isolates, it creates one database per (member,
// datasource), recording each name in the lease BEFORE creating it. On a
// provided server every member shares the server's database.
func provisionDatabases(ctx context.Context, id string, plan *Plan, lease *leaseHandle, provider databaseProvider) (*databaseSet, error) {
	set := &databaseSet{id: id, isolation: IsolationNone}
	var first *Member
	for _, member := range plan.Members {
		if len(member.Databases) > 0 {
			first = member
			break
		}
	}
	if first == nil {
		return set, nil
	}
	if provider == nil {
		return set, newError(CodeDatabaseFailed, first.Project.ID, PhaseDatabases, "no database provider is available")
	}
	conn, digest, err := provider.Provision()
	if err != nil {
		return set, newError(CodeDatabaseFailed, first.Project.ID, PhaseDatabases,
			"provision the postgres server: "+err.Error())
	}
	set.digest = digest
	if err := lease.update(func(l *Lease) { l.ProvisionerDigest = digest }); err != nil {
		return set, newError(CodeLeaseFailed, "", PhaseDatabases, err.Error())
	}
	isolator, isolates := provider.(dbtestenv.DatabaseIsolator)
	if isolates {
		set.isolator = isolator
		set.isolation = IsolationDatabase
	}

	for _, member := range plan.Members {
		for i := range member.Databases {
			binding := &member.Databases[i]
			connection := conn
			if !isolates {
				binding.Database = conn.Database
				binding.connection = &connection
				continue
			}
			name := databaseName(id, member.Project.ID, binding.Datasource)
			if err := lease.update(func(l *Lease) { l.Databases = append(l.Databases, name) }); err != nil {
				return set, newError(CodeLeaseFailed, member.Project.ID, PhaseDatabases, err.Error())
			}
			if err := isolator.CreateDatabase(ctx, digest, name); err != nil {
				return set, newError(CodeDatabaseFailed, member.Project.ID, PhaseDatabases,
					"create the database of datasource "+binding.Datasource+": "+err.Error())
			}
			set.created = append(set.created, name)
			connection.Database = name
			binding.Database = name
			binding.connection = &connection
		}
	}
	return set, nil
}

// drop drops every database the composition created and confirms none of
// this composition's databases is left. leftovers names what it could not
// release; remaining lists the databases that may still exist, which the lease
// keeps for the next reap: a failed drop, a database the confirmation still
// lists, and every created database when the confirmation itself failed.
func (d *databaseSet) drop(ctx context.Context) (leftovers, remaining []string) {
	if d == nil || d.isolator == nil || d.digest == "" {
		return nil, nil
	}
	failed := make(map[string]bool)
	for _, name := range d.created {
		if err := d.isolator.DropDatabase(ctx, d.digest, name); err != nil {
			failed[name] = true
			remaining = append(remaining, name)
			leftovers = append(leftovers, "database "+name+" ("+err.Error()+")")
		}
	}
	listed, err := d.isolator.ListDatabases(ctx, d.digest, databasePrefix(d.id))
	if err != nil {
		if len(d.created) > 0 {
			leftovers = append(leftovers, "databases of composition "+d.id+" could not be listed ("+err.Error()+")")
			remaining = append([]string{}, d.created...)
		}
		return leftovers, remaining
	}
	for _, name := range listed {
		if !failed[name] {
			remaining = append(remaining, name)
			leftovers = append(leftovers, "database "+name)
		}
	}
	return leftovers, remaining
}
