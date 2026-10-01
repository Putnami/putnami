module go.putnami.dev/protocol/identity

go 1.25.7

require (
	go.putnami.dev/protocol/contracts v0.0.0
	go.putnami.dev/protocol/diagnostic v0.0.0
)

require go.putnami.dev/protocol/config v0.0.0 // indirect

replace go.putnami.dev/protocol/contracts => ../contracts

replace go.putnami.dev/protocol/config => ../config

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

// The v0.1 series predates the open-source cut: its sources will not exist in
// the public repository, whose history starts at the v0.2.0 initial commit.
// The range starts at the -0 prerelease floor so the v0.1.x-<sha> candidate
// versions are covered too (a prerelease sorts below its release).
retract [v0.1.0-0, v0.1.99999] // Pre-open-source builds; public history starts at v0.2.0.
