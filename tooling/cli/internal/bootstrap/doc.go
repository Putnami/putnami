// Package bootstrap holds tests for the putnamiw bootstrap wrapper script. The
// wrapper itself is bash (it must run before any Go binary exists); this package
// has no runtime code — see putnamiw_test.go for the behavioral coverage of its
// content-addressed CLI sharing and staging cleanup.
package bootstrap
