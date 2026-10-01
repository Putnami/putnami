module go.putnami.dev/config

go 1.25.7

require (
	go.putnami.dev/errors v0.0.1
	go.putnami.dev/inject v0.0.1
	go.putnami.dev/protocol/features v0.0.0
)

require gopkg.in/yaml.v3 v3.0.1

require (
	github.com/kr/pretty v0.3.1 // indirect
	github.com/rogpeppe/go-internal v1.14.1 // indirect
	gopkg.in/check.v1 v1.0.0-20201130134442-10cb98267c6c // indirect
)

replace (
	go.putnami.dev/errors => ../errors
	go.putnami.dev/inject => ../inject
	go.putnami.dev/protocol/capabilities => ../../../protocols/capabilities
	go.putnami.dev/protocol/diagnostic => ../../../protocols/diagnostic
	go.putnami.dev/protocol/features => ../../../protocols/features
)

// The v0.1 series predates the open-source cut: its sources will not exist in
// the public repository, whose history starts at the v0.2.0 initial commit.
// The range starts at the -0 prerelease floor so the v0.1.x-<sha> candidate
// versions are covered too (a prerelease sorts below its release).
retract [v0.1.0-0, v0.1.99999] // Pre-open-source builds; public history starts at v0.2.0.
