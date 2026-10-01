package workspace_state

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.putnami.dev/cli/model/extension"
	protocolcli "go.putnami.dev/protocol/cli"
	runtimeproto "go.putnami.dev/protocol/runtime"
	"go.putnami.dev/tooling/cli/internal/jobs"
)

const (
	// SessionReportVersion versions the compact report.json document independently
	// from the larger session machine documents. This first slice deliberately
	// contains only the coverage facts needed for replay; later report fields can
	// be added without changing the reader's absent-not-zero semantics.
	SessionReportVersion = 1

	reportFileName          = "report.json"
	ReportCadenceValidation = "validation"
)

// SessionReport is the compact, language-neutral synthesis persisted beside a
// completed session. It contains canonical runtime coverage payloads rather
// than extension-specific Go or TypeScript fields, and never carries raw result
// data (which may include invocation-scoped material).
type SessionReport struct {
	ReportVersion int                    `json:"reportVersion"`
	SessionID     string                 `json:"sessionId"`
	GeneratedAt   string                 `json:"generatedAt"`
	Source        SessionReportSource    `json:"source"`
	Coverage      []SessionCoverageEntry `json:"coverage,omitempty"`
}

// SessionReportSource identifies the validation run that measured coverage.
// Revision is display provenance (normally a short SHA, including the dirty
// suffix when applicable); SessionID remains the fallback provenance.
type SessionReportSource struct {
	Cadence  string `json:"cadence"`
	Revision string `json:"revision,omitempty"`
	Branch   string `json:"branch,omitempty"`
}

// SessionCoverageEntry is one project's canonical coverage fact. TaskKey and
// Provider keep the producer attributable without persisting the producer's
// open result-data map.
type SessionCoverageEntry struct {
	Project  SessionReportProject         `json:"project"`
	TaskKey  string                       `json:"taskKey"`
	Command  string                       `json:"command"`
	Provider string                       `json:"provider,omitempty"`
	Summary  runtimeproto.CoverageSummary `json:"summary"`
}

type SessionReportProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// ValidationCoverage is a trusted replay candidate returned by the session
// store. The persisted entry is paired with its immutable session provenance.
type ValidationCoverage struct {
	Entry       SessionCoverageEntry
	Source      SessionReportSource
	SessionID   string
	GeneratedAt string
}

// WriteReport atomically publishes report.json inside this session. A nil or
// empty report is a no-op: absence means this run did not collect trustworthy
// validation coverage, never zero coverage.
func (s *Session) WriteReport(report *SessionReport) error {
	if report == nil || len(report.Coverage) == 0 {
		return nil
	}
	report.ReportVersion = SessionReportVersion
	report.SessionID = s.ID
	if report.GeneratedAt == "" {
		report.GeneratedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}
	sort.SliceStable(report.Coverage, func(i, j int) bool {
		if report.Coverage[i].Project.ID != report.Coverage[j].Project.ID {
			return report.Coverage[i].Project.ID < report.Coverage[j].Project.ID
		}
		return report.Coverage[i].TaskKey < report.Coverage[j].TaskKey
	})
	if err := validateSessionReport(report, s.ID); err != nil {
		return err
	}

	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal session report: %w", err)
	}
	return atomicWriteSessionFile(filepath.Join(s.dir, reportFileName), data, 0o644)
}

// FinalizeV2WithCoverage publishes the canonical session document and, for a
// completed validation cadence, a compact coverage report beside it. A failed
// threshold or sibling task does not invalidate a measurement; abort does,
// because the run never reached a completed verdict over its planned work.
// Coverage reporting is deliberately post-run synthesis: it never changes task
// parameters, execution, cache keys, or the canonical pass/fail reduction.
func (s *Session) FinalizeV2WithCoverage(
	meta *protocolcli.SessionFile,
	wsRoot string,
	baseline string,
	params extension.ParamMap,
	planned []*jobs.ScheduledJob,
	results map[string]*jobs.JobResult,
	run *jobs.SessionResult,
	version *jobs.JobContextVersion,
) error {
	if err := s.FinalizeV2(meta, wsRoot, baseline); err != nil {
		return err
	}
	if run == nil || run.Aborted || !validationCadenceRequested(params) {
		return nil
	}

	coverage := validationCoverageEntries(planned, results)
	if len(coverage) == 0 {
		return nil
	}
	revision, branch := "", ""
	if version != nil {
		revision, branch = version.Suffix, version.Branch
	}
	return s.WriteReport(&SessionReport{
		Source: SessionReportSource{
			Cadence:  ReportCadenceValidation,
			Revision: revision,
			Branch:   branch,
		},
		Coverage: coverage,
	})
}

func validationCoverageEntries(
	planned []*jobs.ScheduledJob,
	results map[string]*jobs.JobResult,
) []SessionCoverageEntry {
	coverage := make([]SessionCoverageEntry, 0)
	seenProjects := make(map[string]struct{})
	for _, job := range planned {
		if job == nil || job.Project == nil || job.CommandName() != "test" {
			continue
		}
		if _, seen := seenProjects[job.Project.ID]; seen {
			continue
		}
		result := results[job.Key()]
		if result == nil || (result.Status != string(jobs.TaskStatusSuccess) &&
			result.Status != string(jobs.TaskStatusFailed)) {
			continue
		}
		_, summary, _, err := runtimeproto.ExtractResultPayloads(result.Data)
		if err != nil || summary == nil || len(runtimeproto.ValidateCoverageSummary(summary)) > 0 {
			continue
		}
		identity := job.TypedIdentity()
		coverage = append(coverage, SessionCoverageEntry{
			Project: SessionReportProject{
				ID:   job.Project.ID,
				Name: job.Project.Name,
			},
			TaskKey:  identity.Key,
			Command:  identity.Task.Command,
			Provider: identity.Provider.Extension,
			Summary:  *summary,
		})
		seenProjects[job.Project.ID] = struct{}{}
	}
	return coverage
}

