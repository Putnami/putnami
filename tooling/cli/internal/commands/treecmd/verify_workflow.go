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

	collab "go.putnami.dev/protocol/collaboration"
	diag "go.putnami.dev/protocol/diagnostic"
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
	require(!diag.HasErrors(record.Diagnostics), "workflow proof has unresolved native collection errors")
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
		require(group != nil && group.Project == native.projectID && group.AutomaticEvaluation && !diag.HasErrors(group.Diagnostics),
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

func workflowNativeEnvelope(data []byte, contract, operation string) any {
	envelope, diagnostics := collab.ParseEnvelope(data)
	if len(diagnostics) > 0 {
		workflowNativeError("collaboration envelope", diagnostics)
	}
	require(envelope.Contract == contract && envelope.Operation == operation && envelope.Outcome == collab.OutcomeOK,
		"workflow receipt is not a successful native "+contract+"."+operation+" result")
	result, diagnostics := collab.ParseResult(contract, envelope.Version, operation, envelope.Result)
	if len(diagnostics) > 0 {
		workflowNativeError("collaboration result", diagnostics)
	}
	return result
}

func workflowProviderRef(value any, name string) collab.Ref {
	ref := workflowObject(value, name, []string{"source", "id"}, []string{"source", "id"})
	native := collab.Ref{Source: workflowText(ref.get("source"), name+" source"), ID: workflowText(ref.get("id"), name+" ID")}
	if diagnostics := collab.ValidateInput(&collab.TaskRefInput{Ref: native}); len(diagnostics) > 0 {
		workflowNativeError("task reference", diagnostics)
	}
	return native
}

func (r *verifyRun) workflowNextCycleTasks(value any) []any {
	items := workflowList(value, "workflow unit next-cycle tasks")
	seen := map[string]bool{}
	for _, item := range items {
		task := workflowObject(item, "workflow next-cycle task", []string{"kind"}, []string{"kind", "ref", "revision", "receipt", "record"})
		switch workflowText(task.get("kind"), "workflow task kind") {
		case "provider":
			require(!task.has("record"), "provider workflow task cannot have local record")
			ref := workflowProviderRef(task.get("ref"), "workflow task ref")
			key := "provider\x00" + ref.Source + "\x00" + ref.ID
			require(!seen[key], "duplicate workflow next-cycle task")
			seen[key] = true
			require(task.has("revision") == task.has("receipt"), "workflow provider task revision and receipt must be paired")
			if task.has("receipt") {
				revision := workflowText(task.get("revision"), "workflow provider task revision")
				data, _ := r.workflowBytes(task.get("receipt"), "workflow task receipt")
				result, ok := workflowNativeEnvelope(data, "tasks", "get").(*collab.TaskResult)
				require(ok && result.Task.Ref == ref && result.Task.Revision == revision,
					"workflow task receipt does not match native ref and revision")
			}
		case "local":
			require(!task.has("ref") && !task.has("revision") && !task.has("receipt"), "local workflow task has provider fields")
			key := "local\x00" + workflowRefKey(task.get("record"), "local workflow task record")
			require(!seen[key], "duplicate workflow next-cycle task")
			seen[key] = true
			data, _ := r.workflowBytes(task.get("record"), "local workflow task record")
			parsed, err := parseJSON(data)
			if err != nil {
				failErr(err)
			}
			local := workflowObject(parsed, "local workflow task record", []string{"version", "id", "inputs", "nextResult"},
				[]string{"version", "id", "inputs", "nextResult"})
			require(strictEqual(local.get("version"), 1.0), "unsupported local workflow task version")
			workflowText(local.get("id"), "local workflow task ID")
			workflowText(local.get("nextResult"), "local workflow task next result")
			for _, input := range workflowList(local.get("inputs"), "local workflow task inputs") {
				r.checkRef(input)
			}
		default:
			fail("unknown workflow task kind")
		}
	}
	return items
}

func (r *verifyRun) workflowMergedReceipt(unit *jsObject) bool {
	proposalRef := workflowProviderRef(unit.get("proposal"), "workflow unit proposal")
	data, _ := r.workflowBytes(unit.get("receipt"), "workflow merged receipt")
	status, ok := workflowNativeEnvelope(data, "proposals", "status").(*collab.ProposalStatusResult)
	require(ok, "workflow merged receipt has wrong native result")
	proposal := status.Proposal
	require(proposal.Ref == proposalRef,
		"workflow merged receipt belongs to another proposal")
	require(proposal.State == collab.ProposalStateMerged, "workflow merged receipt does not show merged state")
	head := workflowText(proposal.Change.HeadCommit, "workflow merged head commit")
	require(baseSHAPattern.MatchString(head) && head == r.git("rev-parse", "HEAD"), "workflow merged receipt names another head")
	return true
}

// A ready receipt is the unchanged output of an earlier native dossier
// verification. It may be used to resume a unit after that dossier has been
// verified, without treating the unit's authored stage as proof of readiness.
func (r *verifyRun) workflowReadyReceipt(unit *jsObject, expectedBaseSHA string) bool {
	data, _ := r.workflowBytes(unit.get("receipt"), "workflow ready receipt")
	parsed, err := parseJSON(data)
	if err != nil {
		failErr(err)
	}
	receipt := workflowObject(parsed, "workflow ready receipt",
		[]string{"verdict", "stage", "assurance", "binding", "gateSession", "review", "limits"},
		[]string{"verdict", "stage", "assurance", "binding", "gateSession", "review", "limits", "workflow"})
	require(strictEqual(receipt.get("verdict"), "Putnami verified") && strictEqual(receipt.get("stage"), "ready") &&
		strictEqual(receipt.get("assurance"), "local-declared-scope") && strictEqual(receipt.get("limits"), verifiedLimits),
		"workflow ready receipt is not a native ready verdict")
	workflowText(receipt.get("gateSession"), "workflow ready gate session")
	r.checkRef(receipt.get("review"))
	binding := workflowObject(receipt.get("binding"), "workflow ready binding",
		[]string{"fingerprint", "headSHA", "baseSHA"}, []string{"fingerprint", "headSHA", "baseSHA"})
	require(strictEqual(binding.get("fingerprint"), r.fingerprint()) &&
		strictEqual(binding.get("headSHA"), r.git("rev-parse", "HEAD")), "stale workflow ready receipt tree")
	base := workflowText(binding.get("baseSHA"), "workflow ready base")
	require(baseSHAPattern.MatchString(base), "workflow ready receipt has invalid base")
	resolved, resolveErr := capture("git", gitCommand(r.root, "rev-parse", "--verify", base+"^{commit}"))
	require(resolveErr == nil && jsTrim(resolved) == base, "workflow ready receipt has invalid base")
	ancestor, ancestorErr := capture("git", gitCommand(r.root, "merge-base", base, "HEAD"))
	require(ancestorErr == nil && jsTrim(ancestor) == base, "workflow ready receipt has invalid base")
	if expectedBaseSHA != "" {
		require(base == expectedBaseSHA, "workflow ready receipt names another dossier base")
	}
	if receipt.has("workflow") {
		projection := workflowObject(receipt.get("workflow"), "workflow ready projection",
			[]string{"verdict", "stage", "outcome", "currentRequirements", "verifiedRequirements", "laterRequirements", "outcomeComplete", "units", "nativeDiagnostics", "nextAction"},
			[]string{"verdict", "stage", "outcome", "currentRequirements", "verifiedRequirements", "laterRequirements", "outcomeComplete", "units", "nativeDiagnostics", "nextAction"})
		require(strictEqual(projection.get("verdict"), "workflow valid") && strictEqual(projection.get("stage"), "progress"),
			"workflow ready receipt has invalid workflow projection")
	}
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

func (r *verifyRun) workflowDeliveryReceipt(unit *jsObject, final bool, expectedBaseSHA string) (string, string, bool) {
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
	if stage == "ready" {
		ready := r.workflowReadyReceipt(unit, expectedBaseSHA)
		reached = requiredStage == "ready" && ready
	}
	if stage == "merged" {
		merged := r.workflowMergedReceipt(unit)
		reached = requiredStage == "merged" && merged
	}
	return requiredStage, stage, reached
}

func workflowDependencyReadiness(ids, localReady, currentUnits map[string]bool, dependencies map[string][]string, unitStatus map[string]*jsObject, final bool) bool {
	for id, deps := range dependencies {
		for _, dependency := range deps {
			require(ids[dependency] && dependency != id, "workflow unit dependency is unknown or self-referential: "+id)
		}
	}
	visiting := map[string]bool{}
	visited := map[string]bool{}
	derivedReady := map[string]bool{}
	var visit func(string) bool
	visit = func(id string) bool {
		require(!visiting[id], "workflow unit dependency cycle at "+id)
		if visited[id] {
			return derivedReady[id]
		}
		visiting[id] = true
		ready := localReady[id]
		for _, dependency := range dependencies[id] {
			if !visit(dependency) {
				ready = false
				blockers := workflowList(unitStatus[id].get("blockers"), "workflow unit blockers")
				unitStatus[id].set("blockers", append(blockers, "dependency "+dependency+" is incomplete"))
			}
		}
		visiting[id] = false
		visited[id] = true
		derivedReady[id] = ready
		unitStatus[id].set("deliveryReady", ready)
		return ready
	}
	allReached := true
	for id := range ids {
		if !visit(id) {
			allReached = false
			if final && currentUnits[id] && localReady[id] {
				fail("workflow current unit has incomplete dependency: " + id)
			}
		}
	}
	return allReached
}

func workflowUnitWarnings(members []string, verdicts map[string]features.SpecRequirementVerdict, warnings map[string][]diag.Diagnostic) ([]any, []any) {
	diagnostics := []any{}
	blockers := []any{}
	seen := map[string]bool{}
	for _, key := range members {
		if verdicts[key].State == features.RequirementVerified {
			continue
		}
		feature, _, _ := strings.Cut(key, "#")
		for _, finding := range warnings[feature] {
			identity := finding.Code + "\x00" + finding.Field
			if seen[identity] {
				continue
			}
			seen[identity] = true
			diagnostics = append(diagnostics, obj([]string{"severity", "code", "message", "field"},
				string(finding.Severity), finding.Code, finding.Message, finding.Field))
			blockers = append(blockers, "native "+string(finding.Severity)+": "+finding.Code)
		}
	}
	return diagnostics, blockers
}

func workflowUnitRequirementStatuses(id string, members []string, requirements map[string]workflowRequirement, verdicts map[string]features.SpecRequirementVerdict, reports map[string][]any, sources map[string]workflowSource, projects, assigned map[string]bool) ([]any, int, int, bool, bool) {
	statuses := make([]any, 0, len(members))
	current, verified := 0, 0
	complete, unitCurrent := true, false
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
		statuses = append(statuses, obj([]string{"id", "disposition", "state", "checks", "producerReports"},
			key, row.disposition, state, checks, reports[row.feature]))
		if row.disposition == "current" {
			unitCurrent = true
			current++
			if verdict.State == features.RequirementVerified {
				verified++
			} else {
				complete = false
			}
		} else {
			complete = false
		}
	}
	return statuses, current, verified, complete, unitCurrent
}

func (r *verifyRun) workflowUnits(value any, requirements map[string]workflowRequirement, verdicts map[string]features.SpecRequirementVerdict, reports map[string][]any, warnings map[string][]diag.Diagnostic, sources map[string]workflowSource, stageBlockers map[string][]any, final bool, expectedBaseSHA string) (int, int, map[string]bool, map[string]bool, bool, []any) {
	items := workflowList(value, "workflow.units")
	require(len(items) > 0, "workflow needs a delivery unit")
	ids := map[string]bool{}
	projectIDs := map[string]bool{}
	assigned := map[string]bool{}
	localReady := map[string]bool{}
	currentUnits := map[string]bool{}
	dependencies := map[string][]string{}
	unitStatus := map[string]*jsObject{}
	current, complete := 0, 0
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
		dependencies[id] = workflowStrings(unit.get("dependsOn"), "workflow unit dependencies")
		nextCycleTasks := r.workflowNextCycleTasks(unit.get("nextCycleTasks"))
		decisions := workflowList(unit.get("decisions"), "workflow unit decisions")
		for _, decision := range decisions {
			r.checkRef(decision)
		}
		stages, hasStages := stageBlockers[id]
		require(hasStages, "workflow stage inventory missing unit "+id)
		nextResult := workflowText(unit.get("nextResult"), "workflow unit next result")
		requiredStage, stage, reached := r.workflowDeliveryReceipt(unit, final, expectedBaseSHA)
		requirementStatuses, unitRequirements, unitVerified, unitComplete, unitCurrent :=
			workflowUnitRequirementStatuses(id, members, requirements, verdicts, reports, sources, projects, assigned)
		current += unitRequirements
		complete += unitVerified
		if final && unitCurrent {
			require(unitComplete, "workflow current unit lacks passing native checks: "+id)
			require(!oneOf(stage, "planned", "building"), "workflow current unit has not reached candidate: "+id)
			if len(stages) > 0 {
				fail(workflowText(stages[0], "workflow stage blocker"))
			}
			require(len(decisions) == 0, "workflow current unit has unresolved decisions: "+id)
		}
		currentUnits[id] = unitCurrent
		localReady[id] = unitComplete && reached && len(decisions) == 0 && len(stages) == 0
		if !unitComplete {
			require(oneOf(stage, "planned", "building"), "workflow incomplete unit claims delivery: "+id)
		}
		effective := stage
		if requiredStage == "ready" && stage == "candidate" && final && unitComplete {
			effective = "ready"
		}
		nativeDiagnostics, nativeBlockers := workflowUnitWarnings(members, verdicts, warnings)
		blockers := append(slices.Clone(stages), nativeBlockers...)
		if len(decisions) > 0 {
			blockers = append(blockers, "unresolved unit decisions")
		}
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
		status := obj([]string{"id", "outcome", "boundary", "stage", "requiredStage", "requirementsVerified", "requiredStageReached", "requirements", "nativeDiagnostics", "nextResult", "nextCycleTasks", "decisions", "blockers"},
			id, outcome, unit.get("boundary"), effective, requiredStage, unitComplete, reached && unitComplete,
			requirementStatuses, nativeDiagnostics, nextResult, nextCycleTasks, unit.get("decisions"), blockers)
		unitStatus[id] = status
		statuses = append(statuses, status)
	}
	allReached := workflowDependencyReadiness(ids, localReady, currentUnits, dependencies, unitStatus, final)
	for stageUnit := range stageBlockers {
		require(ids[stageUnit], "workflow stage names unknown unit "+stageUnit)
	}
	for key, row := range requirements {
		if row.disposition != "excluded" {
			require(assigned[key], "workflow requirement has no unit "+key)
		}
	}
	return current, complete, ids, projectIDs, allReached, statuses
}

