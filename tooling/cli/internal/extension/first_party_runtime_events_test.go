package extension

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// First-party conformance for the runtime-event stream version.
//
// Since CLI contract 3 the CLI accepts EXACTLY the runtime-event version it
// advertises (`PUTNAMI_RUNTIME_EVENTS=2`), so jobs.parseRawEvent drops a `"v":1`
// line instead of adapting it. For a hand-written shell job that is worse than
// noisy: a script whose only output is a dropped `{"type":"result"}` line still
// exits 0, the runner synthesizes success from the exit code, and a cacheable
// task with an `optionalEmpty` output persists an EMPTY-SUCCESS cache entry that
// every later run replays. The one that motivated this test was
// tooling/scaffold/bin/scaffold-run's no-Go-toolchain SKIP arm.
//
// doc/04-job-execution.md states the rule ("Shell extensions that emit JSONL by
// hand must stamp `"v":2`"); this pins it, because the failure it prevents is
// silent by construction.

// firstPartyBinDirs are the first-party extension script directories. They are
// listed rather than walked so a directory that disappears is a test failure
// (the assertion would otherwise go vacuous) rather than one less thing checked.
var firstPartyBinDirs = []string{
	filepath.Join("go", "extension", "bin"),
	filepath.Join("python", "extension", "bin"),
	filepath.Join("tooling", "scaffold", "bin"),
	filepath.Join("tooling", "samples", "shell-extension", "bin"),
}

// legacyEventVersion matches a v1 stamp on an emitted JSONL line. The `\b`
// keeps `"v":10` — a version this CLI does not have yet, but might — from
// tripping it.
var legacyEventVersion = regexp.MustCompile(`"v"\s*:\s*1\b`)

func TestFirstPartyShellJobsStampRuntimeEventsV2(t *testing.T) {
	repoRoot := findRepoRoot(t)

	scanned := 0
	for _, rel := range firstPartyBinDirs {
		dir := filepath.Join(repoRoot, rel)
		info, err := os.Stat(dir)
		if err != nil || !info.IsDir() {
			t.Fatalf("first-party script directory %s is missing (%v); "+
				"if it moved, update firstPartyBinDirs — a silently empty scan asserts nothing", rel, err)
		}

		err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if d.IsDir() {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			scanned++
			relFile, _ := filepath.Rel(repoRoot, path)
			for i, line := range strings.Split(string(data), "\n") {
				// Prose about the old version is allowed; emitted lines are not.
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					continue
				}
				if legacyEventVersion.MatchString(line) {
					t.Errorf("%s:%d emits a runtime event stamped \"v\":1; "+
						"the CLI accepts exactly the version it advertises (v2), so this line is DROPPED — "+
						"a job whose result event is dropped is reported as a success it never claimed. "+
						"Stamp \"v\":2 (or source the shell-extension sample's bin/putnami-jsonl.sh).\n\t%s",
						relFile, i+1, strings.TrimSpace(line))
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", rel, err)
		}
	}

	if scanned == 0 {
		t.Fatal("no first-party extension scripts were scanned; the assertion never ran")
	}
}
