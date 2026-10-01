package cli

import "strconv"

// The v2 document rules.
//
// Structural shape lives in the field lists below — one per schema $def, so the
// drift test can pin struct tags, validator members and schemas/result-v2.json
// against each other and none of the three can move alone. The cross-field
// rules underneath are the part JSON Schema cannot express, and they are the
// settled decisions this slice exists to fix (doc/02-result-v2.md).

var (
	projectIdentityFields = []field{
		req("id", nonEmptyString),
		req("name", nonEmptyString),
	}

	taskRefFields = []field{
		req("name", nonEmptyString),
		req("command", nonEmptyString),
		opt("step", nonEmptyString),
		req("kind", nonEmptyString),
	}

	providerIdentityFields = []field{
		req("extension", nonEmptyString),
		opt("version", nonEmptyString),
	}

	taskIdentityFields = []field{
		req("key", nonEmptyString),
		req("scope", enumOf(TaskScopeProject, TaskScopeWorkspace)),
		req("project", projectIdentity),
		req("task", taskRef),
		req("provider", providerIdentity),
	}

	resultErrorFields = []field{
		req("code", enumOf("usage", "auth", "api", "signal", "failure")),
		req("message", nonEmptyString),
		opt("next", nonEmptyString),
	}

	diagnosticFields = []field{
		req("severity", enumOf("error", "warning", "info")),
		req("message", nonEmptyString),
		opt("code", nonEmptyString),
		opt("file", nonEmptyString),
		opt("line", intAtLeast(1)),
		opt("column", intAtLeast(1)),
	}

	executionRecordFields = []field{
		req("id", nonEmptyString),
		req("wallMs", intAtLeast(0)),
		req("userCpuMs", intAtLeast(0)),
		req("systemCpuMs", intAtLeast(0)),
		opt("maxRssBytes", intAtLeast(0)),
		opt("ioInBlocks", intAtLeast(0)),
		opt("ioOutBlocks", intAtLeast(0)),
		opt("concurrency", intAtLeast(1)),
		req("tasks", intAtLeast(0)),
	}

	taskRecordFields = []field{
		req("identity", identity),
		opt("executionId", nonEmptyString),
		opt("inputDigest", prefixedSha256Digest),
		req("status", enumOf(TaskStatusSuccess, TaskStatusFailed, TaskStatusCanceled, TaskStatusSkipped)),
		req("reuse", enumOf(TaskReuseNone, TaskReuseLocalCache, TaskReuseRemoteCache, TaskReuseCoalesced)),
		req("exitCode", intAtLeast(0)),
		req("durationMs", intAtLeast(0)),
		opt("taskWallMs", intAtLeast(0)),
		opt("spawnToFirstEventMs", intAtLeast(0)),
		opt("error", resultError),
		opt("diagnostics", arrayOf(diagnostic)),
		opt("testCasesDropped", intAtLeast(1)),
	}

	testCaseFields = []field{
		req("name", nonEmptyStringBytesAtMost(TestCaseMaxTextBytes)),
		req("suite", nonEmptyStringBytesAtMost(TestCaseMaxTextBytes)),
		req("status", enumOf(TestCaseStatusPassed, TestCaseStatusFailed, TestCaseStatusSkipped)),
		req("durationMs", intAtLeast(0)),
		opt("output", nonEmptyStringBytesAtMost(TestCaseMaxOutputBytes)),
		opt("outputTruncated", boolean),
		opt("file", nonEmptyStringBytesAtMost(TestCaseMaxTextBytes)),
		opt("line", intAtLeast(1)),
	}

	taskFailureFields = []field{
		req("identity", identity),
		req("error", resultError),
		opt("diagnostics", arrayOf(diagnostic)),
	}

	runCountsFields = []field{
		req("total", intAtLeast(0)),
		req("succeeded", intAtLeast(0)),
		req("failed", intAtLeast(0)),
		req("canceled", intAtLeast(0)),
		req("skipped", intAtLeast(0)),
	}

	runReuseFields = []field{
		req("localCache", intAtLeast(0)),
		req("remoteCache", intAtLeast(0)),
		req("coalesced", intAtLeast(0)),
	}

	dockerPublishTimingsFields = []field{
		req("buildMs", intAtLeast(0)),
		req("cacheLookupMs", intAtLeast(0)),
		req("cacheTransferMs", intAtLeast(0)),
		req("registryPushMs", intAtLeast(0)),
		req("referencePublishMs", intAtLeast(0)),
		req("digestResolveMs", intAtLeast(0)),
	}

	dockerPublishConcurrencyFields = []field{
		req("configuredCap", intAtLeast(1)),
		req("effective", intAtLeast(0)),
	}

	dockerPublicationFields = []field{
		req("identity", identity),
		req("session", nonEmptyString),
		req("registry", nonEmptyString),
		req("image", nonEmptyString),
		req("immutableRef", nonEmptyString),
		opt("tags", arrayOf(nonEmptyString)),
		req("imageDigest", immutableSHA256),
		req("contentStatus", enumOf("pushed", "retagged", "reused")),
		req("cacheOutcome", enumOf("hit", "miss")),
		req("digestReused", boolean),
		req("timings", dockerPublishTimings),
		req("concurrency", dockerPublishConcurrency),
	}

	runCPUFields = []field{
		req("allocatedMillicores", intAtLeast(1)),
		req("allocatedSource", enumOf(CPUAllocationCgroupQuota, CPUAllocationLogicalCPUs)),
		req("allocatedMs", intAtLeast(0)),
		req("actualMs", intAtLeast(0)),
		req("executions", intAtLeast(1)),
	}

	runLocalCacheFields = []field{
		req("hits", intAtLeast(0)),
		req("misses", intAtLeast(0)),
		req("servedMs", intAtLeast(0)),
		req("keysMs", intAtLeast(0)),
		req("bindingsMs", intAtLeast(0)),
		req("restoreVerifyMs", intAtLeast(0)),
		req("spawnedProcesses", intAtLeast(0)),
	}

	runCacheFields = []field{
		opt("local", runLocalCache),
	}

	runSummaryFields = []field{
		req("outcome", enumOf(RunOutcomeSuccess, RunOutcomeFailure, RunOutcomeAborted)),
		opt("abortedBy", enumOf(AbortedByUser, AbortedBySignal)),
		req("exitCode", intAtLeast(0)),
		req("counts", runCounts),
		req("reuse", runReuse),
		req("durationMs", intAtLeast(0)),
		opt("failures", arrayOf(taskFailure)),
		opt("publications", arrayOf(dockerPublication)),
		opt("cpu", runCPU),
		opt("cache", runCache),
	}

	streamRunSummaryFields = []field{
		req("outcome", enumOf(RunOutcomeSuccess, RunOutcomeFailure, RunOutcomeAborted)),
		opt("abortedBy", enumOf(AbortedByUser, AbortedBySignal)),
		req("exitCode", intAtLeast(0)),
		req("counts", runCounts),
		req("reuse", runReuse),
		req("durationMs", intAtLeast(0)),
		opt("cpu", runCPU),
	}

	planMetricsFields = []field{
		req("tasks", intAtLeast(0)),
		req("edges", intAtLeast(0)),
		req("projects", intAtLeast(0)),
		opt("byCommand", intMapValues),
	}

	plannedTaskFields = []field{
		req("identity", identity),
		opt("dependsOn", arrayOf(nonEmptyString)),
		opt("after", arrayOf(nonEmptyString)),
		req("cache", boolean),
	}

	planSummaryFields = []field{
		req("dryRun", boolean),
		req("metrics", planMetrics),
		req("tasks", arrayOf(plannedTask)),
	}

	sessionGitFields = []field{
		opt("branch", nonEmptyString),
		opt("baseline", nonEmptyString),
	}

	// Every member is required: the object is written only when the fingerprint
	// was computed, and that computation decides all three at once. A partial
	// tree block would be a claim nobody measured.
	sessionTreeFields = []field{
		req("fingerprint", sha256Digest),
		req("dirty", boolean),
		req("headSHA", gitObjectID),
	}

	sessionPlacementFields = []field{
		req("requested", enumOf("local", "remote")),
		req("actual", enumOf("local", "remote")),
		opt("provenance", sessionProvenance),
	}

	// Every member is required: the executing engine states all three from the
	// one bound request it was handed, so a partial block would be a claim
	// nobody executed.
	sessionProvenanceFields = []field{
		req("sourceDigest", prefixedSha256Digest),
		req("inputDigest", prefixedSha256Digest),
		req("submission", hexOfLength(32)),
	}

	sessionSelectionFields = []field{
		req("mode", enumOf(SessionSelectionModes...)),
		req("scoped", boolean),
		opt("projects", arrayOf(nonEmptyString)),
		opt("releaseSetProjects", arrayOf(nonEmptyString)),
	}

	cgroupThrottleFields = []field{
		req("periods", intAtLeast(0)),
		req("throttledPeriods", intAtLeast(0)),
		req("throttledUs", intAtLeast(0)),
	}

	cgroupCPUFields = []field{
		req("periodUs", intAtLeast(1)),
		opt("quotaUs", intAtLeast(1)),
		opt("usageUs", intAtLeast(0)),
		opt("throttle", cgroupThrottle),
	}

	cpuPressureFields = []field{
		req("someStalledUs", intAtLeast(0)),
	}

	hostCPUTimeFields = []field{
		req("stealTicks", intAtLeast(0)),
		req("ioWaitTicks", intAtLeast(0)),
		req("totalTicks", intAtLeast(0)),
	}

	cgroupMemoryLimitFields = []field{
		req("bytes", intAtLeast(1)),
		req("source", enumOf(CgroupMemoryV1, CgroupMemoryV2)),
	}

	memoryCapacityFields = []field{
		opt("physicalBytes", intAtLeast(1)),
		opt("cgroupLimit", cgroupMemoryLimit),
		req("effectiveBytes", intAtLeast(1)),
		req("effectiveSource", enumOf(MemoryCapacityPhysical, MemoryCapacityCgroupLimit)),
	}

	cgroupMemoryCompositionFields = []field{
		req("anonBytes", intAtLeast(0)),
		req("fileBytes", intAtLeast(0)),
		req("shmemBytes", intAtLeast(0)),
	}

	cgroupMemoryLifetimePeakFields = []field{
		req("bytes", intAtLeast(0)),
	}

	cgroupMemoryClosingFields = []field{
		req("currentBytes", intAtLeast(0)),
		opt("composition", cgroupMemoryComposition),
		opt("lifetimePeak", cgroupMemoryLifetimePeak),
	}

	cgroupMemoryEventsFields = []field{
		req("low", intAtLeast(0)),
		req("high", intAtLeast(0)),
		req("max", intAtLeast(0)),
		req("oom", intAtLeast(0)),
		req("oomKill", intAtLeast(0)),
	}

	cgroupMemoryFields = []field{
		req("source", enumOf(CgroupMemoryV1, CgroupMemoryV2)),
		req("closing", cgroupMemoryClosing),
		opt("events", cgroupMemoryEvents),
	}

	memoryPressureFields = []field{
		req("scope", enumOf(MemoryPressureHost, MemoryPressureCgroup)),
		req("someStalledUs", intAtLeast(0)),
		req("fullStalledUs", intAtLeast(0)),
	}

	sessionEnvironmentFields = []field{
		req("os", nonEmptyString),
		req("arch", nonEmptyString),
		req("logicalCpus", intAtLeast(1)),
		opt("cpuModel", nonEmptyString),
		req("windowMs", intAtLeast(0)),
		opt("cgroupCpu", cgroupCPU),
		opt("cpuPressure", cpuPressure),
		opt("hostCpu", hostCPUTime),
		opt("memoryCapacity", memoryCapacity),
		opt("cgroupMemory", cgroupMemory),
		opt("memoryPressure", memoryPressure),
	}

	preparationPhaseRecordFields = []field{
		req("phase", enumOf(
			PreparationPhaseNetwork,
			PreparationPhaseResolution,
			PreparationPhaseVerification,
			PreparationPhaseGeneration,
			PreparationPhaseMutation,
		)),
		req("wallMs", intAtLeast(0)),
		req("steps", intAtLeast(1)),
		opt("cpuMs", intAtLeast(0)),
	}

	sessionPreparationFields = []field{
		req("wallMs", intAtLeast(0)),
		req("parallelism", intAtLeast(1)),
		req("phases", arrayOf(preparationPhaseRecord)),
	}

	machineOutputBudgetFields = []field{
		req("maxBytes", intAtLeast(1)),
		req("maxRecords", intAtLeast(1)),
		req("failureReserveBytes", intAtLeast(1)),
		req("failureReserveRecords", intAtLeast(1)),
		req("finalReserveBytes", intAtLeast(1)),
		req("finalReserveRecords", intAtLeast(1)),
	}

	machineOutputElisionFields = []field{
		req("records", intAtLeast(0)),
		req("bytes", intAtLeast(0)),
	}

	machineOutputElisionsFields = []field{
		req("ordinary", machineOutputElision),
		req("failure", machineOutputElision),
	}

	machineOutputArtifactFields = []field{
		req("sessionId", nonEmptyStringBytesAtMost(64)),
		req("path", enumOf(MachineOutputArtifactPath)),
		req("retention", enumOf(MachineOutputArtifactRetentionSession)),
	}

	machineOutputSummaryFields = []field{
		req("mode", enumOf(MachineOutputModeNormal, MachineOutputModeVerbose)),
		req("sanitization", enumOf(MachineOutputSanitizationV1)),
		req("budget", machineOutputBudget),
		req("elided", machineOutputElisions),
		req("artifact", machineOutputArtifact),
	}

	resultEnvelopeFields = []field{
		req("protocolVersion", protocolVersion),
		req("command", nonEmptyString),
		req("status", enumOf(StatusSuccess, StatusFailure, StatusAborted)),
		req("exitCode", intAtLeast(0)),
		opt("data", anyValue),
		opt("run", runSummary),
		opt("plan", planSummary),
		opt("error", resultError),
	}

	sessionStreamRecordFields = []field{
		req("protocolVersion", protocolVersion),
		req("record", enumOf(RecordTaskStart, RecordTaskEvent, RecordTaskEnd, RecordTestCase, RecordPlanEnd, RecordSessionEnd)),
		req("time", nonEmptyString),
		opt("identity", identity),
		opt("event", openObject),
		opt("task", taskRecord),
		opt("testCase", testCase),
		opt("run", anyValue),
		opt("plan", planSummary),
		opt("machineOutput", machineOutputSummary),
	}

	boundedSessionEndRecordFields = []field{
		req("protocolVersion", protocolVersion),
		req("record", enumOf(RecordSessionEnd)),
		req("time", nonEmptyStringBytesAtMost(64)),
		req("run", streamRunSummary),
		req("machineOutput", machineOutputSummary),
	}

	mcpResultFields = []field{
		req("protocolVersion", protocolVersion),
		req("tool", enumOf(MCPToolRunJobs, MCPToolPlanJobs)),
		req("commands", arrayOf(nonEmptyString)),
		opt("run", runSummary),
		opt("plan", planSummary),
	}

	sessionFileFields = []field{
		req("protocolVersion", protocolVersion),
		req("sessionId", nonEmptyString),
		opt("parentSessionId", nonEmptyString),
		req("startTime", nonEmptyString),
		opt("endTime", nonEmptyString),
		req("commands", arrayOf(nonEmptyString)),
		opt("selection", sessionSelection),
		opt("git", sessionGit),
		opt("tree", sessionTree),
		opt("placement", sessionPlacement),
		req("run", runSummary),
		opt("tasks", arrayOf(taskRecord)),
		opt("executions", arrayOf(executionRecord)),
		opt("environment", sessionEnvironment),
		opt("preparation", sessionPreparation),
		opt("scheduler", anyValue),
		opt("cache", anyValue),
	}

	sessionPlanFileFields = []field{
		req("protocolVersion", protocolVersion),
		req("sessionId", nonEmptyString),
		req("commands", arrayOf(nonEmptyString)),
		req("tasks", arrayOf(plannedTask)),
	}

	reportGitFields = []field{
		opt("branch", nonEmptyString),
		req("sha", gitObjectID),
		opt("dirty", boolean),
		opt("baseline", nonEmptyString),
	}

	reportRunFields = []field{
		req("outcome", enumOf(RunOutcomeSuccess, RunOutcomeFailure, RunOutcomeAborted)),
		req("exitCode", intAtLeast(0)),
		req("counts", runCounts),
		req("reuse", runReuse),
		req("durationMs", intAtLeast(0)),
		opt("cpu", runCPU),
	}

	reportTestsFields = []field{
		req("total", intAtLeast(0)),
		req("passed", intAtLeast(0)),
		req("failed", intAtLeast(0)),
		req("skipped", intAtLeast(0)),
		opt("failureDetailsTruncated", intAtLeast(0)),
	}

	reportCoverageFields = []field{
		req("percentage", percentage),
		req("granularity", enumOf(CoverageStatements, CoverageLines, CoverageFunctions, CoverageBranches)),
		opt("covered", intAtLeast(0)),
		opt("total", intAtLeast(0)),
		req("enforced", boolean),
	}

	reportCommandFields = []field{
		req("command", nonEmptyString),
		req("counts", runCounts),
		req("reuse", runReuse),
		req("freshWallMs", intAtLeast(0)),
		opt("cpuMs", intAtLeast(0)),
		opt("tests", reportTests),
		opt("coverage", reportCoverage),
		req("errors", intAtLeast(0)),
		req("warnings", intAtLeast(0)),
	}

	reportJobFields = []field{
		req("key", nonEmptyString),
		req("project", nonEmptyString),
		req("task", nonEmptyString),
		req("command", nonEmptyString),
		req("outcome", enumOf(TaskStatusSuccess, TaskStatusFailed, TaskStatusCanceled, TaskStatusSkipped)),
		req("reuse", enumOf(TaskReuseNone, TaskReuseLocalCache, TaskReuseRemoteCache, TaskReuseCoalesced)),
		req("durationMs", intAtLeast(0)),
		opt("cpuMs", intAtLeast(0)),
		opt("coverage", reportCoverage),
		opt("diagnostics", reportDiagnostics),
		opt("failureDetailsTruncated", intAtLeast(1)),
		opt("truncatedCount", intAtLeast(1)),
	}

	reportCacheFields = []field{
		req("hits", intAtLeast(0)),
		req("misses", intAtLeast(0)),
		req("restored", intAtLeast(0)),
		req("uploads", intAtLeast(0)),
		req("timeSavedMs", intAtLeast(0)),
		req("bytesFetched", intAtLeast(0)),
		req("bytesUploaded", intAtLeast(0)),
	}

	reportSchedulerFields = []field{
		req("parallelism", intAtLeast(1)),
		req("criticalPathMs", intAtLeast(0)),
	}

	reportFileFields = []field{
		req("protocolVersion", protocolVersion),
		req("sessionId", nonEmptyString),
		req("startTime", nonEmptyString),
		req("endTime", nonEmptyString),
		req("origin", enumOf(ReportOriginCLI, ReportOriginMCP)),
		req("enforceCoverage", boolean),
		opt("git", reportGit),
		req("run", reportRun),
		req("commands", arrayOf(reportCommand)),
		req("jobs", reportJobs),
		req("elidedJobs", intAtLeast(0)),
		opt("cache", reportCache),
		opt("scheduler", reportScheduler),
	}
)

