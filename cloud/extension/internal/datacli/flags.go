package datacli

import clicore "go.putnami.dev/cloud/extension/internal/clicore"

// The db verbs' value-less flags, so `db status --strict mydb` reads mydb as
// the database and not as the value of --strict.
func init() {
	clicore.RegisterBooleanFlags("strict")
}
