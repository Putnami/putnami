package watch

import (
	"context"
	"os"
	"time"

	"go.putnami.dev/cli/model/workspace"
	protocolcli "go.putnami.dev/protocol/cli"
	"go.putnami.dev/tooling/cli/internal/iox"
)

// runJobLoop runs the watch loop for non-serve commands (test, lint, build).
func (s *Session) runJobLoop(ctx context.Context) int {
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(done)
	}()

	changes := s.watcher.Watch(done)

	var lastExitCode int

	for {
		select {
		case <-ctx.Done():
			s.watchRend.Shutdown()
			return lastExitCode

		case changedFiles, ok := <-changes:
			if !ok {
				s.watchRend.Shutdown()
				return lastExitCode
			}

			s.iteration++

			affectedProjects := s.affectedProjects(changedFiles)
			if len(affectedProjects) == 0 {
				continue
			}

			s.watchRend.IterationStart(s.iteration, changedFiles)
			names := make([]string, len(affectedProjects))
			for i, p := range affectedProjects {
				names[i] = p.Name
			}
			s.watchRend.AffectedProjects(names)

			lastExitCode = s.runIterationForProjects(ctx, affectedProjects, changedFiles)
		}
	}
}

// affectedProjects maps a batch of changed files onto the projects to replan.
//
// It is the SHARED change→project calculation (workspace.ProjectsForChangedFiles),
// the same one `--impacted` runs over its git diff. This used to be
// internal/watch/classifier.go, a second algorithm whose comment said it
// "mirrors --impacted"; what the merge was about is the one rule the shared path
// lacked — workspace-scoped task inputs. That rule moved into the shared function
// as ChangeImpactOptions rather than being deleted, so watch still re-runs
// everything on a lockfile edit. The ownership width the two algorithms once
// differed on is no longer a difference: the classifier's nearest-owner rule
// moved into the shared path, so a file inside a nested project again
// attributes to that project only, on both surfaces.
func (s *Session) affectedProjects(changedFiles []string) []*workspace.Project {
	return workspace.ProjectsForChangedFiles(s.cfg.Workspace, changedFiles, workspace.ChangeImpactOptions{
		WorkspaceInputPatterns: s.cfg.WorkspaceInputPatterns,
	})
}

// runIteration executes a single watch iteration with selected projects.
func (s *Session) runIteration(ctx context.Context, changedFiles []string) {
	projects := s.cfg.SelectedProjects
	if len(projects) == 0 {
		projects = s.cfg.Workspace.Projects
	}
	s.runIterationForProjects(ctx, projects, changedFiles)
}

// runIterationForProjects runs one iteration through the engine seam and renders
// its verdict.
//
// Exit codes are the protocol/cli taxonomy. This used to return raw
// ints — 0 success, 1 plan failure, 2 job failure — where 2 is ExitUsage, so a
// failing final iteration made `putnami build --watch` exit "bad usage".
func (s *Session) runIterationForProjects(ctx context.Context, projects []*workspace.Project, _ []string) int {
	start := time.Now()

	if s.cfg.RunIteration == nil {
		// A watch session with no runner would report a clean iteration forever
		// while never rebuilding anything. Fail loudly instead.
		iox.Fprintf(os.Stderr, "putnami: watch: no iteration runner configured\n")
		s.watchRend.IterationEnd(s.iteration, false, time.Since(start))
		return protocolcli.ExitFailure
	}

	outcome := s.cfg.RunIteration(ctx, projects, s.watchRend)
	duration := time.Since(start)

	if outcome.Aborted {
		// Ctrl-C, or the serve loop canceling the previous iteration. Neither
		// "done" nor "failed" describes it, and the loop is about to stop or
		// restart rather than wait for changes. The session's exit code is decided
		// by the caller from the signal, not from this iteration.
		s.watchRend.IterationInterrupted(duration)
		return protocolcli.ExitSuccess
	}

	success := outcome.ExitCode == protocolcli.ExitSuccess
	s.watchRend.IterationEnd(s.iteration, success, duration)
	return outcome.ExitCode
}