// v2FieldSets maps every schema $def that describes an object onto the member
// list the validator enforces for it. The drift test walks this map, so a $def
// added to the schema without a validator (or the reverse) fails the build's
// tests instead of shipping a member nothing checks.
var v2FieldSets = map[string][]field{
	"projectIdentity":          projectIdentityFields,
	"taskRef":                  taskRefFields,
	"providerIdentity":         providerIdentityFields,
	"taskIdentity":             taskIdentityFields,
	"resultError":              resultErrorFields,
	"diagnostic":               diagnosticFields,
	"executionRecord":          executionRecordFields,
	"taskRecord":               taskRecordFields,
	"testCase":                 testCaseFields,
	"taskFailure":              taskFailureFields,
	"runCounts":                runCountsFields,
	"runReuse":                 runReuseFields,
	"dockerPublishTimings":     dockerPublishTimingsFields,
	"dockerPublishConcurrency": dockerPublishConcurrencyFields,
	"dockerPublication":        dockerPublicationFields,
	"runCpu":                   runCPUFields,
	"runLocalCache":            runLocalCacheFields,
	"runCache":                 runCacheFields,
	"runSummary":               runSummaryFields,
	"streamRunSummary":         streamRunSummaryFields,
	"cgroupThrottle":           cgroupThrottleFields,
	"cgroupCpu":                cgroupCPUFields,
	"cpuPressure":              cpuPressureFields,
	"hostCpuTime":              hostCPUTimeFields,
	"cgroupMemoryLimit":        cgroupMemoryLimitFields,
	"memoryCapacity":           memoryCapacityFields,
	"cgroupMemoryComposition":  cgroupMemoryCompositionFields,
	"cgroupMemoryLifetimePeak": cgroupMemoryLifetimePeakFields,
	"cgroupMemoryClosing":      cgroupMemoryClosingFields,
	"cgroupMemoryEvents":       cgroupMemoryEventsFields,
	"cgroupMemory":             cgroupMemoryFields,
	"memoryPressure":           memoryPressureFields,
	"sessionTree":              sessionTreeFields,
	"sessionPlacement":         sessionPlacementFields,
	"sessionProvenance":        sessionProvenanceFields,
	"sessionEnvironment":       sessionEnvironmentFields,
	"preparationPhaseRecord":   preparationPhaseRecordFields,
	"sessionPreparation":       sessionPreparationFields,
	"machineOutputBudget":      machineOutputBudgetFields,
	"machineOutputElision":     machineOutputElisionFields,
	"machineOutputElisions":    machineOutputElisionsFields,
	"machineOutputArtifact":    machineOutputArtifactFields,
	"machineOutputSummary":     machineOutputSummaryFields,
	"planMetrics":              planMetricsFields,
	"plannedTask":              plannedTaskFields,
	"planSummary":              planSummaryFields,
	"resultEnvelope":           resultEnvelopeFields,
	"sessionStreamRecord":      sessionStreamRecordFields,
	"boundedSessionEndRecord":  boundedSessionEndRecordFields,
	"mcpResult":                mcpResultFields,
	"sessionFile":              sessionFileFields,
	"sessionPlanFile":          sessionPlanFileFields,
	"reportGit":                reportGitFields,
	"reportRun":                reportRunFields,
	"reportTests":              reportTestsFields,
	"reportCoverage":           reportCoverageFields,
	"reportCommand":            reportCommandFields,
	"reportJob":                reportJobFields,
	"reportCache":              reportCacheFields,
	"reportScheduler":          reportSchedulerFields,
	"reportFile":               reportFileFields,
}

