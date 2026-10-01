package extension

import (
	"strconv"
	"strings"
)

// ParamMap is the untyped parameter bag exposed to manifest expressions.
type ParamMap = map[string]any

// WhenContext provides the values resolved by a checked expression.
type WhenContext struct {
	Params        ParamMap
	Steps         map[string]*StepResult
	Commands      map[string]bool
	CommandParams map[string]ParamMap
	ProjectType   *string
}

// StepResult is the expression-visible outcome of a completed pipeline step.
type StepResult struct {
	Status string
	Data   map[string]any
}

// ValidateExpressionSyntax validates the manifest expression grammar without
// requiring runtime values for otherwise well-formed paths. It uses the same
// parser as EvaluateExpressionChecked so strict validation and planning cannot
// disagree about which operators, literals, or roots are valid.
func ValidateExpressionSyntax(expr string) bool {
	_, valid := evaluateExpressionChecked(strings.TrimSpace(expr), nil, true, true)
	return valid
}

// EvaluateExpressionChecked evaluates an expression and reports whether every
// operator and operand was valid and every referenced runtime path resolved.
func EvaluateExpressionChecked(expr string, ctx *WhenContext) (bool, bool) {
	return evaluateExpressionChecked(strings.TrimSpace(expr), ctx, true, false)
}

func evaluateExpressionChecked(expr string, ctx *WhenContext, allowEmpty, syntaxOnly bool) (bool, bool) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return allowEmpty, allowEmpty
	}

	if parts, found, valid := splitExpressionOperator(expr, "||"); !valid {
		return false, false
	} else if found {
		result := false
		for _, part := range parts {
			value, ok := evaluateExpressionChecked(part, ctx, false, syntaxOnly)
			if !ok {
				return false, false
			}
			result = result || value
		}
		return result, true
	}

	if parts, found, valid := splitExpressionOperator(expr, "&&"); !valid {
		return false, false
	} else if found {
		result := true
		for _, part := range parts {
			value, ok := evaluateExpressionChecked(part, ctx, false, syntaxOnly)
			if !ok {
				return false, false
			}
			result = result && value
		}
		return result, true
	}

	if strings.HasPrefix(expr, "!") {
		value, valid := evaluateExpressionChecked(expr[1:], ctx, false, syntaxOnly)
		if !valid {
			return false, false
		}
		return !value, true
	}

	if strings.HasPrefix(expr, "(") || strings.HasSuffix(expr, ")") {
		if !strings.HasPrefix(expr, "(") || !strings.HasSuffix(expr, ")") {
			return false, false
		}
		return evaluateExpressionChecked(expr[1:len(expr)-1], ctx, false, syntaxOnly)
	}

	if idx := findExpressionOperator(expr, "=="); idx >= 0 {
		left, leftValid := resolveCheckedExpressionValue(expr[:idx], ctx, syntaxOnly)
		right, rightValid := parseCheckedExpressionLiteral(expr[idx+2:], ctx, syntaxOnly)
		if !leftValid || !rightValid {
			return false, false
		}
		return expressionString(left) == expressionString(right), true
	}

	if idx := findExpressionOperator(expr, "!="); idx >= 0 {
		left, leftValid := resolveCheckedExpressionValue(expr[:idx], ctx, syntaxOnly)
		right, rightValid := parseCheckedExpressionLiteral(expr[idx+2:], ctx, syntaxOnly)
		if !leftValid || !rightValid {
			return false, false
		}
		return expressionString(left) != expressionString(right), true
	}

	value, valid := resolveCheckedExpressionValue(expr, ctx, syntaxOnly)
	if !valid {
		return false, false
	}
	return expressionTruthy(value), true
}

func splitExpressionOperator(expr, op string) ([]string, bool, bool) {
	var parts []string
	depth := 0
	var inQuote byte
	current := strings.Builder{}
	found := false

	for i := 0; i < len(expr); i++ {
		ch := expr[i]
		if inQuote != 0 {
			current.WriteByte(ch)
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			current.WriteByte(ch)
			continue
		}
		if ch == '(' {
			depth++
			current.WriteByte(ch)
			continue
		}
		if ch == ')' {
			depth--
			if depth < 0 {
				return nil, found, false
			}
			current.WriteByte(ch)
			continue
		}
		if depth == 0 && i+len(op) <= len(expr) && expr[i:i+len(op)] == op {
			part := strings.TrimSpace(current.String())
			if part == "" {
				return nil, true, false
			}
			parts = append(parts, part)
			current.Reset()
			found = true
			i += len(op) - 1
			continue
		}
		current.WriteByte(ch)
	}

	if inQuote != 0 || depth != 0 {
		return nil, found, false
	}
	if !found {
		return nil, false, true
	}
	last := strings.TrimSpace(current.String())
	if last == "" {
		return nil, true, false
	}
	return append(parts, last), true, true
}