func workflowRefKey(value any, name string) string {
	ref := workflowObject(value, name, []string{"path", "sha256"}, []string{"path", "sha256"})
	return workflowText(ref.get("path"), name+" path") + "\x00" + workflowText(ref.get("sha256"), name+" digest")
}

func (r *verifyRun) workflowHistoricalRef(value any, snapshots map[string]*jsObject, used map[string]bool) {
	key := workflowRefKey(value, "historical workflow reference")
	if copy, archived := snapshots[key]; archived {
		r.checkRef(copy)
		used[key] = true
		return
	}
	r.checkRef(value)
}

type workflowChallengeOpinion struct {
	artifact string
	context  []string
	findings []string
}

func (r *verifyRun) workflowChallengeReport(reference any, author string, snapshots map[string]*jsObject, used map[string]bool, historical bool) workflowChallengeOpinion {
	data, _ := r.workflowBytes(reference, "workflow challenge review")
	parsed, err := parseJSON(data)
	if err != nil {
		failErr(err)
	}
	opinion := workflowObject(parsed, "workflow challenge review", []string{"version", "reviewer", "author", "artifact", "context", "report", "findings"},
		[]string{"version", "reviewer", "author", "artifact", "context", "report", "findings"})
	require(strictEqual(opinion.get("version"), 1.0), "unsupported original workflow challenge version")
	reviewer := workflowText(opinion.get("reviewer"), "workflow challenge reviewer")
	require(reviewer != author && strictEqual(opinion.get("author"), author), "workflow artifact challenge is not independent")
	r.checkRef(opinion.get("report"))
	if historical {
		r.workflowHistoricalRef(opinion.get("artifact"), snapshots, used)
	} else {
		r.checkRef(opinion.get("artifact"))
	}
	result := workflowChallengeOpinion{artifact: workflowRefKey(opinion.get("artifact"), "workflow challenge artifact")}
	for _, context := range workflowList(opinion.get("context"), "workflow challenge context") {
		if historical {
			r.workflowHistoricalRef(context, snapshots, used)
		} else {
			r.checkRef(context)
		}
		result.context = append(result.context, workflowRefKey(context, "workflow challenge context ref"))
	}
	sort.Strings(result.context)
	for _, item := range workflowList(opinion.get("findings"), "workflow challenge findings") {
		finding := workflowObject(item, "workflow finding", []string{"id", "scenario", "consequence", "correction"},
			[]string{"id", "scenario", "consequence", "correction"})
		result.findings = append(result.findings, workflowText(finding.get("id"), "workflow finding ID"))
		for _, field := range []string{"scenario", "consequence", "correction"} {
			workflowText(finding.get(field), "workflow finding "+field)
		}
	}
	return result
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
		author := workflowText(a.get("author"), "workflow artifact author")
		material, ok := a.get("material").(bool)
		require(ok, "workflow artifact material must be boolean")
		if !a.has("challenge") {
			require((!final && lifecycle != "accepted") || !material, "material workflow artifact lacks independent challenge: "+id)
			continue
		}
		challenge := workflowObject(a.get("challenge"), "workflow challenge", []string{"original", "resolutions"},
			[]string{"original", "revisions", "snapshots", "resolutions"})
		snapshots := map[string]*jsObject{}
		usedSnapshots := map[string]bool{}
		if challenge.has("snapshots") {
			for _, item := range workflowList(challenge.get("snapshots"), "workflow challenge snapshots") {
				snapshot := workflowObject(item, "workflow challenge snapshot", []string{"original", "copy"}, []string{"original", "copy"})
				key := workflowRefKey(snapshot.get("original"), "workflow snapshot original")
				require(snapshots[key] == nil, "duplicate workflow challenge snapshot")
				copy := workflowObject(snapshot.get("copy"), "workflow snapshot copy", []string{"path", "sha256"}, []string{"path", "sha256"})
				copyKey := workflowRefKey(copy, "workflow snapshot copy")
				require(copyKey != key && strictEqual(at(copy, "sha256"), at(snapshot.get("original"), "sha256")),
					"workflow snapshot does not preserve original bytes at a distinct path")
				r.checkRef(copy)
				snapshots[key] = copy
			}
		}
		reviews := []any{challenge.get("original")}
		if challenge.has("revisions") {
			reviews = append(reviews, workflowList(challenge.get("revisions"), "workflow challenge revisions")...)
		}
		require(len(reviews) <= 32, "workflow challenge has too many revisions")
		findingIndex := map[string]int{}
		seenReviews := map[string]bool{}
		var opinions []workflowChallengeOpinion
		for i, review := range reviews {
			reviewKey := workflowRefKey(review, "workflow challenge revision")
			require(!seenReviews[reviewKey], "duplicate workflow challenge revision")
			seenReviews[reviewKey] = true
			opinion := r.workflowChallengeReport(review, author, snapshots, usedSnapshots, i < len(reviews)-1)
			for _, fid := range opinion.findings {
				require(findingIndex[fid] == 0, "duplicate workflow finding "+fid)
				findingIndex[fid] = i + 1
			}
			opinions = append(opinions, opinion)
		}
		for key := range snapshots {
			require(usedSnapshots[key], "workflow challenge snapshot has no examined original ref")
		}
		latest := opinions[len(opinions)-1]
		require(latest.artifact == workflowRefKey(a.get("source"), "workflow current artifact"), "stale workflow artifact challenge")
		var currentContext []string
		for _, context := range workflowList(a.get("context"), "workflow artifact context") {
			currentContext = append(currentContext, workflowRefKey(context, "workflow current context"))
		}
		sort.Strings(currentContext)
		require(slices.Equal(latest.context, currentContext), "stale workflow artifact context challenge")
		resolved := map[string]bool{}
		for _, item := range workflowList(challenge.get("resolutions"), "workflow challenge resolutions") {
			resolution := workflowObject(item, "workflow finding resolution", []string{"id", "state", "note"},
				[]string{"id", "state", "note", "authority"})
			fid := workflowText(resolution.get("id"), "workflow resolution finding ID")
			require(findingIndex[fid] > 0 && !resolved[fid], "unknown or duplicate workflow finding resolution "+fid)
			resolved[fid] = true
			state := workflowText(resolution.get("state"), "workflow finding resolution state")
			require(oneOf(state, "fixed", "refuted", "deferred"), "unknown workflow finding resolution state")
			r.checkRef(resolution.get("note"))
			if state == "deferred" {
				r.checkRef(resolution.get("authority"))
			} else if state == "fixed" {
				require(findingIndex[fid] < len(reviews) && opinions[findingIndex[fid]-1].artifact != latest.artifact,
					"fixed workflow finding lacks later review of corrected artifact: "+fid)
			}
		}
		if final || lifecycle == "accepted" {
			for fid := range findingIndex {
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

func (r *verifyRun) workflowStages(value any) map[string][]any {
	items := workflowList(value, "workflow.stages")
	canonical := []string{"intent", "spec", "design", "prototype", "review", "adr", "candidate", "handoff"}
	names := map[string]map[string]bool{}
	blockers := map[string][]any{}
	for _, item := range items {
		s := workflowObject(item, "workflow stage", []string{"unit", "name", "state", "inputs", "expectedOutput", "exitCriteria"},
			[]string{"unit", "name", "state", "inputs", "expectedOutput", "exitCriteria", "receipt", "reason"})
		unit := workflowText(s.get("unit"), "workflow stage unit")
		if names[unit] == nil {
			names[unit] = map[string]bool{}
			blockers[unit] = []any{}
		}
		name := workflowText(s.get("name"), "workflow stage name")
		require(slices.Contains(canonical, name), "unknown workflow stage "+name)
		require(!names[unit][name], "duplicate workflow stage "+unit+"#"+name)
		names[unit][name] = true
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
		if state == "pending" || state == "blocked" || (name == "candidate" && state != "passed") {
			message := "workflow " + name + " stage incomplete for " + unit
			if name == "candidate" {
				message = "workflow candidate stage incomplete for " + unit
			}
			blockers[unit] = append(blockers[unit], message)
		}
	}
	for unit, stages := range names {
		require(len(stages) == len(canonical), "workflow stage inventory is incomplete for "+unit)
	}
	return blockers
}

func (r *verifyRun) verifyWorkflow(value any, final bool, expectedBaseSHA string) *jsObject {
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
	warnings := map[string][]diag.Diagnostic{}
	globalDiagnostics := []any{}
	if proof != nil {
		for _, finding := range proof.Diagnostics {
			globalDiagnostics = append(globalDiagnostics, obj([]string{"severity", "code", "message", "field"},
				string(finding.Severity), finding.Code, finding.Message, finding.Field))
		}
		for _, group := range proof.Groups {
			if _, known := sources[group.Feature]; !known {
				continue
			}
			warnings[group.Feature] = append(warnings[group.Feature], group.Diagnostics...)
			for _, report := range group.Reports {
				reports[group.Feature] = append(reports[group.Feature], obj([]string{"task", "project", "path", "digest"},
					report.Task, report.Project, report.Path, report.Digest))
			}
		}
	}
	stageBlockers := r.workflowStages(w.get("stages"))
	current, complete, units, projects, reached, statuses := r.workflowUnits(w.get("units"), rows, verdicts, reports, warnings, sources, stageBlockers, final, expectedBaseSHA)
	r.workflowArtifacts(w.get("artifacts"), units, projects, final)
	r.workflowExperiments(w.get("experiments"))
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
	available := map[string]bool{}
	for _, input := range workflowList(handoff.get("inputs"), "workflow handoff inputs") {
		available[workflowRefKey(input, "workflow handoff input")] = true
	}
	for _, item := range workflowList(w.get("units"), "workflow.units") {
		unit := workflowObject(item, "workflow unit", []string{"id"}, []string{"id", "outcome", "boundary", "exclusions", "projects", "requirements", "dependsOn", "requiredStage", "stage", "nextResult", "nextCycleTasks", "decisions", "proposal", "receipt"})
		if unit.get("id") != handoff.get("unit") {
			continue
		}
		for _, item := range workflowList(unit.get("nextCycleTasks"), "workflow unit next-cycle tasks") {
			task := workflowObject(item, "workflow next-cycle task", []string{"kind"}, []string{"kind", "ref", "revision", "receipt", "record"})
			if task.has("receipt") {
				require(available[workflowRefKey(task.get("receipt"), "workflow task receipt")], "workflow handoff omits exact task receipt")
			}
			if task.get("kind") == "local" {
				require(available[workflowRefKey(task.get("record"), "local workflow task record")], "workflow handoff omits local task record")
			}
		}
	}
	workflowStrings(handoff.get("unresolvedFindings"), "workflow handoff unresolved findings")
	if final {
		require(current > 0 && proof != nil, "workflow ready needs current requirements and native producer proof")
		require(current == complete, "workflow current requirements are incomplete")
	}
	if proofTree != "" {
		require(r.fingerprint() == proofTree, "tree changed during workflow verification")
	}
	return obj([]string{"verdict", "stage", "outcome", "currentRequirements", "verifiedRequirements", "laterRequirements", "outcomeComplete", "units", "nativeDiagnostics", "nextAction"},
		"workflow valid", "progress", w.get("outcome"), float64(current), float64(complete), float64(later),
		later == 0 && current > 0 && current == complete && reached, statuses, globalDiagnostics, handoff.get("nextAction"))
}
