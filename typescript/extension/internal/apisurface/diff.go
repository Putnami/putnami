package apisurface

import (
	"fmt"
	"slices"
	"strings"
)

// Change is one incompatible change between two surfaces.
type Change struct {
	// Package is the importer's name of the entry point: the package name with
	// the subpath.
	Package string
	// Message states the change, such as "function greet removed".
	Message string
	// File and Line locate the declaration in the working tree; File is empty
	// when the declaration is gone.
	File string
	Line int
	// Warning says the change may or may not break an importer: a change of
	// written type, or one the check cannot see whole. The check reports it
	// without failing. A change without it breaks an importer whatever the
	// types are.
	Warning bool
}

// Reason says why the exports of a subpath were not compared.
type Reason int

const (
	// MissingNow is a target the working tree does not hold, such as build
	// output that is not committed.
	MissingNow Reason = iota
	// MissingThen is a target the release did not hold.
	MissingThen
	// Script is a released target with no import and no export, or one that
	// assigns its exports with `export =`: a script or a CommonJS module.
	Script
)

// Skipped is one subpath whose exports were not compared.
type Skipped struct {
	Subpath string
	// Target is the file of the condition that stops the comparison.
	Target string
	Reason Reason
}

// Comparison is the answer of Compare.
type Comparison struct {
	// Changes are the incompatible changes, in subpath then name order.
	Changes []Change
	// Subpaths is the number of subpaths the release declared.
	Subpaths int
	// Compared is the number of those subpaths whose exports were compared.
	Compared int
	// Skipped are the subpaths both sides declare whose exports were not
	// compared, with why.
	Skipped []Skipped
}

// unwritten stands for a type the source does not write, in a message.
const unwritten = "<inferred>"

// Incompatible returns the changes of Compare.
func Incompatible(name string, old, current *Surface) []Change {
	return Compare(name, old, current).Changes
}

// Compare returns the changes from old to current that an importer of the
// package name can see. They are of two kinds.
//
// A change that breaks an importer whatever the types are is an error: a
// subpath, an export or a member that is gone; a value exported as a type only,
// or a type that is now a value only; a class or an enum that stops being one;
// a class that becomes abstract; a member or a constructor
// with less visibility, or a property that can no longer be written; a
// constructor that took parameters and is gone.
//
// Any other change is a warning (Change.Warning): a change of written type,
// such as a signature, the type of a variable or a member, a type alias, type
// parameters, heritage clauses or a member added as required; another kind of
// declaration that keeps its uses, such as an interface written as a type
// alias; and a removal the check cannot confirm, because a base or an
// `export *` it cannot read may still provide the name. Whether such a change
// breaks an importer depends on what the check does not read. Every change in
// a declaration the package does not export (Entry.Referenced) is a warning
// too: importers have no name for it.
//
// An importer imports a subpath, and the conditions of the subpath select a
// file for it: the exports of a subpath are what any of its conditions
// exports. A name is gone only when no condition of the subpath exports it
// now. The exports of a subpath are compared when a condition of the release
// names a module the check reads, and every condition now names a file the
// working tree holds.
//
// A type the source does not write is not compared. Neither is a name another
// package declares.
func Compare(name string, old, current *Surface) Comparison {
	var result Comparison
	for _, subpath := range subpaths(old.Entries) {
		result.Subpaths++
		pkg := PackageName(name, subpath)
		then := conditions(old.Entries, subpath)
		now := conditions(current.Entries, subpath)
		if len(now) == 0 {
			result.Changes = append(result.Changes, Change{Package: pkg, Message: "entry point removed"})
			continue
		}
		if gone, ok := deletedTarget(then, now); ok {
			result.Changes = append(result.Changes, Change{Package: pkg,
				Message: fmt.Sprintf("entry point names %s, which the release held and the working tree does not", gone)})
			continue
		}
		if slices.ContainsFunc(then, func(entry Entry) bool { return entry.State == TargetRead }) &&
			!slices.ContainsFunc(now, func(entry Entry) bool { return entry.State != TargetAsset }) {
			result.Changes = append(result.Changes, Change{Package: pkg,
				Message: fmt.Sprintf("entry point now names %s, which is not a source module", now[0].Target)})
			continue
		}
		if missing := slices.IndexFunc(now, func(entry Entry) bool { return entry.State == TargetMissing }); missing >= 0 {
			result.Skipped = append(result.Skipped, Skipped{Subpath: subpath, Target: now[missing].Target, Reason: MissingNow})
			continue
		}
		var read []Entry
		for _, entry := range then {
			if entry.State == TargetRead && !entry.Script {
				read = append(read, entry)
			}
		}
		if len(read) == 0 {
			for _, entry := range then {
				switch {
				case entry.State == TargetMissing:
					result.Skipped = append(result.Skipped, Skipped{Subpath: subpath, Target: entry.Target, Reason: MissingThen})
				case entry.State == TargetRead:
					result.Skipped = append(result.Skipped, Skipped{Subpath: subpath, Target: entry.Target, Reason: Script})
				}
			}
			continue
		}
		result.Compared++
		var held []Entry
		for _, entry := range now {
			if entry.State == TargetRead {
				held = append(held, entry)
			}
		}
		for _, change := range compareEntry(merge(read), merge(held)) {
			change.Package = pkg
			if !slices.Contains(result.Changes, change) {
				result.Changes = append(result.Changes, change)
			}
		}
	}
	return result
}

