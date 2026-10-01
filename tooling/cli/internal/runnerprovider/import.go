package runnerprovider

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	protocolcli "go.putnami.dev/protocol/cli"
	runner "go.putnami.dev/protocol/runner"
	"go.putnami.dev/tooling/cli/internal/workspace_state"
)

// ImportResult reports what an import did.
type ImportResult struct {
	// GateSessionID is the imported gate session, empty when the remote engine
	// refused before recording one.
	GateSessionID string
	// Imported lists the session ids written by this import.
	Imported []string
	// Reused lists the session ids that already existed with identical content.
	Reused []string
}

// ErrSessionCollision reports a local session with the same id but different
// content. Imported records must never overwrite or merge into it.
var ErrSessionCollision = errors.New("session id collision with different content")

type importedFile struct {
	name string
	data []byte
}

type importedSession struct {
	session runner.BundleSession
	files   []importedFile
	reused  bool
}

// ImportBundle validates every bundled session against its digests and the
// canonical document rules, then publishes each session directory atomically
// into the workspace session store. Repeating an import is safe; a different
// record under an existing id is rejected before anything is written.
func ImportBundle(wsRoot, exchangeDir string, bundle runner.SessionBundle, retention int) (ImportResult, error) {
	var result ImportResult
	if err := runner.ValidateSessionBundle(bundle); err != nil {
		return result, err
	}
	store := workspace_state.NewSessionStoreWithRetention(wsRoot, retention)
	reportsRoot := filepath.Join(wsRoot, ".putnami", workspace_state.ReportsDirName)
	staged := make([]importedSession, 0, len(bundle.Sessions))
	for _, session := range bundle.Sessions {
		loaded, err := loadBundledSession(exchangeDir, session)
		if err != nil {
			return result, err
		}
		loaded.reused, err = sessionAlreadyRecorded(filepath.Join(store.Root(), session.ID), reportsRoot, loaded)
		if err != nil {
			return result, err
		}
		staged = append(staged, loaded)
	}
	// Nested sessions first, the gate session last: a reader that finds the
	// gate finds its children already in place.
	for index := len(staged) - 1; index >= 0; index-- {
		entry := staged[index]
		if entry.reused {
			result.Reused = append(result.Reused, entry.session.ID)
			continue
		}
		if err := publishSession(store.Root(), reportsRoot, entry); err != nil {
			return result, err
		}
		result.Imported = append(result.Imported, entry.session.ID)
	}
	if bundle.SessionID != "" {
		if err := store.LinkLatest(bundle.SessionID); err != nil {
			return result, fmt.Errorf("link latest session: %w", err)
		}
		result.GateSessionID = bundle.SessionID
	}
	_ = store.Prune()
	return result, nil
}

func loadBundledSession(exchangeDir string, session runner.BundleSession) (importedSession, error) {
	loaded := importedSession{session: session}
	for _, file := range session.Files {
		path, ok := runner.ExchangeBlobPath(exchangeDir, file.Digest)
		if !ok {
			return loaded, fmt.Errorf("bundle file %s/%s has an invalid digest", session.ID, file.Name)
		}
		data, err := readBounded(path, file.Size)
		if err != nil {
			return loaded, fmt.Errorf("bundle file %s/%s: %w", session.ID, file.Name, err)
		}
		if digest := blobDigest(data); digest != file.Digest {
			return loaded, fmt.Errorf("bundle file %s/%s failed digest verification", session.ID, file.Name)
		}
		if err := validateBundledDocument(session, file.Name, data); err != nil {
			return loaded, fmt.Errorf("bundle file %s/%s: %w", session.ID, file.Name, err)
		}
		loaded.files = append(loaded.files, importedFile{name: file.Name, data: data})
	}
	return loaded, nil
}

