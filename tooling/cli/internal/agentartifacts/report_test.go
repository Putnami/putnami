package agentartifacts

import (
	"encoding/json"
	"strings"
	"testing"
)

// mixedPlan carries one entry of every action, authored out of order so the
// projection has something to sort.
func mixedPlan() *Plan {
	return &Plan{
		Name:          testArtifactName,
		Version:       "2.0.0",
		ArchiveDigest: strings.Repeat("a", 64),
		ManifestHash:  strings.Repeat("b", 64),
		Entries: []Entry{
			{Path: ".agents/skills/plan/SKILL.md", Action: ActionCreate, SHA256: strings.Repeat("1", 64)},
			{Path: ".agents/skills/fix/SKILL.md", Action: ActionUpdate, SHA256: strings.Repeat("2", 64)},
			{Path: ".claude/skills/fix/SKILL.md", Action: ActionCollide, Reason: ReasonModified, Detail: "edited since install"},
			{Path: ".agents/skills/fix/references/fix-heavy.md", Action: ActionRemove, SHA256: strings.Repeat("3", 64)},
			{Path: ".agents/skills/agents/openai.yaml", Action: ActionUnchanged, SHA256: strings.Repeat("4", 64)},
			{Path: ".agents/skills/fix/scripts/finalize-pr.sh", Action: ActionCollide, Reason: ReasonUnmanaged, Detail: "not recorded"},
		},
	}
}

// Both renderings list every section, and both list them in the same
// deterministic order.
func TestReport_ListsEverySectionDeterministically(t *testing.T) {
	plan := mixedPlan()
	// NewReport consumes plan order; a real plan is sorted, so sort here too.
	sorted := *plan
	sorted.Entries = append([]Entry(nil), plan.Entries...)
	for i := 1; i < len(sorted.Entries); i++ {
		for j := i; j > 0 && sorted.Entries[j-1].Path > sorted.Entries[j].Path; j-- {
			sorted.Entries[j-1], sorted.Entries[j] = sorted.Entries[j], sorted.Entries[j-1]
		}
	}

	report := NewReport(&sorted, true)
	if !equalStrings(report.Created, []string{".agents/skills/plan/SKILL.md"}) {
		t.Errorf("created = %v", report.Created)
	}
	if !equalStrings(report.Updated, []string{".agents/skills/fix/SKILL.md"}) {
		t.Errorf("updated = %v", report.Updated)
	}
	if !equalStrings(report.Removed, []string{".agents/skills/fix/references/fix-heavy.md"}) {
		t.Errorf("removed = %v", report.Removed)
	}
	if !equalStrings(report.Unchanged, []string{".agents/skills/agents/openai.yaml"}) {
		t.Errorf("unchanged = %v", report.Unchanged)
	}
	if len(report.Collided) != 2 ||
		report.Collided[0].Path != ".agents/skills/fix/scripts/finalize-pr.sh" ||
		report.Collided[1].Path != ".claude/skills/fix/SKILL.md" {
		t.Errorf("collided = %+v, want both, sorted by path", report.Collided)
	}

	first, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 50; i++ {
		again, err := NewReport(&sorted, true).JSON()
		if err != nil {
			t.Fatal(err)
		}
		if string(again) != string(first) {
			t.Fatalf("iteration %d produced different machine bytes", i)
		}
	}
	if !strings.HasSuffix(string(first), "\n") || strings.HasSuffix(string(first), "\n\n") {
		t.Fatalf("machine report needs exactly one trailing newline: %q", first)
	}

	text := report.Text()
	for i := 0; i < 50; i++ {
		if NewReport(&sorted, true).Text() != text {
			t.Fatalf("iteration %d produced different human text", i)
		}
	}
	// The human rendering names the same paths in the same order.
	for _, path := range []string{
		".agents/skills/fix/scripts/finalize-pr.sh",
		".claude/skills/fix/SKILL.md",
		".agents/skills/plan/SKILL.md",
		".agents/skills/fix/SKILL.md",
		".agents/skills/fix/references/fix-heavy.md",
		".agents/skills/agents/openai.yaml",
	} {
		if !strings.Contains(text, path) {
			t.Errorf("human report omits %s:\n%s", path, text)
		}
	}
	if !strings.Contains(text, ReasonModified) || !strings.Contains(text, ReasonUnmanaged) {
		t.Errorf("human report omits a collision reason:\n%s", text)
	}
}

