package reportstream

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// The sources FoldRun reports in Stats.Source.
const (
	// SourceSessionArtifact is the complete session record the CLI keeps.
	SourceSessionArtifact = "session-artifact"
	// SourceStdout is the wrapped command's stdout, which a bounded v2 stream
	// may have elided records from.
	SourceStdout = "stdout"
)

// sessionArtifactName is the fixed file name of the complete session stream
// (putnami protocols/cli MachineOutputArtifactPath). A session:end that names
// any other path is refused, never followed.
const sessionArtifactName = "events.jsonl"

// sessionsDir is where the CLI keeps its session records, relative to the
// workspace root.
var sessionsDir = filepath.Join(".putnami", "sessions")

// FoldRun returns the report batch for one run that `cloud ci report` wrapped,
// in CI that runs outside Putnami's hosted runners, and a note that says why
// the batch comes from stdout.
//
// A bounded v2 stream elides ordinary records once its live budget is spent,
// so a large run's stdout can lack most test and coverage results. Its final
// session:end names the session whose events.jsonl holds the complete
// sequence. FoldRun folds that file instead, when all of these hold:
//
//   - the stdout carried exactly one session:end, and it names an artifact;
//   - the session id is one path segment and the path is events.jsonl, so the
//     file resolves under <workspaceRoot>/.putnami/sessions/<id>/, symlinks
//     included;
//   - the file reads to the end, holds exactly one session:end, and ends with
//     it, byte-identical to the stdout's, which is how the CLI binds the two
//     copies of one session.
//
// A line the parser cannot fold does not refuse the file: the binding above
// already ties the file to the run, and Stats counts the line as malformed,
// unknown or oversized, as it does on stdout.
//
// Otherwise it returns the stdout fold unchanged, with Stats.ElidedLines set
// to the elision its session:end records report, and a non-empty note. The
// note is empty when the stdout carried neither a session:end nor report data:
// such a command is not a putnami JSONL run, and there is nothing to fall back
// from.
//
// The artifact is streamed through the parser, so its size never reaches
// memory, and MaxArtifactLineBytes caps each line.
func FoldRun(workspaceRoot string, live *Parser) (*Batch, string) {
	stdout := live.Batch()
	stdout.Stats.Source = SourceStdout
	stdout.Stats.ElidedLines = live.elidedRecords

	path, reason := live.sessionArtifactPath(workspaceRoot)
	if reason == "" {
		var artifact *Batch
		artifact, reason = foldSessionArtifact(path, live.lastSessionEnd.line)
		if reason == "" {
			artifact.Stats.Source = SourceSessionArtifact
			artifact.Stats.ElidedLines = 0
			return artifact, ""
		}
	}
	if live.sessionEnds == 0 && !stdout.HasReports() {
		return stdout, ""
	}
	return stdout, reason
}

// sessionArtifactPath resolves the artifact the stdout's session:end names, or
// says why there is none to read.
func (p *Parser) sessionArtifactPath(workspaceRoot string) (string, string) {
	switch {
	case p.sessionEnds == 0:
		return "", "the output has no v2 session:end record"
	case p.sessionEnds > 1:
		return "", fmt.Sprintf("the output holds %d sessions", p.sessionEnds)
	}
	out := p.lastSessionEnd.machineOutput
	if out == nil || out.Artifact.SessionID == "" {
		return "", "its session:end names no session record"
	}
	id := out.Artifact.SessionID
	if !validSessionID(id) {
		return "", fmt.Sprintf("its session:end names an invalid session id %q", id)
	}
	if out.Artifact.Path != sessionArtifactName {
		return "", fmt.Sprintf("its session:end names %q, not %s", out.Artifact.Path, sessionArtifactName)
	}
	root, err := filepath.EvalSymlinks(filepath.Join(workspaceRoot, sessionsDir))
	if err != nil {
		return "", "the session records cannot be read: " + err.Error()
	}
	path, err := filepath.EvalSymlinks(filepath.Join(root, id, sessionArtifactName))
	if err != nil {
		return "", "the session record cannot be read: " + err.Error()
	}
	if path != filepath.Join(root, id, sessionArtifactName) {
		return "", fmt.Sprintf("the session record of %s resolves outside %s", id, sessionsDir)
	}
	return path, ""
}

// validSessionID accepts one plain path segment: letters, digits, '.', '-' and
// '_', and neither "." nor "..". The CLI's ids look like 20261027-230531-ea4d60.
func validSessionID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '_':
		default:
			return false
		}
	}
	return true
}

// foldSessionArtifact streams one session artifact through a fresh parser
// capped at MaxArtifactLineBytes, and refuses it when it cannot be read whole
// or does not end with the stdout's session:end as its only one.
func foldSessionArtifact(path string, sessionEnd []byte) (*Batch, string) {
	file, err := os.Open(path)
	if err != nil {
		return nil, "the session record cannot be read: " + err.Error()
	}
	defer file.Close()
	parser := newParser(MaxArtifactLineBytes)
	if _, err := io.Copy(parser, file); err != nil {
		return nil, "the session record cannot be read: " + err.Error()
	}
	batch := parser.Batch()
	if parser.sessionEnds != 1 || !parser.endsOnSessionEnd || !bytes.Equal(parser.lastSessionEnd.line, sessionEnd) {
		return nil, "the session record does not end with this run's session:end"
	}
	return batch, ""
}