func projectIdentity(v *validator, path string, value any) {
	object(v, path, value, projectIdentityFields)
}

func taskRef(v *validator, path string, value any) {
	object(v, path, value, taskRefFields)
}

func providerIdentity(v *validator, path string, value any) {
	object(v, path, value, providerIdentityFields)
}

// identity validates a typed task identity and enforces that key stays a
// DERIVED view of the structured fields. B2a keeps that derivation
// byte-identical to the v1 plan key; a key that disagrees is a violation, not
// an alternative spelling.
func identity(v *validator, path string, value any) {
	obj := object(v, path, value, taskIdentityFields)
	key, hasKey := childString(obj, "key")
	projectID, hasProject := childString(childObject(obj, "project"), "id")
	taskName, hasTask := childString(childObject(obj, "task"), "name")
	if !hasKey || !hasProject || !hasTask {
		return
	}
	if key != projectID+":"+taskName {
		v.add(ViolationInvalidKey, join(path, "key"))
	}
}

func resultError(v *validator, path string, value any) {
	object(v, path, value, resultErrorFields)
}

func diagnostic(v *validator, path string, value any) {
	object(v, path, value, diagnosticFields)
}

func executionRecord(v *validator, path string, value any) {
	object(v, path, value, executionRecordFields)
}

// taskRecord validates one task's terminal result. Status and reuse are
// orthogonal: reuse never rewrites the verdict, so a reused failure stays
// "failed" and keeps its error.
func taskRecord(v *validator, path string, value any) {
	obj := object(v, path, value, taskRecordFields)
	status, ok := childString(obj, "status")
	if ok && status == TaskStatusSuccess {
		if _, present := childValue(obj, "error"); present {
			v.add(ViolationErrorMismatch, join(path, "error"))
		}
	}
	// A skipped task never looked its key up, so a digest beside it names inputs
	// nothing was keyed on — and a reader grouping records by digest would count
	// a task that never ran as one more observation of that key.
	if ok && status == TaskStatusSkipped {
		if _, present := childValue(obj, "inputDigest"); present {
			v.add(ViolationInvalidValue, join(path, "inputDigest"))
		}
	}
}

// testCase validates the payload of a test:case record. Output belongs to a
// case that did not pass, outputTruncated describes an output that is present,
// and line locates a case inside a file that is named. A member outside those
// pairings is forbidden rather than ignored, so a reader never has to guess
// what a lone flag refers to.
func testCase(v *validator, path string, value any) {
	obj := object(v, path, value, testCaseFields)
	_, hasOutput := childValue(obj, "output")
	if status, ok := childString(obj, "status"); ok && status == TestCaseStatusPassed && hasOutput {
		v.add(ViolationUnexpectedField, join(path, "output"))
	}
	if _, present := childValue(obj, "outputTruncated"); present && !hasOutput {
		v.add(ViolationUnexpectedField, join(path, "outputTruncated"))
	}
	if _, present := childValue(obj, "line"); present {
		if _, hasFile := childValue(obj, "file"); !hasFile {
			v.add(ViolationUnexpectedField, join(path, "line"))
		}
	}
}

