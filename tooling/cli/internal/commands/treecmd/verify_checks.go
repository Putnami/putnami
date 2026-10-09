package treecmd

import (
	"os"
	"regexp"
	"slices"
)

var (
	baseSHAPattern        = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
	contractDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
)

// verifiedLimits states what a verified dossier does not prove.
const verifiedLimits = "Shared write access: attribution, scope completeness and human judgments are conventions; " +
	"no hosted stamp, merge, deployment or acceptance-in-production claim."

// verify checks a v2 dossier against the current tree: its binding, the
// changed files, the policies, every consulted scope, the independent review,
// the gate, the qualification verdicts and the acceptance evidence. expectedBase
// is the --base value, nil when absent.
func (r *verifyRun) verify(record any, expectedBase *string) *jsObject {
	require(strictEqual(at(record, "version"), 2.0), "unsupported local dossier version; v2 scopes required")
	require(!hasOwnMember(record, "domains"), "legacy domains field is not supported in v2")
	binding := at(record, "binding")
	current := r.fingerprint()
	require(strictEqual(at(binding, "fingerprint"), current), "stale dossier tree")
	require(strictEqual(at(binding, "headSHA"), r.git("rev-parse", "HEAD")), "stale dossier HEAD")
	baseSHA := at(binding, "baseSHA")
	require(baseSHAPattern.MatchString(jsToString(baseSHA)), "base must be an immutable Git revision")
	if expectedBase != nil {
		require(strictEqual(r.git("merge-base", *expectedBase, "HEAD"), baseSHA),
			"dossier base differs from intended publication base")
	}
	changed := r.changedFiles(baseSHA)
	require(equalStrings(at(record, "changedFiles"), changed), "changed-file scope differs")
	r.checkPolicies(at(record, "policies"))
	r.checkRef(at(record, "objective"))
	r.checkRef(at(record, "scope"))
	implementers, isList := at(record, "implementers").([]any)
	require(isList && len(implementers) > 0 && allText(implementers), "missing implementer sessions")
	scopes, isList := at(record, "scopes").([]any)
	require(isList, "scopes must be a list")
	r.checkScopes(scopes, current)
	review := at(record, "review")
	r.checkReview(review, implementers, changed, current)
	gate := at(record, "gate")
	session := r.readJSON(r.checkRef(at(gate, "session")))
	report := r.readJSON(r.checkRef(at(gate, "report")))
	r.checkGate(session, report, current, baseSHA, true, nil)
	r.checkQualification(at(record, "qualification"), current)
	r.checkAcceptance(at(record, "acceptance"))
	require(r.fingerprint() == current, "tree changed during verification")
	for _, checked := range r.checked {
		require(r.digest(checked.path) == checked.digest, "evidence changed during verification")
	}
	return obj([]string{"verdict", "stage", "assurance", "binding", "gateSession", "review", "limits"},
		"Putnami verified", "ready", "local-declared-scope", binding,
		at(session, "sessionId"), at(review, "report"), verifiedLimits)
}

// checkPolicies checks that the dossier references exactly the policy files
// of the tree, each with its current digest.
func (r *verifyRun) checkPolicies(policies any) {
	items := array(policies, "record.policies")
	paths := make([]any, 0, len(items))
	for _, policy := range items {
		paths = append(paths, at(policy, "path"))
	}
	recorded, allStrings := sortedStrings(paths)
	present := r.policyPaths()
	require(allStrings && slices.Equal(recorded, present), "missing or extra policy binding")
	for _, policy := range items {
		r.checkRef(policy)
	}
}

