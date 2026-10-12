package apisurface

import (
	"fmt"
	"slices"
	"strings"
)

// Change is one incompatible change between two surfaces.
type Change struct {
	// Package is the importer's name of the entry point: the package name with
	// the subpath, and the conditions when they are not the default one.
	Package string
	// Message states the change, such as "function greet removed".
	Message string
	// File and Line locate the declaration in the working tree; File is empty
	// when the declaration is gone.
	File string
	Line int
	// Warning says the change is one of written type: it may or may not break
	// an importer, and the check reports it without failing. A change without
	// it breaks an importer whatever the types are.
	Warning bool
}

// unwritten stands for a type the source does not write, in a message.
const unwritten = "<inferred>"

// Incompatible returns the changes from old to current that an importer of
// the package name can see, in entry point then name order. They are of two
// kinds.
//
// A change that breaks an importer whatever the types are is an error: an
// entry point, an export or a member that is gone; a declaration of another
// kind, such as a class that becomes abstract or a value exported as a type
// only; a member or a constructor with less visibility, or a property that
// can no longer be written; a constructor that took parameters and is gone.
//
// A change of written type is a warning (Change.Warning): a signature, the
// type of a variable or of a member, a type alias, type parameters, heritage
// clauses, a member added as required. Whether it breaks an importer depends
// on types the check does not have. Every change in a declaration the package
// does not export (Entry.Referenced) is a warning too: importers have no name
// for it.
//
// A type the source does not write is not compared. Neither is a name another
// package declares. The exports of an entry point are compared only when both
// sides read its target as a module.
func Incompatible(name string, old, current *Surface) []Change {
	var changes []Change
	for _, entry := range old.Entries {
		pkg := PackageName(name, entry.Key)
		index := slices.IndexFunc(current.Entries, func(candidate Entry) bool { return candidate.Key == entry.Key })
		if index < 0 {
			changes = append(changes, Change{Package: pkg, Message: "entry point removed"})
			continue
		}
		now := current.Entries[index]
		if entry.State == TargetRead && now.State == TargetAsset {
			changes = append(changes, Change{Package: pkg, Message: fmt.Sprintf("entry point now names %s, which is not a source module", now.Target)})
			continue
		}
		if !Compared(entry, now) {
			continue
		}
		for _, change := range compareEntry(entry, now) {
			change.Package = pkg
			changes = append(changes, change)
		}
	}
	return changes
}

// Compared reports whether the exports of an entry point are compared: both
// sides read its target, and the released one is a module.
func Compared(old, current Entry) bool {
	return old.State == TargetRead && current.State == TargetRead && !old.Script
}

// comparer compares the declarations of one entry point.
type comparer struct {
	// types are the current declarations a heritage clause can name: the
	// exported ones and the referenced ones, by Declaration.ID.
	types map[string]Declaration
}

func compareEntry(old, current Entry) []Change {
	var changes []Change
	for _, star := range old.Stars {
		if !slices.Contains(current.Stars, star) {
			changes = append(changes, Change{Message: fmt.Sprintf("export * from %q removed", star)})
		}
	}
	byName := map[string][]Declaration{}
	c := comparer{types: map[string]Declaration{}}
	for _, declaration := range current.Referenced {
		if declaration.Members != nil {
			c.types[declaration.ID] = declaration
		}
	}
	for _, declaration := range current.Exports {
		byName[declaration.Name] = append(byName[declaration.Name], declaration)
		if declaration.Members != nil && declaration.ID != "" {
			c.types[declaration.ID] = declaration
		}
	}
	for _, declaration := range old.Exports {
		changes = append(changes, c.compareExport(declaration, byName[declaration.Name])...)
	}
	// Importers have no name for a referenced declaration: every change to one
	// is a warning.
	for _, declaration := range old.Referenced {
		if candidate, ok := counterpart(declaration, old.Referenced, current.Referenced); ok {
			for _, change := range c.compareDeclaration(declaration, candidate) {
				change.Warning = true
				changes = append(changes, change)
			}
		}
	}
	return changes
}

