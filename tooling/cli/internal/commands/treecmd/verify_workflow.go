package treecmd

import (
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	features "go.putnami.dev/protocol/features"
)

// The workflow document is a local projection. Native specs own requirement
// identities, and the session's spec-verification record owns check verdicts.
// A pending projection is useful for handoff; only a dossier can mint ready.
type workflowRequirement struct {
	feature, id, disposition, unit string
	checks                         []string
}

type workflowSource struct {
	feature   features.Feature
	spec      features.Spec
	specPath  string
	projectID string
	checks    map[string][]string
}

func workflowObject(value any, name string, required, allowed []string) *jsObject {
	o, ok := value.(*jsObject)
	require(ok, name+" must be an object")
	for _, key := range required {
		require(o.has(key), name+" missing "+key)
	}
	for _, key := range o.keys {
		require(slices.Contains(allowed, key), name+" has unknown field "+key)
	}
	return o
}

func workflowText(value any, name string) string {
	s, ok := textValue(value)
	require(ok, name+" must be nonempty text")
	return s
}

func workflowList(value any, name string) []any {
	list, ok := value.([]any)
	require(ok, name+" must be a list")
	return list
}

func workflowStrings(value any, name string) []string {
	items := workflowList(value, name)
	result := make([]string, 0, len(items))
	for _, item := range items {
		result = append(result, workflowText(item, name+" item"))
	}
	require(!slices.ContainsFunc(result, func(s string) bool { return strings.ContainsAny(s, "\x00\r\n") }), name+" contains control text")
	require(!hasDuplicate(stringList(result)), name+" contains duplicate values")
	return result
}

func (r *verifyRun) workflowBytes(reference any, name string) ([]byte, string) {
	ref := workflowObject(reference, name, []string{"path", "sha256"}, []string{"path", "sha256"})
	file := r.checkRef(ref)
	info, err := os.Stat(r.path(file))
	require(err == nil && info.Size() <= maxEvidenceBytes, name+" exceeds 32 MiB")
	f, err := os.Open(r.path(file))
	if err != nil {
		failErr(fileError(err, file))
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxEvidenceBytes+1))
	if err != nil {
		failErr(fileError(err, file))
	}
	require(len(data) <= maxEvidenceBytes, name+" exceeds 32 MiB")
	return data, file
}

func workflowNativePath(file, kind string) {
	require(file != "" && !filepath.IsAbs(file) && !strings.Contains(file, "\\") && path.Clean(file) == file &&
		file != ".." && !strings.HasPrefix(file, "../"), kind+" must use a workspace-relative slash path")
	if kind == "manifest" {
		require(path.Base(file) == features.ManifestFilename, "workflow manifest path must name putnami.features.json")
	} else if kind == "spec" {
		require(path.Ext(file) == ".json" && path.Base(path.Dir(file)) == features.SpecDirectory,
			"workflow spec path must be a direct specs JSON child")
	} else if kind == "project" {
		require(path.Base(file) == "putnami.json", "workflow project manifest must name putnami.json")
	}
}

func workflowNativeError(kind string, diagnostics any) {
	fail(fmt.Sprintf("invalid native %s: %v", kind, diagnostics))
}