// Empty sections serialize as [] rather than null, so a machine consumer never
// has to distinguish "no files" from "field absent".
func TestReport_EmptySectionsAreEmptyArrays(t *testing.T) {
	report := NewReport(&Plan{Name: testArtifactName, Version: "1.0.0"}, true)
	data, err := report.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "null") {
		t.Fatalf("machine report carries a null section:\n%s", data)
	}

	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"created", "updated", "removed", "unchanged", "collided"} {
		if string(decoded[section]) != "[]" {
			t.Errorf("%s = %s, want []", section, decoded[section])
		}
	}
	for _, field := range []string{"name", "version", "archiveDigest", "manifestHash", "applied"} {
		if _, ok := decoded[field]; !ok {
			t.Errorf("machine report omits %q", field)
		}
	}
}

// The machine report's member set is CLOSED and carries no wall clock: it is
// compared across runs, and a timestamp or a duration makes every comparison
// fail. Asserting the exact key set is what stops one from being added.
func TestReport_MemberSetIsClosedAndClockFree(t *testing.T) {
	data, err := NewReport(mixedPlan(), true).JSON()
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatal(err)
	}

	want := map[string]bool{
		"name": true, "version": true, "archiveDigest": true, "manifestHash": true,
		"applied": true, "created": true, "updated": true, "removed": true,
		"unchanged": true, "collided": true,
	}
	for key := range decoded {
		if !want[key] {
			t.Errorf("machine report grew an unexpected member %q; a report is compared, so every member must be a function of the plan alone", key)
		}
	}
	for key := range want {
		if _, ok := decoded[key]; !ok {
			t.Errorf("machine report lost member %q", key)
		}
	}

	// Same rule for the ownership state, which is diffed by hand far more often.
	state := &State{
		Version: StateVersion, Name: testArtifactName, ArtifactVersion: "1.0.0",
		ArchiveDigest: strings.Repeat("a", 64), ManifestHash: strings.Repeat("b", 64),
		Files: []File{{Path: ".agents/x", SHA256: strings.Repeat("c", 64)}},
	}
	stateBytes, err := MarshalState(state)
	if err != nil {
		t.Fatal(err)
	}
	var decodedState map[string]json.RawMessage
	if err := json.Unmarshal(stateBytes, &decodedState); err != nil {
		t.Fatal(err)
	}
	wantState := map[string]bool{
		"version": true, "name": true, "artifactVersion": true,
		"archiveDigest": true, "manifestHash": true, "files": true,
	}
	for key := range decodedState {
		if !wantState[key] {
			t.Errorf("ownership state grew an unexpected member %q", key)
		}
	}
	for key := range wantState {
		if _, ok := decodedState[key]; !ok {
			t.Errorf("ownership state lost member %q", key)
		}
	}
}

// An aborted run says so in both renderings, and says what to do next.
func TestReport_AbortedRunIsUnmistakable(t *testing.T) {
	report := NewReport(mixedPlan(), false)
	if report.Applied {
		t.Fatal("Applied must be false for an aborted run")
	}
	text := report.Text()
	if !strings.Contains(text, "aborted") || !strings.Contains(text, "nothing was written") {
		t.Fatalf("human report does not state the run was aborted:\n%s", text)
	}
	if !strings.Contains(text, "preserved") {
		t.Fatalf("human report does not say the collided files were preserved:\n%s", text)
	}

	applied := NewReport(mixedPlan(), true).Text()
	if strings.Contains(applied, "nothing was written") {
		t.Fatalf("an applied run must not claim it wrote nothing:\n%s", applied)
	}
}

func TestPlan_SelectAndPredicates(t *testing.T) {
	plan := mixedPlan()
	if got := len(plan.Select(ActionCreate)); got != 1 {
		t.Errorf("Select(create) = %d, want 1", got)
	}
	if got := len(plan.Collisions()); got != 2 {
		t.Errorf("Collisions() = %d, want 2", got)
	}
	if !plan.HasCollisions() || !plan.Mutates() {
		t.Errorf("plan predicates = %v/%v, want both true", plan.HasCollisions(), plan.Mutates())
	}

	clean := &Plan{Entries: []Entry{{Path: "a", Action: ActionUnchanged}}}
	if clean.HasCollisions() || clean.Mutates() {
		t.Errorf("an all-unchanged plan neither collides nor mutates, got %v/%v",
			clean.HasCollisions(), clean.Mutates())
	}
}
