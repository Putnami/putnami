package workspace

import (
	"path/filepath"
	"testing"

	wsproto "go.putnami.dev/protocol/workspace"
	"go.putnami.dev/tooling/cli/internal/runcredential"
)

// A workspace probe starts its extension's executable. Unless the runtime was
// prepared before the run, from the artifact store or by the source build,
// that executable is repository code: a hosted run records it before the
// spawn, and hands its credential to no process started after.
func TestExecProbeProvider_RecordsRepositoryCodeOutsideTheStore(t *testing.T) {
	for name, c := range map[string]struct {
		preparedBeforeRun bool
		records           bool
	}{
		"repository code":         {records: true},
		"prepared before the run": {preparedBeforeRun: true},
	} {
		restore := runcredential.SetForTest("run-bearer")
		provider := &ExecProbeProvider{
			Extension:         "@acme/probe",
			Executable:        filepath.Join(t.TempDir(), "does-not-exist"),
			PreparedBeforeRun: c.preparedBeforeRun,
		}
		// The executable is missing, so the probe fails at the spawn; the record
		// comes first.
		if _, err := provider.Probe(wsproto.ProbeRequest{Version: wsproto.ProbeProtocolVersion, Extension: provider.Extension}); err == nil {
			t.Errorf("%s: probing a missing executable succeeded", name)
		}
		err := runcredential.RequireCustody("the cache provider of @acme/cache")
		restore()
		if recorded := err != nil; recorded != c.records {
			t.Errorf("%s: repository code recorded = %v (%v), want %v", name, recorded, err, c.records)
		}
	}
}
