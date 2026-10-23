package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	features "go.putnami.dev/protocol/features"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/jobs/inputresolve"
	"go.putnami.dev/tooling/cli/internal/jobs/scheduler"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// VerifyCachedVerificationReport corroborates a restored native observation
// against the current plan of the loaded verification commands and the
// task-owned cache entry it derives. It only reads evidence from the local
// store; it never asks a remote cache, executes a test or materializes an
// output.
// Command parameters not recorded by a session cannot be guessed here: an
// entry keyed with nondefault parameters fails closed.
func VerifyCachedVerificationReport(root string, report features.VerificationReportRef) ([]byte, error) {
	ws, err := workspace.Load(root)
	if err != nil {
		return nil, fmt.Errorf("load native workspace for restored report: %w", err)
	}
	paths := make([]string, len(ws.Projects))
	for i, candidate := range ws.Projects {
		paths[i] = candidate.Path
	}
	discovered, err := extension.DiscoverExtensionsDetailed(ws.Root, ws.Config, paths)
	if err != nil {
		return nil, fmt.Errorf("discover native report producer: %w", err)
	}
	snapshot, err := captureVersionSnapshot(ws.Root)
	if err != nil {
		return nil, fmt.Errorf("capture native report version: %w", err)
	}
	req := &Request{WorkspaceRoot: ws.Root, Config: ws.Config,
		Commands:        scheduler.CommandsOfKind(discovered.Extensions, extension.CommandKindVerification),
		VersionSnapshot: snapshot}
	req.inputResolver = inputresolve.New(context.Background(), ws, discovered.Extensions, nil)
	cache := newRunCacheManager(req, ws)
	source := newCachedObservationRecovery(req, ws, discovered.Extensions, cache)
	return source.verifyCachedReport(report)
}

func (source *cachedObservationRecovery) verifyCachedReport(report features.VerificationReportRef) ([]byte, error) {
	project := source.ws.ProjectByID(report.Project)
	if project == nil {
		return nil, fmt.Errorf("restored report names unknown native project %q", report.Project)
	}
	recovered := source.RecoverReports([]*workspace.Project{project})
	for _, native := range recovered.Reports {
		if native.Project != report.Project || native.Task != report.Task || native.Field != report.Path {
			continue
		}
		source.entriesMu.Lock()
		entry := source.entries[native.Path]
		source.entriesMu.Unlock()
		if entry == nil || entry.Result == nil || entry.Result.Status != "success" || entry.Result.Error != nil || entry.Manifest == nil {
			break
		}
		data, err := readCachedVerificationReport(source.cache.StoreRoot(), native.Path)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		if digest != report.Digest {
			break
		}
		outputID := ""
		for _, output := range entry.Outputs {
			if filepath.Join(entry.FilesDir, output.ID) == native.Path {
				outputID = output.ID
				break
			}
		}
		if outputID == "" {
			break
		}
		matched := 0
		for _, file := range entry.Manifest.Files {
			if file.Path == outputID {
				if file.Digest != digest || file.Size != int64(len(data)) {
					break
				}
				matched++
			}
		}
		if matched == 1 {
			return data, nil
		}
		break
	}
	return nil, fmt.Errorf("restored report has no matching successful native cache producer: %s", report.Task)
}

func readCachedVerificationReport(storeRoot, reportPath string) ([]byte, error) {
	storeRoot, err := filepath.Abs(storeRoot)
	if err != nil {
		return nil, err
	}
	relative, err := filepath.Rel(storeRoot, reportPath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("restored report is outside the native cache store: %s", reportPath)
	}
	root, err := os.OpenRoot(storeRoot)
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	component := ""
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		component = filepath.Join(component, part)
		info, err := root.Lstat(component)
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("restored report has an unreadable or symlink path: %s", reportPath)
		}
	}
	f, err := root.Open(relative)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > 16<<20 {
		return nil, fmt.Errorf("restored report is not a bounded regular file: %s", reportPath)
	}
	data, err := io.ReadAll(io.LimitReader(f, 16<<20+1))
	if err != nil {
		return nil, fmt.Errorf("read bounded restored report %s: %w", reportPath, err)
	}
	if len(data) == 0 || len(data) > 16<<20 {
		return nil, fmt.Errorf("restored report is empty or oversized: %s", reportPath)
	}
	return data, nil
}
