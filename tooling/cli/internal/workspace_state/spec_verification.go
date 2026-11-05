package workspace_state

import (
	"fmt"
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
