package transaction

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"

	diag "go.putnami.dev/protocol/diagnostic"
)

// This file defines the cross-language behavioral conformance corpus for the
// transaction / unit-of-work protocol: a single, ordered, typed scenario list
// (the manifest) that BOTH the Go and TypeScript database adapters execute
// against a real Postgres, asserting the same typed Outcome. It mirrors
// protocols/events/conformance.go — declaration + strict parse/validate only,
// no database driver — so the manifest's own shape is conformance-checked by the
// non-gated guard tests even without a live server, while the two adapters'
// gated integration runners interpret the identical manifest.json.
//
// The corpus drives the tx/CAS primitives DIRECTLY (CompareAndSet / ConsumeOnce
// / Rotate wrapped in WithTx / runInTransaction), NOT through the HTTP
// middleware: the two languages' HTTP boundary policies differ by design, so
// the primitive level is where cross-language Outcome parity is exact.

// ConformanceProtocol / ConformanceSuite identify the transaction conformance
// corpus. They are pinned constants a manifest must declare so a runner cannot
// silently execute an unrelated document.
const (
	ConformanceProtocol = "putnami.transaction.v1"
	ConformanceSuite    = "putnami.transaction.conformance.v1"
)

// Conformance diagnostic codes. These are deliberately NOT part of
// ValidErrorCodes (the UnitOfWork/Result taxonomy) — they classify manifest
// validation, a separate concern — mirroring how protocols/events keeps its
// conformance codes out of the wire-shape taxonomy.
const (
	ConformanceErrParse       = "conformance.parse_error"
	ConformanceErrProtocol    = "conformance.invalid_protocol"
	ConformanceErrSuite       = "conformance.invalid_suite"
	ConformanceErrVersion     = "conformance.invalid_protocol_version"
	ConformanceErrRequired    = "conformance.required_field"
	ConformanceErrDuplicate   = "conformance.duplicate_id"
	ConformanceErrEnum        = "conformance.invalid_enum"
	ConformanceErrReference   = "conformance.invalid_reference"
	ConformanceErrArgs        = "conformance.invalid_args"
	ConformanceErrConcurrency = "conformance.invalid_concurrency"
	ConformanceErrIdentifier  = "conformance.invalid_identifier"
)

// conformanceIdent matches a safe, lowercase SQL identifier for a setup table or
// column. Both runners quote identifiers before emitting DDL, but pinning a
// closed shape here keeps a manifest reviewable and the two runners' generated
// DDL byte-identical.
var conformanceIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// Level is the conformance level of a case. Closed enum.
type Level string

// Level values.
const (
	LevelRequired    Level = "required"
	LevelRecommended Level = "recommended"
	LevelOptional    Level = "optional"
)

// Valid reports whether l is a recognized level.
func (l Level) Valid() bool {
	switch l {
	case LevelRequired, LevelRecommended, LevelOptional:
		return true
	default:
		return false
	}
}

// Primitive is the tx/CAS primitive an operation invokes. Closed enum; the value
// maps one-to-one onto the Go (CompareAndSet/ConsumeOnce/Rotate) and TypeScript
// (compareAndSet/consumeOnce/rotate) repository methods.
type Primitive string

// Primitive values.
const (
	PrimitiveCompareAndSet Primitive = "compare-and-set"
	PrimitiveConsumeOnce   Primitive = "consume-once"
	PrimitiveRotate        Primitive = "rotate"
)

// Valid reports whether p is a recognized primitive.
func (p Primitive) Valid() bool {
	switch p {
	case PrimitiveCompareAndSet, PrimitiveConsumeOnce, PrimitiveRotate:
		return true
	default:
		return false
	}
}

// Boundary is the transaction boundary an operation runs inside. Closed enum.
//   - BoundaryNone runs the primitive in autocommit.
//   - BoundaryTransaction wraps it in a single WithTx / runInTransaction.
//   - BoundaryNestedTransaction wraps it in an inner WithTx joined to an outer
//     WithTx on the same pool (join-outer, no savepoints), so the outer boundary
//     governs the shared fate of the inner write.
//
// Runner rule: a transaction (or nested) boundary COMMITS iff the operation's
// Outcome is OutcomeApplied and no fault was injected; any other outcome (or an
// injected fault) rolls the boundary back. This keeps a conflict from
// half-committing and makes the corpus's rollback assertions deterministic
// across both languages.
type Boundary string

