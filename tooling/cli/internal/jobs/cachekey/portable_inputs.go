package cachekey

import (
	"path/filepath"
	"strings"

	jobmodel "go.putnami.dev/cli/model/jobs"
	"go.putnami.dev/tooling/cli/internal/store"
	"go.putnami.dev/tooling/cli/internal/workspace"
)

// PortableTaskInputs is one planned task's declared inputs as the portable
// admission reads them: the files its cache key hashes and the
// environment variables its key reads. It is a read-only projection of the
// SAME declarations ComputeJobCacheHash folds into the key — KeyFilePatterns,
// the closure patterns, the generate assets, the env inputs — resolved
// through the store's own collectors, so the admission cannot disagree with
// the key about what a task requires.
//
// The two file sets are separated because only one of them is a STATEMENT.
// See PortableTaskInputs.Files and .FallbackFiles.
type PortableTaskInputs struct {
	// Key is the plan key of the task.
	Key string
	// Files are the sorted workspace-relative slash paths of every existing
	// file the task's key hashes through an EXPLICIT declaration: the task's
	// declared key files and workspace files, its closure patterns, its own
	// filePatterns, the command's filePatterns parameter, the project's
	// option layers, and the generate assets. Declaring a pattern is the task
	// saying "I read these", which is what makes an ignored match among them a
	// required input worth binding.
	Files []string
	// GoEmbedFiles is the subset of Files selected as raw semantic Go embed
	// inputs. Admission must refuse a portable request if snapshot policy would
	// omit one of these bytes, even when a lifecycle normally recreates it.
	GoEmbedFiles []string
	// FallbackFiles are the paths the key hashes only because the task
	// declares NO file input at all: the whole non-hidden project tree the
	// empty-pattern collector returns. That fallback is what the key does in
	// the absence of a declaration, not a statement of what the task reads, so
	// nothing selected only by it is ever bound — binding it would ship a
	// laptop's build artifacts, or refuse the request over one stale binary
	// past the protocol's file-size limit. It is reported instead.
	FallbackFiles []string
	// Env are the sorted environment variable NAMES a cacheable task's key
	// reads — the task's `env` inputs and the project's `envInputs`. Their
	// values are ambient machine state and never travel.
	Env []string
}

// PortablePlanInputs is the whole plan's admission projection.
type PortablePlanInputs struct {
	// Tasks holds one projection per planned job, in plan order.
	Tasks []PortableTaskInputs
	// Outputs are the sorted workspace-relative slash paths of every output a
	// planned task declares (v3 `declares.outputs` with a literal path, and the
	// legacy cache `outputs`). A required input under one of them is recreated
	// by the executing engine's own lifecycle and is never bound.
	Outputs []string
}

// PortableInputs projects every planned job onto the inputs the portable
// admission decides on. It reads the worktree (the collectors stat and walk)
// and never hashes anything. One call memoizes its collector results, so a
// project tree is walked once for a pattern set however many jobs share it —
// the same reason the key path memoizes through the CacheManager.
// expander is the one the keys pass for their input selectors.
func PortableInputs(ws *workspace.Workspace, planned []*jobmodel.ScheduledJob, commandParams map[string]any, expander store.InputExpander) (PortablePlanInputs, error) {
	inputs := PortablePlanInputs{Tasks: make([]PortableTaskInputs, 0, len(planned))}
	if ws == nil {
		return inputs, nil
	}
	memo := keyFileMemo{}
	for _, job := range planned {
		if job == nil || job.JobDef == nil || job.Project == nil {
			continue
		}
		if err := validateDeclaredGoEmbedSelectors(ws, job, commandParams); err != nil {
			return PortablePlanInputs{}, err
		}
		files, workspaceFiles := KeyFilePatterns(ws, job, commandParams)
		declared, fallback, goEmbed, err := portableKeyFiles(memo, ws, job, files, workspaceFiles, expander)
		if err != nil {
			return PortablePlanInputs{}, err
		}
		inputs.Tasks = append(inputs.Tasks, PortableTaskInputs{
			Key: job.Key(), Files: declared, GoEmbedFiles: goEmbed, FallbackFiles: fallback, Env: portableKeyEnv(ws, job),
		})
	}
	inputs.Outputs = portableDeclaredOutputs(ws, planned)
	return inputs, nil
}

// keyFileMemo holds one PortableInputs call's collector results, keyed by the
// root and the pattern set. It is per call and never process-global: the
// worktree it describes is read at one instant, and a later call must see a
// later tree. A NUL separator cannot appear in a path or in a manifest glob,
// so two different pattern sets cannot share a key.
// The key leaves the expander out: one call passes one expander.
type keyFileMemo map[string]store.KeyFileSelection

