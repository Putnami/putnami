package analyze

import (
	"reflect"
	"testing"

	readers "go.putnami.dev/evidence/readers"
	"go.putnami.dev/protocol/evidence"
	"go.putnami.dev/protocol/features/spectest"
)

func TestOneToolAndRuleReadTwiceIsOneRule(t *testing.T) {
	spectest.Proves(t, "tooling/observe-architecture", "violations-are-placed", "each-violation-goes-to-the-component-of-its-file")
	arch := newArchitecture()
	place := func(v readers.Violation) evidence.ArchitectureViolation {
		return evidence.ArchitectureViolation{Message: v.Message}
	}
	arch.add("archunit", readers.ArchitectureRule{Name: "r", Status: readers.RuleKept}, place)
	arch.add("archunit", readers.ArchitectureRule{Name: "r", Status: readers.RuleBroken, Count: 3, Violations: []readers.Violation{{Message: "a"}}}, place)
	arch.add("archunit", readers.ArchitectureRule{Name: "r", Status: readers.RuleBroken, Count: 1, Violations: []readers.Violation{{Message: "b"}}}, place)
	arch.add("deptrac", readers.ArchitectureRule{Name: "r", Status: readers.RuleKept}, place)
	want := []evidence.ArchitectureRule{
		{Tool: "archunit", Rule: "r", Status: evidence.ArchitectureBroken, ViolationCount: 4, Violations: []evidence.ArchitectureViolation{{Message: "a"}, {Message: "b"}}},
		{Tool: "deptrac", Rule: "r", Status: evidence.ArchitectureKept},
	}
	if got := arch.result(); !reflect.DeepEqual(got, want) {
		t.Errorf("rules %+v, want %+v", got, want)
	}
	if newArchitecture().result() != nil {
		t.Error("no rule read is no architecture member")
	}
}

func TestTheArchitectureBoundsDropTheExcess(t *testing.T) {
	arch := newArchitecture()
	place := func(v readers.Violation) evidence.ArchitectureViolation {
		return evidence.ArchitectureViolation{Message: v.Message}
	}
	many := make([]readers.Violation, evidence.MaxViolations+2)
	for i := range many {
		many[i].Message = "v"
	}
	arch.add("import-linter", readers.ArchitectureRule{Name: "c", Status: readers.RuleBroken, Count: len(many), Violations: many}, place)
	for i := range evidence.MaxArchitectureRules {
		arch.add("archunit", readers.ArchitectureRule{Name: string(rune('a'+i%26)) + string(rune('0'+i/26%10)) + string(rune('0'+i/260)), Status: readers.RuleKept}, place)
	}
	rules := arch.result()
	if len(rules) != evidence.MaxArchitectureRules || arch.droppedRules == 0 {
		t.Errorf("%d rules, %d dropped", len(rules), arch.droppedRules)
	}
	if len(rules[0].Violations) != evidence.MaxViolations || rules[0].ViolationCount != len(many) || arch.droppedViolations != 2 {
		t.Errorf("violations %d of %d, %d dropped", len(rules[0].Violations), rules[0].ViolationCount, arch.droppedViolations)
	}
	if !matchesClass([]string{"*ArchitectureTest"}, "com.acme.api.ArchitectureTest") || matchesClass([]string{"*ArchitectureTest"}, "") || matchesClass([]string{"["}, "a") {
		t.Error("class patterns match dotted names")
	}
}
