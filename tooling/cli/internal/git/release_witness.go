package git

import (
	"bufio"
	"crypto/sha256"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"go.putnami.dev/sdk/extension/linetag"
)

// releaseWitness is the digest of what names the commits a release baseline
// is read from (ReleaseBaselineReader.releaseWitness): the record keys a baseline by it.
type baselineWitness [sha256.Size]byte

// historyRewriters are the variables under which Git reads a commit's history
// other than as its objects name it: replace refs and grafts. With one set, no
// baseline is recorded or answered (tooling/cli ADR 0113 §4).
var historyRewriters = []string{"GIT_REPLACE_REF_BASE", "GIT_NO_REPLACE_OBJECTS", "GIT_GRAFT_FILE"}

// releaseWitness returns the digest of what names the commits a baseline is
// read from (tooling/cli ADR 0113 §4), and false when part of it has no
// witness and nothing is recorded or answered:
//
//   - HEAD's commit and every tag with the object it peels to, as the
//     reader's one read of the tags holds them (linetag.Tags.Snapshot), and
//     the tags HEAD reaches with the object each names (linetag.Tags.Merged);
//   - the repository state: inside a work tree, not shallow, the root's
//     prefix and its physical path;
//   - no history rewrite: no replace ref (loose, packed, or a reftable the
//     reader cannot read), no grafts file, none of historyRewriters set;
//   - the configuration: Git's whole reading (localConfig.Reading), the
//     values of every GIT_ variable, HOME and XDG_CONFIG_HOME.
//
// The tags and the repository state are the reader's one read, which the
// baselines it reads are read from; the other parts are read when it is
// called.
func (r *ReleaseBaselineReader) releaseWitness() (baselineWitness, bool) {
	r.tagsReadOnce.Do(r.openTags)
	return witnessOf(r.root, r.resolved, r.inside, r.shallow, r.prefix, r.tags)
}

// recheckWitness takes the whole witness again, the repository state, HEAD
// and the tags included, from reads of its own: the record calls it before it
// writes the baselines read under the reader's witness, so a baseline read
// while a part moved is not kept.
func (r *ReleaseBaselineReader) recheckWitness() (baselineWitness, bool) {
	resolved, err := filepath.EvalSymlinks(r.root)
	if err != nil {
		return baselineWitness{}, false
	}
	output, _, err := dirReleaseReads{}.state(r.root)
	if err != nil {
		return baselineWitness{}, false
	}
	lines := strings.Split(strings.TrimSuffix(output, "\n"), "\n")
	if len(lines) != 3 {
		return baselineWitness{}, false
	}
	return witnessOf(r.root, resolved, lines[0], lines[1], strings.TrimSuffix(lines[2], "/"), linetag.Open(r.root))
}

// witnessOf computes the witness of the repository at root from its state
// and its tags.
func witnessOf(root, resolved, inside, shallow, prefix string, tagReads *linetag.Tags) (baselineWitness, bool) {
	if resolved == "" || inside != "true" || shallow != "false" {
		return baselineWitness{}, false
	}
	for _, name := range historyRewriters {
		if _, set := os.LookupEnv(name); set {
			return baselineWitness{}, false
		}
	}
	top, err := workTreeRootInProcess(root)
	if err != nil {
		return baselineWitness{}, false
	}
	repo, err := openLocal(top)
	if err != nil {
		return baselineWitness{}, false
	}
	if rewritesHistory(repo.commonDir) {
		return baselineWitness{}, false
	}
	head, tags, err := tagReads.Snapshot()
	if err != nil || head == "" {
		return baselineWitness{}, false
	}
	// The tags HEAD reaches are a read of their own, which LastRelease
	// decides from: a tag deleted or moved between the snapshot and it shows
	// here, and the witness taken again at save differs.
	merged, err := tagReads.Merged()
	if err != nil {
		return baselineWitness{}, false
	}

	hash := sha256.New()
	part := func(values ...string) {
		for _, value := range values {
			hash.Write([]byte(strconv.Itoa(len(value))))
			hash.Write([]byte{':'})
			hash.Write([]byte(value))
		}
	}
	part("root", resolved, top, prefix)
	part("head", head)
	part("tags", strconv.Itoa(len(tags)))
	for _, tag := range tags {
		part(tag.Name, tag.Object)
	}
	part("merged", strconv.Itoa(len(merged)))
	part(merged...)
	part("config", string(repo.config.Reading[:]))
	environment := os.Environ()
	slices.Sort(environment)
	for _, variable := range environment {
		name, _, _ := strings.Cut(variable, "=")
		if strings.HasPrefix(name, "GIT_") || name == "HOME" || name == "XDG_CONFIG_HOME" {
			part("env", variable)
		}
	}
	var witness baselineWitness
	copy(witness[:], hash.Sum(nil))
	return witness, true
}

// rewritesHistory reports whether the repository whose common directory is
// commonDir may read a commit's history other than as its objects name it, or
// keeps its refs where this check cannot see a replace ref: a grafts file, a
// refs/replace directory, a refs/replace/ line in packed-refs, a reftable.
// What it cannot read counts as a rewrite.
func rewritesHistory(commonDir string) bool {
	for _, path := range []string{
		filepath.Join(commonDir, "info", "grafts"),
		filepath.Join(commonDir, "refs", "replace"),
		filepath.Join(commonDir, "reftable"),
	} {
		if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
			return true
		}
	}
	packed, err := os.Open(filepath.Join(commonDir, "packed-refs")) //nolint:gosec // the repository's own refs
	if errors.Is(err, fs.ErrNotExist) {
		return false
	}
	if err != nil {
		return true
	}
	defer packed.Close() //nolint:errcheck // a read-only file
	scanner := bufio.NewScanner(packed)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	for scanner.Scan() {
		if strings.Contains(scanner.Text(), " refs/replace/") {
			return true
		}
	}
	return scanner.Err() != nil
}
