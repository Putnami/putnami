package apisurface

import (
	"fmt"
	"sort"
	"strings"
)

// Change is one incompatible difference between two surfaces: a change that
// can break code importing the package.
type Change struct {
	// Package is the import path of the package the change is in.
	Package string
	// Symbol names the changed declaration: "func Greet", "method Client.Do",
	// "field Config.Port". It is empty when the whole package is removed.
	Symbol string
	// Message states the change, symbol included: "func Greet removed".
	Message string
	// Pos is where the changed declaration now sits; zero when it is gone.
	Pos Position
}

// Incompatible returns every change from old to current that can break code
// importing one of old's packages, sorted by package, then symbol, then
// message. Additions are compatible, with one exception: a method added to an
// interface that other packages can implement.
func Incompatible(old, current *Surface) []Change {
	var changes []Change
	for _, importPath := range sortedKeys(old.Packages) {
		currentPackage, ok := current.Packages[importPath]
		if !ok {
			changes = append(changes, Change{Package: importPath, Message: "package removed"})
			continue
		}
		oldPackage := old.Packages[importPath]
		for _, name := range sortedKeys(oldPackage.Objects) {
			changes = append(changes, compareObject(importPath, oldPackage.Objects[name], currentPackage.Objects[name])...)
		}
	}
	sort.SliceStable(changes, func(i, j int) bool {
		if changes[i].Package != changes[j].Package {
			return changes[i].Package < changes[j].Package
		}
		if changes[i].Symbol != changes[j].Symbol {
			return changes[i].Symbol < changes[j].Symbol
		}
		return changes[i].Message < changes[j].Message
	})
	return changes
}

// changeSet collects the changes of one package.
type changeSet struct {
	pkg     string
	changes []Change
}

func (set *changeSet) add(symbol string, pos Position, format string, args ...any) {
	set.changes = append(set.changes, Change{
		Package: set.pkg,
		Symbol:  symbol,
		Message: symbol + " " + fmt.Sprintf(format, args...),
		Pos:     pos,
	})
}

func compareObject(pkg string, old, current *Object) []Change {
	set := &changeSet{pkg: pkg}
	symbol := string(old.Kind) + " " + old.Name
	switch {
	case current == nil:
		set.add(symbol, Position{}, "removed")
		return set.changes
	case current.Kind != old.Kind:
		set.add(symbol, current.Pos, "is now a %s", current.Kind)
		return set.changes
	}
	switch old.Kind {
	case KindFunc:
		if old.TypeParams.Key != current.TypeParams.Key {
			set.add(symbol, current.Pos, "changed its type parameters from %s to %s",
				orNone(old.TypeParams.Display), orNone(current.TypeParams.Display))
		}
		if old.Type.Key != current.Type.Key {
			set.add(symbol, current.Pos, "changed from %s to %s", old.Type.Display, current.Type.Display)
		}
	case KindVar, KindConst:
		if old.Type.Key != "" && current.Type.Key != "" && old.Type.Key != current.Type.Key {
			set.add(symbol, current.Pos, "changed type from %s to %s", old.Type.Display, current.Type.Display)
		}
	case KindType:
		compareType(set, old, current)
	}
	return set.changes
}

func orNone(params string) string {
	if params == "" {
		return "none"
	}
	return params
}

func compareType(set *changeSet, old, current *Object) {
	symbol := "type " + old.Name
	if old.TypeParams.Key != current.TypeParams.Key {
		set.add(symbol, current.Pos, "changed its type parameters from %s to %s",
			orNone(old.TypeParams.Display), orNone(current.TypeParams.Display))
	}
	switch {
	case old.Form != current.Form:
		set.add(symbol, current.Pos, "changed from %s to %s", describeForm(old), describeForm(current))
	case old.Form == formStruct:
		compareMembers(set, old.Name, "field", old.Fields, current.Fields)
	case old.Form == formInterface:
		compareInterface(set, old, current)
	case old.Type.Key != current.Type.Key:
		set.add(symbol, current.Pos, "changed from %s to %s", describeForm(old), describeForm(current))
	}
	compareMethods(set, old, current)
}

// describeForm renders what a type declaration declares, for a message.
func describeForm(object *Object) string {
	switch object.Form {
	case formStruct:
		return "a struct"
	case formInterface:
		return "an interface"
	case formAlias:
		return "an alias of " + object.Type.Display
	default:
		return object.Type.Display
	}
}

// compareMembers reports each old field that is gone or has another type.
func compareMembers(set *changeSet, owner, label string, old, current map[string]*Member) {
	for _, name := range sortedKeys(old) {
		symbol := label + " " + owner + "." + name
		member, ok := current[name]
		if !ok {
			set.add(symbol, Position{}, "removed")
			continue
		}
		if member.Type.Key != old[name].Type.Key {
			set.add(symbol, member.Pos, "changed type from %s to %s", old[name].Type.Display, member.Type.Display)
		}
	}
}

// compareMethods reports each old method of a type that is gone, has another
// signature, or moved from a value to a pointer receiver, which removes it
// from the value type's method set.
func compareMethods(set *changeSet, old, current *Object) {
	for _, name := range sortedKeys(old.Methods) {
		symbol := "method " + old.Name + "." + name
		before := old.Methods[name]
		after, ok := current.Methods[name]
		if !ok {
			set.add(symbol, Position{}, "removed")
			continue
		}
		if before.Type.Key != after.Type.Key {
			set.add(symbol, after.Pos, "changed from %s to %s", before.Type.Display, after.Type.Display)
		}
		if !before.Pointer && after.Pointer {
			set.add(symbol, after.Pos, "moved from receiver %s to *%s", old.Name, old.Name)
		}
	}
}

// compareInterface reports each interface element removed or changed, and
// each one added unless the old interface was sealed.
func compareInterface(set *changeSet, old, current *Object) {
	for _, name := range sortedKeys(old.Interface) {
		symbol := interfaceSymbol(old.Name, name, old.Interface[name])
		after, ok := current.Interface[name]
		if !ok {
			set.add(symbol, Position{}, "removed")
			continue
		}
		if after.Type.Key != old.Interface[name].Type.Key {
			set.add(symbol, after.Pos, "changed from %s to %s", old.Interface[name].Type.Display, after.Type.Display)
		}
	}
	if old.Sealed {
		return
	}
	for _, name := range sortedKeys(current.Interface) {
		if _, ok := old.Interface[name]; !ok {
			set.add(interfaceSymbol(old.Name, name, current.Interface[name]), current.Interface[name].Pos,
				"added: every implementation outside the package must add it")
		}
	}
}

func interfaceSymbol(owner, key string, member *Member) string {
	if strings.HasPrefix(key, embeddedPrefix) {
		return "interface " + owner + " embedded " + member.Type.Display
	}
	return "interface method " + owner + "." + key
}
