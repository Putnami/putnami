// Package payload runs the agent-readiness collector: it reads a repository
// and its last 90 days of history, and returns the schema v1 payload. It
// never uses the network, never writes to the repository, and never runs the
// repository's own commands; it only runs read-only git commands.
package payload

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.putnami.dev/intelligence/agent-readiness/areas"
	"go.putnami.dev/intelligence/agent-readiness/contract"
	"go.putnami.dev/intelligence/agent-readiness/history"
	"go.putnami.dev/intelligence/agent-readiness/internal/gitrepo"
	"go.putnami.dev/intelligence/agent-readiness/internal/scope"
	"go.putnami.dev/intelligence/agent-readiness/inventory"
	"go.putnami.dev/intelligence/agent-readiness/markers"
)

// DefaultTimeout bounds a collection's wall time.
const DefaultTimeout = 60 * time.Second

// saltBytes is the size of the per-run salt that pseudonymizes authors.
const saltBytes = 32

// Options configure one collection.
type Options struct {
	// Dir is any directory inside the repository.
	Dir string
	// CollectorVersion is the version of the program that collects.
	CollectorVersion string
	// Now is the collection time; zero means the current time.
	Now time.Time
	// Salt pseudonymizes author emails; nil draws 32 random bytes. Only
	// tests set it.
	Salt []byte
	// Timeout bounds the wall time; zero means DefaultTimeout.
	Timeout time.Duration
}

// Result is a collected payload and what the collection cost.
type Result struct {
	Payload contract.Payload
	// Elapsed is the collection's wall time. The payload does not carry it:
	// schema v1 has no field for it.
	Elapsed time.Duration
}

// ErrPrivacy is returned when the payload would carry something that must
// never leave the machine.
var ErrPrivacy = errors.New("payload fails the privacy check")

// Collect reads the repository that contains opts.Dir.
func Collect(ctx context.Context, opts Options) (Result, error) {
	started := time.Now()
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	now = now.UTC().Truncate(time.Second)
	salt := opts.Salt
	if salt == nil {
		salt = make([]byte, saltBytes)
		if _, err := rand.Read(salt); err != nil {
			return Result{}, fmt.Errorf("draw salt: %w", err)
		}
	}
	collected, err := collect(ctx, opts.Dir, now, salt)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Result{}, fmt.Errorf("collection exceeded %s: %w", timeout, err)
		}
		return Result{}, err
	}
	collected.Meta.CollectorVersion = opts.CollectorVersion
	violations, err := contract.PrivacyViolations(collected)
	if err != nil {
		return Result{}, err
	}
	if len(violations) > 0 {
		return Result{}, fmt.Errorf("%w: %s", ErrPrivacy, strings.Join(violations, "; "))
	}
	return Result{Payload: collected, Elapsed: time.Since(started)}, nil
}

func collect(ctx context.Context, dir string, now time.Time, salt []byte) (contract.Payload, error) {
	repo, err := gitrepo.Open(ctx, dir)
	if err != nil {
		return contract.Payload{}, err
	}
	head, _, err := repo.Head(ctx)
	if err != nil {
		return contract.Payload{}, err
	}
	roots, err := repo.RootCommits(ctx)
	if err != nil {
		return contract.Payload{}, err
	}
	commitsTotal, err := repo.CountCommits(ctx)
	if err != nil {
		return contract.Payload{}, err
	}
	files, err := repo.Tree(ctx)
	if err != nil {
		return contract.Payload{}, err
	}
	generated, err := scope.Load(ctx, repo)
	if err != nil {
		return contract.Payload{}, err
	}
	contents, err := repo.ReadFiles(ctx, wanted(files))
	if err != nil {
		return contract.Payload{}, err
	}
	var paths []string
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	layout := areas.Detect(paths, func([]string) map[string][]byte { return contents })
	signals, err := repo.Grep(ctx, markers.SignalPattern, markers.SignalPathspecs...)
	if err != nil {
		return contract.Payload{}, err
	}
	signalFiles := markers.SignalFiles(signals)
	signalContents, err := repo.ReadFiles(ctx, signalFiles)
	if err != nil {
		return contract.Payload{}, err
	}
	// The window's count and HEAD's sample read skips with the same rule.
	signal := markers.NewSignalContext(paths, contents, signalContents)
	// Counting the lines the window added to the matched files diffs them
	// again, so it runs beside the history passes rather than after them.
	var signalsAdded map[string]int
	signalsErr := make(chan error, 1)
	go func() {
		var err error
		signalsAdded, err = history.AddedMatches(ctx, repo, now, markers.SignalPattern, signalFiles, signal.Counts)
		signalsErr <- err
	}()
	window, err := history.Read(ctx, repo, now, generated.Authored)
	if waitErr := <-signalsErr; err == nil {
		err = waitErr
	}
	if err != nil {
		return contract.Payload{}, err
	}
	dates, err := repo.Dates(ctx, markers.Dated(files, layout))
	if err != nil {
		return contract.Payload{}, err
	}
	atHead := make(map[string]bool, len(paths))
	for _, file := range paths {
		atHead[file] = true
	}
	repoActivity, areaActivity := markers.Measure(layout, window.Commits, func(file string) bool { return atHead[file] })

	inv := inventory.Detect(inventory.Input{
		Files: files, Authored: generated.Authored, Contents: contents, Changed: dates.Changed, Now: now.Unix(),
	})
	inv.CommitsTotal = commitsTotal
	inv.Commits90d = repoActivity.Commits
	inv.ActiveContributors90d = len(repoActivity.Authors)
	agentCommits, agentReverted := repoActivity.AgentCommits, repoActivity.AgentReverted
	inv.AgentCommits90d, inv.AgentCommitsReverted90d = &agentCommits, &agentReverted
	fingerprint, age, born := rootFacts(roots, now)
	inv.RepoAgeDays = age

	return contract.Payload{
		Meta: contract.PayloadMeta{
			MethodVersion:   contract.MethodVersion,
			CollectedAt:     now,
			RepoFingerprint: fingerprint,
			HeadCommit:      head,
		},
		Inventory: inv,
		Areas:     areaList(layout, files, generated, repoActivity, areaActivity, salt),
		Markers: markers.Compute(markers.Input{
			Now: now.Unix(), Born: born, Files: files, Layout: layout, Repo: repoActivity, Areas: areaActivity,
			Changes: window.Changes, Contents: contents, Dates: dates, Signals: signals, SignalContents: signalContents, SignalsAdded: signalsAdded,
		}),
	}, nil
}