// counterpart returns the current referenced declaration that stands for an
// old one: the declaration of the same name in the same file, else the one of
// the same name when each side has exactly one, which is a declaration whose
// file moved. A declaration nothing refers to any more has no counterpart: the
// declaration that named it changed, and that change is reported.
func counterpart(old Declaration, olds, currents []Declaration) (Declaration, bool) {
	same := func(candidate Declaration) bool {
		return candidate.Name == old.Name && candidate.typeSpace() == old.typeSpace()
	}
	var named []Declaration
	for _, candidate := range currents {
		if !same(candidate) {
			continue
		}
		if candidate.File == old.File {
			return candidate, true
		}
		named = append(named, candidate)
	}
	before := 0
	for _, candidate := range olds {
		if same(candidate) {
			before++
		}
	}
	if len(named) == 1 && before == 1 {
		return named[0], true
	}
	return Declaration{}, false
}

// base returns the declaration the heritage clause at index names, when the
// reader resolved it in the declaring file and the check reads its members.
func (c comparer) base(declaration Declaration, index int) (Declaration, bool) {
	if index >= len(declaration.Bases) || declaration.Bases[index] == "" {
		return Declaration{}, false
	}
	base, ok := c.types[declaration.Bases[index]]
	return base, ok
}

// inherited returns the members the bases of a declaration bring, through
// every level. complete is false when the check cannot read one of the bases.
func (c comparer) inherited(declaration Declaration, seen map[string]bool) (members []Member, complete bool) {
	complete = true
	for i, clause := range declaration.Heritage {
		if !strings.HasPrefix(clause, "extends ") {
			continue
		}
		base, ok := c.base(declaration, i)
		if !ok {
			complete = false
			continue
		}
		if seen[base.ID] {
			continue
		}
		seen[base.ID] = true
		members = append(members, base.Members...)
		above, all := c.inherited(base, seen)
		members = append(members, above...)
		complete = complete && all
	}
	return members, complete
}

// opaque reports whether the reader has no declaration to compare: the name
// comes from another package, or its declaration was not found.
func (d Declaration) opaque() bool {
	return d.Kind == "external" || d.Kind == "unknown"
}

// typeSpace reports whether the declaration names a type only. A class and an
// enum name a value as well, so they are compared as values.
func (d Declaration) typeSpace() bool {
	return d.Kind == "interface" || d.Kind == "type"
}

// label names a declaration in a message.
func (d Declaration) label() string {
	switch d.Kind {
	case "external":
		return "re-export " + d.Name
	case "unknown":
		return "export " + d.Name
	}
	return d.Kind + " " + d.Name
}

// compareExport compares one old declaration with the current declarations of
// the same name.
func (c comparer) compareExport(old Declaration, candidates []Declaration) []Change {
	if len(candidates) == 0 {
		return []Change{{Message: old.label() + " removed"}}
	}
	if old.opaque() {
		return nil
	}
	var other *Declaration
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.opaque() {
			return nil
		}
		if candidate.typeSpace() == old.typeSpace() {
			return c.compareDeclaration(old, *candidate)
		}
		other = candidate
	}
	return []Change{changed(*other, fmt.Sprintf("%s is now %s", old.label(), withArticle(other.Kind)))}
}

func changed(current Declaration, message string) Change {
	return Change{Message: message, File: current.File, Line: current.Line}
}

// typeChanged is a change of written type: a warning.
func typeChanged(current Declaration, message string) Change {
	change := changed(current, message)
	change.Warning = true
	return change
}

func withArticle(kind string) string {
	if strings.ContainsRune("aeiou", rune(kind[0])) {
		return "an " + kind
	}
	return "a " + kind
}

