package runnerprovider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	protocolcli "go.putnami.dev/protocol/cli"
	runner "go.putnami.dev/protocol/runner"
)

const (
	gateID   = "20260916-101500-0a1b2c"
	nestedID = "20260916-101501-ffeedd"
)

func sessionDocument(id, parent string, exitCode int) []byte {
	run := protocolcli.RunSummary{Outcome: protocolcli.RunOutcomeSuccess, ExitCode: exitCode, Counts: protocolcli.RunCounts{Total: 1, Succeeded: 1}}
	if exitCode != 0 {
		run = protocolcli.RunSummary{Outcome: protocolcli.RunOutcomeFailure, ExitCode: exitCode, Counts: protocolcli.RunCounts{Total: 1, Failed: 1}}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	data, _ := json.Marshal(&protocolcli.SessionFile{
		ProtocolVersion: protocolcli.ResultProtocolVersion, SessionID: id, ParentSessionID: parent,
		StartTime: now, EndTime: now, Commands: []string{"build"}, Run: run,
		Placement: &protocolcli.SessionPlacement{Requested: "remote", Actual: "remote"},
	})
	return data
}

func planDocument(id string) []byte {
	data, _ := json.Marshal(&protocolcli.SessionPlanFile{ProtocolVersion: protocolcli.ResultProtocolVersion, SessionID: id, Commands: []string{"build"}, Tasks: []protocolcli.PlannedTask{}})
	return data
}

// stageBundle writes the files into the exchange directory by digest and
// returns the bundle that names them.
func stageBundle(t *testing.T, exchange string, sessions map[string]map[string][]byte, gate string) runner.SessionBundle {
	t.Helper()
	bundle := runner.SessionBundle{Attempt: "attempt-1", SessionID: gate}
	ids := []string{}
	for id := range sessions {
		ids = append(ids, id)
	}
	if gate != "" {
		ids = append([]string{gate}, runner.SortStrings(removeString(ids, gate))...)
	}
	for _, id := range ids {
		session := runner.BundleSession{ID: id}
		for name, data := range sessions[id] {
			sum := sha256.Sum256(data)
			digest := "sha256:" + hex.EncodeToString(sum[:])
			path, _ := runner.ExchangeBlobPath(exchange, digest)
			if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			session.Files = append(session.Files, runner.BundleFile{Name: name, Digest: digest, Size: int64(len(data))})
		}
		var document protocolcli.SessionFile
		_ = json.Unmarshal(sessions[id][runner.BundleSessionFile], &document)
		session.ParentID = document.ParentSessionID
		bundle.Sessions = append(bundle.Sessions, session)
	}
	return bundle
}

func removeString(values []string, target string) []string {
	out := []string{}
	for _, value := range values {
		if value != target {
			out = append(out, value)
		}
	}
	return out
}

func TestImportBundleIsAtomicIdempotentAndCollisionSafe(t *testing.T) {
	t.Parallel()
	root, exchange := t.TempDir(), t.TempDir()
	bundle := stageBundle(t, exchange, map[string]map[string][]byte{
		gateID:   {runner.BundleSessionFile: sessionDocument(gateID, "", 0), runner.BundlePlanFile: planDocument(gateID), runner.BundleEventsFile: []byte("{\"record\":\"x\"}\n\n"), runner.BundleSpecFile: []byte(`{"sessionId":"` + gateID + `"}`)},
		nestedID: {runner.BundleSessionFile: sessionDocument(nestedID, gateID, 0)},
	}, gateID)
	result, err := ImportBundle(root, exchange, bundle, 20)
	if err != nil {
		t.Fatal(err)
	}
	if result.GateSessionID != gateID || strings.Join(result.Imported, ",") != nestedID+","+gateID || len(result.Reused) != 0 {
		t.Fatalf("first import = %+v", result)
	}
	for _, name := range []string{runner.BundleSessionFile, runner.BundlePlanFile, runner.BundleEventsFile, runner.BundleSpecFile} {
		if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions", gateID, name)); err != nil {
			t.Fatalf("imported file %s: %v", name, err)
		}
	}
	if target, err := os.Readlink(filepath.Join(root, ".putnami", "sessions", "latest")); err != nil || filepath.Base(target) != gateID {
		t.Fatalf("latest = %s, %v", target, err)
	}
	if entries, _ := filepath.Glob(filepath.Join(root, ".putnami", "sessions", ".import-*")); len(entries) != 0 {
		t.Fatalf("staging directories left behind: %v", entries)
	}
	again, err := ImportBundle(root, exchange, bundle, 20)
	if err != nil || len(again.Imported) != 0 || len(again.Reused) != 2 {
		t.Fatalf("repeat import = %+v, %v", again, err)
	}
	if err := os.WriteFile(filepath.Join(root, ".putnami", "sessions", nestedID, runner.BundleSessionFile), sessionDocument(nestedID, gateID, 1), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ImportBundle(root, exchange, bundle, 20)
	if !errors.Is(err, ErrSessionCollision) {
		t.Fatalf("collision with different content: %v", err)
	}
}

func TestImportBundleRefusesInvalidEvidence(t *testing.T) {
	t.Parallel()
	cases := map[string]func(map[string]map[string][]byte) (gate string){
		"session id mismatch": func(s map[string]map[string][]byte) string {
			s[gateID][runner.BundleSessionFile] = sessionDocument(nestedID, "", 0)
			return gateID
		},
		"parent link mismatch": func(s map[string]map[string][]byte) string {
			s[gateID][runner.BundleSessionFile] = sessionDocument(gateID, nestedID, 0)
			return gateID
		},
		"unfinished session": func(s map[string]map[string][]byte) string {
			var document protocolcli.SessionFile
			_ = json.Unmarshal(sessionDocument(gateID, "", 0), &document)
			document.EndTime = ""
			s[gateID][runner.BundleSessionFile], _ = json.Marshal(document)
			return gateID
		},
		"schema violation": func(s map[string]map[string][]byte) string {
			s[gateID][runner.BundleSessionFile] = []byte(`{"protocolVersion":2,"sessionId":"` + gateID + `"}`)
			return gateID
		},
		"plan for another session": func(s map[string]map[string][]byte) string {
			s[gateID][runner.BundlePlanFile] = planDocument(nestedID)
			return gateID
		},
		"event stream not objects": func(s map[string]map[string][]byte) string {
			s[gateID][runner.BundleEventsFile] = []byte("[1,2]\n")
			return gateID
		},
		"spec record for another session": func(s map[string]map[string][]byte) string {
			s[gateID][runner.BundleSpecFile] = []byte(`{"sessionId":"other"}`)
			return gateID
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			root, exchange := t.TempDir(), t.TempDir()
			sessions := map[string]map[string][]byte{gateID: {runner.BundleSessionFile: sessionDocument(gateID, "", 0), runner.BundlePlanFile: planDocument(gateID)}}
			gate := mutate(sessions)
			bundle := stageBundle(t, exchange, sessions, gate)
			if _, err := ImportBundle(root, exchange, bundle, 20); err == nil {
				t.Fatal("invalid evidence imported")
			}
			if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
				t.Fatalf("refused import wrote into the store: %v", err)
			}
		})
	}
}