func (m keyFileMemo) collect(root string, patterns []string, expander store.InputExpander) ([]string, error) {
	selection, err := m.collectSelection(root, patterns, expander)
	return selection.Files, err
}

func (m keyFileMemo) collectSelection(root string, patterns []string, expander store.InputExpander) (store.KeyFileSelection, error) {
	key := root + "\x00" + strings.Join(patterns, "\x00")
	if cached, found := m[key]; found {
		return cached, nil
	}
	files, err := store.CollectKeyFileSelection(root, patterns, expander)
	if err != nil {
		return store.KeyFileSelection{}, err
	}
	m[key] = files
	return files, nil
}

// portableKeyFiles resolves the file set ComputeJobCacheHash hashes for job
// from its KeyFilePatterns, as workspace-relative slash paths, split into the
// paths an explicit declaration selects and the paths the whole-tree fallback
// alone selects. Paths outside the workspace root cannot be captured and are
// dropped from both.
//
// The fallback is deliberately independent of the RUN's cache bypass: whether
// --no-cache was typed does not change what a task reads, and the bound set
// travels inside the request's identity, which must not move with a flag.
func portableKeyFiles(memo keyFileMemo, ws *workspace.Workspace, job *jobmodel.ScheduledJob, files, workspaceFiles []string, expander store.InputExpander) (declared, fallback, goEmbed []string, resultErr error) {
	projectRoot := filepath.Join(ws.Root, job.Project.Path)
	var absolute, fallbackAbsolute, semanticAbsolute []string
	collect := func(root string, patterns []string, explicit bool) []string {
		if resultErr != nil {
			return nil
		}
		selection, err := memo.collectSelection(root, patterns, expander)
		resultErr = err
		if explicit {
			semanticAbsolute = append(semanticAbsolute, selection.GoEmbed...)
		}
		return selection.Files
	}
	switch {
	case len(files) > 0:
		absolute = append(absolute, collect(projectRoot, files, true)...)
	case jobmodel.CanUseCache(job):
		fallbackAbsolute = append(fallbackAbsolute, collect(projectRoot, nil, false)...)
	}
	if len(workspaceFiles) > 0 {
		absolute = append(absolute, collect(ws.Root, workspaceFiles, true)...)
	}
	if patterns := closureKeyPatterns(job); len(patterns) > 0 {
		for _, root := range ProjectClosureRoots(ws, job.Project) {
			absolute = append(absolute, collect(root, patterns, true)...)
		}
	}
	if resultErr != nil {
		return nil, nil, nil, resultErr
	}
	absolute = append(absolute, store.ExtraKeyFiles(GenerateAssetFiles(ws, job))...)
	return workspaceRelativePaths(ws.Root, absolute), workspaceRelativePaths(ws.Root, fallbackAbsolute), workspaceRelativePaths(ws.Root, semanticAbsolute), nil
}

// portableKeyEnv returns the environment variable names a cacheable job's key
// reads. A job that computes no key reads no ambient environment through it.
func portableKeyEnv(ws *workspace.Workspace, job *jobmodel.ScheduledJob) []string {
	if !jobmodel.CanUseCache(job) {
		return nil
	}
	var names []string
	if policy := job.JobDef.TaskCachePolicy; policy != nil && policy.Key != nil {
		names = append(names, policy.Key.Env...)
	}
	names = append(names, ProjectEnvInputs(ws, job)...)
	return jobmodel.DedupeSorted(names)
}

// portableDeclaredOutputs lists the workspace-relative output paths every
// planned task declares, so an ignored input under one of them is recognized
// as something the executing engine recreates rather than a required input.
func portableDeclaredOutputs(ws *workspace.Workspace, planned []*jobmodel.ScheduledJob) []string {
	var absolute []string
	for _, owner := range DeclaredOutputOwners(planned, ws) {
		if owner.Ref.Path != "" {
			absolute = append(absolute, filepath.FromSlash(owner.Ref.Path))
		}
	}
	for _, job := range planned {
		if job == nil || job.JobDef == nil || job.Project == nil || job.JobDef.TaskCachePolicy == nil {
			continue
		}
		for _, output := range job.JobDef.TaskCachePolicy.Outputs {
			if output.Path != "" {
				absolute = append(absolute, filepath.Join(ws.Root, job.Project.Path, filepath.FromSlash(output.Path)))
			}
		}
	}
	return workspaceRelativePaths(ws.Root, absolute)
}

// workspaceRelativePaths rebases absolute paths onto the workspace root as
// sorted, unique slash paths, dropping any path outside the root.
func workspaceRelativePaths(wsRoot string, absolute []string) []string {
	var relative []string
	for _, p := range absolute {
		rel, err := filepath.Rel(wsRoot, p)
		if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		relative = append(relative, filepath.ToSlash(rel))
	}
	return jobmodel.DedupeSorted(relative)
}
