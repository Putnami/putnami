package jobs

import (
	"fmt"
	"path/filepath"
	"slices"

	extproto "go.putnami.dev/protocol/extension"
	putnamigit "go.putnami.dev/tooling/cli/internal/git"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// releaseBaselineIdentity resolves the releaseBaseline runtime input of a task
// that declared it into its `name=value` cache-key pair, and answers "" for a
// task that did not.
//
// The value is the project's release baseline under its version line's tag
// pattern (putnamigit.ReadReleaseBaseline): the repository state, the pattern,
// the line's last tag HEAD reaches, the object that tag holds at the project
// directory, and whether a commit since the tag that touches the project
// declares a breaking change. It names no commit HEAD reaches, no ref and no
// path, so a branch and its squash merge share the key when the squash message
// declares the same break, and every input of a verdict that compares the
// working tree with the last release moves it: a squash title that adds or
// drops the breaking marker moves it too.
//
// A git failure is the key's error, so the task runs uncached instead of
// keying a verdict to a baseline nobody read. One run reads each project's
// baseline once.
func releaseBaselineIdentity(ws *workspace.Workspace, job *ScheduledJob, cache *store.CacheManager) (string, error) {
	if job == nil || job.JobDef == nil || job.JobDef.TaskCachePolicy == nil || job.JobDef.TaskCachePolicy.Key == nil ||
		!slices.Contains(job.JobDef.TaskCachePolicy.Key.Runtime, extproto.RuntimeInputReleaseBaseline) {
		return "", nil
	}
	if job.Project == nil {
		return "", fmt.Errorf("the %s input needs a project", extproto.RuntimeInputReleaseBaseline)
	}
	dir := filepath.Join(ws.Root, job.Project.Path)
	pattern := lineTagPattern(ws, job.Project.Line)
	value, err := cache.Digest("release-baseline\x00"+dir+"\x00"+pattern, func() (string, error) {
		baseline, err := putnamigit.ReadReleaseBaseline(dir, pattern)
		if err != nil {
			return "", err
		}
		return baseline.Value(), nil
	})
	if err != nil {
		return "", fmt.Errorf("read the release baseline of %s: %w", job.Project.Name, err)
	}
	return extproto.RuntimeInputReleaseBaseline + "=" + value, nil
}
