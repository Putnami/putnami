package extension

import (
	"strconv"
	"strings"

	proto "go.putnami.dev/protocol/extension"
)

// ParamMap is an alias so existing callers retain built-in map compatibility.
type ParamMap = proto.ParamMap

// WhenContext provides the evaluation context for plan-time expressions.
// Params and Steps are the historical expression roots. Commands,
// CommandParams, and ProjectType are planner-owned facts: the effective
// provider/project command set, each command's fully resolved parameter bag,
// and the target project's resolved classification.
type WhenContext = proto.WhenContext

// StepResult holds the result of a completed pipeline step.
type StepResult = proto.StepResult

// EvaluateExpression evaluates a when/if expression against a context.
//
// Supported forms:
//
//	params.X                  → truthy check
//	params.X == 'value'       → equality
//	params.X != 'value'       → inequality
//	!params.X                 → negation
//	expr && expr              → logical AND
//	expr || expr              → logical OR
//	steps.X.status == 'success' → step result check
//	commands.build                 → effective provider/project command membership
//	commandParams.test.race        → resolved test parameter
//	project.type == 'library'      → resolved project classification
//
// Returns true for empty expressions (no condition = always run).
// Returns false for malformed expressions (fail-closed).
func EvaluateExpression(expr string, ctx *WhenContext) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return true
	}

	// Handle || (lower precedence)
	if parts := splitOperator(expr, "||"); len(parts) > 1 {
		for _, part := range parts {
			if EvaluateExpression(part, ctx) {
				return true
			}
		}
		return false
	}

	// Handle &&
	if parts := splitOperator(expr, "&&"); len(parts) > 1 {
		for _, part := range parts {
			if !EvaluateExpression(part, ctx) {
				return false
			}
		}
		return true
	}

	// Handle negation
	if strings.HasPrefix(expr, "!") {
		return !EvaluateExpression(expr[1:], ctx)
	}

	// Handle parentheses
	if strings.HasPrefix(expr, "(") && strings.HasSuffix(expr, ")") {
		return EvaluateExpression(expr[1:len(expr)-1], ctx)
	}

	// Handle == comparison
	if idx := findOperator(expr, "=="); idx >= 0 {
		left := resolveExprValue(strings.TrimSpace(expr[:idx]), ctx)
		right := parseLiteral(strings.TrimSpace(expr[idx+2:]), ctx)
		return toString(left) == toString(right)
	}

	// Handle != comparison
	if idx := findOperator(expr, "!="); idx >= 0 {
		left := resolveExprValue(strings.TrimSpace(expr[:idx]), ctx)
		right := parseLiteral(strings.TrimSpace(expr[idx+2:]), ctx)
		return toString(left) != toString(right)
	}

	// Simple truthy check
	value := resolveExprValue(expr, ctx)
	return isTruthy(value)
}

// EvaluateExpressionChecked evaluates an expression and reports whether every
// operator and operand was valid. It is the safety-sensitive counterpart to
// EvaluateExpression: legacy pipeline conditions keep their historical
// missing-value semantics, while callers such as finalizes.pruneIf can require
// BOTH a true result and a valid expression before applying an optimization.
//
// A missing path is invalid here, including under negation and in a comparison.
// A present value whose value is nil remains valid, so an explicit null can
// still be compared without conflating it with a missing operand.
func EvaluateExpressionChecked(expr string, ctx *WhenContext) (bool, bool) {
	return proto.EvaluateExpressionChecked(expr, ctx)
}

// ValidateExpressionSyntax validates the same grammar used by checked
// evaluation without requiring runtime values for well-formed paths.
func ValidateExpressionSyntax(expr string) bool {
	return proto.ValidateExpressionSyntax(expr)
}

func lookupExpressionMap(current any, key string) (any, bool) {
	m, ok := current.(map[string]any)
	if !ok {
		return nil, false
	}
	value, present := m[key]
	return value, present
}

