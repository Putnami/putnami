package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	// ReleaseBaselineUntagged: HEAD reaches no tag of the line.
	ReleaseBaselineUntagged = "untagged"
	// ReleaseBaselineTagged: HEAD reaches a tag of the line.
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
	// Tag is the line's last tag HEAD reaches, when State is tagged.
	Tag string `json:"tag,omitempty"`
	// Object is the type and ID of the object Tag holds at the project
	// directory ("tree <id>"); empty when the directory is absent at Tag.
	Object string `json:"object,omitempty"`
	// Breaking reports whether a commit after Tag up to HEAD that touches the
	// project directory declares a breaking change (ParseConventional).
	Breaking bool `json:"breaking,omitempty"`
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

// ReadReleaseBaseline reads the release baseline of the project in dir, whose
// version line renders its tags with tagPattern.
//
// It reads in the order the check does, and stops where the check stops:
// outside a work tree, then without a commit, then in a shallow clone, then
// without a reachable line tag. A tagged line adds the object the tag holds
// at dir and whether a commit since the tag that touches dir declares a
// breaking change. A git failure is an error, never a state: a state read
// from a failure would key the check's verdict to a baseline it did not see.
func ReadReleaseBaseline(dir, tagPattern string) (ReleaseBaseline, error) {
	baseline := ReleaseBaseline{Pattern: tagPattern}
	output, stderr, err := runCapture(dir, "rev-parse", "--is-inside-work-tree", "--is-shallow-repository", "--show-prefix")
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
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

	if _, _, err := runCapture(dir, "rev-parse", "--verify", "--quiet", "HEAD^{commit}"); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return ReleaseBaseline{}, fmt.Errorf("read HEAD of %s: %w", dir, err)
		}
		baseline.State = ReleaseBaselineNoCommit
		return baseline, nil
	}
	if shallow {
		baseline.State = ReleaseBaselineShallow
		return baseline, nil
	}

	tag, commit, ok, err := LastReachableTag(dir, tagPattern)
	if err != nil {
		return ReleaseBaseline{}, err
	}
	if !ok {
		refs, err := run(dir, "for-each-ref", "--count=1", "--format=%(refname)", "refs/tags")
		if err != nil {
			return ReleaseBaseline{}, fmt.Errorf("read the tags: %w", err)
		}
		baseline.State, baseline.HasTags = ReleaseBaselineUntagged, strings.TrimSpace(refs) != ""
		return baseline, nil
	}

	baseline.State, baseline.Tag = ReleaseBaselineTagged, tag
	if baseline.Object, err = objectAtTag(dir, tag, prefix); err != nil {
		return ReleaseBaseline{}, err
	}
	commits, err := CommitsSince(dir, commit, []string{"."})
	if err != nil {
		return ReleaseBaseline{}, err
	}
	for _, c := range commits {
		if _, breaking, ok := ParseConventional(c.Subject, c.Body); ok && breaking {
			baseline.Breaking = true
			break
		}
	}
	return baseline, nil
}

// objectAtTag returns the type and ID of the object tag holds at prefix, a
// slash path relative to the work tree's root without a trailing slash; empty
// when prefix is absent at tag. The empty prefix is the root, whose object is
// the tag's tree.
func objectAtTag(dir, tag, prefix string) (string, error) {
	ref := "refs/tags/" + tag
	if prefix == "" {
		output, err := run(dir, "rev-parse", "--verify", "--end-of-options", ref+"^{tree}")
		if err != nil {
			return "", fmt.Errorf("read the tree of %s: %w", tag, err)
		}
		return "tree " + strings.TrimSpace(output), nil
	}
	output, stderr, err := runCaptureEnv(dir, append(os.Environ(), "GIT_LITERAL_PATHSPECS=1"),
		"ls-tree", "-z", "--full-tree", ref, "--", prefix)
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