// Boundary values.
const (
	BoundaryNone              Boundary = "none"
	BoundaryTransaction       Boundary = "transaction"
	BoundaryNestedTransaction Boundary = "nested-transaction"
)

// Valid reports whether b is a recognized boundary.
func (b Boundary) Valid() bool {
	switch b {
	case BoundaryNone, BoundaryTransaction, BoundaryNestedTransaction:
		return true
	default:
		return false
	}
}

// Fault is an abnormal exit injected inside a boundary AFTER the primitive has
// run, exercising the rollback + connection-release invariant. Closed enum. The
// corpus pins the portable trigger (callback-error) that both runtimes model
// identically; each adapter keeps its native panic (Go) / timeout (both) tests
// locally, because those triggers are not portable.
type Fault string

// Fault values.
const (
	FaultNone          Fault = "none"
	FaultCallbackError Fault = "callback-error"
)

// Valid reports whether f is a recognized fault.
func (f Fault) Valid() bool {
	switch f {
	case FaultNone, FaultCallbackError:
		return true
	default:
		return false
	}
}

// ColumnType is a setup column's Postgres type. Closed enum, deliberately small
// so both runners render identical DDL.
type ColumnType string

// ColumnType values.
const (
	ColumnText    ColumnType = "text"
	ColumnBoolean ColumnType = "boolean"
	ColumnInteger ColumnType = "integer"
)

// Valid reports whether c is a recognized column type.
func (c ColumnType) Valid() bool {
	switch c {
	case ColumnText, ColumnBoolean, ColumnInteger:
		return true
	default:
		return false
	}
}

// Manifest is the ordered transaction conformance corpus both adapters execute.
type Manifest struct {
	Schema string `json:"$schema,omitempty"`
	// Protocol pins ConformanceProtocol.
	Protocol string `json:"protocol"`
	// Suite pins ConformanceSuite.
	Suite string `json:"suite"`
	// ProtocolVersion pins the transaction ProtocolVersion.
	ProtocolVersion int `json:"protocolVersion"`
	// Cases is the ordered scenario list, run top-to-bottom by both runners.
	Cases []Case `json:"cases"`
}

// Case is one behavioral scenario: a setup (tables + seed), a list of operations
// over the tx/CAS primitives, optional concurrency groups running operations in
// parallel with an expected outcome multiset, and optional row assertions the
// runner checks after the operations.
type Case struct {
	// ID uniquely identifies the case.
	ID string `json:"id"`
	// Level is the conformance level.
	Level Level `json:"level"`
	// Summary is an optional human description; never load-bearing.
	Summary string `json:"summary,omitempty"`
	// Setup declares the tables (DDL + seed rows) the case runs against.
	Setup Setup `json:"setup"`
	// Operations are the primitive invocations. A sequential operation carries an
	// Expect; a concurrent operation is referenced by exactly one Concurrency
	// group and carries no Expect.
	Operations []Operation `json:"operations"`
	// Concurrency groups operations that run in parallel, each asserting an
	// outcome multiset rather than a per-operation outcome.
	Concurrency []Concurrency `json:"concurrency,omitempty"`
	// Asserts are post-condition row checks (e.g. "the row is still unconsumed"),
	// the observable proof that a rollback undid a write.
	Asserts []RowAssert `json:"asserts,omitempty"`
}

// Setup declares the tables a case provisions.
type Setup struct {
	// Tables are the tables provisioned (created and seeded) before the case's
	// operations run.
	Tables []Table `json:"tables"`
}

// Table is a single setup table: its name, columns, and optional seed rows. Both
// runners render the identical CREATE TABLE and INSERT statements from it.
type Table struct {
	// Name is the table's SQL identifier (a safe lowercase identifier; both
	// runners quote it before emitting DDL).
	Name string `json:"name"`
	// Columns are the table's columns, rendered into CREATE TABLE in order.
	Columns []Column `json:"columns"`
	// Seed is the optional set of rows inserted after the table is created.
	Seed []Row `json:"seed,omitempty"`
}

// Column is one setup column.
type Column struct {
	// Name is the column's SQL identifier.
	Name string `json:"name"`
	// Type is the column's Postgres type (closed enum so both runners emit
	// identical DDL).
	Type ColumnType `json:"type"`
	// PrimaryKey marks the column as the table's PRIMARY KEY.
	PrimaryKey bool `json:"primaryKey,omitempty"`
	// NotNull adds a NOT NULL constraint to the column.
	NotNull bool `json:"notNull,omitempty"`
	// Default is an optional raw SQL default expression (e.g. "false"). It is
	// emitted verbatim into the column's DEFAULT clause, so keep it to simple
	// literals both runtimes render identically.
	Default string `json:"default,omitempty"`
}

