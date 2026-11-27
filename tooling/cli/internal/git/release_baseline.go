package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"

	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/sdk/extension/gitread"
	"go.putnami.dev/sdk/extension/linetag"
)

// The repository states a release baseline distinguishes. A check that
// compares the working tree with its line's last release answers each one
// differently, so each one is a different baseline.
const (
	// ReleaseBaselineUnmanaged: the directory is outside every git work tree.
	ReleaseBaselineUnmanaged = "unmanaged"
	// ReleaseBaselineNoCommit: the repository has no commit at HEAD.
	ReleaseBaselineNoCommit = "no-commit"
	// ReleaseBaselineShallow: the clone is shallow, so its tags and history
	// are incomplete.
	ReleaseBaselineShallow = "shallow"
	// ReleaseBaselineUntagged: the line has no last release (linetag.LastRelease).
	ReleaseBaselineUntagged = "untagged"
	// ReleaseBaselineTagged: the line has a last release.
	ReleaseBaselineTagged = "tagged"
)

// ReleaseBaseline is what git says about a project's last release, as a check
// that compares the working tree with that release reads it. It names no
// commit HEAD reaches and no ref HEAD is on: two histories with the same
// baseline, such as a branch and its squash merge, give the same value. A
// squash whose message adds or drops the breaking marker is another baseline.
type ReleaseBaseline struct {
	// State is one of the ReleaseBaseline* states.
	State string `json:"state"`
	// Pattern is the line's tag pattern.
	Pattern string `json:"pattern"`
	// HasTags reports, for an untagged line, whether the repository has a
	// tag of any line.
	HasTags bool `json:"hasTags,omitempty"`
	// Tag is the tag of the line's last release, when State is tagged.
	Tag string `json:"tag,omitempty"`
	// Object is the type and ID of the object Tag holds at the project
	// directory ("tree <id>"); empty when the directory is absent at Tag.
	Object string `json:"object,omitempty"`
	// Breaking reports whether a commit after Tag up to HEAD that touches the
	// project directory declares a breaking change (protocolcli.ParseConventionalCommit).
	Breaking bool `json:"breaking,omitempty"`
	// Commit is the commit Tag points at, and Prefix the project directory
	// relative to the work tree's root, in slash form ("" at the root). Neither
	// is part of Value: they locate the files Tag holds for the job that reads
	// them, and the key holds Object, which names those files.
	Commit string `json:"-"`
	Prefix string `json:"-"`
}

// Value is the baseline's canonical encoding: the same baseline always gives
// the same bytes.
func (b ReleaseBaseline) Value() string {
	data, err := json.Marshal(b)
	if err != nil {
		// A struct of strings and booleans always encodes.
		panic(fmt.Sprintf("encode release baseline: %v", err))
	}
	return string(data)
}

// projectReads counts the baselines read past the line's last release: the
// reads of the object the release holds at the project and of the commits
// since it, which a recorded baseline saves. A test reads it.
var projectReads atomic.Int64

// lastRelease resolves a line's last release. It is a variable only so a test
// can move a tag between that read and the reads that follow it.
var lastRelease = linetag.LastRelease

// ReadReleaseBaseline reads the release baseline of the project in dir, whose
// version line renders its tags with tagPattern.
//
// It reads in the order the check does, and stops where the check stops:
// outside a work tree, then without a commit, then in a shallow clone, then
// without a release of the line. A tagged line adds the object the tag holds
// at dir and whether a commit since the tag that touches dir declares a
// breaking change. A git failure is an error, never a state: a state read
// from a failure would key the check's verdict to a baseline it did not see.
func ReadReleaseBaseline(dir, tagPattern string) (ReleaseBaseline, error) {
	return readReleaseBaseline(dir, tagPattern, dirReleaseReads{})
}

// releaseReads are the reads of a release baseline that do not depend on the
// project directory but on its repository: its state, its HEAD, its tags and
// a line's last release among them.
type releaseReads interface {
	// state answers rev-parse's --is-inside-work-tree,
	// --is-shallow-repository and --show-prefix lines in dir.
	state(dir string) (string, string, error)
	// hasCommit answers whether HEAD names a commit; a false answer with a
	// nil error is a repository without one.
	hasCommit(dir string) (bool, error)
	release(dir, tagPattern string) (linetag.Release, bool, error)
	// head names the commit a line's last release was reached from, up to
	// which the commits since the release are read.
	head(dir string) (string, error)
	// hasTags answers whether the repository has a tag of any line.
	hasTags(dir string) (bool, error)
}

// dirReleaseReads asks git in the project directory each time.
type dirReleaseReads struct{}

func (dirReleaseReads) state(dir string) (string, string, error) {
	return gitread.RunCapture(dir, "rev-parse", "--is-inside-work-tree", "--is-shallow-repository", "--show-prefix")
}