func taskFailure(v *validator, path string, value any) {
	object(v, path, value, taskFailureFields)
}

func runCounts(v *validator, path string, value any) {
	object(v, path, value, runCountsFields)
}

func runReuse(v *validator, path string, value any) {
	object(v, path, value, runReuseFields)
}

func dockerPublishTimings(v *validator, path string, value any) {
	object(v, path, value, dockerPublishTimingsFields)
}

func dockerPublishConcurrency(v *validator, path string, value any) {
	obj := object(v, path, value, dockerPublishConcurrencyFields)
	cap, hasCap := childInt(obj, "configuredCap")
	effective, hasEffective := childInt(obj, "effective")
	if hasCap && hasEffective && effective > cap {
		v.add(ViolationInvalidValue, join(path, "effective"))
	}
}

func dockerPublication(v *validator, path string, value any) {
	obj := object(v, path, value, dockerPublicationFields)
	image, hasImage := childString(obj, "image")
	immutableRef, hasImmutableRef := childString(obj, "immutableRef")
	digest, hasDigest := childString(obj, "imageDigest")
	status, hasStatus := childString(obj, "contentStatus")
	cacheOutcome, hasCacheOutcome := childString(obj, "cacheOutcome")
	digestReused, hasDigestReused := childBool(obj, "digestReused")
	if hasImage && hasImmutableRef && hasDigest && isImmutableSHA256(digest) && immutableRef != image+"@"+digest {
		v.add(ViolationInvalidValue, join(path, "immutableRef"))
	}
	if hasStatus && hasCacheOutcome && hasDigestReused {
		valid := cacheOutcome == "miss" && status == "pushed" && !digestReused ||
			cacheOutcome == "hit" && (status == "retagged" || status == "reused") && digestReused
		if !valid {
			v.add(ViolationInvalidValue, join(path, "contentStatus"))
		}
	}
}

func runCPU(v *validator, path string, value any) {
	object(v, path, value, runCPUFields)
}

func runLocalCache(v *validator, path string, value any) {
	object(v, path, value, runLocalCacheFields)
}

func runCache(v *validator, path string, value any) {
	object(v, path, value, runCacheFields)
}

// runSummary validates the canonical run verdict, including the two decisions
// B1a implements: unified strict success and abort-over-failure precedence.
func runSummary(v *validator, path string, value any) {
	obj := object(v, path, value, runSummaryFields)
	checkRunArithmetic(v, path, obj)
	checkRunVerdict(v, path, obj)
	checkRunCPUBudget(v, path, obj)
	checkRunCache(v, path, obj)
}

func checkRunCache(v *validator, path string, obj map[string]any) {
	local := childObject(childObject(obj, "cache"), "local")
	if local == nil {
		return
	}
	reuseLocal, hasReuseLocal := childInt(childObject(obj, "reuse"), "localCache")
	hits, hasHits := childInt(local, "hits")
	if hasReuseLocal && hasHits && reuseLocal != hits {
		v.add(ViolationCountMismatch, join(path, "cache.local.hits"))
	}
	served, hasServed := childInt(local, "servedMs")
	duration, hasDuration := childInt(obj, "durationMs")
	if hasServed && hasDuration && served > duration {
		v.add(ViolationCountMismatch, join(path, "cache.local.servedMs"))
	}
	var phaseTotal int64
	hasAllPhases := true
	for _, member := range []string{"keysMs", "bindingsMs", "restoreVerifyMs"} {
		phase, ok := childInt(local, member)
		if !ok {
			hasAllPhases = false
			continue
		}
		phaseTotal += phase
	}
	// Producers partition the exact served timeline, then truncate each of its
	// three phase totals independently to milliseconds. Consequently their sum
	// cannot exceed servedMs and can trail it by at most two milliseconds.
	if hasServed && hasAllPhases &&
		(phaseTotal > served || served-phaseTotal > 2) {
		v.add(ViolationCountMismatch, join(path, "cache.local.servedMs"))
	}
}

// checkRunCPUBudget enforces the one arithmetic tie the CPU balance carries:
// allocatedMs is allocatedMillicores applied over the SAME durationMs this
// summary states, so the two cannot be published against different walls.
//
// The relation is stated as truncating integer division, which is exactly how a
// producer computes it; a reader that recomputes gets the same number.
// actualMs is deliberately unconstrained against allocatedMs: a run CAN exceed
// its allocation for a while — a cgroup quota is enforced per period, not per
// run — and clamping the measurement to the budget would hide precisely the
// oversubscription this block exists to show.
func checkRunCPUBudget(v *validator, path string, obj map[string]any) {
	cpu := childObject(obj, "cpu")
	if cpu == nil {
		return
	}
	durationMs, hasDuration := childInt(obj, "durationMs")
	millicores, hasMillicores := childInt(cpu, "allocatedMillicores")
	allocatedMs, hasAllocated := childInt(cpu, "allocatedMs")
	if !hasDuration || !hasMillicores || !hasAllocated {
		return
	}
	if allocatedMs != durationMs*millicores/1000 {
		v.add(ViolationCountMismatch, join(path, "cpu.allocatedMs"))
	}
}

// immutableSHA256 accepts the OCI digest form downstream deployers can pin.
// It deliberately rejects image tags and other algorithms: a terminal
// publication fact must never let a mutable reference masquerade as provenance.
func immutableSHA256(v *validator, path string, value any) {
	digest, ok := value.(string)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	if !isImmutableSHA256(digest) {
		v.add(ViolationInvalidValue, path)
	}
}

func isImmutableSHA256(digest string) bool {
	const prefix = "sha256:"
	if len(digest) != len(prefix)+64 || digest[:len(prefix)] != prefix {
		return false
	}
	for _, b := range digest[len(prefix):] {
		if !(b >= '0' && b <= '9') && !(b >= 'a' && b <= 'f') {
			return false
		}
	}
	return true
}

// checkRunArithmetic enforces that the two histograms describe the same task
// set: counts spans every selected task and must add up, and reuse — the
// provenance histogram over those same tasks — can never exceed it. Keeping
// reuse OUT of the verdict histogram is what removes v1's strict/lenient fork.
func checkRunArithmetic(v *validator, path string, obj map[string]any) {
	counts := childObject(obj, "counts")
	total, hasTotal := childInt(counts, "total")
	succeeded, hasSucceeded := childInt(counts, "succeeded")
	failed, hasFailed := childInt(counts, "failed")
	canceled, hasCanceled := childInt(counts, "canceled")
	skipped, hasSkipped := childInt(counts, "skipped")
	complete := hasTotal && hasSucceeded && hasFailed && hasCanceled && hasSkipped
	if complete && total != succeeded+failed+canceled+skipped {
		v.add(ViolationCountMismatch, join(path, "counts.total"))
	}

	reuse := childObject(obj, "reuse")
	local, hasLocal := childInt(reuse, "localCache")
	remote, hasRemote := childInt(reuse, "remoteCache")
	coalesced, hasCoalesced := childInt(reuse, "coalesced")
	if hasTotal && hasLocal && hasRemote && hasCoalesced && local+remote+coalesced > total {
		v.add(ViolationCountMismatch, join(path, "reuse"))
	}

	// A listed failure set must be complete: a reused failure that the run
	// counted cannot then be left out of the list an agent reads.
	if failures, ok := childArray(obj, "failures"); ok && hasFailed && int64(len(failures)) != failed {
		v.add(ViolationCountMismatch, join(path, "failures"))
	}
}

// checkRunVerdict enforces the settled verdict rules:
//
//   - UNIFIED SUCCESS (strict): "success" requires counts.failed == 0 over
//     every selected task, reuse included. There is no lenient variant.
//   - PRECEDENCE: aborted > failure > success. An aborted run reports
//     "aborted" even when tasks also failed.
//   - EXIT-CODE AGREEMENT: 0 for success, 130 for aborted, any other non-zero
//     code for failure (so a forwarded workload code still fits).
func checkRunVerdict(v *validator, path string, obj map[string]any) {
	outcome, hasOutcome := childString(obj, "outcome")
	if !hasOutcome {
		return
	}
	_, hasAbortedBy := childValue(obj, "abortedBy")
	failed, hasFailed := childInt(childObject(obj, "counts"), "failed")

	switch {
	case outcome == RunOutcomeAborted && !hasAbortedBy:
		v.add(ViolationMissingField, join(path, "abortedBy"))
	case outcome != RunOutcomeAborted && hasAbortedBy:
		// An abort source without the aborted outcome is exactly the v1 JSON
		// envelope's ordering, where an interrupted run that also had failures
		// was reported as a plain failure.
		v.add(ViolationOutcomeMismatch, join(path, "outcome"))
	case outcome == RunOutcomeSuccess && hasFailed && failed > 0:
		v.add(ViolationOutcomeMismatch, join(path, "outcome"))
	case outcome == RunOutcomeFailure && hasFailed && failed == 0:
		v.add(ViolationOutcomeMismatch, join(path, "outcome"))
	}
	checkExitCodeForOutcome(v, join(path, "exitCode"), outcome, obj)
}

