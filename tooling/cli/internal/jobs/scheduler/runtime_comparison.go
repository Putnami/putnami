package scheduler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"go.putnami.dev/sdk/extension/dirlink"
	"go.putnami.dev/tooling/cli/internal/artifactstore"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/runcredential"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// Reasons CompareExtensionRuntimes gives when it cannot call a runtime
// unchanged. They end up in the selection's explanation line.
const (
	runtimeReasonDiffers         = "its runtime differs from the base's"
	runtimeReasonBaseNotPrepared = "the base runtime is not in the artifact store"
	runtimeReasonNoBase          = "the diff has no base commit"
)

// CompareExtensionRuntimes answers, for each candidate project that is an
// in-workspace extension preparing its runtime from source, whether the
// runtime this tree prepares is byte for byte the one diffBase prepares
// (workspace.ImpactedSelectionOptions.CompareRuntimes). A candidate that is no
// such extension gets no answer.
//
// The base runtime comes from a detached worktree of diffBase and lands in the
// same artifact store, under the digest of the base's inputs, so a later run
// against the same base finds it. A run that holds the run credential never
// prepares one: it executes no repository code during the run (ADR 0055), so
// it only looks the base runtime up in the store, and an extension whose base
// runtime is not there keeps its edge. Every failure keeps the edge too:
// selection may only shrink on a proof.
func CompareExtensionRuntimes(
	ctx context.Context,
	ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription,
	candidates []string,
	diffBase string,
) []workspace.RuntimeComparison {
	if ws == nil || len(candidates) == 0 {
		return nil
	}
	byProject := sourcePreparedExtensionsByProject(ws, extensions)
	var compared []*extension.ExtensionDescription
	var projects []string
	for _, id := range candidates {
		if ext := byProject[id]; ext != nil {
			compared = append(compared, ext)
			projects = append(projects, id)
		}
	}
	if len(compared) == 0 {
		return nil
	}
	answers := make([]workspace.RuntimeComparison, len(compared))
	for i, id := range projects {
		answers[i] = workspace.RuntimeComparison{Project: id}
	}
	if strings.TrimSpace(diffBase) == "" {
		for i := range answers {
			answers[i].Reason = runtimeReasonNoBase
		}
		return answers
	}
	root, err := dirlink.Resolve(ws.Root)
	if err != nil {
		root = ws.Root
	}
	baseRoot, cleanup, err := checkoutBase(ctx, root, diffBase)
	if err != nil {
		for i := range answers {
			answers[i].Reason = "check out the base: " + err.Error()
		}
		return answers
	}
	defer cleanup()

	artifacts := artifactstore.New(store.ResolveArtifactStoreRoot(ws.Root))
	hosted := runcredential.Hosted()
	baseWS := &workspace.Workspace{Root: baseRoot}
	for i, ext := range compared {
		answers[i].Unchanged, answers[i].Reason = compareOneRuntime(ctx, ws, baseWS, root, artifacts, ext, hosted)
	}
	return answers
}

// compareOneRuntime prepares, or loads, the runtime of ext from this tree and
// from the base worktree, and compares their contents.
func compareOneRuntime(
	ctx context.Context,
	ws, baseWS *workspace.Workspace,
	root string,
	artifacts *artifactstore.Store,
	ext *extension.ExtensionDescription,
	hosted bool,
) (bool, string) {
	if err := SynchronizeExtensionRuntimes(ctx, ws, []*extension.ExtensionDescription{ext}, nil); err != nil {
		return false, "prepare this tree's runtime: " + err.Error()
	}
	headDigest := ext.RuntimeDigest

	resolved, err := dirlink.Resolve(ext.Path)
	if err != nil {
		resolved = ext.Path
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return false, "the extension is outside the workspace"
	}
	base := *ext
	base.Path = filepath.Join(baseWS.Root, rel)
	base.RuntimeExecutable, base.RuntimeDigest = "", ""
	if _, err := os.Stat(filepath.Join(base.Path, extension.ManifestFilename)); err != nil {
		return false, "the extension does not exist at the base"
	}

	var baseDigest string
	if hosted {
		// A hosted run looks the base runtime up and prepares nothing.
		baseDigest, err = runtimeInputDigest(baseWS.Root, &base)
		if err != nil {
			return false, "digest the base runtime inputs: " + err.Error()
		}
		if !artifacts.Has(baseDigest) {
			return false, runtimeReasonBaseNotPrepared
		}
	} else {
		if err := SynchronizeExtensionRuntimes(ctx, baseWS, []*extension.ExtensionDescription{&base}, nil); err != nil {
			return false, "prepare the base runtime: " + err.Error()
		}
		baseDigest = base.RuntimeDigest
	}
	if headDigest == baseDigest {
		return true, ""
	}
	same, err := sameRuntimeContents(
		filepath.Join(artifacts.Path(headDigest), "runtime"),
		filepath.Join(artifacts.Path(baseDigest), "runtime"),
	)
	if err != nil {
		return false, "compare the runtimes: " + err.Error()
	}
	if !same {
		return false, runtimeReasonDiffers
	}
	return true, ""
}