func (r *verifyRun) workflowSources(value any) map[string]workflowSource {
	sources := workflowList(value, "workflow.sources")
	require(len(sources) > 0, "workflow.sources must name native intent")
	result := map[string]workflowSource{}
	for _, item := range sources {
		source := workflowObject(item, "workflow source", []string{"feature", "manifest", "spec"}, []string{"feature", "manifest", "spec"})
		featureID := workflowText(source.get("feature"), "workflow source feature")
		_, exists := result[featureID]
		require(!exists, "duplicate workflow feature "+featureID)
		manifestBytes, manifestPath := r.workflowBytes(source.get("manifest"), "workflow manifest")
		specBytes, specPath := r.workflowBytes(source.get("spec"), "workflow spec")
		workflowNativePath(manifestPath, "manifest")
		workflowNativePath(specPath, "spec")
		projectDir := path.Dir(manifestPath)
		require(path.Dir(specPath) == path.Join(projectDir, features.SpecDirectory),
			"workflow spec and feature manifest have different native owners")
		manifest, diagnostics := features.ParseAndValidateManifest(manifestBytes)
		if len(diagnostics) > 0 {
			workflowNativeError("manifest", diagnostics)
		}
		spec, diagnostics := features.ParseAndValidateSpec(specBytes)
		if len(diagnostics) > 0 {
			workflowNativeError("spec", diagnostics)
		}
		require(spec.Feature == featureID, "workflow spec feature differs from source")
		var declared *features.Feature
		for i := range manifest.Features {
			if manifest.Features[i].ID == featureID {
				declared = &manifest.Features[i]
				break
			}
		}
		require(declared != nil, "workflow feature absent from native manifest: "+featureID)
		checks := map[string][]string{}
		for _, requirement := range spec.Requirements {
			checks[requirement.ID] = nil
			for _, native := range declared.Requirements {
				if native.ID == requirement.ID && native.Verification != nil {
					checks[requirement.ID] = slices.Clone(native.Verification.Checks)
				}
			}
		}
		projectID := "/"
		if projectDir != "." {
			projectID = "/" + projectDir
		}
		result[featureID] = workflowSource{feature: *declared, spec: *spec, specPath: specPath, projectID: projectID, checks: checks}
	}
	return result
}

func (r *verifyRun) workflowProof(value any, current string) (*features.SpecVerificationRecord, any) {
	proof := workflowObject(value, "workflow proof", []string{"session", "specVerification"}, []string{"session", "specVerification"})
	sessionPath := r.checkRef(proof.get("session"))
	verificationBytes, verificationPath := r.workflowBytes(proof.get("specVerification"), "workflow spec verification")
	require(filepath.Clean(r.path(verificationPath)) == filepath.Join(filepath.Dir(r.path(sessionPath)), features.SpecVerificationRecordFilename),
		"workflow spec verification is not the session sibling artifact")
	session := r.readJSON(sessionPath)
	require(strictEqual(at(session, "protocolVersion"), 2.0), "workflow proof needs a v2 session")
	require(!truthy(at(session, "parentSessionId")) && text(at(session, "endTime")), "workflow proof session is nested or unfinished")
	require(strictEqual(at(at(session, "tree"), "fingerprint"), current), "stale workflow proof tree")
	checkGateRun(at(session, "run"))
	record, diagnostics := features.ParseSpecVerificationRecord(verificationBytes)
	if len(diagnostics) > 0 {
		workflowNativeError("spec verification", diagnostics)
	}
	if diagnostics = features.ValidateSpecVerificationRecord(record); len(diagnostics) > 0 {
		workflowNativeError("spec verification", diagnostics)
	}
	require(record.SessionID == workflowText(at(session, "sessionId"), "workflow proof session ID"),
		"workflow spec verification/session mismatch")
	return record, session
}