// checkExitCodeForOutcome pins the exit code an outcome must carry. It is the
// same derivation on every surface, which is what makes "the envelope and the
// process agree" checkable rather than aspirational.
func checkExitCodeForOutcome(v *validator, path, outcome string, obj map[string]any) {
	code, ok := childInt(obj, "exitCode")
	if !ok {
		return
	}
	switch outcome {
	case RunOutcomeSuccess:
		if code != ExitSuccess {
			v.add(ViolationExitCodeMismatch, path)
		}
	case RunOutcomeAborted:
		if code != ExitSignal {
			v.add(ViolationExitCodeMismatch, path)
		}
	case RunOutcomeFailure:
		if code == ExitSuccess || code == ExitSignal {
			v.add(ViolationExitCodeMismatch, path)
		}
	}
}

func planMetrics(v *validator, path string, value any) {
	object(v, path, value, planMetricsFields)
}

func plannedTask(v *validator, path string, value any) {
	object(v, path, value, plannedTaskFields)
}

func planSummary(v *validator, path string, value any) {
	obj := object(v, path, value, planSummaryFields)
	metrics := childObject(obj, "metrics")
	taskCount, hasTaskCount := childInt(metrics, "tasks")
	tasks, hasTasks := childArray(obj, "tasks")
	if hasTaskCount && hasTasks && taskCount != int64(len(tasks)) {
		v.add(ViolationCountMismatch, join(path, "metrics.tasks"))
	}
}

func sessionGit(v *validator, path string, value any) {
	object(v, path, value, sessionGitFields)
}

func sessionTree(v *validator, path string, value any) {
	object(v, path, value, sessionTreeFields)
}

// sessionPlacement applies the one semantic rule beside the member set:
// provenance is the executing engine's statement about a bound request, and a
// bound request executes only as the remote placement, so a block beside a
// local execution is a claim nothing executed.
func sessionPlacement(v *validator, path string, value any) {
	obj := object(v, path, value, sessionPlacementFields)
	if _, present := obj["provenance"]; present {
		if actual, _ := obj["actual"].(string); actual != "remote" {
			v.add(ViolationInvalidValue, join(path, "provenance"))
		}
	}
}

func sessionProvenance(v *validator, path string, value any) {
	object(v, path, value, sessionProvenanceFields)
}

func sessionSelection(v *validator, path string, value any) {
	object(v, path, value, sessionSelectionFields)
}

func cgroupThrottle(v *validator, path string, value any) {
	object(v, path, value, cgroupThrottleFields)
}

func cgroupCPU(v *validator, path string, value any) {
	object(v, path, value, cgroupCPUFields)
}

func cpuPressure(v *validator, path string, value any) {
	object(v, path, value, cpuPressureFields)
}

func hostCPUTime(v *validator, path string, value any) {
	obj := object(v, path, value, hostCPUTimeFields)
	checkHostCPUShares(v, path, obj)
}

func cgroupMemoryLimit(v *validator, path string, value any) {
	object(v, path, value, cgroupMemoryLimitFields)
}

func memoryCapacity(v *validator, path string, value any) {
	obj := object(v, path, value, memoryCapacityFields)
	physical, hasPhysical := childInt(obj, "physicalBytes")
	limit := childObject(obj, "cgroupLimit")
	cgroup, hasCgroup := childInt(limit, "bytes")
	effective, hasEffective := childInt(obj, "effectiveBytes")
	source, hasSource := childString(obj, "effectiveSource")

	// Effective capacity is a projection of a measured stable bound, never an
	// independent estimate. With both inputs it is their minimum.
	if !hasPhysical && !hasCgroup {
		v.add(ViolationInvalidValue, join(path, "effectiveSource"))
		return
	}
	if hasEffective {
		want := physical
		if !hasPhysical || (hasCgroup && cgroup < physical) {
			want = cgroup
		}
		if effective != want {
			v.add(ViolationCountMismatch, join(path, "effectiveBytes"))
		}
	}
	if !hasSource || !hasEffective {
		return
	}
	switch source {
	case MemoryCapacityPhysical:
		if !hasPhysical || effective != physical {
			v.add(ViolationCountMismatch, join(path, "effectiveSource"))
		}
	case MemoryCapacityCgroupLimit:
		if !hasCgroup || effective != cgroup {
			v.add(ViolationCountMismatch, join(path, "effectiveSource"))
		}
	}
}

func cgroupMemoryComposition(v *validator, path string, value any) {
	obj := object(v, path, value, cgroupMemoryCompositionFields)
	file, hasFile := childInt(obj, "fileBytes")
	shmem, hasShmem := childInt(obj, "shmemBytes")
	if hasFile && hasShmem && shmem > file {
		v.add(ViolationCountMismatch, join(path, "shmemBytes"))
	}
}

func cgroupMemoryLifetimePeak(v *validator, path string, value any) {
	object(v, path, value, cgroupMemoryLifetimePeakFields)
}

func cgroupMemoryClosing(v *validator, path string, value any) {
	object(v, path, value, cgroupMemoryClosingFields)
}

func cgroupMemoryEvents(v *validator, path string, value any) {
	object(v, path, value, cgroupMemoryEventsFields)
}

func cgroupMemory(v *validator, path string, value any) {
	obj := object(v, path, value, cgroupMemoryFields)
	if source, ok := childString(obj, "source"); ok && source == CgroupMemoryV1 {
		if _, present := childValue(obj, "events"); present {
			v.add(ViolationUnexpectedField, join(path, "events"))
		}
		if closing := childObject(obj, "closing"); closing != nil {
			if _, present := childValue(closing, "composition"); present {
				v.add(ViolationUnexpectedField, join(path, "closing.composition"))
			}
		}
	}
}

func memoryPressure(v *validator, path string, value any) {
	obj := object(v, path, value, memoryPressureFields)
	some, hasSome := childInt(obj, "someStalledUs")
	full, hasFull := childInt(obj, "fullStalledUs")
	if hasSome && hasFull && full > some {
		v.add(ViolationCountMismatch, join(path, "fullStalledUs"))
	}
}

// checkHostCPUShares keeps the /proc/stat window internally consistent: steal
// and iowait are COLUMNS of the same total, so neither can exceed it. A
// producer that mixed two sample windows, or subtracted them in the wrong
// order, would land here rather than publish a steal share above 100%.
func checkHostCPUShares(v *validator, path string, obj map[string]any) {
	total, hasTotal := childInt(obj, "totalTicks")
	if !hasTotal {
		return
	}
	for _, name := range []string{"ioWaitTicks", "stealTicks"} {
		if ticks, ok := childInt(obj, name); ok && ticks > total {
			v.add(ViolationCountMismatch, join(path, name))
		}
	}
}

func sessionEnvironment(v *validator, path string, value any) {
	obj := object(v, path, value, sessionEnvironmentFields)
	cgroup := childObject(obj, "cgroupMemory")
	cgroupSource, hasCgroupSource := childString(cgroup, "source")
	if pressure := childObject(obj, "memoryPressure"); pressure != nil {
		if scope, ok := childString(pressure, "scope"); ok && scope == MemoryPressureCgroup &&
			(!hasCgroupSource || cgroupSource != CgroupMemoryV2) {
			v.add(ViolationInvalidValue, join(path, "memoryPressure.scope"))
		}
	}
	if limit := childObject(childObject(obj, "memoryCapacity"), "cgroupLimit"); limit != nil {
		if limitSource, ok := childString(limit, "source"); ok && hasCgroupSource && limitSource != cgroupSource {
			v.add(ViolationCountMismatch, join(path, "cgroupMemory.source"))
		}
	}
}

func preparationPhaseRecord(v *validator, path string, value any) {
	object(v, path, value, preparationPhaseRecordFields)
}

func sessionPreparation(v *validator, path string, value any) {
	obj := object(v, path, value, sessionPreparationFields)
	checkPreparationPhases(v, path, obj)
}