// Row is a seed row: a column-name → value map. Values are JSON scalars
// (string / bool / number / null) bound as positional parameters.
type Row map[string]any

// Operation is one primitive invocation.
type Operation struct {
	// ID uniquely identifies the operation within its case.
	ID string `json:"id"`
	// Table names the setup table the operation targets.
	Table string `json:"table"`
	// Primitive selects the tx/CAS primitive.
	Primitive Primitive `json:"primitive"`
	// Boundary is the transaction boundary the primitive runs inside (default
	// BoundaryNone).
	Boundary Boundary `json:"boundary,omitempty"`
	// Fault injects an abnormal exit inside the boundary after the primitive runs
	// (default FaultNone).
	Fault Fault `json:"fault,omitempty"`
	// Repeat runs the operation this many times (default 1). Used to stress the
	// rollback + connection-release path.
	Repeat int `json:"repeat,omitempty"`
	// Args carries the primitive arguments.
	Args OpArgs `json:"args"`
	// Expect is the expected Outcome for a sequential operation. It is empty for
	// an operation referenced by a Concurrency group (asserted via the multiset).
	Expect Outcome `json:"expect,omitempty"`
}

// OpArgs is the union of arguments across the primitives. Each primitive reads
// the subset it needs; the validator enforces the required subset per primitive.
type OpArgs struct {
	// KeyColumn is the column identifying the target row; shared by all primitives.
	KeyColumn string `json:"keyColumn,omitempty"`
	// Key is the KeyColumn value selecting the target row.
	Key any `json:"key,omitempty"`

	// Column is the value column CompareAndSet reads and writes.
	Column string `json:"column,omitempty"`
	// Expected is the value CompareAndSet requires Column to currently hold
	// (may be null to compare against SQL NULL).
	Expected any `json:"expected,omitempty"`
	// Next is the value CompareAndSet writes to Column when Expected matches.
	Next any `json:"next,omitempty"`

	// ClaimGuard is the column ConsumeOnce checks to ensure the row has not
	// already been claimed.
	ClaimGuard string `json:"claimGuard,omitempty"`
	// Set is the column ConsumeOnce writes to record the one-time claim.
	Set string `json:"set,omitempty"`

	// PredecessorKey is the key of the row Rotate revokes.
	PredecessorKey any `json:"predecessorKey,omitempty"`
	// StateColumn is the lifecycle-state column Rotate updates on the predecessor.
	StateColumn string `json:"stateColumn,omitempty"`
	// Revoked is the state value Rotate writes to the predecessor's StateColumn.
	Revoked any `json:"revoked,omitempty"`
	// SuccessorColumns names the columns of the successor row Rotate inserts,
	// positionally paired with SuccessorValues.
	SuccessorColumns []string `json:"successorColumns,omitempty"`
	// SuccessorValues are the values of the successor row Rotate inserts,
	// positionally paired with SuccessorColumns.
	SuccessorValues []any `json:"successorValues,omitempty"`
}

// Concurrency groups operations that run in parallel. Its Expect is an outcome
// multiset: the set of outcomes the group's operations must collectively emit
// (e.g. exactly one applied + one already-consumed-conflict for two concurrent
// consumers), independent of which operation produced which.
type Concurrency struct {
	// ID uniquely identifies the concurrency group within its case.
	ID string `json:"id"`
	// Operations lists the IDs of the case's operations run in parallel; each
	// referenced operation belongs to exactly one group.
	Operations []string `json:"operations"`
	// Expect is the outcome multiset the group must collectively emit: each
	// Outcome mapped to the exact number of operations that must produce it.
	Expect map[Outcome]int `json:"expect"`
}

// RowAssert is a post-operation count check: the number of rows in Table
// matching the raw SQL predicate Where must equal Count. It is the observable
// proof that a boundary committed or rolled back as required.
type RowAssert struct {
	// Table is the table the count check runs against.
	Table string `json:"table"`
	// Where is the raw SQL predicate selecting the rows to count.
	Where string `json:"where"`
	// Count is the exact number of matching rows the assertion requires.
	Count int `json:"count"`
}