// checkScopes checks each consulted scope: attributed, bound to its context
// and proposal, accepted at design and at realization on the current tree,
// and every objection resolved by a named authority.
func (r *verifyRun) checkScopes(scopes []any, current string) {
	scopeIDs := map[string]bool{}
	for _, consultation := range scopes {
		require(!hasOwnMember(consultation, "domain") && !hasOwnMember(consultation, "guardian"),
			"legacy domain or guardian field is not supported in v2")
		scope, isText := textValue(at(consultation, "scope"))
		require(isText && !scopeIDs[scope], "duplicate or missing scope")
		scopeIDs[scope] = true
		require(text(at(consultation, "owner")), "missing consulted owner attribution")
		contexts := at(consultation, "context")
		require(lengthAbove0(contexts), "missing consulted scope context")
		for _, context := range each(contexts) {
			r.checkRef(context)
		}
		proposal := at(consultation, "proposal")
		r.checkRef(proposal)
		for _, phase := range []string{"design", "realization"} {
			opinion := at(consultation, phase)
			r.checkRef(at(opinion, "note"))
			require(strictEqual(at(opinion, "proposalSha256"), at(proposal, "sha256")), "stale scope proposal agreement")
			digests := []any{}
			for _, context := range array(contexts, "consultation.context") {
				digests = append(digests, at(context, "sha256"))
			}
			sortedDigests, allStrings := sortedStrings(digests)
			require(allStrings && equalStrings(at(opinion, "contextSha256"), sortedDigests), "stale scope context agreement")
			require(strictEqual(at(opinion, "position"), "accepted"), "unresolved scope position")
		}
		require(strictEqual(at(at(consultation, "realization"), "fingerprint"), current), "stale realization opinion")
		for _, objection := range each(orEmpty(at(consultation, "objections"))) {
			r.checkRef(at(objection, "note"))
			r.checkRef(at(objection, "resolution"))
			require(text(at(objection, "authority")), "unattributed objection resolution")
		}
	}
}

// checkReview checks the review: by a session that implemented nothing, on the
// current tree, covering every changed file once, with every finding
// resolved.
func (r *verifyRun) checkReview(review any, implementers []any, changed []string, current string) {
	author, isText := textValue(at(review, "author"))
	require(isText && !includesString(implementers, author), "review must name an independent session")
	require(strictEqual(at(review, "fingerprint"), current), "stale review tree")
	r.checkRef(at(review, "report"))
	coverage := array(at(review, "coverage"), "coverage")
	paths := make([]any, 0, len(coverage))
	for _, item := range coverage {
		paths = append(paths, at(item, "path"))
	}
	covered, allStrings := sortedStrings(paths)
	require(allStrings && slices.Equal(covered, changed), "review coverage must account for every changed file exactly once")
	for _, item := range coverage {
		require(oneOf(at(item, "status"), "reviewed", "excluded") && text(at(item, "reason")),
			"missing review coverage or exclusion reason")
	}
	findingIDs := map[string]bool{}
	for _, finding := range each(at(review, "findings")) {
		id, isText := textValue(at(finding, "id"))
		require(isText && !findingIDs[id], "missing or duplicate finding identity")
		findingIDs[id] = true
		require(oneOf(at(finding, "severity"), "low", "medium", "high", "critical"), "unknown finding severity")
		require(text(at(finding, "location")) && text(at(finding, "scenario")), "finding lacks location/scenario")
		state := at(finding, "state")
		require(oneOf(state, "fixed", "refuted", "deferred"), "unresolved finding")
		r.checkRef(at(finding, "resolution"))
		if strictEqual(state, "deferred") {
			require(text(at(finding, "authority")), "deferral has no authority")
		}
	}
}

// checkGate holds the gate rules a dossier and a reused finalizer gate share.
// The native plan against the base decides whether the recorded selection is
// wide enough. exactBaseline makes an impacted gate name baseSHA itself; floor
// names commands required even when the CI policy omits them.
func (r *verifyRun) checkGate(session, report any, current string, baseSHA any, exactBaseline bool, floor []string) {
	require(strictEqual(at(session, "protocolVersion"), 2.0) && strictEqual(at(report, "protocolVersion"), 2.0),
		"expected v2 session and report")
	require(text(at(session, "sessionId")) && strictEqual(at(report, "sessionId"), at(session, "sessionId")),
		"gate report/session mismatch")
	require(!truthy(at(session, "parentSessionId")) && text(at(session, "endTime")), "gate is nested or unfinished")
	require(strictEqual(at(at(session, "tree"), "fingerprint"), current), "stale gate tree")
	for _, value := range []any{session, report} {
		checkGateRun(at(value, "run"))
	}
	require(strictEqual(at(report, "enforceCoverage"), true), "coverage enforcement not proved")
	selection := at(session, "selection")
	mode := at(selection, "mode")
	require(oneOf(mode, "impacted", "all"), "gate must cover impacted or all projects")
	if strictEqual(mode, "all") {
		require(strictEqual(at(selection, "scoped"), false), "all-project gate was narrowed")
	} else if exactBaseline {
		require(strictEqual(optional(at(session, "git"), "baseline"), baseSHA),
			"impacted gate must name the dossier immutable base SHA")
	}
	commands := map[string]bool{}
	if listed := at(session, "commands"); !nullish(listed) {
		for _, command := range each(listed) {
			if name, isString := command.(string); isString {
				commands[name] = true
			}
		}
	}
	tasks := orEmpty(at(session, "tasks"))
	for _, task := range each(tasks) {
		if strictEqual(at(task, "status"), "success") && strictEqual(at(task, "exitCode"), 0.0) {
			if name, isString := at(at(at(task, "identity"), "task"), "command").(string); isString {
				commands[name] = true
			}
		}
	}
	required, planFlags := r.requiredCommands(floor, report)
	require(!slices.ContainsFunc(required, func(command string) bool { return !commands[command] }),
		"gate lacks required commands or their executed companions")
	recorded := []any{}
	for _, task := range array(tasks, "session.tasks") {
		recorded = append(recorded, at(at(task, "identity"), "key"))
	}
	require(!hasDuplicate(recorded), "duplicate gate task identity")
	plan := r.v.Plan
	if plan == nil {
		plan = workspacePlan
	}
	planned, err := plan(r.root, required, planFlags, jsToString(baseSHA))
	if err != nil {
		failErr(err)
	}
	require(!slices.ContainsFunc(planned, func(key string) bool { return !includesString(recorded, key) }),
		"gate is narrower than the native unfiltered impacted plan")
}

