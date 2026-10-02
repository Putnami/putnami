package apisurface

import (
	"go/ast"
	"go/types"
	"path"
	"strconv"
	"strings"
	"unicode"
)

// renderer renders the type expressions of one source file. imports maps each
// name the file gives an import to the import's path.
type renderer struct {
	imports map[string]string
}

// newRenderer returns the renderer of a parsed file. An import without an
// explicit name is known by the name its path implies, the rule goimports
// applies (assumedName); a blank or dot import names nothing.
func newRenderer(file *ast.File) renderer {
	imports := map[string]string{}
	for _, spec := range file.Imports {
		importPath, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			continue
		}
		name := assumedName(importPath)
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if name == "_" || name == "." || name == "" {
			continue
		}
		imports[name] = importPath
	}
	return renderer{imports: imports}
}

// assumedName is the package name an import path implies: its last element,
// or the one before a major version suffix such as /v2, without a "go-"
// prefix, and cut at the first character an identifier cannot hold. A package
// whose name differs from it is rendered by the name the file uses.
func assumedName(importPath string) string {
	base := path.Base(importPath)
	if strings.HasPrefix(base, "v") {
		if _, err := strconv.Atoi(base[1:]); err == nil {
			if dir := path.Dir(importPath); dir != "." {
				base = path.Base(dir)
			}
		}
	}
	base = strings.TrimPrefix(base, "go-")
	if i := strings.IndexFunc(base, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_'
	}); i >= 0 {
		base = base[:i]
	}
	return base
}

// renamedIdent is an identifier a rendering renamed, with its name before.
type renamedIdent struct {
	ident *ast.Ident
	name  string
}

// renderType renders a type expression. The rendering is
// go/types.ExprString's: one line, no comments, no struct tags, and no
// dependence on how the source was formatted. Both renderings drop parameter
// and result names and write each package qualifier as its import path, so
// renaming a parameter or an import is not a change.
//
// The Key also replaces each type parameter params names by its placeholder
// and spells the predeclared aliases one way: any as interface{}, byte as
// uint8 and rune as int32. A Key is therefore equal for two spellings of one
// type, which is what a comparison reads.
//
// It drops parameter names in place, and restores every identifier it renames,
// so rendering an expression twice gives the same Text.
func (r renderer) renderType(expr ast.Expr, params map[string]string) Text {
	dropParameterNames(expr)
	renamed := r.qualify(expr)
	display := types.ExprString(expr)
	renamed = append(renamed, normalize(expr, params)...)
	key := types.ExprString(expr)
	for i := len(renamed) - 1; i >= 0; i-- {
		renamed[i].ident.Name = renamed[i].name
	}
	return Text{Key: key, Display: display}
}

// renderSignature renders a function type without its type parameters, which
// renderTypeParams renders apart.
func (r renderer) renderSignature(fn *ast.FuncType, params map[string]string) Text {
	return r.renderType(&ast.FuncType{Params: fn.Params, Results: fn.Results}, params)
}

// renderTypeParams renders a type parameter list as "[T any, U comparable]".
// The Key names each parameter by its placeholder.
func (r renderer) renderTypeParams(list *ast.FieldList, params map[string]string) Text {
	if list == nil || len(list.List) == 0 {
		return Text{}
	}
	var display, key []string
	for _, field := range list.List {
		constraint := r.renderType(field.Type, params)
		for _, name := range field.Names {
			display = append(display, name.Name+" "+constraint.Display)
			placeholder, ok := params[name.Name]
			if !ok {
				placeholder = "_"
			}
			key = append(key, placeholder+" "+constraint.Key)
		}
	}
	return Text{
		Key:     "[" + strings.Join(key, ", ") + "]",
		Display: "[" + strings.Join(display, ", ") + "]",
	}
}

// qualify renames the package of every qualified identifier below root to the
// import path the file gives it, and returns what it renamed. A qualifier the
// file does not import keeps its spelling.
func (r renderer) qualify(root ast.Node) []renamedIdent {
	var renamed []renamedIdent
	seen := map[*ast.Ident]bool{}
	ast.Inspect(root, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		qualifier, ok := selector.X.(*ast.Ident)
		if !ok || seen[qualifier] {
			return true
		}
		seen[qualifier] = true
		if importPath, ok := r.imports[qualifier.Name]; ok && importPath != qualifier.Name {
			renamed = append(renamed, renamedIdent{ident: qualifier, name: qualifier.Name})
			qualifier.Name = importPath
		}
		return true
	})
	return renamed
}

// predeclaredAliases are the predeclared names that denote another type, and
// the spelling a Key uses for each.
var predeclaredAliases = map[string]string{
	"any":  "interface{}",
	"byte": "uint8",
	"rune": "int32",
}

// normalize renames, below root, every identifier that names a type parameter
// to its placeholder, and every predeclared alias to its Key spelling, and
// returns what it renamed. Field, method and package names, and qualified
// names, are not type references and keep their spelling.
func normalize(root ast.Node, params map[string]string) []renamedIdent {
	names := map[*ast.Ident]bool{}
	ast.Inspect(root, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			names[n.Sel] = true
			if qualifier, ok := n.X.(*ast.Ident); ok {
				names[qualifier] = true
			}
		case *ast.Field:
			for _, name := range n.Names {
				names[name] = true
			}
		}
		return true
	})
	var renamed []renamedIdent
	ast.Inspect(root, func(node ast.Node) bool {
		ident, ok := node.(*ast.Ident)
		if !ok || names[ident] {
			return true
		}
		names[ident] = true
		spelling, ok := params[ident.Name]
		if !ok {
			spelling, ok = predeclaredAliases[ident.Name]
		}
		if ok {
			renamed = append(renamed, renamedIdent{ident: ident, name: ident.Name})
			ident.Name = spelling
		}
		return true
	})
	return renamed
}

// dropParameterNames rewrites every function type below root so its
// parameters and results are unnamed, one field per value: `func(a, b int)`
// becomes `func(int, int)`.
func dropParameterNames(root ast.Node) {
	ast.Inspect(root, func(node ast.Node) bool {
		if fn, ok := node.(*ast.FuncType); ok {
			fn.Params = unnamed(fn.Params)
			fn.Results = unnamed(fn.Results)
		}
		return true
	})
}

func unnamed(list *ast.FieldList) *ast.FieldList {
	if list == nil {
		return nil
	}
	out := &ast.FieldList{Opening: list.Opening, Closing: list.Closing}
	for _, field := range list.List {
		count := max(len(field.Names), 1)
		for range count {
			out.List = append(out.List, &ast.Field{Type: field.Type})
		}
	}
	return out
}