// sourcePreparedExtensionsByProject maps each project of ws to the extension
// rooted at its directory whose runtime it prepares from local source.
func sourcePreparedExtensionsByProject(
	ws *workspace.Workspace,
	extensions []*extension.ExtensionDescription,
) map[string]*extension.ExtensionDescription {
	byDir := make(map[string]*extension.ExtensionDescription, len(extensions))
	for _, ext := range extensions {
		if ext == nil || ext.Runtime == nil || ext.Runtime.Prepare == nil || !extension.WorkspacePathExtension(ws.Root, ext) {
			continue
		}
		dir, err := dirlink.Resolve(ext.Path)
		if err != nil {
			continue
		}
		byDir[filepath.Clean(dir)] = ext
	}
	byProject := make(map[string]*extension.ExtensionDescription)
	for _, project := range ws.Projects {
		dir, err := dirlink.Resolve(filepath.Join(ws.Root, filepath.FromSlash(project.Path)))
		if err != nil {
			continue
		}
		if ext := byDir[filepath.Clean(dir)]; ext != nil {
			byProject[project.ID] = ext
		}
	}
	return byProject
}

// checkoutBase checks diffBase out into a detached worktree of the repository
// at root, and returns its directory with the function that removes it. It
// reads git objects only: no repository code runs.
func checkoutBase(ctx context.Context, root, diffBase string) (string, func(), error) {
	parent, err := os.MkdirTemp("", "putnami-base-runtime-")
	if err != nil {
		return "", nil, err
	}
	dir := filepath.Join(parent, "tree")
	git := func(args ...string) error {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if err := git("worktree", "add", "--detach", "--quiet", dir, diffBase); err != nil {
		_ = os.RemoveAll(parent)
		return "", nil, err
	}
	cleanup := func() {
		_ = git("worktree", "remove", "--force", dir)
		_ = os.RemoveAll(parent)
		_ = git("worktree", "prune")
	}
	return dir, cleanup, nil
}

// sameRuntimeContents reports whether two prepared runtime directories hold the
// same files, with the same executable bits and the same bytes.
func sameRuntimeContents(a, b string) (bool, error) {
	da, err := runtimeContentsDigest(a)
	if err != nil {
		return false, err
	}
	db, err := runtimeContentsDigest(b)
	if err != nil {
		return false, err
	}
	return da == db, nil
}

// runtimeContentsDigest hashes every regular file and symlink under dir, by
// relative path, executable bit and content, in sorted path order.
func runtimeContentsDigest(dir string) (string, error) {
	type entry struct {
		rel  string
		full string
		mode fs.FileMode
	}
	var entries []entry
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		entries = append(entries, entry{rel: filepath.ToSlash(rel), full: path, mode: info.Mode()})
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].rel < entries[j].rel })
	h := sha256.New()
	for _, e := range entries {
		fmt.Fprintf(h, "%s\x00%o\x00", e.rel, e.mode&(fs.ModeType|0o111))
		if e.mode&fs.ModeSymlink != 0 {
			target, err := os.Readlink(e.full)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(h, "%s\x00", target)
			continue
		}
		f, err := os.Open(e.full)
		if err != nil {
			return "", err
		}
		_, err = io.Copy(h, f)
		_ = f.Close()
		if err != nil {
			return "", err
		}
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
