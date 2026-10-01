// Package gosource answers questions about Go source files from their bytes
// alone: which package a file declares, and whether a build constraint keeps it
// out of every ordinary build.
//
// It exists because two callers need the same answer. The module packager reads
// it to decide whose //go:embed directives must be staged; the workspace probe
// reads it to decide whether a module is an application or a library. This
// repository has already paid once for letting one question have two
// implementations — three hand-rolled go.mod parsers, consolidated into
// internal/toolchain by an earlier migration — and "the build compiles this
// file" is exactly the kind of predicate two copies drift on.
//
// Everything here is PURE: a function of the file bytes. No `go` invocation, no
// module cache, no network, and deliberately no GOOS/GOARCH lookup. The
// workspace probe's answer digests into core's workspace snapshot, so a byte
// that varies between two runs over one tree — or between two hosts probing one
// tree — is a cache-correctness bug. It is also the more useful answer for classification: a module whose
// only main package is behind `//go:build windows` still produces a binary, and
// calling it a library on a Linux runner would be wrong rather than merely
// host-dependent.
package gosource

import (
	"errors"
	"go/build/constraint"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// headerPrefixBytes is how much of a file is read before falling back to the
// whole file. Both facts this package reports live in a Go file's header — the
// build constraint must precede the package clause, and the package clause
// precedes every declaration — so a prefix that reaches the package clause
// carries the complete answer. 16 KiB clears the largest license-plus-doc
// preamble in this repository by a wide margin; a file that somehow needs more
// takes the full read rather than a truncated guess.
const headerPrefixBytes = 16 << 10

// Header is what this package reads out of a Go file's preamble.
type Header struct {
	// Package is the identifier the package clause declares, or "" when the
	// bytes carry no parseable package clause.
	Package string
	// Constraint is the file's build-constraint expression, or nil when it
	// declares none.
	Constraint constraint.Expr
}

// buildable reports whether SOME tag assignment satisfies the file's build
// constraint.
//
// Tags are brute-forced over both truth values with `ignore` pinned false —
// nothing sets `-tags ignore`; it is the conventional generator-script guard —
// so `//go:build ignore` is unbuildable while platform- or custom-tag-gated
// files are buildable. Constraints with too many distinct tags to enumerate are
// treated as buildable, which is the conservative direction for both callers:
// the packager stages an embed it might not have needed, and the probe calls a
// module an application it might have called a library.
func (h Header) buildable() bool {
	if h.Constraint == nil {
		return true
	}
	tagSet := make(map[string]struct{})
	collectConstraintTags(h.Constraint, tagSet)
	delete(tagSet, "ignore")
	tags := make([]string, 0, len(tagSet))
	for tag := range tagSet {
		tags = append(tags, tag)
	}
	sort.Strings(tags)
	if len(tags) > 12 {
		return true
	}
	for mask := 0; mask < 1<<len(tags); mask++ {
		assignment := make(map[string]bool, len(tags))
		for i, tag := range tags {
			assignment[tag] = mask&(1<<i) != 0
		}
		if h.Constraint.Eval(func(tag string) bool { return assignment[tag] }) {
			return true
		}
	}
	return false
}

// headerOf reads a Go file's header out of the bytes in hand.
func headerOf(src []byte) Header {
	return Header{Package: packageClause(src), Constraint: buildConstraintExpr(src)}
}

// ExcludedByBuildConstraint reports whether the file's build constraint can
// never be satisfied in a normal build.
func ExcludedByBuildConstraint(src []byte) bool {
	return !headerOf(src).buildable()
}

// readHeader reads the header of the Go file at path.
//
// Only a prefix is read when that prefix already carries the package clause;
// callers walk whole module trees, and reading every source file end to end to
// learn its first identifier is work no answer depends on.
func readHeader(path string) (Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return Header{}, err
	}
	prefix := make([]byte, headerPrefixBytes)
	n, err := io.ReadFull(f, prefix)
	closeErr := f.Close()
	switch {
	case err == nil || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF):
	default:
		return Header{}, err
	}
	if closeErr != nil {
		return Header{}, closeErr
	}
	prefix = prefix[:n]

	header := headerOf(prefix)
	if header.Package != "" || n < headerPrefixBytes {
		// Either the prefix reached the package clause, or it WAS the whole
		// file and there is nothing more to read.
		return header, nil
	}
	src, err := os.ReadFile(path)
	if err != nil {
		return Header{}, err
	}
	return headerOf(src), nil
}