// ParseManifest decodes a Manifest from JSON in strict mode (unknown fields
// rejected). A non-nil manifest is returned only when decoding produced no
// errors.
func ParseManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, []diag.Diagnostic{diag.Errorf(ConformanceErrParse, "", "failed to parse conformance manifest: %v", err)}
	}
	return &m, nil
}

// ParseAndValidateManifest runs strict parsing followed by structural
// validation.
func ParseAndValidateManifest(data []byte) (*Manifest, []diag.Diagnostic) {
	m, diags := ParseManifest(data)
	if diag.HasErrors(diags) {
		return nil, diags
	}
	return m, append(diags, ValidateManifest(m)...)
}

// ValidateManifest checks the corpus's structural invariants: pinned
// protocol/suite/version, unique case ids, closed enums, table/operation
// references that resolve, and — the load-bearing concurrency invariant — that
// every operation is either sequential (with an expected Outcome) or referenced
// by exactly one concurrency group whose expected multiset sums to the group
// size.
func ValidateManifest(m *Manifest) []diag.Diagnostic {
	if m == nil {
		return []diag.Diagnostic{diag.Errorf(ConformanceErrParse, "", "conformance manifest is nil")}
	}
	var diags []diag.Diagnostic
	if m.Protocol != ConformanceProtocol {
		diags = append(diags, diag.Errorf(ConformanceErrProtocol, "protocol", "expected %q, got %q", ConformanceProtocol, m.Protocol))
	}
	if m.Suite != ConformanceSuite {
		diags = append(diags, diag.Errorf(ConformanceErrSuite, "suite", "expected %q, got %q", ConformanceSuite, m.Suite))
	}
	if m.ProtocolVersion != ProtocolVersion {
		diags = append(diags, diag.Errorf(ConformanceErrVersion, "protocolVersion", "expected %d, got %d", ProtocolVersion, m.ProtocolVersion))
	}
	if len(m.Cases) == 0 {
		diags = append(diags, diag.Errorf(ConformanceErrRequired, "cases", "conformance manifest requires at least one case"))
	}
	seen := map[string]bool{}
	for i := range m.Cases {
		diags = append(diags, validateCase(i, &m.Cases[i], seen)...)
	}
	return diags
}

func validateCase(index int, c *Case, seen map[string]bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if c.ID == "" {
		diags = append(diags, diag.Errorf(ConformanceErrRequired, "cases", "case at index %d requires id", index))
	} else if seen[c.ID] {
		diags = append(diags, diag.Errorf(ConformanceErrDuplicate, "cases", "duplicate conformance case %q", c.ID))
	} else {
		seen[c.ID] = true
	}
	if !c.Level.Valid() {
		diags = append(diags, diag.Errorf(ConformanceErrEnum, "level", "case %q has unknown level %q", c.ID, c.Level))
	}

	tables := map[string]map[string]bool{}
	diags = append(diags, validateSetup(c, tables)...)

	opIDs := map[string]bool{}
	for i := range c.Operations {
		diags = append(diags, validateOperation(c, &c.Operations[i], tables, opIDs)...)
	}

	// grouped tracks which operations a concurrency group claims, so the
	// sequential/concurrent partition below can flag an operation that is neither
	// (unreachable) or both (ambiguous).
	grouped := map[string]int{}
	diags = append(diags, validateConcurrency(c, opIDs, grouped)...)

	for i := range c.Operations {
		op := &c.Operations[i]
		inGroup := grouped[op.ID] > 0
		hasExpect := op.Expect != ""
		switch {
		case inGroup && hasExpect:
			diags = append(diags, diag.Errorf(ConformanceErrConcurrency, "operations",
				"case %q operation %q is in a concurrency group and must not also carry expect", c.ID, op.ID))
		case !inGroup && !hasExpect:
			diags = append(diags, diag.Errorf(ConformanceErrRequired, "operations",
				"case %q operation %q must carry expect or be referenced by a concurrency group", c.ID, op.ID))
		case hasExpect && !op.Expect.Valid():
			diags = append(diags, diag.Errorf(ConformanceErrEnum, "operations",
				"case %q operation %q has unknown expect outcome %q", c.ID, op.ID, op.Expect))
		}
	}

	for i := range c.Asserts {
		a := c.Asserts[i]
		if _, ok := tables[a.Table]; !ok {
			diags = append(diags, diag.Errorf(ConformanceErrReference, "asserts",
				"case %q assert references unknown table %q", c.ID, a.Table))
		}
		if a.Count < 0 {
			diags = append(diags, diag.Errorf(ConformanceErrArgs, "asserts",
				"case %q assert count must be >= 0, got %d", c.ID, a.Count))
		}
	}
	return diags
}

