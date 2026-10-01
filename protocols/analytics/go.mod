module go.putnami.dev/protocol/analytics

go 1.25.7

require go.putnami.dev/protocol/diagnostic v0.0.0

replace go.putnami.dev/protocol/diagnostic => ../diagnostic

// The v0.1 series predates the open-source cut: its sources will not exist in
// the public repository, whose history starts at the v0.2.0 initial commit.
// The range starts at the -0 prerelease floor so the v0.1.x-<sha> candidate
// versions are covered too (a prerelease sorts below its release).
retract [v0.1.0-0, v0.1.99999] // Pre-open-source builds; public history starts at v0.2.0.