// subpathOf returns the subpath of an entry: Entry.Subpath, else the part of
// its key before the conditions.
func subpathOf(entry Entry) string {
	if entry.Subpath != "" {
		return entry.Subpath
	}
	subpath, _, _ := strings.Cut(entry.Key, " ")
	return subpath
}

// subpaths returns the subpaths of entries, in order.
func subpaths(entries []Entry) []string {
	var found []string
	for _, entry := range entries {
		if subpath := subpathOf(entry); !slices.Contains(found, subpath) {
			found = append(found, subpath)
		}
	}
	slices.Sort(found)
	return found
}

// conditions returns the entries of one subpath.
func conditions(entries []Entry, subpath string) []Entry {
	var found []Entry
	for _, entry := range entries {
		if subpathOf(entry) == subpath {
			found = append(found, entry)
		}
	}
	return found
}

// deletedTarget returns a target that a condition names now, that the working
// tree does not hold, and that a condition of the release named and held: the
// file was deleted while the package still declares it.
func deletedTarget(then, now []Entry) (string, bool) {
	for _, entry := range now {
		if entry.State != TargetMissing {
			continue
		}
		if slices.ContainsFunc(then, func(released Entry) bool {
			return released.State == TargetRead && released.Target == entry.Target
		}) {
			return entry.Target, true
		}
	}
	return "", false
}

// merge returns one entry that exports what any of entries exports.
func merge(entries []Entry) Entry {
	var merged Entry
	seen := map[string]bool{}
	add := func(into *[]Declaration, declaration Declaration) {
		key := declaration.Name + "\x00" + declaration.Kind + "\x00" + declaration.File + "\x00" + declaration.ID
		if !seen[key] {
			seen[key] = true
			*into = append(*into, declaration)
		}
	}
	for _, entry := range entries {
		for _, declaration := range entry.Exports {
			add(&merged.Exports, declaration)
		}
		for _, declaration := range entry.Referenced {
			add(&merged.Referenced, declaration)
		}
		for _, star := range entry.Stars {
			if !slices.Contains(merged.Stars, star) {
				merged.Stars = append(merged.Stars, star)
			}
		}
	}
	return merged
}

// comparer compares the declarations of one subpath.
type comparer struct {
	// oldTypes and types are the declarations a heritage clause can name on
	// each side: the exported ones and the referenced ones, by Declaration.ID.
	oldTypes, types map[string]Declaration
	// stars are the `export *` of the working tree the reader cannot follow:
	// a name gone from the exports may still come through one of them.
	stars []string
}

// typesOf returns the declarations of an entry that have members, by ID.
func typesOf(entry Entry) map[string]Declaration {
	types := map[string]Declaration{}
	for _, declarations := range [][]Declaration{entry.Referenced, entry.Exports} {
		for _, declaration := range declarations {
			if declaration.Members != nil && declaration.ID != "" {
				types[declaration.ID] = declaration
			}
		}
	}
	return types
}