func (r *verifyRun) workflowVerdicts(record *features.SpecVerificationRecord, session any, source map[string]workflowSource) map[string]features.SpecRequirementVerdict {
	result := map[string]features.SpecRequirementVerdict{}
	if record == nil {
		return result
	}
	require(len(record.Diagnostics) == 0, "workflow proof has unresolved native collection diagnostics")
	instant, err := time.Parse(time.RFC3339, record.GeneratedAt)
	require(err == nil, "workflow proof has invalid evaluation instant")
	produced := map[string]bool{}
	for _, task := range workflowList(at(session, "tasks"), "workflow proof session tasks") {
		if strictEqual(at(task, "status"), "success") && strictEqual(at(task, "exitCode"), 0.0) {
			if key, ok := at(at(task, "identity"), "key").(string); ok {
				produced[key] = true
			}
		}
	}
	for featureID, native := range source {
		requirements := native.checks
		var group *features.SpecVerificationGroup
		for i := range record.Groups {
			if record.Groups[i].Feature == featureID && record.Groups[i].Spec == native.specPath {
				require(group == nil, "duplicate workflow proof feature group "+featureID)
				group = &record.Groups[i]
			}
		}
		require(group != nil && group.Project == native.projectID && group.AutomaticEvaluation && len(group.Diagnostics) == 0,
			"workflow proof has no complete native group for "+featureID)
		require(group.Counts == features.CountSpecVerdicts(group.Requirements) &&
			group.Blocked == features.GroupBlocks(group.Mode, group.Requirements, group.Diagnostics),
			"workflow proof has inconsistent native group decision for "+featureID)
		var observations []features.VerificationObservation
		for _, report := range group.Reports {
			require(produced[report.Task] || report.Restored, "workflow proof report has no successful producer task: "+report.Task)
			require(strings.HasPrefix(report.Digest, "sha256:") && contractDigestPattern.MatchString(report.Digest),
				"workflow proof report has invalid digest")
			data, _ := r.workflowBytes(obj([]string{"path", "sha256"}, report.Path, strings.TrimPrefix(report.Digest, "sha256:")), "workflow producer report")
			parsed, diagnostics := features.ParseAndValidateVerificationReport(data)
			if len(diagnostics) > 0 {
				workflowNativeError("producer report", diagnostics)
			}
			for _, observation := range parsed.Observations {
				if observation.Feature != featureID {
					continue
				}
				expected, known := requirements[observation.Requirement]
				if !known {
					fail("workflow proof report has unknown requirement " + featureID + "#" + observation.Requirement)
				}
				require(slices.Contains(expected, observation.Check), "workflow proof report has unknown check "+featureID+"#"+observation.Requirement+"#"+observation.Check)
				observations = append(observations, observation)
			}
		}
		require(len(group.Requirements) == len(requirements), "workflow proof requirement inventory differs for "+featureID)
		evaluated := features.EvaluateSpecRequirements(featureID, native.spec.Requirements, native.feature.Requirements, observations, instant)
		evaluatedByID := map[string]features.SpecRequirementEvaluation{}
		for _, evaluation := range evaluated {
			evaluatedByID[evaluation.Requirement] = evaluation
		}
		for _, verdict := range group.Requirements {
			expected, known := requirements[verdict.Requirement]
			require(known, "workflow proof has unknown requirement "+featureID+"#"+verdict.Requirement)
			key := featureID + "#" + verdict.Requirement
			require(result[key].Requirement == "", "workflow proof has duplicate requirement "+key)
			actual := make([]string, 0, len(verdict.Checks))
			for _, check := range verdict.Checks {
				actual = append(actual, check.Check)
			}
			sort.Strings(actual)
			sortedExpected := slices.Clone(expected)
			sort.Strings(sortedExpected)
			require(slices.Equal(actual, sortedExpected), "workflow proof check inventory differs for "+key)
			evaluation := evaluatedByID[verdict.Requirement]
			require(verdict.State == evaluation.State, "workflow proof does not match native producer observations for "+key)
			if evaluation.Evaluation != nil {
				for _, computed := range evaluation.Evaluation.Checks {
					match := false
					for _, recorded := range verdict.Checks {
						if recorded.Check == computed.Check && recorded.State == computed.State {
							match = true
						}
					}
					require(match, "workflow proof check differs from native producer observation: "+key+"#"+computed.Check)
				}
			}
			result[key] = verdict
		}
	}
	return result
}

func (r *verifyRun) workflowRequirements(value any, source map[string]workflowSource) (map[string]workflowRequirement, int) {
	rows := workflowList(value, "workflow.requirements")
	result := map[string]workflowRequirement{}
	later := 0
	for _, item := range rows {
		row := workflowObject(item, "workflow requirement", []string{"feature", "id", "disposition"},
			[]string{"feature", "id", "disposition", "unit", "reason", "authority"})
		featureID := workflowText(row.get("feature"), "workflow requirement feature")
		id := workflowText(row.get("id"), "workflow requirement ID")
		native, sourceKnown := source[featureID]
		checks, known := native.checks[id]
		key := featureID + "#" + id
		require(sourceKnown && known, "unknown workflow requirement "+key)
		require(result[key].id == "", "duplicate workflow requirement "+key)
		disposition := workflowText(row.get("disposition"), "workflow requirement disposition")
		require(oneOf(disposition, "current", "later", "excluded"), "unknown workflow requirement disposition")
		unit := ""
		if disposition == "excluded" {
			require(!row.has("unit"), "excluded workflow requirement cannot name a unit")
			r.checkRef(row.get("reason"))
			r.checkRef(row.get("authority"))
		} else {
			unit = workflowText(row.get("unit"), "workflow requirement unit")
			require(!row.has("reason") && !row.has("authority"), "non-excluded workflow requirement has exclusion fields")
			if disposition == "later" {
				later++
			}
		}
		result[key] = workflowRequirement{feature: featureID, id: id, disposition: disposition, unit: unit, checks: checks}
	}
	count := 0
	for _, native := range source {
		count += len(native.checks)
	}
	require(len(result) == count, "workflow requirement inventory omits native spec requirements")
	return result, later
}