// ToolingIgnoresPath reports whether the go toolchain ignores the
// slash-separated module-relative path: `testdata` directories and any segment
// beginning with `_` or `.` are invisible to the build.
func ToolingIgnoresPath(rel string) bool {
	for _, s := range strings.Split(rel, "/") {
		if s == "testdata" || strings.HasPrefix(s, "_") || strings.HasPrefix(s, ".") {
			return true
		}
	}
	return false
}

// ModuleHasMain reports whether the module rooted at dir contains a package
// main — that is, whether the module can produce an executable at all.
//
// Three exclusions define "contains":
//
//   - a subdirectory carrying its own go.mod is a DIFFERENT module, and its
//     binary is not this module's;
//   - `vendor`, `testdata`, and `_`/`.`-prefixed directories are invisible to
//     the build, so a vendored command is not this module's command;
//   - a file no tag assignment can build (`//go:build ignore`, the generator
//     script convention) is never linked into anything.
//
// `_test.go` files are excluded too.
//
// The walk is lexical and deterministic, and it stops at the first main package.
func ModuleHasMain(dir string) (bool, error) {
	found := false
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := d.Name()
		if d.IsDir() {
			if p == dir {
				return nil
			}
			if name == "vendor" || ToolingIgnoresPath(name) {
				return fs.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(p, "go.mod")); statErr == nil {
				return fs.SkipDir
			} else if !os.IsNotExist(statErr) {
				return statErr
			}
			return nil
		}
		if filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") || ToolingIgnoresPath(name) {
			return nil
		}
		header, readErr := readHeader(p)
		if readErr != nil {
			return readErr
		}
		if header.Package == "main" && header.buildable() {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return false, err
	}
	return found, nil
}

// packageClause returns the identifier the package clause declares, or "" when
// src carries none that parses.
//
// go/parser is the Go grammar itself, so a `package main` inside a string
// literal or a block comment cannot be mistaken for the clause — the reason
// this is not a line scan. PackageClauseOnly stops the parser at the clause, so
// a syntax error further down the file (or a truncated prefix) is not an error
// here.
func packageClause(src []byte) string {
	file, err := parser.ParseFile(token.NewFileSet(), "", src, parser.PackageClauseOnly)
	if err != nil || file == nil || file.Name == nil {
		return ""
	}
	return file.Name.Name
}

// buildConstraintExpr returns the file's build-constraint expression, or nil
// when it has none: the first `//go:build` line wins; otherwise legacy
// `// +build` lines are ANDed together per the build-constraint spec. Only
// lines before the package clause are considered.
func buildConstraintExpr(src []byte) constraint.Expr {
	var plus constraint.Expr
	for _, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "package ") || trimmed == "package" {
			break
		}
		if constraint.IsGoBuild(trimmed) {
			if expr, err := constraint.Parse(trimmed); err == nil {
				return expr
			}
			return nil
		}
		if constraint.IsPlusBuild(trimmed) {
			expr, err := constraint.Parse(trimmed)
			if err != nil {
				continue
			}
			if plus == nil {
				plus = expr
			} else {
				plus = &constraint.AndExpr{X: plus, Y: expr}
			}
		}
	}
	return plus
}

func collectConstraintTags(expr constraint.Expr, out map[string]struct{}) {
	switch v := expr.(type) {
	case *constraint.TagExpr:
		out[v.Tag] = struct{}{}
	case *constraint.NotExpr:
		collectConstraintTags(v.X, out)
	case *constraint.AndExpr:
		collectConstraintTags(v.X, out)
		collectConstraintTags(v.Y, out)
	case *constraint.OrExpr:
		collectConstraintTags(v.X, out)
		collectConstraintTags(v.Y, out)
	}
}