// checkPreparationPhases keeps the decomposition from becoming a list of
// unrelated numbers. Two rules, both locally checkable:
//
//   - A phase appears AT MOST ONCE. The block is a HISTOGRAM over ownership
//     classes, so a repeated class means the producer emitted per-step rows
//     under a summary's contract, and every ratio a reader computes from it
//     would be wrong in a way nothing else in the document reveals. That is the
//     same failure the run histograms report, so it carries the same code.
//   - At parallelism 1 the phase walls cannot overlap, so they must fit inside
//     the stage's own wall. Above 1 they may overlap and the check is skipped —
//     which is precisely why parallelism is a required member rather than a
//     nice-to-have: without it, a reader cannot tell an overlapping sum from a
//     broken one. (Millisecond truncation is safe here: each phase truncates
//     down, and the sum of floors never exceeds the floor of the sum.)
func checkPreparationPhases(v *validator, path string, obj map[string]any) {
	phases, ok := childArray(obj, "phases")
	if !ok {
		return
	}
	seen := make(map[string]bool, len(phases))
	var summed int64
	for i, entry := range phases {
		record, isObject := entry.(map[string]any)
		if !isObject {
			continue
		}
		if name, has := childString(record, "phase"); has {
			if seen[name] {
				v.add(ViolationCountMismatch, join(path, "phases["+strconv.Itoa(i)+"].phase"))
			}
			seen[name] = true
		}
		if wall, has := childInt(record, "wallMs"); has {
			summed += wall
		}
	}
	parallelism, hasParallelism := childInt(obj, "parallelism")
	stageWall, hasWall := childInt(obj, "wallMs")
	if hasParallelism && parallelism == 1 && hasWall && summed > stageWall {
		v.add(ViolationCountMismatch, join(path, "wallMs"))
	}
}

func validateResultEnvelope(v *validator, root map[string]any) {
	obj := object(v, "", root, resultEnvelopeFields)
	checkEnvelopeVerdict(v, obj)
	checkEnvelopeRunAgreement(v, obj)
	if _, hasPlan := childValue(obj, "plan"); hasPlan {
		for _, name := range []string{"data", "run", "error"} {
			if _, present := childValue(obj, name); present {
				v.add(ViolationUnexpectedField, name)
			}
		}
		if status, ok := childString(obj, "status"); ok && status != StatusSuccess {
			v.add(ViolationInvalidValue, "status")
		}
		if exitCode, ok := childInt(obj, "exitCode"); ok && exitCode != ExitSuccess {
			v.add(ViolationExitCodeMismatch, "exitCode")
		}
	}
}

// checkEnvelopeVerdict ties status, error and exitCode together. v2 reports an
// abort DISTINCTLY (status "aborted", error class "signal", code 130) instead
// of folding it into "failure", so the precedence costs the reader nothing: the
// failure counts stay in run.counts.failed and run.failures.
func checkEnvelopeVerdict(v *validator, obj map[string]any) {
	status, ok := childString(obj, "status")
	if !ok {
		return
	}
	errValue, hasError := childValue(obj, "error")
	if status == StatusSuccess {
		if hasError {
			v.add(ViolationErrorMismatch, "error")
		}
	} else if !hasError {
		v.add(ViolationMissingField, "error")
	}
	if errObj, isObject := errValue.(map[string]any); isObject {
		checkErrorClass(v, status, errObj)
	}
	checkExitCodeForOutcome(v, "exitCode", envelopeOutcome(status), obj)
}

// envelopeOutcome maps the envelope status onto the run outcome vocabulary. The
// two are the same three-valued verdict; the mapping exists so one exit-code
// derivation serves both.
func envelopeOutcome(status string) string {
	switch status {
	case StatusSuccess:
		return RunOutcomeSuccess
	case StatusAborted:
		return RunOutcomeAborted
	default:
		return RunOutcomeFailure
	}
}

// checkErrorClass keeps the error class and the verdict from disagreeing: only
// an aborted document carries "signal", and an aborted one carries nothing else.
func checkErrorClass(v *validator, status string, errObj map[string]any) {
	code, ok := childString(errObj, "code")
	if !ok {
		return
	}
	signal := code == "signal"
	if (status == StatusAborted) != signal {
		v.add(ViolationErrorMismatch, "error.code")
	}
}

// checkEnvelopeRunAgreement enforces that a job run's envelope and its run
// summary are one verdict, not two that can drift apart.
func checkEnvelopeRunAgreement(v *validator, obj map[string]any) {
	run := childObject(obj, "run")
	if run == nil {
		return
	}
	if status, ok := childString(obj, "status"); ok {
		if outcome, has := childString(run, "outcome"); has && outcome != status {
			v.add(ViolationOutcomeMismatch, "run.outcome")
		}
	}
	envelopeCode, hasEnvelope := childInt(obj, "exitCode")
	runCode, hasRun := childInt(run, "exitCode")
	if hasEnvelope && hasRun && envelopeCode != runCode {
		v.add(ViolationExitCodeMismatch, "run.exitCode")
	}
}

func validateSessionStreamRecord(v *validator, root map[string]any) {
	obj := object(v, "", root, sessionStreamRecordFields)
	_, hasMachineOutput := childValue(obj, "machineOutput")
	if run, hasRun := childValue(obj, "run"); hasRun {
		if hasMachineOutput {
			streamRunSummary(v, "run", run)
		} else {
			runSummary(v, "run", run)
		}
	}
	record, ok := childString(obj, "record")
	if !ok {
		return
	}
	requireMembers(v, obj, streamMembersFor(record))
	if record != RecordSessionEnd && hasMachineOutput {
		v.add(ViolationUnexpectedField, "machineOutput")
	}
	if record == RecordSessionEnd && hasMachineOutput {
		nonEmptyStringBytesAtMost(64)(v, "time", obj["time"])
	}
}

// streamMembersFor returns the members a stream record variant must carry. Any
// other variant member is forbidden on that variant, so a reader can switch on
// `record` alone and know exactly what is present.
func streamMembersFor(record string) []string {
	switch record {
	case RecordPlanEnd:
		return []string{"plan"}
	case RecordSessionEnd:
		return []string{"run"}
	case RecordTaskEvent:
		return []string{"identity", "event"}
	case RecordTaskEnd:
		return []string{"identity", "task"}
	case RecordTestCase:
		return []string{"identity", "testCase"}
	default:
		return []string{"identity"}
	}
}

func streamRunSummary(v *validator, path string, value any) {
	obj := object(v, path, value, streamRunSummaryFields)
	checkRunArithmetic(v, path, obj)
	checkRunVerdict(v, path, obj)
	checkRunCPUBudget(v, path, obj)
}

func machineOutputBudget(v *validator, path string, value any) {
	object(v, path, value, machineOutputBudgetFields)
}

func machineOutputElision(v *validator, path string, value any) {
	obj := object(v, path, value, machineOutputElisionFields)
	records, hasRecords := childInt(obj, "records")
	bytes, hasBytes := childInt(obj, "bytes")
	if hasRecords && hasBytes && (records == 0) != (bytes == 0) {
		v.add(ViolationCountMismatch, path)
	}
}

func machineOutputElisions(v *validator, path string, value any) {
	object(v, path, value, machineOutputElisionsFields)
}

func machineOutputArtifact(v *validator, path string, value any) {
	object(v, path, value, machineOutputArtifactFields)
}

func machineOutputSummary(v *validator, path string, value any) {
	obj := object(v, path, value, machineOutputSummaryFields)
	mode, hasMode := childString(obj, "mode")
	budget := childObject(obj, "budget")
	want, known := MachineOutputBudgetFor(mode)
	if !hasMode || !known || budget == nil {
		return
	}
	checkMachineOutputBudgetMember(v, join(path, "budget"), budget, "maxBytes", want.MaxBytes)
	checkMachineOutputBudgetMember(v, join(path, "budget"), budget, "maxRecords", int64(want.MaxRecords))
	checkMachineOutputBudgetMember(v, join(path, "budget"), budget, "failureReserveBytes", want.FailureReserveBytes)
	checkMachineOutputBudgetMember(v, join(path, "budget"), budget, "failureReserveRecords", int64(want.FailureReserveRecords))
	checkMachineOutputBudgetMember(v, join(path, "budget"), budget, "finalReserveBytes", want.FinalReserveBytes)
	checkMachineOutputBudgetMember(v, join(path, "budget"), budget, "finalReserveRecords", int64(want.FinalReserveRecords))
}

func checkMachineOutputBudgetMember(v *validator, path string, budget map[string]any, name string, want int64) {
	if got, ok := childInt(budget, name); ok && got != want {
		v.add(ViolationInvalidValue, join(path, name))
	}
}