func (r *verifyRun) workflowMergedReceipt(unit *jsObject) bool {
	proposalRef := workflowObject(unit.get("proposal"), "workflow unit proposal", []string{"source", "id"}, []string{"source", "id"})
	workflowText(proposalRef.get("source"), "workflow proposal source")
	workflowText(proposalRef.get("id"), "workflow proposal ID")
	data, _ := r.workflowBytes(unit.get("receipt"), "workflow merged receipt")
	parsed, err := parseJSON(data)
	if err != nil {
		failErr(err)
	}
	status := parsed
	if hasOwnMember(parsed, "data") {
		status = at(parsed, "data")
	}
	proposal := at(status, "proposal")
	ref := at(proposal, "ref")
	require(strictEqual(at(ref, "source"), proposalRef.get("source")) && strictEqual(at(ref, "id"), proposalRef.get("id")),
		"workflow merged receipt belongs to another proposal")
	require(strictEqual(at(proposal, "state"), "merged"), "workflow merged receipt does not show merged state")
	head := workflowText(at(at(proposal, "change"), "headCommit"), "workflow merged head commit")
	require(baseSHAPattern.MatchString(head) && head == r.git("rev-parse", "HEAD"), "workflow merged receipt names another head")
	return true
}

func (r *verifyRun) workflowProjectTargets(value any) map[string]bool {
	items := workflowList(value, "workflow unit projects")
	require(len(items) > 0, "workflow unit has no project scope")
	projects := map[string]bool{}
	for _, item := range items {
		project := workflowObject(item, "workflow project target", []string{"id", "manifest"}, []string{"id", "manifest"})
		id := workflowText(project.get("id"), "workflow project ID")
		data, file := r.workflowBytes(project.get("manifest"), "workflow project manifest")
		workflowNativePath(file, "project")
		require(path.Base(file) == "putnami.json", "workflow project manifest must name putnami.json")
		derived := "/"
		if dir := path.Dir(file); dir != "." {
			derived = "/" + dir
		}
		require(id == derived && !projects[id], "unknown or duplicate workflow project target "+id)
		manifest, err := parseJSON(data)
		if err != nil {
			failErr(err)
		}
		workflowText(at(manifest, "name"), "workflow project manifest name")
		projects[id] = true
	}
	return projects
}

