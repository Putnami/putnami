package apisurface

import (
	"go/ast"
	"go/types"
	"strings"
)

// renderType renders a type expression. Parameter and result names are
// dropped from every function type it contains, and the Key replaces each
// type parameter params names by its placeholder. The rendering is
// go/types.ExprString's: one line, no comments, no struct tags, and no
// dependence on how the source was formatted.
//
// It rewrites expr in place, so each expression is rendered once.
func renderType(expr ast.Expr, params map[string]string) Text {
	dropParameterNames(expr)
	display := types.ExprString(expr)
	renameTypeParams(expr, params)
	return Text{Key: types.ExprString(expr), Display: display}
}

// renderSignature renders a function type without its type parameters, which
// renderTypeParams renders apart.
func renderSignature(fn *ast.FuncType, params map[string]string) Text {
	return renderType(&ast.FuncType{Params: fn.Params, Results: fn.Results}, params)
}

// renderTypeParams renders a type parameter list as "[T any, U comparable]".
// The Key names each parameter by its placeholder.
func renderTypeParams(list *ast.FieldList, params map[string]string) Text {
	if list == nil || len(list.List) == 0 {
		return Text{}
	}
	var display, key []string
	for _, field := range list.List {
		constraint := renderType(field.Type, params)
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

// renameTypeParams replaces every identifier below root that names a type
// parameter by the parameter's placeholder. Field, method and package-member
// names are names, not type references, and keep their spelling.
func renameTypeParams(root ast.Node, params map[string]string) {
	if len(params) == 0 {
		return
	}
	names := map[*ast.Ident]bool{}
	ast.Inspect(root, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.SelectorExpr:
			names[n.Sel] = true
		case *ast.Field:
			for _, name := range n.Names {
				names[name] = true
			}
		}
		return true
	})
	ast.Inspect(root, func(node ast.Node) bool {
		if ident, ok := node.(*ast.Ident); ok && !names[ident] {
			if placeholder, ok := params[ident.Name]; ok {
				ident.Name = placeholder
			}
		}
		return true
	})
}
