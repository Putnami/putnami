package workspace

// Visibility is a project's import boundary: which other projects of the
// workspace may import it.
//
// It is declared by the project that is imported, never by the importer, and it
// governs REAL imports — a `go.mod` require or replace, a `package.json`
// dependency on a workspace package — never the `dependencies` a putnami.json
// declares. A declared edge with no import behind it is a phantom edge, and a
// boundary enforced on phantom edges would be a boundary nothing in the build
// actually respects.
//
// A service declares nothing: its contract is its public surface, so a contract
// edge from a generated client to its provider is always allowed, and the
// provider's implementation stays private.
type Visibility string

const (
	// VisibilityScope makes a project importable by the projects of its own
	// scope only. It is the default.
	VisibilityScope Visibility = "scope"
	// VisibilityPublic makes a project importable from anywhere in the
	// workspace.
	VisibilityPublic Visibility = "public"
)

// DefaultVisibility is the effective visibility of a project that declares
// none. It is the CLOSED value: a project is private to its scope until its
// owner states otherwise, so a new shared library is a decision someone takes
// rather than one the default makes for them.
const DefaultVisibility = VisibilityScope

// ValidVisibilities is the closed set, in declaration order.
var ValidVisibilities = []Visibility{VisibilityScope, VisibilityPublic}

// Valid reports whether v is a member of the closed set. The empty value is
// NOT valid: it means "nothing was declared", which ResolveVisibility answers,
// not this.
func (v Visibility) Valid() bool {
	for _, known := range ValidVisibilities {
		if v == known {
			return true
		}
	}
	return false
}

// ResolveVisibility answers the effective visibility of a declaration. An
// empty or unknown value resolves to DefaultVisibility: an unknown value is
// already an authoring error the validator reports, and resolving it to the
// closed default keeps the boundary from widening on a typo.
func ResolveVisibility(v Visibility) Visibility {
	if v.Valid() {
		return v
	}
	return DefaultVisibility
}

// DependencySource names the manifest family one dependency edge was derived
// from. It is the edge's provenance, and it is what separates an edge the build
// really performs from one a putnami.json merely declares.
type DependencySource string

const (
	// DependencySourceDeclared is an edge a declaration states: putnami.json
	// `dependencies`, or an equivalent declaration inside a provider's own
	// manifest. Nothing in the source tree has to import the target for it to
	// exist.
	DependencySourceDeclared DependencySource = "declared"
	// DependencySourceGoModule is an edge a go.mod `require` or `replace` of a
	// workspace module states.
	DependencySourceGoModule DependencySource = "go-module"
	// DependencySourcePackageJSON is an edge a package.json dependency on a
	// workspace package states.
	DependencySourcePackageJSON DependencySource = "package-json"
	// DependencySourceContract is the edge from a generated client to the
	// provider whose contract it was generated from. It is derived by core from
	// the committed client manifest and is never reported by a provider.
	DependencySourceContract DependencySource = "contract"
)

// ValidDependencySources is the closed set, in declaration order.
var ValidDependencySources = []DependencySource{
	DependencySourceDeclared,
	DependencySourceGoModule,
	DependencySourcePackageJSON,
	DependencySourceContract,
}

// Valid reports whether s is a member of the closed set.
func (s DependencySource) Valid() bool {
	for _, known := range ValidDependencySources {
		if s == known {
			return true
		}
	}
	return false
}

// ReportableByProvider reports whether a provider may claim this source for an
// edge it answers with. A provider reports what its own manifest family states;
// DependencySourceContract is core's own derivation and belongs to no
// provider's answer.
func (s DependencySource) ReportableByProvider() bool {
	return s.Valid() && s != DependencySourceContract
}

// IsImport reports whether this source is a real import — code that reads code
// — as opposed to a declaration. Visibility is enforced on imports only.
func (s DependencySource) IsImport() bool {
	return s == DependencySourceGoModule || s == DependencySourcePackageJSON
}

// StrongerDependencySource is the deterministic winner when two providers
// describe one edge. An import outranks a declaration, because one provider
// having read a real import is enough to make the edge an import; two import
// families are both true, and the lexicographically smaller one is kept so the
// merged answer never depends on provider order.
func StrongerDependencySource(a, b DependencySource) DependencySource {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	case a.IsImport() && !b.IsImport():
		return a
	case b.IsImport() && !a.IsImport():
		return b
	case a <= b:
		return a
	default:
		return b
	}
}