func (r *verifyRun) workflowUnits(value any, requirements map[string]workflowRequirement, verdicts map[string]features.SpecRequirementVerdict, reports map[string][]any, sources map[string]workflowSource, final bool) (int, int, map[string]bool, map[string]bool, bool, []any) {
	items := workflowList(value, "workflow.units")
	require(len(items) > 0, "workflow needs a delivery unit")
	ids := map[string]bool{}
	projectIDs := map[string]bool{}
	assigned := map[string]bool{}
	current, complete := 0, 0
	allReached := true
	statuses := make([]any, 0, len(items))
	for _, item := range items {
		unit := workflowObject(item, "workflow unit", []string{"id", "outcome", "boundary", "exclusions", "projects", "requirements", "dependsOn", "requiredStage", "stage", "nextResult", "nextCycleTasks", "decisions"},
			[]string{"id", "outcome", "boundary", "exclusions", "projects", "requirements", "dependsOn", "requiredStage", "stage", "nextResult", "nextCycleTasks", "decisions", "proposal", "receipt"})
		id := workflowText(unit.get("id"), "workflow unit ID")
		require(!ids[id], "duplicate workflow unit "+id)
		ids[id] = true
		outcome := workflowText(unit.get("outcome"), "workflow unit outcome")
		r.checkRef(unit.get("boundary"))
		for _, exclusion := range workflowList(unit.get("exclusions"), "workflow unit exclusions") {
			r.checkRef(exclusion)
		}
		projects := r.workflowProjectTargets(unit.get("projects"))
		for project := range projects {
			projectIDs[project] = true
		}
		members := workflowStrings(unit.get("requirements"), "workflow unit requirements")
		require(len(members) > 0, "workflow unit has no requirements")
		workflowStrings(unit.get("dependsOn"), "workflow unit dependencies")
		nextCycleTasks := workflowStrings(unit.get("nextCycleTasks"), "workflow unit next-cycle tasks")
		for _, decision := range workflowList(unit.get("decisions"), "workflow unit decisions") {
			r.checkRef(decision)
		}
		nextResult := workflowText(unit.get("nextResult"), "workflow unit next result")
		requiredStage := workflowText(unit.get("requiredStage"), "workflow unit required stage")
		stage := workflowText(unit.get("stage"), "workflow unit stage")
		require(oneOf(requiredStage, "candidate", "ready", "merged", "published", "deployed", "adopted") &&
			oneOf(stage, "planned", "building", "candidate", "ready", "merged", "published", "deployed", "adopted"), "unknown workflow delivery stage")
		if oneOf(stage, "ready", "merged", "published", "deployed", "adopted") {
			r.checkRef(unit.get("receipt"))
		} else {
			require(!unit.has("receipt"), "workflow unit has receipt before delivery")
		}
		reached := stage == requiredStage && requiredStage == "candidate"
		if requiredStage == "ready" && stage == "candidate" && final {
			reached = true // The enclosing verified dossier supplies this transition.
		}
		if requiredStage == "merged" && stage == "merged" {
			reached = r.workflowMergedReceipt(unit)
		}
		if !reached {
			allReached = false
		}
		unitComplete := true
		unitCurrent := false
		requirementStatuses := make([]any, 0, len(members))
		for _, key := range members {
			row, known := requirements[key]
			require(known && row.unit == id, "workflow unit has unknown or foreign requirement "+key)
			require(projects[sources[row.feature].projectID], "workflow unit omits native source project for "+key)
			require(!assigned[key], "workflow requirement assigned to multiple units "+key)
			assigned[key] = true
			verdict := verdicts[key]
			state := string(verdict.State)
			if state == "" {
				state = "pending"
			}
			checks := make([]any, 0, len(verdict.Checks))
			for _, check := range verdict.Checks {
				checks = append(checks, obj([]string{"id", "state", "path", "symbol"},
					check.Check, string(check.State), check.Path, check.Symbol))
			}
			requirementStatuses = append(requirementStatuses, obj([]string{"id", "disposition", "state", "checks", "producerReports"},
				key, row.disposition, state, checks, reports[row.feature]))
			if row.disposition == "current" {
				unitCurrent = true
				current++
				if verdicts[key].State != features.RequirementVerified {
					unitComplete = false
				} else {
					complete++
				}
			} else {
				unitComplete = false
			}
		}
		if final && unitCurrent {
			require(unitComplete, "workflow current unit lacks passing native checks: "+id)
			require(!oneOf(stage, "planned", "building"), "workflow current unit has not reached candidate: "+id)
		}
		if !unitComplete {
			require(oneOf(stage, "planned", "building"), "workflow incomplete unit claims delivery: "+id)
		}
		effective := stage
		if requiredStage == "ready" && stage == "candidate" && final && unitComplete {
			effective = "ready"
		}
		blockers := []any{}
		if !unitComplete {
			blockers = append(blockers, "native requirements pending or incomplete")
		}
		if !reached {
			if oneOf(requiredStage, "published", "deployed", "adopted") {
				blockers = append(blockers, "native receipt adapter unavailable for "+requiredStage)
			} else {
				blockers = append(blockers, "required "+requiredStage+" receipt pending")
			}
		}
		statuses = append(statuses, obj([]string{"id", "outcome", "boundary", "stage", "requiredStage", "requirementsVerified", "requiredStageReached", "requirements", "nextResult", "nextCycleTasks", "decisions", "blockers"},
			id, outcome, unit.get("boundary"), effective, requiredStage, unitComplete, reached && unitComplete,
			requirementStatuses, nextResult, nextCycleTasks, unit.get("decisions"), blockers))
	}
	for _, item := range items {
		unit, ok := item.(*jsObject)
		require(ok, "workflow unit must be an object")
		id := workflowText(unit.get("id"), "workflow unit ID")
		for _, dependency := range workflowStrings(unit.get("dependsOn"), "workflow unit dependencies") {
			require(ids[dependency] && dependency != id, "workflow unit dependency is unknown or self-referential: "+id)
		}
	}
	for key, row := range requirements {
		if row.disposition != "excluded" {
			require(assigned[key], "workflow requirement has no unit "+key)
		}
	}
	return current, complete, ids, projectIDs, allReached, statuses
}

