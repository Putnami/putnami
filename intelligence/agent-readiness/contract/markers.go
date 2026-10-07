package contract

// Unit is how a marker's Value is measured.
type Unit string

const (
	// UnitDays counts whole days.
	UnitDays Unit = "days"
	// UnitRatio is a share from 0 to 1.
	UnitRatio Unit = "ratio"
	// UnitCount counts occurrences or people.
	UnitCount Unit = "count"
	// UnitLines counts changed lines.
	UnitLines Unit = "lines"
)

// MarkerDefinition names a marker, the step it measures and the unit of its
// Value. https://putnami.dev/agent-readiness/method defines what each state means,
// the evidence command, and the thresholds.
type MarkerDefinition struct {
	ID   string
	Step Step
	Unit Unit
}

// MarkerCatalog lists the markers of methods 0.1 to 0.4 in the order the
// method page lists them. Method 0.4 reads recover.contained-changes in place
// of recover.small-changes; a 0.4 payload still carries both, and each
// method scores only the markers it defines.
var MarkerCatalog = []MarkerDefinition{
	{ID: "understand.instructions", Step: StepUnderstand, Unit: UnitDays},
	{ID: "understand.commands", Step: StepUnderstand, Unit: UnitDays},
	{ID: "understand.area-docs", Step: StepUnderstand, Unit: UnitDays},
	{ID: "bound.declared-areas", Step: StepBound, Unit: UnitRatio},
	{ID: "bound.boundary-rules", Step: StepBound, Unit: UnitDays},
	{ID: "bound.cross-area-changes", Step: StepBound, Unit: UnitRatio},
	{ID: "verify.tests", Step: StepVerify, Unit: UnitRatio},
	{ID: "verify.static-checks", Step: StepVerify, Unit: UnitDays},
	{ID: "verify.reliable-signal", Step: StepVerify, Unit: UnitCount},
	{ID: "verify.pinned-toolchain", Step: StepVerify, Unit: UnitDays},
	{ID: "recover.ownership", Step: StepRecover, Unit: UnitCount},
	{ID: "recover.small-changes", Step: StepRecover, Unit: UnitLines},
	{ID: "recover.contained-changes", Step: StepRecover, Unit: UnitRatio},
}

// LookupMarker returns the definition of a marker id.
func LookupMarker(id string) (MarkerDefinition, bool) {
	for _, definition := range MarkerCatalog {
		if definition.ID == id {
			return definition, true
		}
	}
	return MarkerDefinition{}, false
}
