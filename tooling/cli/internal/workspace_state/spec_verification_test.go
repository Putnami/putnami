package workspace_state

import (
	"os"
	"path/filepath"
	"testing"

	features "go.putnami.dev/protocol/features"
)

func TestWriteSpecVerificationPersistsBesideTheSessionAndReplays(t *testing.T) {
	root := t.TempDir()
	store := NewSessionStore(root)
	session, err := store.Create()
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	t.Cleanup(session.Close)

	record := &features.SpecVerificationRecord{
		ProtocolVersion: features.SpecVerificationRecordProtocolVersion,
		GeneratedAt:     "2026-08-23T12:00:00Z",
		Groups: []features.SpecVerificationGroup{{
			Project: "/billing", Feature: "billing/invoicing",
			Mode: features.VerificationModeEnforce, ModeSource: features.VerificationModeSourceProject,
			AutomaticEvaluation: true, Blocked: true,
			Requirements: []features.SpecRequirementVerdict{{Requirement: "issue", State: features.RequirementMissing}},
		}},
	}
	if err := session.WriteSpecVerification(record); err != nil {
		t.Fatalf("write spec verification: %v", err)
	}
	// The caller's record must not be mutated by the session stamping.
	if record.SessionID != "" {
		t.Errorf("WriteSpecVerification stamped the caller's record: %q", record.SessionID)
	}

	replayed, found, err := store.ReadSpecVerification(session.ID)
	if err != nil || !found {
		t.Fatalf("replay = (%v, %v)", found, err)
	}
	if replayed.SessionID != session.ID {
		t.Errorf("replayed sessionId = %q, want %q", replayed.SessionID, session.ID)
	}
	if len(replayed.Groups) != 1 || !replayed.Groups[0].Blocked {
		t.Errorf("replayed groups = %+v", replayed.Groups)
	}

	// A nil record is absence, never an empty file.
	empty, err := store.Create()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(empty.Close)
	if err := empty.WriteSpecVerification(nil); err != nil {
		t.Fatalf("nil record: %v", err)
	}
	if _, found, err := store.ReadSpecVerification(empty.ID); found || err != nil {
		t.Errorf("a gate-free session reported a record (found %v, err %v)", found, err)
	}

	// A record claiming another session is refused on read.
	forged := filepath.Join(root, ".putnami", "sessions", empty.ID, features.SpecVerificationRecordFilename)
	data, err := os.ReadFile(filepath.Join(root, ".putnami", "sessions", session.ID, features.SpecVerificationRecordFilename))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(forged, data, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, found, err := store.ReadSpecVerification(empty.ID); !found || err == nil {
		t.Errorf("a record claiming another session was accepted (found %v, err %v)", found, err)
	}
}