func (r *verifyRun) workflowArtifacts(value any, units, projects map[string]bool, final bool) {
	items := workflowList(value, "workflow.artifacts")
	ids := map[string]bool{}
	for _, item := range items {
		a := workflowObject(item, "workflow artifact", []string{"id", "kind", "contractVersion", "lifecycle", "units", "scopes", "source", "context", "author", "material"},
			[]string{"id", "kind", "contractVersion", "lifecycle", "units", "scopes", "source", "context", "author", "material", "challenge"})
		id := workflowText(a.get("id"), "workflow artifact ID")
		require(!ids[id], "duplicate workflow artifact "+id)
		ids[id] = true
		require(oneOf(a.get("kind"), "spec", "design", "prototype", "adr", "review", "other"), "unknown workflow artifact kind")
		workflowText(a.get("contractVersion"), "workflow artifact contract version")
		lifecycle := workflowText(a.get("lifecycle"), "workflow artifact lifecycle")
		require(oneOf(lifecycle, "proposed", "challenged", "accepted", "superseded"), "unknown workflow artifact lifecycle")
		for _, unit := range workflowStrings(a.get("units"), "workflow artifact units") {
			require(units[unit], "workflow artifact names unknown unit "+unit)
		}
		scopes := workflowStrings(a.get("scopes"), "workflow artifact scopes")
		require(len(scopes) > 0, "workflow artifact has no scope")
		for _, scope := range scopes {
			require(projects[scope], "workflow artifact names unknown project scope "+scope)
		}
		r.checkRef(a.get("source"))
		for _, context := range workflowList(a.get("context"), "workflow artifact context") {
			r.checkRef(context)
		}
		workflowText(a.get("author"), "workflow artifact author")
		material, ok := a.get("material").(bool)
		require(ok, "workflow artifact material must be boolean")
		if !a.has("challenge") {
			require((!final && lifecycle != "accepted") || !material, "material workflow artifact lacks independent challenge: "+id)
			continue
		}
		challenge := workflowObject(a.get("challenge"), "workflow challenge", []string{"original", "resolutions"}, []string{"original", "resolutions"})
		originalBytes, _ := r.workflowBytes(challenge.get("original"), "original workflow challenge")
		original, err := parseJSON(originalBytes)
		if err != nil {
			failErr(err)
		}
		o := workflowObject(original, "original workflow challenge", []string{"version", "reviewer", "author", "artifact", "context", "report", "findings"},
			[]string{"version", "reviewer", "author", "artifact", "context", "report", "findings"})
		require(strictEqual(o.get("version"), 1.0), "unsupported original workflow challenge version")
		reviewer := workflowText(o.get("reviewer"), "original workflow challenge reviewer")
		require(reviewer != a.get("author") && strictEqual(o.get("author"), a.get("author")), "workflow artifact challenge is not independent")
		r.checkRef(o.get("report"))
		r.checkRef(o.get("artifact"))
		require(strictEqual(at(o.get("artifact"), "path"), at(a.get("source"), "path")) &&
			strictEqual(at(o.get("artifact"), "sha256"), at(a.get("source"), "sha256")), "stale workflow artifact challenge")
		var digests []string
		for _, context := range workflowList(a.get("context"), "workflow artifact context") {
			digests = append(digests, workflowText(at(context, "path"), "workflow context path")+"#"+
				workflowText(at(context, "sha256"), "workflow context digest"))
		}
		sort.Strings(digests)
		var examined []string
		for _, context := range workflowList(o.get("context"), "original workflow challenge context") {
			r.checkRef(context)
			examined = append(examined, workflowText(at(context, "path"), "original context path")+"#"+
				workflowText(at(context, "sha256"), "original context digest"))
		}
		sort.Strings(examined)
		require(slices.Equal(examined, digests), "stale workflow artifact context challenge")
		findingIDs := map[string]bool{}
		for _, item := range workflowList(o.get("findings"), "original workflow challenge findings") {
			finding := workflowObject(item, "original workflow finding", []string{"id", "scenario", "consequence", "correction"},
				[]string{"id", "scenario", "consequence", "correction"})
			fid := workflowText(finding.get("id"), "workflow finding ID")
			require(!findingIDs[fid], "duplicate workflow finding "+fid)
			findingIDs[fid] = true
			for _, field := range []string{"scenario", "consequence", "correction"} {
				workflowText(finding.get(field), "workflow finding "+field)
			}
		}
		resolved := map[string]bool{}
		for _, item := range workflowList(challenge.get("resolutions"), "workflow challenge resolutions") {
			resolution := workflowObject(item, "workflow finding resolution", []string{"id", "state", "note"},
				[]string{"id", "state", "note", "authority"})
			fid := workflowText(resolution.get("id"), "workflow resolution finding ID")
			require(findingIDs[fid] && !resolved[fid], "unknown or duplicate workflow finding resolution "+fid)
			resolved[fid] = true
			state := workflowText(resolution.get("state"), "workflow finding resolution state")
			require(oneOf(state, "fixed", "refuted", "deferred"), "unknown workflow finding resolution state")
			r.checkRef(resolution.get("note"))
			if state == "deferred" {
				r.checkRef(resolution.get("authority"))
			}
		}
		if final || lifecycle == "accepted" {
			for fid := range findingIDs {
				require(resolved[fid], "unresolved material workflow finding: "+fid)
			}
		}
	}
}