// compareDeclaration compares two declarations of one name that both name a
// value, or both name a type.
func (c comparer) compareDeclaration(old, current Declaration) []Change {
	label := old.label()
	if old.Kind != current.Kind {
		switch {
		case old.Kind == "function" && current.Kind == "variable" && current.Type == "",
			old.Kind == "variable" && old.Type == "" && current.Kind == "function":
			// A variable without a written type may hold the same function.
			return nil
		case old.typeSpace() && old.Members != nil && current.Members != nil:
			// An interface and an alias of an object type declare the same
			// members: they are compared below.
		default:
			return []Change{changed(current, fmt.Sprintf("%s is now %s", label, withArticle(current.Kind)))}
		}
	}
	var changes []Change
	if !old.typeSpace() && !old.TypeOnly && current.TypeOnly {
		changes = append(changes, changed(current, label+" is now exported as a type only"))
	}
	if !old.Abstract && current.Abstract {
		changes = append(changes, changed(current, label+" is now abstract"))
	}
	if old.Kind == "enum" && !old.Const && current.Const {
		changes = append(changes, changed(current, label+" is now a const enum"))
	}
	for _, name := range old.Namespace {
		if !slices.Contains(current.Namespace, name) {
			changes = append(changes, changed(current, fmt.Sprintf("namespace member %s.%s removed", old.Name, name)))
		}
	}
	if !typeParamsCompatible(old.TypeParams, current.TypeParams) {
		changes = append(changes, typeChanged(current, fmt.Sprintf("%s changed its type parameters from %s to %s",
			label, renderTypeParams(old.TypeParams), renderTypeParams(current.TypeParams))))
	}
	switch {
	case old.Kind == "function":
		if message := compareSignatures(old.Signatures, current.Signatures); message != "" {
			changes = append(changes, typeChanged(current, label+" "+message))
		}
	case old.Kind == "variable":
		if old.Type != "" && current.Type != "" && old.Type != current.Type {
			changes = append(changes, typeChanged(current, fmt.Sprintf("%s changed type from %s to %s", label, old.Type, current.Type)))
		}
	case old.Kind == "type" && (old.Members == nil || current.Members == nil):
		switch {
		case old.Members == nil && current.Members == nil:
			if old.Type != current.Type {
				changes = append(changes, typeChanged(current, fmt.Sprintf("%s changed from %s to %s", label, old.Type, current.Type)))
			}
		case old.Members == nil:
			changes = append(changes, typeChanged(current, fmt.Sprintf("%s changed from %s to an object type", label, old.Type)))
		default:
			changes = append(changes, typeChanged(current, fmt.Sprintf("%s changed from an object type to %s", label, current.Type)))
		}
	default:
		for _, clause := range old.Heritage {
			if !slices.Contains(current.Heritage, clause) {
				changes = append(changes, typeChanged(current, fmt.Sprintf("%s no longer %s", label, clause)))
			}
		}
		if old.Kind == "interface" && current.Kind == "interface" {
			changes = append(changes, c.addedBases(old, current)...)
		}
		changes = append(changes, c.compareMembers(old, current)...)
	}
	return changes
}