func resolveCommandParam(segments []string, ctx *WhenContext) (any, bool) {
	if ctx == nil || ctx.CommandParams == nil || len(segments) < 3 || segments[1] == "" {
		return nil, false
	}
	params, present := ctx.CommandParams[segments[1]]
	if !present || params == nil {
		return nil, false
	}
	var current any = params
	for _, seg := range segments[2:] {
		next, ok := lookupExpressionMap(current, seg)
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

// splitOperator splits on a logical operator, respecting string literals and parens.
func splitOperator(expr, op string) []string {
	var parts []string
	depth := 0
	var inQuote byte
	current := strings.Builder{}

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
			current.WriteByte(ch)
			continue
		}

		if depth == 0 && i+len(op) <= len(expr) && expr[i:i+len(op)] == op {
			parts = append(parts, current.String())
			current.Reset()
			i += len(op) - 1
			continue
		}

		current.WriteByte(ch)
	}

	if s := strings.TrimSpace(current.String()); s != "" {
		parts = append(parts, s)
	}

	return parts
}

// findOperator finds the position of an operator outside quotes.
func findOperator(expr, op string) int {
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

// resolveExprValue resolves a dotted path against the context.
func resolveExprValue(path string, ctx *WhenContext) any {
	path = strings.TrimSpace(path)
	segments := strings.Split(path, ".")
	if ctx == nil {
		return nil
	}

	if segments[0] == "params" {
		if ctx.Params == nil {
			return nil
		}
		var current any = ctx.Params
		for _, seg := range segments[1:] {
			next, ok := lookupExpressionMap(current, seg)
			if !ok {
				return nil
			}
			current = next
		}
		return current
	}

	if segments[0] == "commands" {
		if ctx.Commands == nil || len(segments) != 2 || segments[1] == "" {
			return nil
		}
		return ctx.Commands[segments[1]]
	}

	if segments[0] == "commandParams" {
		value, _ := resolveCommandParam(segments, ctx)
		return value
	}

	if segments[0] == "project" {
		if ctx.ProjectType == nil || len(segments) != 2 || segments[1] != "type" {
			return nil
		}
		return *ctx.ProjectType
	}

	if segments[0] == "steps" {
		if ctx.Steps == nil || len(segments) < 2 {
			return nil
		}
		stepResult := ctx.Steps[segments[1]]
		if stepResult == nil {
			return nil
		}
		if len(segments) == 2 {
			return stepResult
		}
		// Navigate into step result
		var current any
		switch segments[2] {
		case "status":
			current = stepResult.Status
		case "data":
			current = any(stepResult.Data)
		default:
			return nil
		}
		for _, seg := range segments[3:] {
			next, ok := lookupExpressionMap(current, seg)
			if !ok {
				return nil
			}
			current = next
		}
		return current
	}

	return parseLiteral(path, ctx)
}

// parseLiteral parses a value that may be a literal or a path reference.
func parseLiteral(value string, ctx *WhenContext) any {
	value = strings.TrimSpace(value)
	// An omitted operand is malformed, not a path reference. Returning nil
	// keeps expression evaluation fail-closed; recursing through
	// resolveExprValue("") would otherwise overflow the stack.
	if value == "" {
		return nil
	}

	// String literals
	if len(value) >= 2 && ((strings.HasPrefix(value, "'") && strings.HasSuffix(value, "'")) ||
		(strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\""))) {
		return value[1 : len(value)-1]
	}

	// Boolean literals
	if value == "true" {
		return true
	}
	if value == "false" {
		return false
	}

	// Null/undefined
	if value == "null" || value == "undefined" {
		return nil
	}

	// Number
	if n, err := strconv.ParseFloat(value, 64); err == nil {
		return n
	}

	// Path reference. Unknown bare tokens are malformed and fail closed; sending
	// one back through resolveExprValue would recurse into parseLiteral forever.
	if value == "params" || value == "steps" || value == "commands" || value == "commandParams" || value == "project" ||
		strings.HasPrefix(value, "params.") || strings.HasPrefix(value, "steps.") ||
		strings.HasPrefix(value, "commands.") || strings.HasPrefix(value, "commandParams.") || strings.HasPrefix(value, "project.") {
		return resolveExprValue(value, ctx)
	}
	return nil
}

func isTruthy(value any) bool {
	if value == nil {
		return false
	}
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case float64:
		return v != 0
	case string:
		return v != "" && v != "false"
	default:
		return true // objects, step results
	}
}

func toString(value any) string {
	if value == nil {
		return ""
	}
	switch v := value.(type) {
	case string:
		return v
	case bool:
		if v {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(v)
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	default:
		return ""
	}
}