func compareEntry(old, current Entry) []Change {
	var changes []Change
	// The reader cannot read what an `export *` it cannot follow exports, on
	// either side: it is not compared, whatever its specifier says.
	var stars []string
	for _, star := range append(slices.Clone(old.Stars), current.Stars...) {
		if !slices.Contains(stars, star) {
			stars = append(stars, star)
			changes = append(changes, Change{Warning: true,
				Message: fmt.Sprintf("export * from %q is not compared: the check cannot read the module it names", star)})
		}
	}
	byName := map[string][]Declaration{}
	for _, declaration := range current.Exports {
		byName[declaration.Name] = append(byName[declaration.Name], declaration)
	}
	c := comparer{oldTypes: typesOf(old), types: typesOf(current), stars: current.Stars}
	for _, declaration := range old.Exports {
		changes = append(changes, c.compareExport(declaration, byName[declaration.Name])...)
	}
	// Importers have no name for a referenced declaration: every change to one
	// is a warning.
	for _, declaration := range old.Referenced {
		if candidate, ok := counterpart(declaration, old.Referenced, current.Referenced); ok {
			for _, change := range c.compareDeclaration(declaration, candidate, nil) {
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
// reader resolved it in the declaring file and types holds its members.
func base(types map[string]Declaration, declaration Declaration, index int) (Declaration, bool) {
	if index >= len(declaration.Bases) || declaration.Bases[index] == "" {
		return Declaration{}, false
	}
	found, ok := types[declaration.Bases[index]]
	return found, ok
}

// inherited returns the members the bases of a declaration bring, through
// every level, from types. complete is false when the check cannot read one of
// the bases: a declaration of another package, a namespace member, a mixin
// call.
func inherited(types map[string]Declaration, declaration Declaration, seen map[string]bool) (members []Member, complete bool) {
	complete = true
	for i, clause := range declaration.Heritage {
		if !strings.HasPrefix(clause, "extends ") {
			continue
		}
		found, ok := base(types, declaration, i)
		if !ok {
			complete = false
			continue
		}
		if seen[found.ID] {
			continue
		}
		seen[found.ID] = true
		members = append(members, found.Members...)
		above, all := inherited(types, found, seen)
		members = append(members, above...)
		complete = complete && all
	}
	return members, complete
}

// inheritedConstructor returns the constructor a class without one of its own
// takes: the one of the nearest base class that declares one. ok is false when
// no base declares one; complete is false when the check cannot read a base it
// passes on the way.
func inheritedConstructor(types map[string]Declaration, class Declaration) (constructor Member, ok, complete bool) {
	seen := map[string]bool{class.ID: true}
	for {
		next := -1
		for i, clause := range class.Heritage {
			if strings.HasPrefix(clause, "extends ") {
				next = i
				break
			}
		}
		if next < 0 {
			return Member{}, false, true
		}
		found, readable := base(types, class, next)
		if !readable {
			return Member{}, false, false
		}
		if seen[found.ID] {
			return Member{}, false, true
		}
		seen[found.ID] = true
		if index := slices.IndexFunc(found.Members, func(member Member) bool { return member.Kind == "constructor" }); index >= 0 {
			return found.Members[index], true, true
		}
		class = found
	}
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
		if len(c.stars) > 0 {
			return []Change{{Warning: true, Message: fmt.Sprintf("%s removed, unless an export * the check cannot read exports it", old.label())}}
		}
		return []Change{{Message: old.label() + " removed"}}
	}
	if old.opaque() {
		return nil
	}
	var other *Declaration
	var merged []Declaration
	for i := range candidates {
		if candidates[i].typeSpace() && !old.typeSpace() {
			merged = append(merged, candidates[i])
		}
	}
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.opaque() {
			return nil
		}
		if candidate.typeSpace() == old.typeSpace() {
			return c.compareDeclaration(old, *candidate, merged)
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
// value, or both name a type. merged are the interfaces of the name that
// merge with a current class: their members are the class's too.
func (c comparer) compareDeclaration(old, current Declaration, merged []Declaration) []Change {
	label := old.label()
	if old.Kind != current.Kind {
		message := fmt.Sprintf("%s is now %s", label, withArticle(current.Kind))
		switch {
		case old.Kind == "function" && current.Kind == "variable" && current.Type == "",
			old.Kind == "variable" && old.Type == "" && current.Kind == "function":
			// A variable without a written type may hold the same function.
			return nil
		case old.typeSpace() && old.Members != nil && current.Members != nil:
			// An interface and an alias of an object type declare the same
			// members: they are compared below.
		case old.Kind == "class", old.Kind == "enum":
			// Importers lose `new`, `instanceof` and `extends` of a class, or the
			// members of an enum.
			return []Change{changed(current, message)}
		default:
			// The name keeps its uses: an interface and a type alias both name a
			// type, and a function and a variable both name a value.
			return []Change{typeChanged(current, message)}
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
	if old.Unread || current.Unread {
		changes = append(changes, typeChanged(current, fmt.Sprintf(
			"the members of namespace %s are not compared: the check cannot read the whole module it names", old.Name)))
	} else {
		for _, name := range old.Namespace {
			if !slices.Contains(current.Namespace, name) {
				changes = append(changes, changed(current, fmt.Sprintf("namespace member %s.%s removed", old.Name, name)))
			}
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
		changes = append(changes, c.compareMembers(old, current, merged)...)
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
		found, ok := base(c.types, current, i)
		if !ok {
			changes = append(changes, typeChanged(current, fmt.Sprintf(
				"%s now extends %s, which the check cannot read: it may add a required member", old.label(), name)))
			continue
		}
		members, complete := inherited(c.types, found, map[string]bool{found.ID: true})
		if !complete {
			changes = append(changes, typeChanged(current, fmt.Sprintf(
				"%s now extends %s, whose own bases the check cannot read: they may add a required member", old.label(), name)))
			continue
		}
		var required []string
		for _, member := range append(slices.Clone(found.Members), members...) {
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
// or enums. A member counts as kept when the current declaration declares it,
// inherits it from a base it reads, or, for a class, takes it from an
// interface that merges with it. A member the release declared or inherited
// that none of these holds is removed; when a current base cannot be read, it
// may still provide the member, and the removal is a warning.
func (c comparer) compareMembers(old, current Declaration, merged []Declaration) []Change {
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
	provided, complete := inherited(c.types, current, map[string]bool{current.ID: true})
	if current.Kind == "class" {
		for _, iface := range merged {
			above, all := inherited(c.types, iface, map[string]bool{iface.ID: true})
			provided = append(append(provided, iface.Members...), above...)
			complete = complete && all
		}
	}
	lookup := func(member Member) (Member, bool, bool) {
		if now, found := find(current.Members, member.Name); found {
			return now, true, true
		}
		if member.Kind == "constructor" {
			if current.Kind != "class" {
				return Member{}, false, true
			}
			constructor, found, all := inheritedConstructor(c.types, current)
			return constructor, found, all
		}
		now, found := find(provided, member.Name)
		return now, found, complete
	}
	removed := func(member Member, label string, readable bool) {
		switch {
		case !readable:
			warn(member, label+" removed, unless a base the check cannot read declares it")
		default:
			changes = append(changes, changed(current, label+" removed"))
		}
	}
	for _, member := range old.Members {
		// A private member was never part of the API.
		if member.Access == "private" {
			continue
		}
		label := memberLabel(old, member)
		now, found, readable := lookup(member)
		if !found {
			// A class that declares no constructor and inherits none takes one
			// without parameters: only a call that passed arguments breaks.
			if member.Kind != "constructor" || slices.ContainsFunc(member.Signatures, func(signature Signature) bool { return len(signature.Params) > 0 }) {
				removed(member, label, readable)
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
	// A member the release inherited from a base it read is part of the API
	// as much as one it declared.
	released, _ := inherited(c.oldTypes, old, map[string]bool{old.ID: true})
	var reported []string
	for _, member := range released {
		if member.Access == "private" || member.Kind == "constructor" || slices.Contains(reported, member.Name) {
			continue
		}
		if _, own := find(old.Members, member.Name); own {
			continue
		}
		reported = append(reported, member.Name)
		if _, found, readable := lookup(member); !found {
			removed(member, memberLabel(old, member), readable)
		}
	}
	for _, member := range current.Members {
		if _, found := find(old.Members, member.Name); found {
			continue
		}
		if _, found := find(released, member.Name); found {
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