// checkGateRun checks the run block of a gate session or report: green, with
// consistent non-negative counts and no failed or canceled task.
func checkGateRun(run any) {
	require(strictEqual(at(run, "outcome"), "success") && strictEqual(at(run, "exitCode"), 0.0), "gate failed")
	counts := at(run, "counts")
	require(strictEqual(at(counts, "failed"), 0.0) && strictEqual(at(counts, "canceled"), 0.0),
		"gate contains failed or canceled tasks")
	for _, key := range []string{"total", "succeeded", "failed", "canceled", "skipped"} {
		count := at(counts, key)
		number, _ := count.(float64)
		require(isSafeInteger(count) && number >= 0, "invalid gate counts")
	}
	sum := 0.0
	for _, key := range []string{"succeeded", "failed", "canceled", "skipped"} {
		number, _ := at(counts, key).(float64)
		sum += number
	}
	require(strictEqual(at(counts, "total"), sum), "inconsistent gate counts")
}

// requiredCommands reads the commands putnami.ci.json version 3 requires,
// followed by the floor commands it omits, and checks that report proves every
// flag the policy appends to the gate: --fix=false needs the report's
// fix: false, --enforce-coverage and --continue-on-error need nothing more, and
// any other flag fails. It also returns the flags the native plan carries: the
// policy's --enforce-coverage and --fix=false flags in policy order, never
// --continue-on-error. Without a policy file, a non-empty floor is the whole
// requirement and the native plan carries no flag.
func (r *verifyRun) requiredCommands(floor []string, report any) (required, planFlags []string) {
	exists := func() bool {
		_, err := os.Stat(r.path("putnami.ci.json"))
		return err == nil
	}
	if len(floor) > 0 && !exists() {
		return floor, nil
	}
	require(exists(), "no CI command policy; resolve policy before verification")
	policy := r.readJSON("putnami.ci.json")
	require(strictEqual(at(policy, "version"), 3.0), "unsupported CI policy version")
	flags, isList := orEmpty(at(policy, "flags")).([]any)
	require(isList && !slices.ContainsFunc(flags, func(flag any) bool {
		return !oneOf(flag, "--enforce-coverage", "--continue-on-error", "--fix=false")
	}), "CI flags require evidence this verifier does not support")
	if slices.ContainsFunc(flags, func(flag any) bool { return strictEqual(flag, "--fix=false") }) {
		require(strictEqual(at(report, "fix"), false), "CI flag --fix=false not proved by the gate report")
	}
	planFlags = []string{}
	for _, flag := range flags {
		if text, isText := flag.(string); isText && oneOf(text, "--enforce-coverage", "--fix=false") {
			planFlags = append(planFlags, text)
		}
	}
	result := []string{}
	for _, command := range each(at(policy, "commands")) {
		switch command := command.(type) {
		case string:
			result = append(result, command)
		case *jsObject:
			if !strictEqual(command.get("failOnError"), false) {
				name, isText := textValue(command.get("name"))
				require(isText, "invalid CI command")
				result = append(result, name)
			}
		}
	}
	require(len(result) > 0, "empty required command policy")
	required = []string{}
	for _, command := range append(result, floor...) {
		if !slices.Contains(required, command) {
			required = append(required, command)
		}
	}
	return required, planFlags
}

