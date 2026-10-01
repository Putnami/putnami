package test

import (
	"fmt"
	"path"
	"slices"
	"strings"
)

// listModulePackages names the packages of the module rooted at dir: the
// ones `go list ./...` owns there, run with env, sorted so every argument
// list built from it is deterministic. This is the one membership rule of a
// project's coverage. A pattern cannot express it: `-coverpkg=./...`
// matches by directory prefix against the test binary's build list, and a
// module-path pattern such as `example.com/a/...` matches the same way, so
// both instrument a module nested under dir that one of dir's tests imports
// and charge its statements to dir's module. The package enumeration stops
// at the module boundary.
//
// `-e` keeps the listing from failing on a pattern that matches nothing or
// on a directory Go ignores: what it prints for those carries .Error and the
// template drops it. A package with a syntax error is listed without an
// error, so `go test` still reports it as today.
func listModulePackages(goBinary, dir string, env []string) ([]string, error) {
	args := []string{"list", "-e", "-f", "{{if not .Error}}{{.ImportPath}}{{end}}", "./..."}
	output, err := runGoCommand(goBinary, args, dir, env)
	if err != nil {
		message := fmt.Sprintf("go list ./... in %s: %v", dir, err)
		if said := strings.TrimSpace(string(output)); said != "" {
			message += "\n" + said
		}
		return nil, fmt.Errorf("%s", message)
	}
	var packages []string
	for _, line := range strings.Split(string(output), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			packages = append(packages, line)
		}
	}
	slices.Sort(packages)
	return packages, nil
}

// unionPackages merges the members' package lists into one sorted list
// without duplicates: the -coverpkg of a batch-scoped invocation.
func unionPackages(projects []*batchProject) []string {
	size := 0
	for _, project := range projects {
		size += len(project.packages)
	}
	union := make([]string, 0, size)
	for _, project := range projects {
		union = append(union, project.packages...)
	}
	slices.Sort(union)
	return slices.Compact(union)
}

// packageOwners maps every package the members own to the member that owns
// it. Instrumentation membership and attribution membership are the same
// lists, so a package is charged to the project that listed it and to no
// other, whatever prefix its import path shares with another member's module
// path.
func packageOwners(projects []*batchProject) map[string]string {
	owners := make(map[string]string)
	for _, project := range projects {
		for _, pkg := range project.packages {
			owners[pkg] = project.ref.ID
		}
	}
	return owners
}

// coverageBlockPackage returns the import path of the package one profile
// block belongs to: the directory of the file the block names before its
// position.
func coverageBlockPackage(line string) string {
	file := strings.Fields(line)[0]
	if colon := strings.LastIndex(file, ":"); colon >= 0 {
		file = file[:colon]
	}
	return path.Dir(file)
}