func (dirReleaseReads) hasCommit(dir string) (bool, error) {
	if _, _, err := gitread.RunCapture(dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return false, fmt.Errorf("read HEAD of %s: %w", dir, err)
		}
		return false, nil
	}
	return true, nil
}

func (dirReleaseReads) release(dir, tagPattern string) (linetag.Release, bool, error) {
	return lastRelease(dir, tagPattern)
}

func (dirReleaseReads) head(string) (string, error) { return "HEAD", nil }

func (dirReleaseReads) hasTags(dir string) (bool, error) {
	refs, err := gitread.Run(dir, "for-each-ref", "--count=1", "--format=%(refname)", "refs/tags")
	if err != nil {
		return false, fmt.Errorf("read the tags: %w", err)
	}
	return strings.TrimSpace(refs) != "", nil
}

func readReleaseBaseline(dir, tagPattern string, reads releaseReads) (ReleaseBaseline, error) {
	baseline := ReleaseBaseline{Pattern: tagPattern}
	output, stderr, err := reads.state(dir)
	if err != nil {
		// Only a directory outside every repository is unmanaged: a refused
		// repository, or a git that cannot run, is a failure, never a state.
		if OutsideRepository(gitread.Unmanaged(dir)) {
			baseline.State = ReleaseBaselineUnmanaged
			return baseline, nil
		}
		return ReleaseBaseline{}, fmt.Errorf("read the repository state of %s: %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	// The prefix line is empty at the work tree's root, so lines are split,
	// not fields.
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) == 0 || lines[0] != "true" {
		baseline.State = ReleaseBaselineUnmanaged
		return baseline, nil
	}
	if len(lines) != 3 {
		return ReleaseBaseline{}, fmt.Errorf("read the repository state of %s: unexpected git rev-parse output %q", dir, output)
	}
	shallow, prefix := lines[1] == "true", strings.TrimSuffix(lines[2], "/")

	if committed, err := reads.hasCommit(dir); err != nil {
		return ReleaseBaseline{}, err
	} else if !committed {
		baseline.State = ReleaseBaselineNoCommit
		return baseline, nil
	}
	if shallow {
		baseline.State = ReleaseBaselineShallow
		return baseline, nil
	}

	last, ok, err := reads.release(dir, tagPattern)
	if err != nil {
		return ReleaseBaseline{}, err
	}
	if !ok {
		hasTags, err := reads.hasTags(dir)
		if err != nil {
			return ReleaseBaseline{}, err
		}
		baseline.State, baseline.HasTags = ReleaseBaselineUntagged, hasTags
		return baseline, nil
	}

	projectReads.Add(1)
	baseline.State, baseline.Tag = ReleaseBaselineTagged, last.Tag
	baseline.Commit, baseline.Prefix = last.Commit, prefix
	// The object is read from the commit LastRelease returned, never through
	// the tag's name, so the key's object, Breaking and the files a job
	// receives all describe one commit even when the tag moves meanwhile.
	if baseline.Object, err = objectAtCommit(dir, last.Commit, prefix); err != nil {
		return ReleaseBaseline{}, err
	}
	head, err := reads.head(dir)
	if err != nil {
		return ReleaseBaseline{}, err
	}
	commits, err := gitread.CommitsBetween(dir, last.Commit, head, []string{"."})
	if err != nil {
		return ReleaseBaseline{}, err
	}
	for _, c := range commits {
		if parsed, ok := protocolcli.ParseConventionalCommit(c.Subject, c.Body); ok && parsed.Breaking {
			baseline.Breaking = true
			break
		}
	}
	return baseline, nil
}

// objectAtCommit returns the type and ID of the object commit holds at
// prefix, a slash path relative to the work tree's root without a trailing
// slash; empty when prefix is absent at commit. The empty prefix is the root,
// whose object is the commit's tree.
func objectAtCommit(dir, commit, prefix string) (string, error) {
	if prefix == "" {
		output, err := gitread.Run(dir, "rev-parse", "--verify", "--end-of-options", commit+"^{tree}")
		if err != nil {
			return "", fmt.Errorf("read the tree of %s: %w", commit, err)
		}
		return "tree " + strings.TrimSpace(output), nil
	}
	output, stderr, err := gitread.RunCaptureEnv(dir, append(os.Environ(), "GIT_LITERAL_PATHSPECS=1"),
		"ls-tree", "-z", "--full-tree", commit, "--", prefix)
	if err != nil {
		return "", fmt.Errorf("git ls-tree (in %s): %w: %s", dir, err, strings.TrimSpace(stderr))
	}
	for record := range strings.SplitSeq(output, "\x00") {
		meta, name, found := strings.Cut(record, "\t")
		if !found || name != prefix {
			continue
		}
		// An entry is "<mode> <type> <object>".
		fields := strings.Fields(meta)
		if len(fields) != 3 {
			return "", fmt.Errorf("git ls-tree: unexpected entry %q", record)
		}
		return fields[1] + " " + fields[2], nil
	}
	return "", nil
}
