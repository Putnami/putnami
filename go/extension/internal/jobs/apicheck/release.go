package apicheck

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/go/extension/internal/apisurface"
	job "go.putnami.dev/protocol/job"
	"go.putnami.dev/sdk/extension/linetag"
)

// baseline is what the check reads of its line's last release: the
// repository state, the line's tag pattern, and for a tagged line the tag,
// what the tag holds at the project directory, and whether a commit since the
// tag declares a breaking change. The job context carries it when the task
// declares the releaseBaseline and releaseBaselineFiles runtime inputs
// (contextBaseline); without it the check reads Git (gitBaseline). Both read
// the same states in the same order, so one key gives one verdict.
type baseline struct {
	// state is one of the job.ReleaseBaseline* states.
	state string
	// pattern is the line's tag pattern.
	pattern string
	// hasTags reports, for an untagged line, whether the repository has a tag
	// of any line.
	hasTags bool
	// tag is the line's last release, when state is tagged.
	tag string
	// release reads what tag holds at the project directory, when state is
	// tagged.
	release release
	// breaking reports, when state is tagged, whether a commit since tag that
	// touches the project declares a breaking change. It is asked only when
	// the check finds an incompatible change.
	breaking func() (bool, error)
}

// contextBaseline is the baseline the job context carries. The context names
// the directory that holds the files the tag holds at the project directory;
// a tagged baseline without one says the project directory is absent at the
// tag, because the task declares releaseBaselineFiles and the CLI that writes
// the member extracts the files for such a task.
func contextBaseline(fact *job.ReleaseBaseline) (baseline, error) {
	if !slices.Contains(job.ReleaseBaselineStates, fact.State) {
		return baseline{}, fmt.Errorf("the job context's release baseline has the unknown state %q", fact.State)
	}
	b := baseline{state: fact.State, pattern: fact.Pattern, hasTags: fact.HasTags, tag: fact.Tag}
	if fact.State == job.ReleaseBaselineTagged {
		breaking := fact.Breaking
		b.release = directoryRelease{dir: fact.Directory}
		b.breaking = func() (bool, error) { return breaking, nil }
	}
	return b, nil
}

// gitBaseline reads the baseline of the project in projectDir from Git, in the
// order the CLI's release baseline reads it: outside a work tree, then without
// a commit, then in a shallow clone, then without a release of the line.
func gitBaseline(workspaceRoot, projectDir string) (baseline, error) {
	state, err := readRepoState(projectDir)
	if errors.Is(err, errNotARepository) {
		return baseline{state: job.ReleaseBaselineUnmanaged}, nil
	}
	if err != nil {
		return baseline{}, err
	}
	if !state.hasHead {
		return baseline{state: job.ReleaseBaselineNoCommit}, nil
	}
	pattern := linePattern(workspaceRoot, projectDir)
	if state.shallow {
		return baseline{state: job.ReleaseBaselineShallow, pattern: pattern}, nil
	}
	last, ok, err := linetag.LastRelease(projectDir, pattern)
	if err != nil {
		return baseline{}, err
	}
	if !ok {
		tagged, err := hasTags(projectDir)
		if err != nil {
			return baseline{}, err
		}
		return baseline{state: job.ReleaseBaselineUntagged, pattern: pattern, hasTags: tagged}, nil
	}
	// The tag's files and the commits since it are read from the commit
	// LastRelease resolved, never through the tag's name again, so a tag
	// moved meanwhile cannot pair this tag with another commit's files.
	release := last.Commit
	return baseline{
		state: job.ReleaseBaselineTagged, pattern: pattern, tag: last.Tag,
		release: gitRelease{dir: projectDir, rev: release},
		breaking: func() (bool, error) {
			commits, err := commitsSince(projectDir, release)
			if err != nil {
				return false, err
			}
			return slices.ContainsFunc(commits, declaresBreak), nil
		},
	}, nil
}

