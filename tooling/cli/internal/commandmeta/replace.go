package commandmeta

// ReplaceForTest makes commands and globals the catalog and the global-flag
// table every function of this package reads, until the returned restore
// runs. It lets a renderer test prove a surface on a small catalog it builds,
// so the test does not change when a command of the CLI does.
//
// The catalog is process state: a test that calls ReplaceForTest must not run
// in parallel with another test of its package, and must call restore before
// it ends, for example with t.Cleanup.
func ReplaceForTest(commands []Command, globals []GlobalFlag) (restore func()) {
	previousCatalog, previousGlobals := catalog, globalFlags
	catalog = append([]Command(nil), commands...)
	globalFlags = append([]GlobalFlag(nil), globals...)
	return func() {
		catalog, globalFlags = previousCatalog, previousGlobals
	}
}