func wanted(files []gitrepo.File) []string {
	paths := make([]string, len(files))
	for i, file := range files {
		paths[i] = file.Path
	}
	seen := map[string]bool{}
	var all []string
	for _, group := range [][]string{areas.Wanted(paths), inventory.Wanted(files), markers.Wanted(files)} {
		for _, file := range group {
			if !seen[file] {
				seen[file] = true
				all = append(all, file)
			}
		}
	}
	sort.Strings(all)
	return all
}

// rootFacts returns the repository fingerprint, the SHA-256 of the lexically
// smallest root commit id, and the age in days and the committer time of the
// oldest root commit.
func rootFacts(roots map[string]int64, now time.Time) (string, int, int64) {
	ids := make([]string, 0, len(roots))
	var oldest int64
	for id, at := range roots {
		ids = append(ids, id)
		if oldest == 0 || at < oldest {
			oldest = at
		}
	}
	sort.Strings(ids)
	if len(ids) == 0 {
		return "", 0, 0
	}
	sum := sha256.Sum256([]byte(ids[0]))
	age := max(0, int((now.Unix()-oldest)/(24*60*60)))
	return hex.EncodeToString(sum[:]), age, oldest
}

func areaList(layout areas.Layout, files []gitrepo.File, generated *scope.Scope, repo markers.Activity, activity []markers.Activity, salt []byte) []contract.Area {
	type size struct {
		files, lines int
		languages    map[string]int
	}
	sizes := make([]size, len(layout.Areas))
	for _, file := range files {
		area := layout.Assign(file.Path)
		if area < 0 || !file.Text || !generated.Authored(file.Path) {
			continue
		}
		sizes[area].files++
		sizes[area].lines += file.Lines
		if language := inventory.Language(file.Path); language != "" {
			if sizes[area].languages == nil {
				sizes[area].languages = map[string]int{}
			}
			sizes[area].languages[language] += file.Lines
		}
	}
	list := make([]contract.Area, 0, len(layout.Areas))
	for i, area := range layout.Areas {
		share := 0.0
		if repo.Commits > 0 {
			share = float64(activity[i].Commits) / float64(repo.Commits)
		}
		list = append(list, contract.Area{
			Name:          area.Name,
			Path:          area.Path,
			Source:        area.Source,
			Role:          area.Role,
			Language:      dominant(sizes[i].languages),
			Files:         sizes[i].files,
			Lines:         sizes[i].lines,
			Commits90d:    activity[i].Commits,
			ShareOfChange: float64(int(share*10000+0.5)) / 10000,
			Authors90d:    Pseudonyms(activity[i].Authors, salt),
		})
	}
	return list
}

func dominant(languages map[string]int) string {
	best, bestLines := "", -1
	for name, lines := range languages {
		if lines > bestLines || (lines == bestLines && name < best) {
			best, bestLines = name, lines
		}
	}
	return best
}

// Pseudonyms returns the sorted per-run pseudonyms of author emails: the
// first 16 hex digits of SHA-256(salt + ":" + lowercase email).
func Pseudonyms(emails map[string]bool, salt []byte) []string {
	out := make([]string, 0, len(emails))
	for email := range emails {
		sum := sha256.Sum256(append(append(append([]byte{}, salt...), ':'), strings.ToLower(email)...))
		out = append(out, hex.EncodeToString(sum[:])[:16])
	}
	sort.Strings(out)
	return out
}
