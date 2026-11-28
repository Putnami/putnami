package test

import "strings"

// binaryFlags are the settings of a test run that decide what its test
// binaries compile to: coverage instrumentation and the race detector. Every
// other flag test-exec passes (-run, -count, -short, -timeout, -json, -p, ...)
// configures how the binaries run, not their bytes.
//
// test-exec renders them into its `go test` arguments, and test-identity-exec
// lists the binaries with the same ones, so the identity names the compile
// actions of the binaries the run tests (ADR 0017).
type binaryFlags struct {
	// cover reports that the run instruments coverage.
	cover bool
	// coverPackages are what -coverpkg names. Empty with cover set
	// instruments with -cover alone.
	coverPackages []string
	// covermode is -covermode, empty for Go's default.
	covermode string
	// race is -race.
	race bool
}

// args renders the flags in the order test-exec passes them, with
// -coverprofile profile after the coverage flags when cover is set and
// profile is not empty. A run of `go test` and a listing of its binaries pass
// the same flags but the profile, which only the run writes.
func (f binaryFlags) args(profile string) []string {
	var args []string
	if f.cover {
		args = append(args, "-cover")
		if len(f.coverPackages) > 0 {
			args = append(args, "-coverpkg", strings.Join(f.coverPackages, ","))
		}
		// Passing an empty -coverprofile makes `go test` read the next
		// argument as the path.
		if profile != "" {
			args = append(args, "-coverprofile", profile)
		}
	}
	if f.covermode != "" {
		args = append(args, "-covermode", f.covermode)
	}
	if f.race {
		args = append(args, "-race")
	}
	return args
}

// projectBinaryFlags derives the binaryFlags of one project's own run of
// `go test ./...` in dir: the solo run and a member of a batch under
// coverage-scope project. Coverage names the packages the project's module
// owns, listed with env (listModulePackages). A batch under coverage-scope
// batch instruments its members' union instead, which depends on the batch
// and on nothing a member's own key names (ADR 0008).
func projectBinaryFlags(options batchOptions, goBinary, dir string, env []string) (binaryFlags, error) {
	flags := binaryFlags{
		cover:     coverageEnabled(options),
		covermode: options.covermode,
		race:      options.race,
	}
	if !flags.cover {
		return flags, nil
	}
	packages, err := listModulePackages(goBinary, dir, env)
	if err != nil {
		return binaryFlags{}, err
	}
	flags.coverPackages = packages
	return flags, nil
}