func validateMCPResult(v *validator, root map[string]any) {
	obj := object(v, "", root, mcpResultFields)
	tool, ok := childString(obj, "tool")
	if !ok {
		return
	}
	if tool == MCPToolRunJobs {
		requireMembers(v, obj, []string{"run"})
		return
	}
	requireMembers(v, obj, []string{"plan"})
}

func validateSessionFile(v *validator, root map[string]any) {
	obj := object(v, "", root, sessionFileFields)
	checkExecutionReferences(v, obj)
	checkParentSessionReference(v, obj)
}

// checkParentSessionReference refuses a record that names itself as its own
// parent. Self-parenting is never a real nesting relation, it is the exact
// shape of a producer bug — a run that inherited the ambient parent id and then
// stamped it beside its own id — and a consumer that trusted it would drop the
// top-level run out of its gate ledger entirely.
func checkParentSessionReference(v *validator, obj map[string]any) {
	parent, hasParent := childString(obj, "parentSessionId")
	if !hasParent || parent == "" {
		return
	}
	if id, ok := childString(obj, "sessionId"); ok && id == parent {
		v.add(ViolationInvalidKey, "parentSessionId")
	}
}

// checkExecutionReferences enforces the one cross-member rule the physical
// ledger needs: a task's executionId must name an execution the same document
// declares. A dangling reference would silently drop a task's cost from the
// physical roll-up while the record still looks attributed, which is precisely
// the double-count-or-lose-count failure the ledger exists to remove.
//
// The converse is deliberately NOT a violation: an execution nothing references
// is real work (a superseded retry attempt), and dropping it would understate
// the machine's cost.
func checkExecutionReferences(v *validator, obj map[string]any) {
	tasks, hasTasks := childArray(obj, "tasks")
	if !hasTasks || len(tasks) == 0 {
		return
	}
	declared := make(map[string]bool)
	if executions, ok := childArray(obj, "executions"); ok {
		for _, item := range executions {
			execution, isObject := item.(map[string]any)
			if !isObject {
				continue
			}
			if id, ok := childString(execution, "id"); ok {
				declared[id] = true
			}
		}
	}
	for i, item := range tasks {
		task, isObject := item.(map[string]any)
		if !isObject {
			continue
		}
		id, ok := childString(task, "executionId")
		if !ok || id == "" || declared[id] {
			continue
		}
		v.add(ViolationInvalidKey, join(indexPath("tasks", i), "executionId"))
	}
}

func validateSessionPlanFile(v *validator, root map[string]any) {
	object(v, "", root, sessionPlanFileFields)
}

func reportGit(v *validator, path string, value any) {
	object(v, path, value, reportGitFields)
}

// reportRun validates the report's verdict. It reuses the run summary's
// arithmetic and CPU-budget rules verbatim — the member names are the same on
// purpose, so the two documents cannot start counting differently — and states
// the verdict rules the report's own shape needs: it has no abortedBy, because
// an abort source is a run-ledger fact and the report keeps only the verdict.
func reportRun(v *validator, path string, value any) {
	obj := object(v, path, value, reportRunFields)
	checkRunArithmetic(v, path, obj)
	checkReportRunVerdict(v, path, obj)
	checkRunCPUBudget(v, path, obj)
}

func checkReportRunVerdict(v *validator, path string, obj map[string]any) {
	outcome, hasOutcome := childString(obj, "outcome")
	if !hasOutcome {
		return
	}
	failed, hasFailed := childInt(childObject(obj, "counts"), "failed")
	switch {
	case outcome == RunOutcomeSuccess && hasFailed && failed > 0:
		v.add(ViolationOutcomeMismatch, join(path, "outcome"))
	case outcome == RunOutcomeFailure && hasFailed && failed == 0:
		v.add(ViolationOutcomeMismatch, join(path, "outcome"))
	}
	checkExitCodeForOutcome(v, join(path, "exitCode"), outcome, obj)
}

// reportTests keeps the outcome counters one measurement rather than
// independent numbers: total is the sum of the buckets, exactly as the run
// histogram is. failureDetailsTruncated is orthogonal omission accounting.
func reportTests(v *validator, path string, value any) {
	obj := object(v, path, value, reportTestsFields)
	total, hasTotal := childInt(obj, "total")
	passed, hasPassed := childInt(obj, "passed")
	failed, hasFailed := childInt(obj, "failed")
	skipped, hasSkipped := childInt(obj, "skipped")
	if hasTotal && hasPassed && hasFailed && hasSkipped && total != passed+failed+skipped {
		v.add(ViolationCountMismatch, join(path, "total"))
	}
}

// reportCoverage validates one coverage measurement and keeps its two forms
// agreeing: a producer that states covered and total has stated the same
// measurement twice, and the counts cannot describe more covered units than
// there are units.
func reportCoverage(v *validator, path string, value any) {
	obj := object(v, path, value, reportCoverageFields)
	covered, hasCovered := childInt(obj, "covered")
	total, hasTotal := childInt(obj, "total")
	if hasCovered && hasTotal && covered > total {
		v.add(ViolationCountMismatch, join(path, "covered"))
	}
}

// reportCommand validates one command's synthesis, reusing the run histogram
// arithmetic: a command's counts span its own selected tasks and must add up the
// same way the run's do.
func reportCommand(v *validator, path string, value any) {
	obj := object(v, path, value, reportCommandFields)
	checkRunArithmetic(v, path, obj)
}

// reportJobs validates the bounded job list. The bound is a CONTRACT clause, not
// a producer preference: a consumer that accepts this document accepts a stated
// worst case, so a list past the cap is rejected rather than quietly read.
func reportJobs(v *validator, path string, value any) {
	items, ok := value.([]any)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	if len(items) > ReportMaxJobs {
		v.add(ViolationInvalidValue, path)
	}
	for i, element := range items {
		reportJob(v, indexPath(path, i), element)
	}
}

// reportJob validates one job line. The key stays a DERIVED view of project and
// task, the same rule TaskIdentity carries — the report flattens the identity,
// it does not invent a second spelling of it.
//
// The producer-side omission count preserves the honest-truncation clause: a
// truncatedCount may accompany a short list only to the extent the producer
// already omitted details. Any remainder was dropped by report reduction, so
// the list must be full. The producer count must itself be included in the
// total, never exceed it or appear alone.
func reportJob(v *validator, path string, value any) {
	obj := object(v, path, value, reportJobFields)
	key, hasKey := childString(obj, "key")
	project, hasProject := childString(obj, "project")
	task, hasTask := childString(obj, "task")
	if hasKey && hasProject && hasTask && key != project+":"+task {
		v.add(ViolationInvalidKey, join(path, "key"))
	}
	truncated, hasTruncated := childInt(obj, "truncatedCount")
	producerOmitted, hasProducerOmitted := childInt(obj, "failureDetailsTruncated")
	// The field checks already report non-positive values. Stop here rather than
	// cascading a second arithmetic violation from an invalid operand.
	if (hasTruncated && truncated <= 0) || (hasProducerOmitted && producerOmitted <= 0) {
		return
	}
	if hasProducerOmitted && (!hasTruncated || producerOmitted > truncated) {
		v.add(ViolationCountMismatch, join(path, "truncatedCount"))
		return
	}
	if !hasTruncated || truncated <= producerOmitted {
		return
	}
	diagnostics, hasDiagnostics := childArray(obj, "diagnostics")
	if !hasDiagnostics || len(diagnostics) != ReportMaxJobDiagnostics {
		v.add(ViolationCountMismatch, join(path, "truncatedCount"))
	}
}

// reportDiagnostics validates a job's diagnostics under the report's two
// bounds: how many one job may carry, and how long one message may be.
func reportDiagnostics(v *validator, path string, value any) {
	items, ok := value.([]any)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	if len(items) > ReportMaxJobDiagnostics {
		v.add(ViolationInvalidValue, path)
	}
	for i, element := range items {
		reportDiagnostic(v, indexPath(path, i), element)
	}
}

// reportDiagnostic is the shared diagnostic shape plus the report's message
// bound. The bound is in UTF-8 BYTES, which is what a size budget is actually
// spent in; the schema's maxLength counts code points and is therefore the
// weaker of the two, so a document this validator accepts always satisfies it.
func reportDiagnostic(v *validator, path string, value any) {
	obj := object(v, path, value, diagnosticFields)
	if message, ok := childString(obj, "message"); ok && len(message) > ReportMaxMessageBytes {
		v.add(ViolationInvalidValue, join(path, "message"))
	}
}