// addedBases reports each base an interface gains that can add a required
// member: one the check cannot read, or one that brings a required member the
// interface did not already have.
func (c comparer) addedBases(old, current Declaration) []Change {
	var changes []Change
	for i, clause := range current.Heritage {
		if slices.Contains(old.Heritage, clause) || !strings.HasPrefix(clause, "extends ") {
			continue
		}
		name := strings.TrimPrefix(clause, "extends ")
		base, ok := c.base(current, i)
		if !ok {
			changes = append(changes, typeChanged(current, fmt.Sprintf(
				"%s now extends %s, which the check cannot read: it may add a required member", old.label(), name)))
			continue
		}
		members, complete := c.inherited(base, map[string]bool{base.ID: true})
		if !complete {
			changes = append(changes, typeChanged(current, fmt.Sprintf(
				"%s now extends %s, whose own bases the check cannot read: they may add a required member", old.label(), name)))
			continue
		}
		var required []string
		for _, member := range append(slices.Clone(base.Members), members...) {
			had := slices.ContainsFunc(old.Members, func(own Member) bool { return own.Name == member.Name })
			if !had && !member.Optional && member.Access != "private" && !slices.Contains(required, member.Name) {
				required = append(required, member.Name)
			}
		}
		if len(required) > 0 {
			changes = append(changes, typeChanged(current, fmt.Sprintf(
				"%s now extends %s, which adds the required member%s %s: every value of the type built outside the package must have %s",
				old.label(), name, plural(len(required)), strings.Join(required, ", "), itOrThem(len(required)))))
		}
	}
	return changes
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

func itOrThem(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// compareMembers compares the members of two classes, interfaces, object types
// or enums.
func (c comparer) compareMembers(old, current Declaration) []Change {
	var changes []Change
	at := func(member Member, message string) {
		changes = append(changes, Change{Message: message, File: current.File, Line: member.Line})
	}
	warn := func(member Member, message string) {
		changes = append(changes, Change{Message: message, File: current.File, Line: member.Line, Warning: true})
	}
	find := func(members []Member, name string) (Member, bool) {
		index := slices.IndexFunc(members, func(member Member) bool { return member.Name == name })
		if index < 0 {
			return Member{}, false
		}
		return members[index], true
	}
	for _, member := range old.Members {
		// A private member was never part of the API.
		if member.Access == "private" {
			continue
		}
		label := memberLabel(old, member)
		now, found := find(current.Members, member.Name)
		if !found && member.Kind != "constructor" {
			// A base of the current declaration may still provide the member.
			inherited, _ := c.inherited(current, map[string]bool{current.ID: true})
			now, found = find(inherited, member.Name)
		}
		if !found {
			// A class that writes no constructor takes its base class's, or one
			// without parameters: a call that passed arguments no longer holds.
			if member.Kind != "constructor" || slices.ContainsFunc(member.Signatures, func(signature Signature) bool { return len(signature.Params) > 0 }) {
				changes = append(changes, changed(current, label+" removed"))
			}
			continue
		}
		if now.Access != "" && now.Access != member.Access {
			at(now, fmt.Sprintf("%s is now %s", label, now.Access))
			continue
		}
		switch {
		case member.Kind == "enum member":
			if member.Type != "" && now.Type != "" && member.Type != now.Type {
				warn(now, fmt.Sprintf("%s changed value from %s to %s", label, member.Type, now.Type))
			}
			continue
		case len(member.Signatures) > 0 && len(now.Signatures) > 0:
			if message := compareSignatures(member.Signatures, now.Signatures); message != "" {
				warn(now, label+" "+message)
			}
		case len(member.Signatures) > 0 && now.Type != "", member.Type != "" && len(now.Signatures) > 0:
			warn(now, fmt.Sprintf("%s is now %s", label, withArticle(now.Kind)))
		case member.Type != "" && now.Type != "" && member.Type != now.Type:
			warn(now, fmt.Sprintf("%s changed type from %s to %s", label, member.Type, now.Type))
		}
		switch {
		case member.Optional && !now.Optional:
			warn(now, label+" is now required")
		case !member.Optional && now.Optional:
			warn(now, label+" is now optional")
		}
		if !member.Abstract && now.Abstract {
			warn(now, label+" is now abstract")
		}
		if !member.Readonly && now.Readonly {
			at(now, label+" is now read-only")
		}
	}
	for _, member := range current.Members {
		if _, found := find(old.Members, member.Name); found {
			continue
		}
		label := memberLabel(current, member)
		switch {
		case member.Kind == "constructor" && member.Access != "":
			at(member, fmt.Sprintf("%s is now %s", label, member.Access))
		case member.Access == "private":
			// A private member is not part of the API.
		case current.typeSpace() && (member.Kind == "property" || member.Kind == "method") && !member.Optional:
			warn(member, label+" added as required: every value of the type built outside the package must have it")
		case member.Abstract:
			warn(member, label+" added as abstract: every subclass outside the package must implement it")
		case member.Kind == "constructor" && !slices.ContainsFunc(old.Heritage, func(clause string) bool { return strings.HasPrefix(clause, "extends ") }) &&
			!slices.ContainsFunc(member.Signatures, func(signature Signature) bool { return requiredParams(signature) == 0 }):
			warn(member, label+" added with required parameters")
		}
	}
	return changes
}

// memberLabel names a member in a message, such as "method Client.send" or
// "static property Client.count".
func memberLabel(owner Declaration, member Member) string {
	name, static := strings.CutPrefix(member.Name, "static ")
	prefix := ""
	if static {
		prefix = "static "
	}
	switch member.Kind {
	case "constructor":
		return "constructor of " + owner.Name
	case "call signature", "construct signature", "index signature":
		return fmt.Sprintf("%s %s of %s", member.Kind, name, owner.Name)
	}
	return fmt.Sprintf("%s%s %s.%s", prefix, member.Kind, owner.Name, name)
}

// compareSignatures returns how the call signatures changed, or "" when every
// old signature still has the current one at its position that accepts its
// calls with the same written types.
//
// Overloads are compared in declaration order, because a call takes the first
// overload that accepts it: an overload is compatible only when it is added
// after the ones that existed.
func compareSignatures(old, current []Signature) string {
	single := len(old) == 1 && len(current) == 1
	for i, signature := range old {
		switch {
		case i >= len(current):
			return fmt.Sprintf("no longer has the signature %s", renderSignature(signature))
		case signatureCompatible(signature, current[i]):
		case single:
			return fmt.Sprintf("changed from %s to %s", renderSignature(signature), renderSignature(current[i]))
		default:
			return fmt.Sprintf("changed overload %d from %s to %s", i+1, renderSignature(signature), renderSignature(current[i]))
		}
	}
	return ""
}

// signatureCompatible reports whether every call old accepts, current accepts
// with the same written types: the parameters keep their types, a required
// parameter may become optional, and an added parameter is optional.
func signatureCompatible(old, current Signature) bool {
	if !typeParamsCompatible(old.TypeParams, current.TypeParams) || len(current.Params) < len(old.Params) {
		return false
	}
	// A `this` parameter that is added or changed constrains every call.
	if current.This != "" && current.This != old.This {
		return false
	}
	for i, param := range current.Params {
		if i >= len(old.Params) {
			if !param.Optional && !param.Rest {
				return false
			}
			continue
		}
		before := old.Params[i]
		if before.Rest != param.Rest || (before.Optional && !param.Optional) || !sameWritten(before.Type, param.Type) {
			return false
		}
	}
	return sameWritten(old.Returns, current.Returns)
}

// sameWritten reports whether two written types are the same; a type one side
// does not write is not compared.
func sameWritten(old, current string) bool {
	return old == "" || current == "" || old == current
}

// typeParamsCompatible reports whether a use of the old type parameters is a
// use of the current ones: the old ones keep their constraints and their
// defaults, and every added one has a default.
func typeParamsCompatible(old, current []TypeParam) bool {
	if len(current) < len(old) {
		return false
	}
	for i, param := range current {
		if i >= len(old) {
			if param.Default == "" {
				return false
			}
			continue
		}
		if old[i].Constraint != param.Constraint || (old[i].Default != "" && old[i].Default != param.Default) {
			return false
		}
	}
	return true
}

func requiredParams(signature Signature) int {
	count := 0
	for _, param := range signature.Params {
		if !param.Optional && !param.Rest {
			count++
		}
	}
	return count
}

func renderTypeParams(params []TypeParam) string {
	if len(params) == 0 {
		return "none"
	}
	parts := make([]string, len(params))
	for i, param := range params {
		parts[i] = fmt.Sprintf("$%d", i)
		if param.Constraint != "" {
			parts[i] += " extends " + param.Constraint
		}
		if param.Default != "" {
			parts[i] += " = " + param.Default
		}
	}
	return "<" + strings.Join(parts, ", ") + ">"
}

// renderSignature writes a signature the way a function type is written, with
// types only: "(string, boolean?) => string".
func renderSignature(signature Signature) string {
	params := make([]string, len(signature.Params))
	for i, param := range signature.Params {
		text := param.Type
		if text == "" {
			text = unwritten
		}
		switch {
		case param.Rest:
			text = "..." + text
		case param.Optional && strings.Contains(text, " => "):
			text = "(" + text + ")?"
		case param.Optional:
			text += "?"
		}
		params[i] = text
	}
	if signature.This != "" {
		params = append([]string{"this: " + signature.This}, params...)
	}
	returns := signature.Returns
	if returns == "" {
		returns = unwritten
	}
	typeParams := ""
	if len(signature.TypeParams) > 0 {
		typeParams = renderTypeParams(signature.TypeParams)
	}
	return fmt.Sprintf("%s(%s) => %s", typeParams, strings.Join(params, ", "), returns)
}
