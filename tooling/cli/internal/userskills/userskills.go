// Package userskills writes the skills an extension pinned in the user scope
// names for repositories without a workspace (agentContent.userScope) under
// the user's home, and removes them (CLI ADR 0085). The ownership rules are
// internal/agentartifacts'.
package userskills

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"

	"go.putnami.dev/tooling/cli/internal/agentartifacts"
	"go.putnami.dev/tooling/cli/internal/extension"
	"go.putnami.dev/tooling/cli/internal/iox"
	"go.putnami.dev/tooling/cli/internal/lockfile"
)

// userScopeSkillHosts are the directories, relative to the user's home, where
// the agent hosts read personal skills: Claude Code and Codex. A built
// content tree places each skill under both.
var userScopeSkillHosts = []string{".claude/skills/", ".agents/skills/"}

// ErrCollide reports a user-scope install whose skills would
// overwrite a file the person owns. Nothing was written.
var ErrCollide = errors.New("user-scope skills not written: files under the home directory would be overwritten")

// InstallUserScopeSkills writes, under home, the skills the extension that the
// user scope at userRoot pins names in agentContent.userScope, and only them
// (CLI ADR 0085). The content is read through the user-scope pin and verified
// like a workspace's; the files are planned and applied by
// internal/agentartifacts, rooted at home, with the ownership record in the
// user scope. One collision writes nothing. A pin whose skills are already in
// place writes nothing, and an extension that names no skill writes nothing,
// except that it removes skills an earlier release of it wrote.
func Install(userRoot, home, name string, out io.Writer) error {
	source, err := extension.LocateUserScopeAgentContent(userRoot, name)
	if errors.Is(err, extension.ErrNoAgentContent) {
		return Remove(userRoot, home, name, out)
	}
	if err != nil {
		return fmt.Errorf("read the agent content of %s: %w", name, err)
	}
	skills := source.Contribution.UserScope
	if len(skills) == 0 {
		return Remove(userRoot, home, name, out)
	}
	artifact, err := agentartifacts.LoadArtifact(filepath.Join(source.Root, filepath.FromSlash(source.Contribution.Path)), name,
		lockfile.AgentArtifactLockEntry{Version: source.Version, Integrity: source.ManifestHash, ManifestHash: source.Contribution.ManifestSHA256})
	if err != nil {
		return err
	}
	artifact, err = userScopeSubset(artifact, skills)
	if err != nil {
		return err
	}
	state, err := agentartifacts.LoadState(userRoot, name)
	if err != nil {
		return err
	}
	plan, err := agentartifacts.BuildPlan(home, artifact, state)
	if err != nil {
		return err
	}
	if plan.HasCollisions() {
		iox.Fprint(out, agentartifacts.NewReport(plan, false).Text())
		return ErrCollide
	}
	recorded := stateMatches(state, artifact)
	if !plan.Mutates() && recorded {
		iox.Fprintf(out, "  ✓ Skills %s already in place\n", strings.Join(skills, ", "))
		return nil
	}
	if err := agentartifacts.Apply(home, artifact, plan); err != nil {
		return err
	}
	if err := agentartifacts.WriteState(userRoot, agentartifacts.StateFor(artifact)); err != nil {
		return err
	}
	report := agentartifacts.NewReport(plan, true)
	iox.Fprintf(out, "  ✓ Skills %s written for Claude Code (~/.claude/skills) and Codex (~/.agents/skills) (%d created, %d updated, %d removed)\n",
		strings.Join(skills, ", "), len(report.Created), len(report.Updated), len(report.Removed))
	return nil
}

// RemoveUserScopeSkills removes, under home, the skills a user-scope install
// of name wrote: each file the ownership record names and that still holds the
// recorded bytes. A file the person edited is kept and reported. The record is
// deleted last. Without a record there is nothing to remove.
func Remove(userRoot, home, name string, out io.Writer) error {
	state, err := agentartifacts.LoadState(userRoot, name)
	if err != nil {
		return err
	}
	if state == nil {
		return nil
	}
	plan, err := agentartifacts.BuildRemovalPlan(home, state)
	if err != nil {
		return err
	}
	released, err := agentartifacts.ApplyRemoval(home, plan)
	if err != nil {
		return err
	}
	if err := agentartifacts.RemoveState(userRoot, name); err != nil {
		return err
	}
	iox.Fprintf(out, "  ✓ Removed the user-scope skills of %s (%d files)\n", name, len(plan.Select(agentartifacts.ActionRemove)))
	for _, path := range released {
		iox.Fprintf(out, "  ! Kept %s under your home: it was edited since it was installed\n", path)
	}
	return nil
}

// stateMatches reports whether state records exactly the artifact: the same
// release and the same files, byte for byte.
func stateMatches(state *agentartifacts.State, artifact *agentartifacts.Artifact) bool {
	if state == nil {
		return false
	}
	want := agentartifacts.StateFor(artifact)
	return state.ArtifactVersion == want.ArtifactVersion && state.ArchiveDigest == want.ArchiveDigest &&
		state.ManifestHash == want.ManifestHash && slices.Equal(state.Files, want.Files)
}

// userScopeSubset keeps the files of the named skills under the hosts that
// read skills from the user's home. A name the content does not ship as a
// skill on every such host is refused: the package step refuses it, so a
// release that carries one was not built by it.
func userScopeSubset(artifact *agentartifacts.Artifact, skills []string) (*agentartifacts.Artifact, error) {
	subset := *artifact
	subset.Files = nil
	for _, skill := range skills {
		for _, host := range userScopeSkillHosts {
			prefix := host + skill + "/"
			found := false
			for _, file := range artifact.Files {
				if strings.HasPrefix(file.Path, prefix) {
					subset.Files = append(subset.Files, file)
					found = found || file.Path == prefix+"SKILL.md"
				}
			}
			if !found {
				return nil, fmt.Errorf("extension %s names user-scope skill %s, but its content ships no %sSKILL.md: re-package the extension",
					artifact.Name, skill, prefix)
			}
		}
	}
	return &subset, nil
}