func (r *verifyRun) workflowExperiments(value any) {
	items := workflowList(value, "workflow.experiments")
	ids := map[string]bool{}
	for _, item := range items {
		e := workflowObject(item, "workflow experiment", []string{"id", "question", "effortCeiling", "inputs", "asset", "instructions", "observed", "simulated", "assumptions", "conclusion", "uncertainty"},
			[]string{"id", "question", "effortCeiling", "inputs", "asset", "instructions", "observed", "simulated", "assumptions", "conclusion", "uncertainty"})
		id := workflowText(e.get("id"), "workflow experiment ID")
		require(!ids[id], "duplicate workflow experiment "+id)
		ids[id] = true
		for _, field := range []string{"question", "effortCeiling", "conclusion", "uncertainty"} {
			workflowText(e.get(field), "workflow experiment "+field)
		}
		for _, input := range workflowList(e.get("inputs"), "workflow experiment inputs") {
			r.checkRef(input)
		}
		for _, field := range []string{"asset", "instructions", "observed"} {
			r.checkRef(e.get(field))
		}
		workflowStrings(e.get("simulated"), "workflow experiment simulated behavior")
		workflowStrings(e.get("assumptions"), "workflow experiment assumptions")
	}
}

func (r *verifyRun) workflowStages(value any, final bool) {
	items := workflowList(value, "workflow.stages")
	canonical := []string{"intent", "spec", "design", "prototype", "review", "adr", "candidate", "handoff"}
	names := map[string]bool{}
	for _, item := range items {
		s := workflowObject(item, "workflow stage", []string{"name", "state", "inputs", "expectedOutput", "exitCriteria"},
			[]string{"name", "state", "inputs", "expectedOutput", "exitCriteria", "receipt", "reason"})
		name := workflowText(s.get("name"), "workflow stage name")
		require(slices.Contains(canonical, name), "unknown workflow stage "+name)
		require(!names[name], "duplicate workflow stage "+name)
		names[name] = true
		state := workflowText(s.get("state"), "workflow stage state")
		require(oneOf(state, "pending", "blocked", "passed", "not-applicable"), "unknown workflow stage state")
		for _, input := range workflowList(s.get("inputs"), "workflow stage inputs") {
			r.checkRef(input)
		}
		workflowText(s.get("expectedOutput"), "workflow stage expected output")
		workflowText(s.get("exitCriteria"), "workflow stage exit criteria")
		if state == "passed" {
			r.checkRef(s.get("receipt"))
		} else if state == "not-applicable" || state == "blocked" {
			r.checkRef(s.get("reason"))
		} else {
			require(!s.has("receipt"), "pending workflow stage has a receipt")
		}
		if final && name == "candidate" {
			require(state == "passed", "workflow candidate stage incomplete")
		}
	}
	require(len(names) == len(canonical), "workflow stage inventory is incomplete")
}