func validateSetup(c *Case, tables map[string]map[string]bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if len(c.Setup.Tables) == 0 {
		diags = append(diags, diag.Errorf(ConformanceErrRequired, "setup.tables", "case %q requires at least one setup table", c.ID))
	}
	for _, tbl := range c.Setup.Tables {
		if !conformanceIdent.MatchString(tbl.Name) {
			diags = append(diags, diag.Errorf(ConformanceErrIdentifier, "setup.tables",
				"case %q table name %q is not a safe identifier", c.ID, tbl.Name))
		}
		if len(tbl.Columns) == 0 {
			diags = append(diags, diag.Errorf(ConformanceErrRequired, "setup.tables",
				"case %q table %q requires at least one column", c.ID, tbl.Name))
		}
		cols := map[string]bool{}
		for _, col := range tbl.Columns {
			if !conformanceIdent.MatchString(col.Name) {
				diags = append(diags, diag.Errorf(ConformanceErrIdentifier, "setup.tables",
					"case %q table %q column name %q is not a safe identifier", c.ID, tbl.Name, col.Name))
			}
			if !col.Type.Valid() {
				diags = append(diags, diag.Errorf(ConformanceErrEnum, "setup.tables",
					"case %q table %q column %q has unknown type %q", c.ID, tbl.Name, col.Name, col.Type))
			}
			cols[col.Name] = true
		}
		for _, row := range tbl.Seed {
			for key := range row {
				if !cols[key] {
					diags = append(diags, diag.Errorf(ConformanceErrReference, "setup.tables",
						"case %q table %q seed row references unknown column %q", c.ID, tbl.Name, key))
				}
			}
		}
		tables[tbl.Name] = cols
	}
	return diags
}

func validateOperation(c *Case, op *Operation, tables map[string]map[string]bool, seen map[string]bool) []diag.Diagnostic {
	var diags []diag.Diagnostic
	if op.ID == "" {
		diags = append(diags, diag.Errorf(ConformanceErrRequired, "operations", "case %q has an operation with no id", c.ID))
	} else if seen[op.ID] {
		diags = append(diags, diag.Errorf(ConformanceErrDuplicate, "operations", "case %q has duplicate operation id %q", c.ID, op.ID))
	} else {
		seen[op.ID] = true
	}
	if _, ok := tables[op.Table]; !ok {
		diags = append(diags, diag.Errorf(ConformanceErrReference, "operations",
			"case %q operation %q references unknown table %q", c.ID, op.ID, op.Table))
	}
	if !op.Primitive.Valid() {
		diags = append(diags, diag.Errorf(ConformanceErrEnum, "operations",
			"case %q operation %q has unknown primitive %q", c.ID, op.ID, op.Primitive))
	}
	if op.Boundary != "" && !op.Boundary.Valid() {
		diags = append(diags, diag.Errorf(ConformanceErrEnum, "operations",
			"case %q operation %q has unknown boundary %q", c.ID, op.ID, op.Boundary))
	}
	if op.Fault != "" && !op.Fault.Valid() {
		diags = append(diags, diag.Errorf(ConformanceErrEnum, "operations",
			"case %q operation %q has unknown fault %q", c.ID, op.ID, op.Fault))
	}
	if op.Repeat < 0 {
		diags = append(diags, diag.Errorf(ConformanceErrArgs, "operations",
			"case %q operation %q repeat must be >= 0, got %d", c.ID, op.ID, op.Repeat))
	}
	diags = append(diags, validateArgs(c, op)...)
	return diags
}