// checkQualification checks the local qualification verdicts: one passing,
// clean verdict on the current tree for each required workload and none
// other, or a not-applicable note when no workload is required.
func (r *verifyRun) checkQualification(qualification any, current string) {
	workloads, isList := at(qualification, "required").([]any)
	require(isList && allText(workloads) && !hasDuplicate(workloads), "invalid workload scope")
	seen := []any{}
	for _, result := range each(at(qualification, "results")) {
		envelope := r.readJSON(r.checkRef(result))
		verdict := envelope
		if hasOwnMember(envelope, "data") {
			verdict = at(envelope, "data")
		}
		checkQualificationVerdict(verdict, current)
		seen = append(seen, at(verdict, "project"))
	}
	projects, allStrings := sortedStrings(seen)
	required, _ := sortedStrings(workloads)
	require(allStrings && slices.Equal(projects, required), "missing, duplicate or out-of-scope qualification")
	if len(workloads) == 0 {
		r.checkRef(at(qualification, "notApplicable"))
	}
}

func checkQualificationVerdict(verdict any, current string) {
	require(strictEqual(at(verdict, "protocolVersion"), 1.0), "unsupported qualification verdict")
	require(strictEqual(at(verdict, "state"), "passed") && strictEqual(at(at(verdict, "cleanup"), "state"), "clean"),
		"qualification failed or cleanup incomplete")
	binding := at(verdict, "binding")
	require(strictEqual(at(binding, "kind"), "tree") && strictEqual(at(binding, "fingerprint"), current),
		"stale qualification tree")
	require(strictEqual(at(at(verdict, "target"), "kind"), "local"), "expected local qualification")
	require(text(at(verdict, "finishedAt")), "qualification unfinished")
	require(allPassed(at(verdict, "phases")), "qualification phases incomplete")
	requests := at(verdict, "requests")
	require(allPassed(requests), "qualification requests missing or failed")
	requestList, _ := requests.([]any)
	contract := at(verdict, "contract")
	require(strictEqual(at(contract, "requests"), float64(len(requestList))), "qualification request count mismatch")
	require(contractDigestPattern.MatchString(jsToString(at(contract, "digest"))), "qualification contract digest missing")
	leftovers := at(at(verdict, "cleanup"), "leftovers")
	leftoverList, isList := leftovers.([]any)
	require(nullish(leftovers) || (isList && len(leftoverList) == 0), "qualification cleanup has leftovers")
}

// checkAcceptance checks that at least one functional requirement passed, each
// with its evidence.
func (r *verifyRun) checkAcceptance(acceptance any) {
	require(lengthAbove0(acceptance), "no functional acceptance evidence")
	for _, item := range each(acceptance) {
		require(text(at(item, "requirement")) && strictEqual(at(item, "status"), "passed"), "acceptance incomplete")
		r.checkRef(at(item, "evidence"))
	}
}

// allPassed reports whether value is a non-empty array whose every element
// has state "passed". It stops at the first element that has not.
func allPassed(value any) bool {
	items, isList := value.([]any)
	if !isList || len(items) == 0 {
		return false
	}
	for _, item := range items {
		if !strictEqual(at(item, "state"), "passed") {
			return false
		}
	}
	return true
}

// lengthAbove0 is value.length > 0 for a value the check iterates next.
func lengthAbove0(value any) bool {
	positive, err := lengthPositive(value)
	if err != nil {
		failErr(err)
	}
	return positive
}

// orEmpty is value ?? [].
func orEmpty(value any) any {
	if nullish(value) {
		return []any{}
	}
	return value
}

// textValue returns value as a string when text(value) holds.
func textValue(value any) (string, bool) {
	s, isString := value.(string)
	return s, isString && text(s)
}

func allText(values []any) bool {
	return !slices.ContainsFunc(values, func(value any) bool { return !text(value) })
}

// oneOf is [choices...].includes(value).
func oneOf(value any, choices ...string) bool {
	s, isString := value.(string)
	return isString && slices.Contains(choices, s)
}
