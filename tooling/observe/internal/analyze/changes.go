package analyze

import (
	"sort"

	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/tooling/observe/internal/gitbase"
	"go.putnami.dev/tooling/observe/internal/lockdiff"
)

// changedLineCoverage crosses the lines the change adds with the merged line
// coverage. It needs a base and at least one coverage report.
func (r *Result) changedLineCoverage(in Input, lines map[string]lineHits) {
	if in.AddedLines == nil || !hasCoverage(in) {
		return
	}
	out := &evidence.ChangedLineCoverage{Files: []evidence.ChangedLineCoverageFile{}}
	for _, file := range in.ChangedFiles {
		if file.Status == gitbase.StatusDeleted {
			continue
		}
		hits, instrumented := lines[file.Path]
		if !instrumented {
			continue
		}
		entry := evidence.ChangedLineCoverageFile{Path: file.Path}
		for _, n := range in.AddedLines[file.Path] {
			count, ok := hits[n]
			if !ok {
				continue
			}
			entry.TotalLines++
			if count > 0 {
				entry.CoveredLines++
			} else if len(entry.UncoveredLines) < evidence.MaxUncoveredLines {
				entry.UncoveredLines = append(entry.UncoveredLines, n)
			}
		}
		out.CoveredLines += entry.CoveredLines
		out.TotalLines += entry.TotalLines
		out.Files = append(out.Files, entry)
		if len(out.Files) == evidence.MaxChangedLineCoverageFile {
			break
		}
	}
	sort.Slice(out.Files, func(i, j int) bool { return out.Files[i].Path < out.Files[j].Path })
	r.ChangedLineCoverage = out
}

func hasCoverage(in Input) bool {
	for _, report := range in.Reports {
		if report.Kind == "coverage" {
			return true
		}
	}
	return false
}

// dependencyChanges diffs each changed lockfile between the base and the
// head.
func (r *Result) dependencyChanges(in Input) {
	if in.Show == nil {
		return
	}
	for _, file := range in.ChangedFiles {
		ecosystem, read, ok := lockReader(in, file.Path)
		if !ok {
			continue
		}
		basePath := file.Path
		if file.PreviousPath != "" {
			basePath = file.PreviousPath
		}
		before, ok := r.readLock(in, Base, basePath, file.Status != gitbase.StatusAdded, read)
		if !ok {
			continue
		}
		after, ok := r.readLock(in, Head, file.Path, file.Status != gitbase.StatusDeleted, read)
		if !ok {
			continue
		}
		owner := r.Owners[file.Path]
		for _, change := range lockdiff.Diff(before, after) {
			if len(r.DependencyChanges) == evidence.MaxDependencyChanges {
				return
			}
			r.DependencyChanges = append(r.DependencyChanges, evidence.DependencyChange{
				Ecosystem: ecosystem, Lockfile: file.Path, Name: bounded(change.Name), Change: change.Change,
				From: bounded(change.From), To: bounded(change.To), Component: owner,
			})
		}
	}
}

// lockReader returns the reader of a lockfile: a detector's own, such as the
// Go detector's for go.mod, else the generic one its name selects.
func lockReader(in Input, file string) (string, lockdiff.Reader, bool) {
	if ecosystem, read, ok := in.Set.Lockfile(file); ok {
		return ecosystem, func(data []byte) (lockdiff.Packages, error) {
			packages, err := read(data)
			return lockdiff.Packages(packages), err
		}, true
	}
	kind, ok := lockdiff.KindOf(file)
	return kind.Ecosystem, kind.Read, ok
}

// readLock reads one side of a lockfile. A side the change has no file on
// reads as no package.
func (r *Result) readLock(in Input, side Revision, file string, expected bool, read lockdiff.Reader) (lockdiff.Packages, bool) {
	if !expected {
		return lockdiff.Packages{}, true
	}
	data, found, err := in.Show(side, file)
	if err != nil {
		r.warn(CodeLockfileUnread, file, "the lockfile %s cannot be read: %v", file, err)
		return nil, false
	}
	if !found {
		return lockdiff.Packages{}, true
	}
	packages, err := read(data)
	if err != nil {
		r.warn(CodeLockfileUnread, file, "the lockfile %s cannot be read: %v", file, err)
		return nil, false
	}
	return packages, true
}