func TestImportBundleVerifiesDigestsBeforeWriting(t *testing.T) {
	t.Parallel()
	root, exchange := t.TempDir(), t.TempDir()
	bundle := stageBundle(t, exchange, map[string]map[string][]byte{gateID: {runner.BundleSessionFile: sessionDocument(gateID, "", 0)}}, gateID)
	path, _ := runner.ExchangeBlobPath(exchange, bundle.Sessions[0].Files[0].Digest)
	tampered, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tampered[len(tampered)-2] ^= 0x01 // same length, different bytes
	if err := os.WriteFile(path, tampered, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := ImportBundle(root, exchange, bundle, 20); err == nil || !strings.Contains(err.Error(), "digest verification") {
		t.Fatalf("tampered blob imported: %v", err)
	}
	bundle.Sessions[0].Files[0].Size++
	if _, err := ImportBundle(root, exchange, bundle, 20); err == nil {
		t.Fatal("size mismatch imported")
	}
	if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
		t.Fatalf("refused import wrote into the store: %v", err)
	}
}

// A session directory that appears between the pre-check and the rename is
// re-checked for identical content; whether the race is benign or a
// collision, the staging directory never survives the call.
func TestPublishSessionCleansStagingOnTheRenameRace(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	sessionsRoot := filepath.Join(root, ".putnami", "sessions")
	reportsRoot := filepath.Join(root, ".putnami", "reports")
	document := sessionDocument(gateID, "", 0)
	entry := importedSession{session: runner.BundleSession{ID: gateID}, files: []importedFile{{name: runner.BundleSessionFile, data: document}}}
	final := filepath.Join(sessionsRoot, gateID)
	if err := os.MkdirAll(final, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(final, runner.BundleSessionFile), document, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publishSession(sessionsRoot, reportsRoot, entry); err != nil {
		t.Fatalf("benign race: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(sessionsRoot, ".import-*")); len(leftovers) != 0 {
		t.Fatalf("staging directory leaked on the benign race: %v", leftovers)
	}
	if err := os.WriteFile(filepath.Join(final, runner.BundleSessionFile), sessionDocument(gateID, "", 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publishSession(sessionsRoot, reportsRoot, entry); !errors.Is(err, ErrSessionCollision) {
		t.Fatalf("collision on the race: %v", err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(sessionsRoot, ".import-*")); len(leftovers) != 0 {
		t.Fatalf("staging directory leaked on the collision: %v", leftovers)
	}
}

// A session that states its provenance states which submission it answers:
// one that names other execution inputs is another submission's verdict and
// is refused after the bytes verified, before the exit code is adopted.
func TestImportOutcomeRefusesAnotherSubmissionsProvenance(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	provenance := func(input string) []byte {
		var document protocolcli.SessionFile
		if err := json.Unmarshal(sessionDocument(gateID, "", 0), &document); err != nil {
			t.Fatal(err)
		}
		document.Placement.Provenance = &protocolcli.SessionProvenance{
			SourceDigest: "sha256:" + strings.Repeat("ab", 32), InputDigest: input, Submission: strings.Repeat("cd", 16),
		}
		data, _ := json.Marshal(&document)
		return data
	}
	mine, theirs := "sha256:"+strings.Repeat("11", 32), "sha256:"+strings.Repeat("22", 32)
	source, submission := "sha256:"+strings.Repeat("ab", 32), strings.Repeat("cd", 16)
	var stderr strings.Builder
	for name, bound := range map[string]Outcome{
		"foreign inputs":     {Attempt: "attempt-1", InputDigest: theirs, SourceDigest: source, Submission: submission},
		"foreign source":     {Attempt: "attempt-1", InputDigest: mine, SourceDigest: "sha256:" + strings.Repeat("ef", 32), Submission: submission},
		"foreign submission": {Attempt: "attempt-1", InputDigest: mine, SourceDigest: source, Submission: strings.Repeat("99", 16)},
	} {
		exchange := t.TempDir()
		bundle := stageBundle(t, exchange, map[string]map[string][]byte{gateID: {runner.BundleSessionFile: provenance(mine), runner.BundlePlanFile: planDocument(gateID)}}, gateID)
		_, err := importOutcome(root, exchange, 20, &stderr, bound, bundle)
		if err == nil || !strings.Contains(err.Error(), "refusing to adopt another submission's verdict") {
			t.Fatalf("%s adopted: %v", name, err)
		}
		// Refused before anything is published: no session directory, no latest.
		if _, err := os.Stat(filepath.Join(root, ".putnami", "sessions")); !os.IsNotExist(err) {
			t.Fatalf("%s: a refused bundle reached the session store (%v)", name, err)
		}
	}
	exchange := t.TempDir()
	bundle := stageBundle(t, exchange, map[string]map[string][]byte{gateID: {runner.BundleSessionFile: provenance(mine), runner.BundlePlanFile: planDocument(gateID)}}, gateID)
	outcome, err := importOutcome(t.TempDir(), exchange, 20, &stderr, Outcome{Attempt: "attempt-1", InputDigest: mine, SourceDigest: source, Submission: submission}, bundle)
	if err != nil || outcome.SessionID != gateID || outcome.ExitCode != 0 {
		t.Fatalf("own provenance refused: %+v, %v", outcome, err)
	}
}
