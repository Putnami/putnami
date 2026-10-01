package workspace_state

import (
	"fmt"
	"os"
	"path/filepath"

	features "go.putnami.dev/protocol/features"
)

// WriteSpecVerification atomically persists the session's spec-verification
// record beside the other session documents, so `specs verify --session` can
// reproduce what the gate decided without reinterpreting deleted temporary
// files. A nil record is a no-op: absence means the gate did not run, never
// that it found nothing. The persisted record is derived history — an audit
// surface, never an input to current maturity.
func (s *Session) WriteSpecVerification(record *features.SpecVerificationRecord) error {
	if record == nil {
		return nil
	}
	stamped := *record
	stamped.SessionID = s.ID
	data, err := features.MarshalSpecVerificationRecord(&stamped)
	if err != nil {
		return fmt.Errorf("marshal spec verification record: %w", err)
	}
	return atomicWriteSessionFile(filepath.Join(s.dir, features.SpecVerificationRecordFilename), data, 0o644)
}

// ReadSpecVerification loads one session's persisted spec-verification
// record. Absence is (nil, false, nil): the session simply carried no gate.
func (ss *SessionStore) ReadSpecVerification(sessionID string) (*features.SpecVerificationRecord, bool, error) {
	data, err := os.ReadFile(filepath.Join(ss.root, sessionID, features.SpecVerificationRecordFilename))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	record, diagnostics := features.ParseSpecVerificationRecord(data)
	if record == nil {
		return nil, true, fmt.Errorf("session %s carries an unreadable spec verification record: %v", sessionID, diagnostics)
	}
	if record.SessionID != sessionID {
		return nil, true, fmt.Errorf("session %s carries a spec verification record claiming session %q", sessionID, record.SessionID)
	}
	return record, true, nil
}