func findExpressionOperator(expr, op string) int {
	var inQuote byte
	for i := 0; i < len(expr)-len(op)+1; i++ {
		ch := expr[i]
		if inQuote != 0 {
			if ch == inQuote {
				inQuote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			inQuote = ch
			continue
		}
		if expr[i:i+len(op)] == op {
			return i
		}
	}
	return -1
}

func resolveCheckedExpressionValue(path string, ctx *WhenContext, syntaxOnly bool) (any, bool) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, false
	}
	segments := strings.Split(path, ".")
	for _, segment := range segments {
		if segment == "" {
			return nil, false
		}
	}

	switch segments[0] {
	case "params":
		if len(segments) < 2 {
			return nil, false
		}
		if syntaxOnly {
			return true, true
		}
		if ctx == nil || ctx.Params == nil {
			return nil, false
		}
		return lookupCheckedExpressionPath(ctx.Params, segments[1:])
	case "commands":
		if len(segments) != 2 {
			return nil, false
		}
		if syntaxOnly {
			return true, true
		}
		if ctx == nil || ctx.Commands == nil {
			return nil, false
		}
		return ctx.Commands[segments[1]], true
	case "commandParams":
		if len(segments) < 3 {
			return nil, false
		}
		if syntaxOnly {
			return true, true
		}
		if ctx == nil || ctx.CommandParams == nil {
			return nil, false
		}
		params, present := ctx.CommandParams[segments[1]]
		if !present || params == nil {
			return nil, false
		}
		return lookupCheckedExpressionPath(params, segments[2:])
	case "project":
		if len(segments) != 2 || segments[1] != "type" {
			return nil, false
		}
		if syntaxOnly {
			return true, true
		}
		if ctx == nil || ctx.ProjectType == nil {
			return nil, false
		}
		return *ctx.ProjectType, true
	case "steps":
		if len(segments) < 2 || (len(segments) > 2 && segments[2] != "status" && segments[2] != "data") ||
			(len(segments) > 3 && segments[2] == "status") {
			return nil, false
		}
		if syntaxOnly {
			return true, true
		}
		if ctx == nil || ctx.Steps == nil {
			return nil, false
		}
		stepResult, present := ctx.Steps[segments[1]]
		if !present || stepResult == nil {
			return nil, false
		}
		if len(segments) == 2 {
			return stepResult, true
		}
		if segments[2] == "status" {
			return stepResult.Status, true
		}
		if len(segments) == 3 {
			return stepResult.Data, true
		}
		return lookupCheckedExpressionPath(stepResult.Data, segments[3:])
	default:
		return parseCheckedExpressionLiteral(path, ctx, syntaxOnly)
	}
}

func lookupCheckedExpressionPath(root any, segments []string) (any, bool) {
	current := root
	for _, segment := range segments {
		object, ok := current.(ParamMap)
		if !ok {
			return nil, false
		}
		next, present := object[segment]
		if !present {
			return nil, false
		}
		current = next
	}
	return current, true
}

func parseCheckedExpressionLiteral(value string, ctx *WhenContext, syntaxOnly bool) (any, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, false
	}
	if value[0] == '\'' || value[0] == '"' || value[len(value)-1] == '\'' || value[len(value)-1] == '"' {
		if len(value) < 2 || value[0] != value[len(value)-1] {
			return nil, false
		}
		return value[1 : len(value)-1], true
	}
	if value == "true" {
		return true, true
	}
	if value == "false" {
		return false, true
	}
	if value == "null" || value == "undefined" {
		return nil, true
	}
	if n, err := strconv.ParseFloat(value, 64); err == nil {
		return n, true
	}
	if value == "params" || value == "steps" || value == "commands" || value == "commandParams" || value == "project" ||
		strings.HasPrefix(value, "params.") || strings.HasPrefix(value, "steps.") ||
		strings.HasPrefix(value, "commands.") || strings.HasPrefix(value, "commandParams.") || strings.HasPrefix(value, "project.") {
		return resolveCheckedExpressionValue(value, ctx, syntaxOnly)
	}
	return nil, false
}

func expressionTruthy(value any) bool {
	if value == nil {
		return false
	}
	switch typed := value.(type) {
	case bool:
		return typed
	case int:
		return typed != 0
	case float64:
		return typed != 0
	case string:
		return typed != "" && typed != "false"
	default:
		return true
	}
}

func expressionString(value any) string {
	if value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(typed)
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	default:
		return ""
	}
}