func validateArgs(c *Case, op *Operation) []diag.Diagnostic {
	var diags []diag.Diagnostic
	req := func(field, value string) {
		if value == "" {
			diags = append(diags, diag.Errorf(ConformanceErrArgs, "operations.args",
				"case %q operation %q (%s) requires args.%s", c.ID, op.ID, op.Primitive, field))
		}
	}
	reqValue := func(field string, value any) {
		if value == nil {
			diags = append(diags, diag.Errorf(ConformanceErrArgs, "operations.args",
				"case %q operation %q (%s) requires args.%s", c.ID, op.ID, op.Primitive, field))
		}
	}
	switch op.Primitive {
	case PrimitiveCompareAndSet:
		req("keyColumn", op.Args.KeyColumn)
		req("column", op.Args.Column)
		reqValue("key", op.Args.Key)
		reqValue("next", op.Args.Next)
	case PrimitiveConsumeOnce:
		req("keyColumn", op.Args.KeyColumn)
		req("claimGuard", op.Args.ClaimGuard)
		req("set", op.Args.Set)
		reqValue("key", op.Args.Key)
	case PrimitiveRotate:
		req("keyColumn", op.Args.KeyColumn)
		req("stateColumn", op.Args.StateColumn)
		reqValue("predecessorKey", op.Args.PredecessorKey)
		reqValue("revoked", op.Args.Revoked)
		if len(op.Args.SuccessorColumns) == 0 {
			diags = append(diags, diag.Errorf(ConformanceErrArgs, "operations.args",
				"case %q operation %q (rotate) requires args.successorColumns", c.ID, op.ID))
		}
		if len(op.Args.SuccessorColumns) != len(op.Args.SuccessorValues) {
			diags = append(diags, diag.Errorf(ConformanceErrArgs, "operations.args",
				"case %q operation %q (rotate) successorColumns/successorValues length mismatch: %d != %d",
				c.ID, op.ID, len(op.Args.SuccessorColumns), len(op.Args.SuccessorValues)))
		}
	}
	return diags
}

func validateConcurrency(c *Case, opIDs map[string]bool, grouped map[string]int) []diag.Diagnostic {
	var diags []diag.Diagnostic
	seenGroups := map[string]bool{}
	for _, g := range c.Concurrency {
		if g.ID == "" {
			diags = append(diags, diag.Errorf(ConformanceErrRequired, "concurrency", "case %q has a concurrency group with no id", c.ID))
		} else if seenGroups[g.ID] {
			diags = append(diags, diag.Errorf(ConformanceErrDuplicate, "concurrency", "case %q has duplicate concurrency group %q", c.ID, g.ID))
		} else {
			seenGroups[g.ID] = true
		}
		if len(g.Operations) < 2 {
			diags = append(diags, diag.Errorf(ConformanceErrConcurrency, "concurrency",
				"case %q concurrency group %q must reference at least two operations", c.ID, g.ID))
		}
		for _, id := range g.Operations {
			if !opIDs[id] {
				diags = append(diags, diag.Errorf(ConformanceErrReference, "concurrency",
					"case %q concurrency group %q references unknown operation %q", c.ID, g.ID, id))
				continue
			}
			grouped[id]++
			if grouped[id] > 1 {
				diags = append(diags, diag.Errorf(ConformanceErrConcurrency, "concurrency",
					"case %q operation %q is referenced by more than one concurrency group", c.ID, id))
			}
		}
		total := 0
		if len(g.Expect) == 0 {
			diags = append(diags, diag.Errorf(ConformanceErrRequired, "concurrency",
				"case %q concurrency group %q requires an expected outcome multiset", c.ID, g.ID))
		}
		for outcome, count := range g.Expect {
			if !outcome.Valid() {
				diags = append(diags, diag.Errorf(ConformanceErrEnum, "concurrency",
					"case %q concurrency group %q expects unknown outcome %q", c.ID, g.ID, outcome))
			}
			if count <= 0 {
				diags = append(diags, diag.Errorf(ConformanceErrConcurrency, "concurrency",
					"case %q concurrency group %q outcome %q count must be > 0, got %d", c.ID, g.ID, outcome, count))
			}
			total += count
		}
		if len(g.Expect) > 0 && total != len(g.Operations) {
			diags = append(diags, diag.Errorf(ConformanceErrConcurrency, "concurrency",
				"case %q concurrency group %q expected multiset sums to %d but references %d operations",
				c.ID, g.ID, total, len(g.Operations)))
		}
	}
	return diags
}

// EffectiveRepeat returns the operation's repeat count, treating the zero value
// as a single run. Both runners call this so a manifest that omits repeat and
// one that sets repeat:1 behave identically.
func (op Operation) EffectiveRepeat() int {
	if op.Repeat <= 0 {
		return 1
	}
	return op.Repeat
}

// EffectiveBoundary returns the operation's boundary, treating the empty value
// as BoundaryNone.
func (op Operation) EffectiveBoundary() Boundary {
	if op.Boundary == "" {
		return BoundaryNone
	}
	return op.Boundary
}

// String renders a diagnostic-friendly identity for an operation.
func (op Operation) String() string {
	return fmt.Sprintf("%s(%s)", op.ID, op.Primitive)
}