func validationCadenceRequested(params extension.ParamMap) bool {
	for _, name := range []string{"enforce-coverage", "enforceCoverage"} {
		value, ok := params[name]
		if !ok {
			continue
		}
		switch value := value.(type) {
		case bool:
			return value
		case string:
			parsed, err := strconv.ParseBool(strings.TrimSpace(value))
			return err == nil && parsed
		}
		return false
	}
	return false
}

// LatestValidationCoverage scans completed reports newest-first and returns
// the newest trustworthy validation fact for each requested project. Sessions
// without a report are deliberately skipped, so ordinary inner-loop runs do
// not erase the last validation measurement merely by becoming `latest`.
func (ss *SessionStore) LatestValidationCoverage(commands, projectIDs []string) map[string]ValidationCoverage {
	if !slices.Contains(commands, "test") {
		return nil
	}
	wanted := make(map[string]struct{}, len(projectIDs))
	for _, id := range projectIDs {
		if id = strings.TrimSpace(id); id != "" {
			wanted[id] = struct{}{}
		}
	}

	coverage := make(map[string]ValidationCoverage)
	ids, err := ss.List()
	if err != nil {
		return coverage
	}
	for _, id := range ids {
		if len(wanted) > 0 && len(coverage) == len(wanted) {
			break
		}
		report, ok := ss.readTrustedValidationReport(id)
		if !ok {
			continue
		}
		for _, entry := range report.Coverage {
			projectID := entry.Project.ID
			if len(wanted) > 0 {
				if _, ok := wanted[projectID]; !ok {
					continue
				}
			}
			if _, exists := coverage[projectID]; exists {
				continue
			}
			coverage[projectID] = ValidationCoverage{
				Entry:       entry,
				Source:      report.Source,
				SessionID:   report.SessionID,
				GeneratedAt: report.GeneratedAt,
			}
		}
	}
	return coverage
}

func (ss *SessionStore) readTrustedValidationReport(sessionID string) (*SessionReport, bool) {
	dir := filepath.Join(ss.root, sessionID)
	sessionData, err := os.ReadFile(filepath.Join(dir, "session.json"))
	if err != nil || len(protocolcli.ValidateDocument(protocolcli.DocumentSessionFile, sessionData)) > 0 {
		return nil, false
	}
	var session protocolcli.SessionFile
	if err := json.Unmarshal(sessionData, &session); err != nil ||
		session.SessionID != sessionID || session.EndTime == "" ||
		session.Run.Outcome == protocolcli.RunOutcomeAborted {
		return nil, false
	}

	reportData, err := os.ReadFile(filepath.Join(dir, reportFileName))
	if err != nil {
		return nil, false
	}
	var report SessionReport
	if err := json.Unmarshal(reportData, &report); err != nil {
		return nil, false
	}
	if err := validateSessionReport(&report, sessionID); err != nil {
		return nil, false
	}
	return &report, true
}

func validateSessionReport(report *SessionReport, sessionID string) error {
	if report == nil {
		return fmt.Errorf("validate session report: nil report")
	}
	if report.ReportVersion != SessionReportVersion {
		return fmt.Errorf("validate session report: unsupported reportVersion %d", report.ReportVersion)
	}
	if report.SessionID == "" || report.SessionID != sessionID {
		return fmt.Errorf("validate session report: sessionId %q does not match %q", report.SessionID, sessionID)
	}
	if _, err := time.Parse(time.RFC3339Nano, report.GeneratedAt); err != nil {
		return fmt.Errorf("validate session report: invalid generatedAt: %w", err)
	}
	if report.Source.Cadence != ReportCadenceValidation {
		return fmt.Errorf("validate session report: cadence %q is not validation", report.Source.Cadence)
	}
	if len(report.Coverage) == 0 {
		return fmt.Errorf("validate session report: coverage is empty")
	}
	seen := make(map[string]struct{}, len(report.Coverage))
	for i := range report.Coverage {
		entry := &report.Coverage[i]
		if entry.Project.ID == "" || entry.Project.Name == "" || entry.TaskKey == "" || entry.Command != "test" {
			return fmt.Errorf("validate session report: coverage[%d] has incomplete identity", i)
		}
		key := entry.Project.ID + "\x00" + entry.Command
		if _, duplicate := seen[key]; duplicate {
			return fmt.Errorf("validate session report: duplicate coverage for project %q command %q", entry.Project.ID, entry.Command)
		}
		seen[key] = struct{}{}
		if diagnostics := runtimeproto.ValidateCoverageSummary(&entry.Summary); len(diagnostics) > 0 {
			return fmt.Errorf("validate session report: coverage[%d] is invalid: %v", i, diagnostics)
		}
	}
	return nil
}

func atomicWriteSessionFile(path string, data []byte, mode os.FileMode) (err error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".report-*.tmp")
	if err != nil {
		return fmt.Errorf("create session report temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("chmod session report temp file: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write session report temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync session report temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close session report temp file: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publish session report: %w", err)
	}
	return nil
}