func (r *verifyRun) verifyWorkflow(value any, final bool) *jsObject {
	w := workflowObject(value, "workflow", []string{"version", "outcome", "mandate", "sources", "requirements", "units", "artifacts", "experiments", "stages", "handoff"},
		[]string{"version", "outcome", "mandate", "sources", "requirements", "units", "artifacts", "experiments", "stages", "handoff", "proof"})
	require(strictEqual(w.get("version"), 1.0), "unsupported local workflow version; expected v1")
	workflowText(w.get("outcome"), "workflow outcome")
	r.checkRef(w.get("mandate"))
	sources := r.workflowSources(w.get("sources"))
	rows, later := r.workflowRequirements(w.get("requirements"), sources)
	var proof *features.SpecVerificationRecord
	var session any
	proofTree := ""
	if w.has("proof") {
		proofTree = r.fingerprint()
		proof, session = r.workflowProof(w.get("proof"), proofTree)
	}
	verdicts := r.workflowVerdicts(proof, session, sources)
	reports := map[string][]any{}
	if proof != nil {
		for _, group := range proof.Groups {
			if _, known := sources[group.Feature]; !known {
				continue
			}
			for _, report := range group.Reports {
				reports[group.Feature] = append(reports[group.Feature], obj([]string{"task", "project", "path", "digest"},
					report.Task, report.Project, report.Path, report.Digest))
			}
		}
	}
	current, complete, units, projects, reached, statuses := r.workflowUnits(w.get("units"), rows, verdicts, reports, sources, final)
	r.workflowArtifacts(w.get("artifacts"), units, projects, final)
	r.workflowExperiments(w.get("experiments"))
	r.workflowStages(w.get("stages"), final)
	handoff := workflowObject(w.get("handoff"), "workflow handoff", []string{"mandate", "unit", "scopes", "receiver", "responsibility", "inputs", "expectedOutput", "acceptance", "nextAction", "unresolvedFindings"},
		[]string{"mandate", "unit", "scopes", "receiver", "responsibility", "inputs", "expectedOutput", "acceptance", "nextAction", "unresolvedFindings"})
	r.checkRef(handoff.get("mandate"))
	require(strictEqual(at(handoff.get("mandate"), "path"), at(w.get("mandate"), "path")) &&
		strictEqual(at(handoff.get("mandate"), "sha256"), at(w.get("mandate"), "sha256")), "workflow handoff changes mandate")
	require(units[workflowText(handoff.get("unit"), "workflow handoff unit")], "workflow handoff names unknown unit")
	handoffScopes := workflowStrings(handoff.get("scopes"), "workflow handoff scopes")
	require(len(handoffScopes) > 0, "workflow handoff has no scope")
	for _, scope := range handoffScopes {
		require(projects[scope], "workflow handoff names unknown project scope "+scope)
	}
	for _, field := range []string{"receiver", "responsibility", "expectedOutput", "acceptance", "nextAction"} {
		workflowText(handoff.get(field), "workflow handoff "+field)
	}
	for _, input := range workflowList(handoff.get("inputs"), "workflow handoff inputs") {
		r.checkRef(input)
	}
	workflowStrings(handoff.get("unresolvedFindings"), "workflow handoff unresolved findings")
	if final {
		require(current > 0 && proof != nil, "workflow ready needs current requirements and native producer proof")
		require(current == complete, "workflow current requirements are incomplete")
	}
	if proofTree != "" {
		require(r.fingerprint() == proofTree, "tree changed during workflow verification")
	}
	return obj([]string{"verdict", "stage", "outcome", "currentRequirements", "verifiedRequirements", "laterRequirements", "outcomeComplete", "units", "nextAction"},
		"workflow valid", "progress", w.get("outcome"), float64(current), float64(complete), float64(later),
		later == 0 && current > 0 && current == complete && reached, statuses, handoff.get("nextAction"))
}
