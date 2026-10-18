package finalize

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"

	wire "go.putnami.dev/protocol/extension/hostrpc"
	features "go.putnami.dev/protocol/features"
	wsproto "go.putnami.dev/protocol/workspace"
)

// WorkspaceOwners answers, for each workspace-relative path, the projects
// that own it, in the order given. The host's workspace.owners is one.
type WorkspaceOwners func(ctx context.Context, paths []string) ([]wire.PathOwners, error)

// HostOwners resolves the Ownership of changed paths as the core does, with
// the owning projects taken from owners instead of a workspace the
// executable loads. A changed path outside the workspace has no owner, and
// owners is not asked about it. A path the host channel cannot carry, such
// as one that is not valid UTF-8, fails closed.
func HostOwners(root string, owners WorkspaceOwners) RequirementOwners {
	return func(ctx context.Context, base string, paths []string) (Ownership, error) {
		var ownership Ownership
		prefix, err := workspacePrefix(root)
		if err != nil {
			return ownership, fmt.Errorf("resolve changed project ownership: %w", err)
		}
		unowned := map[string]bool{}
		var changed, asked []string
		for _, candidate := range paths {
			relative, inside := strings.CutPrefix(candidate, prefix)
			if !inside {
				unowned[candidate] = true
				continue
			}
			// ValidWorkspacePath rejects a path that is not valid UTF-8.
			if !wire.ValidWorkspacePath(relative) {
				return ownership, fmt.Errorf("resolve changed project ownership: changed path %q is not a valid UTF-8 workspace path, so the putnami host cannot name its owners", candidate)
			}
			changed = append(changed, candidate)
			asked = append(asked, relative)
		}
		checked, holders := map[string]bool{}, map[string]bool{}
		if len(asked) > 0 {
			answer, err := owners(ctx, asked)
			if err != nil {
				return ownership, fmt.Errorf("resolve changed project ownership: %w", err)
			}
			if len(answer) != len(asked) {
				return ownership, fmt.Errorf("resolve changed project ownership: the host answered %d entries for %d paths", len(answer), len(asked))
			}
			for i, entry := range answer {
				if entry.Path != asked[i] {
					return ownership, fmt.Errorf("resolve changed project ownership: the host answered %q for %q", entry.Path, asked[i])
				}
				if len(entry.Projects) == 0 {
					unowned[changed[i]] = true
				}
				for _, project := range entry.Projects {
					if checked[project.ID] {
						continue
					}
					checked[project.ID] = true
					ok, err := featurelessProject(root, base, project.Path)
					if err != nil {
						return ownership, fmt.Errorf("resolve changed project ownership: %w", err)
					}
					if !ok {
						holders[project.ID] = true
					}
				}
			}
		}
		ownership.Projects = slices.Sorted(maps.Keys(holders))
		ownership.Unowned = slices.Sorted(maps.Keys(unowned))
		return ownership, nil
	}
}

// featurelessProject reports whether the project at projectPath declares
// featureAuthority.none and authors no feature both on the branch and at
// base, so a change cannot exempt its own project. A project new on the
// branch has only its head state. The branch state is the project's
// putnami.json on disk, read with the function the workspace loader calls.
// The loader keeps a config its diagnostics flag, so they do not matter here.
// A file it cannot decode, such as one with a type error in any field, gives
// no config, and a project without a config is not featureless.
func featurelessProject(root, base, projectPath string) (bool, error) {
	config, _ := wsproto.LoadProjectConfigWithDiagnostics(filepath.Join(root, projectPath))
	if config == nil || !featureless(config.FeatureAuthority) {
		return false, nil
	}
	if _, err := os.Stat(filepath.Join(root, projectPath, features.ManifestFilename)); !errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	manifest, found, err := fileAt(root, base, path.Join(projectPath, wsproto.ConfigFilename))
	if err != nil || !found {
		return err == nil, err
	}
	var baseConfig struct {
		FeatureAuthority *wsproto.ProjectFeatureAuthority `json:"featureAuthority"`
	}
	if err := json.Unmarshal(manifest, &baseConfig); err != nil || !featureless(baseConfig.FeatureAuthority) {
		return false, nil
	}
	_, authored, err := fileAt(root, base, path.Join(projectPath, features.ManifestFilename))
	return err == nil && !authored, err
}

func featureless(authority *wsproto.ProjectFeatureAuthority) bool {
	return authority != nil && strings.TrimSpace(authority.None) != "" && strings.TrimSpace(authority.Owner) == ""
}

// gitTimeout bounds each git command the ownership check runs, as the core
// bounds its own.
const gitTimeout = 30 * time.Second

// workspacePrefix returns dir's path below the repository top level, with a
// trailing slash, or "" at the top level. Diff paths carry this prefix.
func workspacePrefix(dir string) (string, error) {
	output, err := gitOutput(dir, "rev-parse", "--show-prefix")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(output), nil
}

// fileAt returns the bytes of file, relative to dir, at revision. found is
// false when revision has no such file.
func fileAt(dir, revision, file string) (content []byte, found bool, err error) {
	if !validRevisionArgument(revision) {
		return nil, false, fmt.Errorf("invalid revision %q", revision)
	}
	listed, err := gitOutput(dir, "ls-tree", "-z", "--name-only", revision, "--", file)
	if err != nil || listed == "" {
		return nil, false, err
	}
	output, err := gitOutput(dir, "show", revision+":./"+file)
	if err != nil {
		return nil, false, err
	}
	return []byte(output), true, nil
}

func validRevisionArgument(ref string) bool {
	if ref == "" || len(ref) > 1024 || ref != strings.TrimSpace(ref) || strings.HasPrefix(ref, "-") {
		return false
	}
	for _, character := range ref {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

// gitOutput runs git in dir and returns its stdout.
func gitOutput(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnvironment(os.Environ())
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			err = fmt.Errorf("timed out after %s", gitTimeout)
		}
		return "", fmt.Errorf("git %s (in %s): %w: %s", args[0], dir, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
