module go.putnami.dev/protocol/job

go 1.25.7

require (
	// cli owns the typed task identity the v2 context reuses (context_v2.go).
	go.putnami.dev/protocol/cli v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
	// extension is TEST-ONLY: it pins the staging-root names against the task
	// contract's declared-output roots (context_v2_test.go).
	go.putnami.dev/protocol/extension v0.0.0
)

replace go.putnami.dev/protocol/cli => ../cli

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

replace go.putnami.dev/protocol/extension => ../extension

// The v0.1 series predates the open-source cut: its sources will not exist in
// the public repository, whose history starts at the v0.2.0 initial commit.
// The range starts at the -0 prerelease floor so the v0.1.x-<sha> candidate
// versions are covered too (a prerelease sorts below its release).
retract [v0.1.0-0, v0.1.99999] // Pre-open-source builds; public history starts at v0.2.0.