// release reads what the line's last tag holds at the project directory. Only
// the regular files the tag holds exist for it: a symbolic link, a submodule
// or a directory at a path is absent, whether the files come from the job
// context's extraction, which holds regular files alone, or from Git.
type release interface {
	// files returns the files the API surface reads, with slash paths
	// relative to the project directory, as filesAtTag selects them.
	files() ([]apisurface.File, error)
	// file returns the regular file at rel, a slash path relative to the
	// project directory, and false when the tag holds none there.
	file(rel string) (releasedFile, bool, error)
}

// releasedFile is one regular file the tag holds: its size, known before its
// content is read, and a reader of its content.
type releasedFile struct {
	size int
	read func() ([]byte, error)
}

// gitRelease reads the tree of rev, the commit of the line's last tag, from
// the repository holding dir.
type gitRelease struct {
	dir string
	rev string
}

func (r gitRelease) files() ([]apisurface.File, error) { return filesAtTag(r.dir, r.rev) }

func (r gitRelease) file(rel string) (releasedFile, bool, error) {
	entry, found, err := entryAtTag(r.dir, r.rev, rel)
	if err != nil || !found || !entry.regular {
		return releasedFile{}, false, err
	}
	return releasedFile{size: entry.size, read: func() ([]byte, error) {
		contents, err := readBlobs(r.dir, []string{entry.object})
		if err != nil {
			return nil, err
		}
		return contents[0], nil
	}}, true, nil
}

// directoryRelease reads the directory the job context names, which holds the
// regular files the tag holds at the project directory; an empty dir says the
// project directory is absent at the tag. The job reads it and never writes
// to it.
type directoryRelease struct {
	dir string
}

func (r directoryRelease) files() ([]apisurface.File, error) {
	if r.dir == "" {
		return nil, nil
	}
	var listed []string
	err := filepath.WalkDir(r.dir, func(file string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(r.dir, file)
		if err != nil {
			return err
		}
		if rel = filepath.ToSlash(rel); !inUnkeyedDirectory(rel) {
			listed = append(listed, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("read the release baseline directory %s: %w", r.dir, err)
	}
	slices.Sort(listed)
	paths := apisurface.Reads(listed)
	files := make([]apisurface.File, len(paths))
	for i, name := range paths {
		data, err := os.ReadFile(filepath.Join(r.dir, filepath.FromSlash(name))) //nolint:gosec // a file of the release baseline directory
		if err != nil {
			return nil, fmt.Errorf("read the release baseline directory %s: %w", r.dir, err)
		}
		files[i] = apisurface.File{Path: name, Data: data}
	}
	return files, nil
}

// file matches each name of rel exactly, case included, as Git does at the
// tag, so a case-insensitive file system does not find a differently spelled
// file.
func (r directoryRelease) file(rel string) (releasedFile, bool, error) {
	if r.dir == "" {
		return releasedFile{}, false, nil
	}
	parts := strings.Split(rel, "/")
	for i, part := range parts {
		dir := filepath.Join(r.dir, filepath.FromSlash(path.Join(parts[:i]...)))
		entries, err := os.ReadDir(dir)
		if errors.Is(err, fs.ErrNotExist) {
			return releasedFile{}, false, nil
		}
		if err != nil {
			return releasedFile{}, false, fmt.Errorf("read the release baseline directory %s: %w", r.dir, err)
		}
		entry, _ := directoryEntry(entries, part)
		if entry == nil {
			return releasedFile{}, false, nil
		}
		last := i == len(parts)-1
		if (last && !entry.Type().IsRegular()) || (!last && !entry.IsDir()) {
			return releasedFile{}, false, nil
		}
	}
	file := filepath.Join(r.dir, filepath.FromSlash(rel))
	info, err := os.Lstat(file)
	if err != nil {
		return releasedFile{}, false, fmt.Errorf("read the release baseline directory %s: %w", r.dir, err)
	}
	return releasedFile{size: int(info.Size()), read: func() ([]byte, error) {
		return os.ReadFile(file) //nolint:gosec // a file of the release baseline directory
	}}, true, nil
}