// reportCache validates the typed cache subset. Restores are a subset of hits:
// a key that was not held cannot have been materialized, so the reverse means
// the two counters came from different accountings.
func reportCache(v *validator, path string, value any) {
	obj := object(v, path, value, reportCacheFields)
	hits, hasHits := childInt(obj, "hits")
	restored, hasRestored := childInt(obj, "restored")
	if hasHits && hasRestored && restored > hits {
		v.add(ViolationCountMismatch, join(path, "restored"))
	}
}

func reportScheduler(v *validator, path string, value any) {
	object(v, path, value, reportSchedulerFields)
}

// gitObjectID accepts a full git object id — 40 lowercase hex for a sha1
// repository, 64 for a sha256 one. An abbreviation, a symbolic name ("HEAD") or
// a branch is rejected: the sha is the join key a longitudinal consumer files
// the report under, and a reference that can move is not one.
func gitObjectID(v *validator, path string, value any) {
	id, ok := value.(string)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	if len(id) != 40 && len(id) != 64 {
		v.add(ViolationInvalidValue, path)
		return
	}
	for i := 0; i < len(id); i++ {
		b := id[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			v.add(ViolationInvalidValue, path)
			return
		}
	}
}

// sha256Digest requires a lowercase hex sha256. It is deliberately stricter than
// gitObjectID: a tree fingerprint is a JOIN KEY two agents compare as strings,
// so an uppercase or abbreviated spelling of the same digest would silently read
// as a different tree.
func sha256Digest(v *validator, path string, value any) {
	digest, ok := value.(string)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	if len(digest) != 64 {
		v.add(ViolationInvalidValue, path)
		return
	}
	for i := 0; i < len(digest); i++ {
		b := digest[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			v.add(ViolationInvalidValue, path)
			return
		}
	}
}

// prefixedSha256Digest requires the runner contract's digest spelling:
// `sha256:` followed by 64 lowercase hex characters. It is the spelling the
// source manifest and execution request use, so a reader compares the session's
// provenance with a request identity as one string.
func prefixedSha256Digest(v *validator, path string, value any) {
	digest, ok := value.(string)
	if !ok {
		v.add(ViolationInvalidType, path)
		return
	}
	const prefix = "sha256:"
	if len(digest) != len(prefix)+64 || digest[:len(prefix)] != prefix || !lowercaseHex(digest[len(prefix):]) {
		v.add(ViolationInvalidValue, path)
	}
}

// hexOfLength requires exactly n lowercase hex characters.
func hexOfLength(n int) func(v *validator, path string, value any) {
	return func(v *validator, path string, value any) {
		text, ok := value.(string)
		if !ok {
			v.add(ViolationInvalidType, path)
			return
		}
		if len(text) != n || !lowercaseHex(text) {
			v.add(ViolationInvalidValue, path)
		}
	}
}

func lowercaseHex(text string) bool {
	for i := 0; i < len(text); i++ {
		b := text[i]
		if (b < '0' || b > '9') && (b < 'a' || b > 'f') {
			return false
		}
	}
	return true
}

func validateReportFile(v *validator, root map[string]any) {
	obj := object(v, "", root, reportFileFields)
	checkReportJobBudget(v, obj)
	checkReportCommands(v, obj)
}

// checkReportJobBudget ties the bounded job list back to the run it summarizes.
//
// Two rules, and together they make the truncation honest:
//
//   - ACCOUNTING: len(jobs) + elidedJobs equals the run's own task total, so a
//     reader can always tell a small run from a truncated one, and a producer
//     cannot drop tasks without saying how many.
//   - EXHAUSTION: nothing is elided until the budget is FULL. Otherwise a
//     producer could ship one job, call the other 744 elided, and satisfy the
//     accounting rule while carrying none of the information the bound was sized
//     to allow.
func checkReportJobBudget(v *validator, obj map[string]any) {
	jobs, hasJobs := childArray(obj, "jobs")
	elided, hasElided := childInt(obj, "elidedJobs")
	if !hasJobs || !hasElided {
		return
	}
	if total, hasTotal := childInt(childObject(childObject(obj, "run"), "counts"), "total"); hasTotal {
		if int64(len(jobs))+elided != total {
			v.add(ViolationCountMismatch, "elidedJobs")
		}
	}
	if elided > 0 && len(jobs) != ReportMaxJobs {
		v.add(ViolationCountMismatch, "jobs")
	}
}

// commandRollup is what one pass over the per-command rows yields for the
// run-level checks. CountsComplete is false when a row stated no total, so the
// sum is partial and cannot be compared.
type commandRollup struct {
	Totals         int64
	CPUMs          int64
	CountsComplete bool
}

// rollUpReportCommands walks the per-command rows ONCE: it reports a repeated
// command — the list is a HISTOGRAM over root commands, not a per-task log, the
// same rule the preparation phases carry — and returns the sums the run-level
// rules are checked against.
func rollUpReportCommands(v *validator, commands []any) commandRollup {
	seen := make(map[string]bool, len(commands))
	rollup := commandRollup{CountsComplete: true}
	for i, entry := range commands {
		record, isObject := entry.(map[string]any)
		if !isObject {
			rollup.CountsComplete = false
			continue
		}
		if name, has := childString(record, "command"); has {
			if seen[name] {
				v.add(ViolationCountMismatch, join(indexPath("commands", i), "command"))
			}
			seen[name] = true
		}
		total, hasTotal := childInt(childObject(record, "counts"), "total")
		if !hasTotal {
			rollup.CountsComplete = false
		}
		rollup.Totals += total
		if cpu, has := childInt(record, "cpuMs"); has {
			rollup.CPUMs += cpu
		}
	}
	return rollup
}

// checkReportCommands keeps the per-command synthesis a partition of the run
// rather than a list of unrelated rows:
//
//   - The per-command totals SUM to the run's total, because every selected task
//     belongs to exactly one root command. A sum that disagrees means the report
//     is describing a different task set than its own verdict does.
//   - The per-command CPU never exceeds the run's ACTUAL CPU. Per-command CPU is
//     the sum of fresh tasks' shares of their executions, so superseded attempts
//     — real cost, belonging to the run's ledger and to no command — can push the
//     run's figure above the commands' but never below it.
func checkReportCommands(v *validator, obj map[string]any) {
	commands, ok := childArray(obj, "commands")
	if !ok {
		return
	}
	rollup := rollUpReportCommands(v, commands)
	runTotal, hasRunTotal := childInt(childObject(childObject(obj, "run"), "counts"), "total")
	if rollup.CountsComplete && hasRunTotal && rollup.Totals != runTotal {
		v.add(ViolationCountMismatch, "commands")
	}
	actual, hasActual := childInt(childObject(childObject(obj, "run"), "cpu"), "actualMs")
	if hasActual && rollup.CPUMs > actual {
		v.add(ViolationInvalidValue, "commands")
	}
}

// variantMembers is the set of members any variant of a document may carry.
var variantMembers = []string{"identity", "event", "task", "testCase", "run", "plan"}

// requireMembers enforces a variant's member set: everything in required must
// be present, and every other variant member must be absent.
func requireMembers(v *validator, obj map[string]any, required []string) {
	need := make(map[string]bool, len(required))
	for _, name := range required {
		need[name] = true
		if _, present := childValue(obj, name); !present {
			v.add(ViolationMissingField, name)
		}
	}
	for _, name := range variantMembers {
		if need[name] {
			continue
		}
		if _, present := childValue(obj, name); present {
			v.add(ViolationUnexpectedField, name)
		}
	}
}

// childValue reads a member, nil-safe on the container.
func childValue(obj map[string]any, name string) (any, bool) {
	if obj == nil {
		return nil, false
	}
	value, ok := obj[name]
	return value, ok
}

func childObject(obj map[string]any, name string) map[string]any {
	value, ok := childValue(obj, name)
	if !ok {
		return nil
	}
	child, _ := value.(map[string]any)
	return child
}

func childString(obj map[string]any, name string) (string, bool) {
	value, ok := childValue(obj, name)
	if !ok {
		return "", false
	}
	text, isString := value.(string)
	return text, isString
}

func childBool(obj map[string]any, name string) (bool, bool) {
	value, ok := childValue(obj, name)
	if !ok {
		return false, false
	}
	flag, isBool := value.(bool)
	return flag, isBool
}

func childInt(obj map[string]any, name string) (int64, bool) {
	value, ok := childValue(obj, name)
	if !ok {
		return 0, false
	}
	return wholeNumber(value)
}

func childArray(obj map[string]any, name string) ([]any, bool) {
	value, ok := childValue(obj, name)
	if !ok {
		return nil, false
	}
	items, isArray := value.([]any)
	return items, isArray
}
