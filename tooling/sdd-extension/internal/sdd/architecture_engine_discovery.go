package sdd

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	archproto "go.putnami.dev/protocol/architecture"
	diag "go.putnami.dev/protocol/diagnostic"
)

var excludedDiscoveryDirectories = map[string]bool{
	"node_modules": true,
	"vendor":       true,
}

// Discover reads exact manifests recursively and the two fixed workspace-root
// debt artifacts. Directory symlinks are never followed and file symlinks are
// rejected, so repository containment does not depend on their targets.
func Discover(root string) DiscoveryResult {
	var result DiscoveryResult
	root, err := filepath.Abs(root)
	if err != nil {
		result.Diagnostics = append(result.Diagnostics, diag.Errorf(ErrorCodeReadFailure, "workspace", "resolve workspace root: %v", err))
		return result
	}
	paths, diagnostics := discoverManifestPaths(root)
	result.Diagnostics = append(result.Diagnostics, diagnostics...)
	for _, relative := range paths {
		absolute := filepath.Join(root, filepath.FromSlash(relative))
		data, readErr := readBoundedRegularFile(absolute)
		if readErr != nil {
			result.Diagnostics = append(result.Diagnostics, diag.Errorf(ErrorCodeReadFailure, relative, "read architecture manifest: %v", readErr))
			continue
		}
		// Repository validation below owns semantic validation for every parsed
		// manifest. Discovery performs only strict decoding so one bad field is
		// never reported once here and a second time by ValidateRepository.
		manifest, findings := archproto.ParseManifest(data)
		appendPathDiagnostics(&result.Diagnostics, relative, findings)
		if manifest != nil {
			result.Sources = append(result.Sources, archproto.ManifestSource{Path: relative, Manifest: manifest})
		}
	}
	result.Baseline, diagnostics = discoverBaseline(root)
	result.Diagnostics = append(result.Diagnostics, diagnostics...)
	result.Waivers, diagnostics = discoverWaivers(root)
	result.Diagnostics = append(result.Diagnostics, diagnostics...)
	sortDiagnostics(result.Diagnostics)
	return result
}

func discoverManifestPaths(root string) ([]string, []diag.Diagnostic) {
	paths := make([]string, 0)
	var diagnostics []diag.Diagnostic
	err := filepath.WalkDir(root, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			relative := workspaceRelative(root, current)
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeReadFailure, relative, "walk architecture declarations: %v", walkErr))
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if current == root {
			return nil
		}
		if entry.IsDir() {
			// Hidden directories cover VCS, tool state, and generated outputs.
			// Exact manifests under visible fixtures or testdata remain authority:
			// silently skipping them would let a declared domain vanish by rename.
			if excludedDiscoveryDirectories[entry.Name()] || strings.HasPrefix(entry.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Name() != archproto.ManifestFilename {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			diagnostics = append(diagnostics, diag.Errorf(ErrorCodeReadFailure, workspaceRelative(root, current), "architecture manifest must be a regular file, not a symlink"))
			return nil
		}
		paths = append(paths, workspaceRelative(root, current))
		if len(paths) > MaximumManifests {
			return errManifestLimit
		}
		return nil
	})
	if errors.Is(err, errManifestLimit) {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeDiscoveryLimit, "workspace", "architecture manifest count exceeds %d", MaximumManifests))
	} else if err != nil {
		diagnostics = append(diagnostics, diag.Errorf(ErrorCodeReadFailure, "workspace", "walk architecture declarations: %v", err))
	}
	sort.Strings(paths)
	return paths, diagnostics
}

var errManifestLimit = errors.New("architecture manifest limit exceeded")

func discoverBaseline(root string) (*archproto.Baseline, []diag.Diagnostic) {
	path := filepath.Join(root, archproto.BaselineFilename)
	data, err := readOptionalBoundedRegularFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeReadFailure, archproto.BaselineFilename, "read architecture baseline: %v", err)}
	}
	baseline, diagnostics := archproto.ParseAndValidateBaseline(data)
	var sourced []diag.Diagnostic
	appendPathDiagnostics(&sourced, archproto.BaselineFilename, diagnostics)
	return baseline, sourced
}

func discoverWaivers(root string) (*archproto.WaiverFile, []diag.Diagnostic) {
	path := filepath.Join(root, archproto.WaiverFilename)
	data, err := readOptionalBoundedRegularFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ErrorCodeReadFailure, archproto.WaiverFilename, "read architecture waivers: %v", err)}
	}
	waivers, diagnostics := archproto.ParseAndValidateWaiverFile(data)
	var sourced []diag.Diagnostic
	appendPathDiagnostics(&sourced, archproto.WaiverFilename, diagnostics)
	return waivers, sourced
}

func readOptionalBoundedRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file is not regular")
	}
	return readBoundedFile(path, info)
}

func readBoundedRegularFile(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("file is not regular")
	}
	return readBoundedFile(path, info)
}

func readBoundedFile(path string, info fs.FileInfo) ([]byte, error) {
	if info.Size() > MaximumManifestBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", MaximumManifestBytes)
	}
	data, err := os.ReadFile(path) //nolint:gosec // path comes from a contained workspace walk or fixed root filename
	if err != nil {
		return nil, err
	}
	if len(data) > MaximumManifestBytes {
		return nil, fmt.Errorf("file exceeds %d bytes", MaximumManifestBytes)
	}
	return data, nil
}

func workspaceRelative(root, absolute string) string {
	relative, err := filepath.Rel(root, absolute)
	if err != nil {
		return filepath.ToSlash(absolute)
	}
	return filepath.ToSlash(relative)
}

func appendPathDiagnostics(target *[]diag.Diagnostic, source string, diagnostics []diag.Diagnostic) {
	for _, diagnostic := range diagnostics {
		if diagnostic.Field == "" {
			diagnostic.Field = source
		} else {
			diagnostic.Field = source + "#" + diagnostic.Field
		}
		*target = append(*target, diagnostic)
	}
}

func sortDiagnostics(diagnostics []diag.Diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if diag.SeverityRank(left.Severity) != diag.SeverityRank(right.Severity) {
			return diag.SeverityRank(left.Severity) < diag.SeverityRank(right.Severity)
		}
		if left.Severity != right.Severity {
			return left.Severity < right.Severity
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		if left.Code != right.Code {
			return left.Code < right.Code
		}
		return left.Message < right.Message
	})
}