func readBounded(path string, size int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() != size {
		return nil, fmt.Errorf("expected a regular file of %d bytes", size)
	}
	data, err := io.ReadAll(io.LimitReader(file, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) != size {
		return nil, fmt.Errorf("file changed while reading")
	}
	return data, nil
}

func blobDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// validateBundledDocument applies each admitted file's own contract: the
// canonical session and plan documents, the run report, the coverage report
// and the event stream. Identities inside every document must name the
// session that carries it.
func validateBundledDocument(session runner.BundleSession, name string, data []byte) error {
	switch name {
	case runner.BundleSessionFile:
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, data); len(violations) != 0 {
			return fmt.Errorf("invalid session document: %v", violations)
		}
		var document protocolcli.SessionFile
		if err := json.Unmarshal(data, &document); err != nil {
			return err
		}
		if document.SessionID != session.ID || document.ParentSessionID != session.ParentID {
			return fmt.Errorf("session document identifies %s (parent %q), bundle says %s (parent %q)", document.SessionID, document.ParentSessionID, session.ID, session.ParentID)
		}
		if document.EndTime == "" {
			return fmt.Errorf("session document is not finalized")
		}
	case runner.BundlePlanFile:
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentSessionPlanFile, data); len(violations) != 0 {
			return fmt.Errorf("invalid plan document: %v", violations)
		}
		var document protocolcli.SessionPlanFile
		if err := json.Unmarshal(data, &document); err != nil {
			return err
		}
		if document.SessionID != session.ID {
			return fmt.Errorf("plan document identifies %s, bundle says %s", document.SessionID, session.ID)
		}
	case runner.BundleRunReportFile:
		if violations := protocolcli.ValidateDocument(protocolcli.DocumentReportFile, data); len(violations) != 0 {
			return fmt.Errorf("invalid run report: %v", violations)
		}
		return requireSessionID(data, session.ID)
	case runner.BundleReportFile:
		var report workspace_state.SessionReport
		if err := json.Unmarshal(data, &report); err != nil {
			return err
		}
		if report.ReportVersion != workspace_state.SessionReportVersion || report.SessionID != session.ID {
			return fmt.Errorf("coverage report identifies %s (version %d)", report.SessionID, report.ReportVersion)
		}
	case runner.BundleSpecFile:
		return requireSessionID(data, session.ID)
	case runner.BundleEventsFile:
		scanner := bufio.NewScanner(bytes.NewReader(data))
		scanner.Buffer(make([]byte, 0, 64<<10), maxResponseBytes)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var record map[string]json.RawMessage
			if err := json.Unmarshal(line, &record); err != nil {
				return fmt.Errorf("event stream holds a non-object line: %w", err)
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("event stream: %w", err)
		}
	default:
		return fmt.Errorf("file %q is not admitted", name)
	}
	return nil
}

func requireSessionID(data []byte, sessionID string) error {
	var document struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(data, &document); err != nil {
		return err
	}
	if document.SessionID != sessionID {
		return fmt.Errorf("document identifies session %q, bundle says %s", document.SessionID, sessionID)
	}
	return nil
}

// sessionAlreadyRecorded reports whether the session directory already holds
// exactly the bundled bytes (a safe repeat) and fails on any difference.
func sessionAlreadyRecorded(dir, reportsRoot string, loaded importedSession) (bool, error) {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("%w: %s is not a session directory", ErrSessionCollision, dir)
	}
	for _, file := range loaded.files {
		existing, err := os.ReadFile(sessionFilePath(dir, reportsRoot, loaded.session.ID, file.name))
		if err != nil || !bytes.Equal(existing, file.data) {
			return false, fmt.Errorf("%w: session %s file %s differs from the imported record", ErrSessionCollision, loaded.session.ID, file.name)
		}
	}
	return true, nil
}

func sessionFilePath(dir, reportsRoot, sessionID, name string) string {
	if name == runner.BundleRunReportFile {
		return filepath.Join(reportsRoot, sessionID+".json")
	}
	return filepath.Join(dir, name)
}

// publishSession stages every file, syncs it, and renames the directory into
// place. A directory that appeared meanwhile is re-checked for identical
// content; anything else is a collision.
func publishSession(sessionsRoot, reportsRoot string, entry importedSession) (resultErr error) {
	if err := os.MkdirAll(sessionsRoot, 0o755); err != nil {
		return err
	}
	staging, err := os.MkdirTemp(sessionsRoot, ".import-"+entry.session.ID+"-*")
	if err != nil {
		return err
	}
	// The staging directory either becomes the session (rename) or is removed:
	// on failure, and on the race where the session appeared meanwhile.
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(staging)
		}
	}()
	if err := os.Chmod(staging, 0o755); err != nil {
		return err
	}
	var runReport []byte
	for _, file := range entry.files {
		if file.name == runner.BundleRunReportFile {
			runReport = file.data
			continue
		}
		if err := writeSynced(filepath.Join(staging, file.name), file.data); err != nil {
			return err
		}
	}
	final := filepath.Join(sessionsRoot, entry.session.ID)
	if err := os.Rename(staging, final); err != nil {
		reused, checkErr := sessionAlreadyRecorded(final, reportsRoot, entry)
		if checkErr != nil {
			return checkErr
		}
		if !reused {
			return fmt.Errorf("publish session %s: %w", entry.session.ID, err)
		}
		return nil
	}
	published = true
	if runReport != nil {
		if err := os.MkdirAll(reportsRoot, 0o755); err != nil {
			return err
		}
		target := filepath.Join(reportsRoot, entry.session.ID+".json")
		if existing, err := os.ReadFile(target); err == nil {
			if !bytes.Equal(existing, runReport) {
				return fmt.Errorf("%w: run report %s", ErrSessionCollision, entry.session.ID)
			}
			return nil
		}
		temp := target + ".import-" + filepath.Base(staging) + ".tmp"
		if err := writeSynced(temp, runReport); err != nil {
			return err
		}
		if err := os.Rename(temp, target); err != nil {
			_ = os.Remove(temp)
			return err
		}
	}
	return nil
}

func writeSynced(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	return errors.Join(writeErr, syncErr, closeErr)
}
