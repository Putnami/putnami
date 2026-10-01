package extension

import "testing"

func TestEvaluateExpression(t *testing.T) {
	presentNull := &WhenContext{Params: map[string]any{"value": nil}}
	library := "library"
	tests := []struct {
		name      string
		expr      string
		ctx       *WhenContext
		checked   bool
		want      bool
		wantValid bool
	}{
		{
			name: "empty expression is true",
			expr: "",
			ctx:  &WhenContext{},
			want: true,
		},
		{
			name: "truthy param",
			expr: "params.compile",
			ctx:  &WhenContext{Params: map[string]any{"compile": true}},
			want: true,
		},
		{
			name: "falsy param",
			expr: "params.compile",
			ctx:  &WhenContext{Params: map[string]any{"compile": false}},
			want: false,
		},
		{
			name: "undefined param",
			expr: "params.compile",
			ctx:  &WhenContext{Params: map[string]any{}},
			want: false,
		},
		{
			name: "negation of truthy",
			expr: "!params.compile",
			ctx:  &WhenContext{Params: map[string]any{"compile": true}},
			want: false,
		},
		{
			name: "negation of undefined",
			expr: "!params.missing",
			ctx:  &WhenContext{Params: map[string]any{"compile": true}},
			want: true,
		},
		{
			name: "equality match",
			expr: "params.tool == 'all'",
			ctx:  &WhenContext{Params: map[string]any{"tool": "all"}},
			want: true,
		},
		{
			name: "equality no match",
			expr: "params.tool == 'golangci-lint'",
			ctx:  &WhenContext{Params: map[string]any{"tool": "all"}},
			want: false,
		},
		{
			name: "inequality true",
			expr: "params.tool != 'golangci-lint'",
			ctx:  &WhenContext{Params: map[string]any{"tool": "all"}},
			want: true,
		},
		{
			name: "inequality false",
			expr: "params.tool != 'all'",
			ctx:  &WhenContext{Params: map[string]any{"tool": "all"}},
			want: false,
		},
		{
			name: "OR with second true",
			expr: "params.tool == 'all' || params.tool == 'golangci-lint'",
			ctx:  &WhenContext{Params: map[string]any{"tool": "golangci-lint"}},
			want: true,
		},
		{
			name: "OR with both false",
			expr: "params.tool == 'all' || params.tool == 'golangci-lint'",
			ctx:  &WhenContext{Params: map[string]any{"tool": "other"}},
			want: false,
		},
		{
			name: "AND with both true",
			expr: "params.a && params.b",
			ctx:  &WhenContext{Params: map[string]any{"a": true, "b": true}},
			want: true,
		},
		{
			name: "AND with one false",
			expr: "params.a && params.b",
			ctx:  &WhenContext{Params: map[string]any{"a": true, "b": false}},
			want: false,
		},
		{
			name: "complex build condition - default build",
			expr: "params.transpile || !params.transpile && !params.types && !params.compile",
			ctx:  &WhenContext{Params: map[string]any{}},
			want: true,
		},
		{
			name: "complex build condition - explicit transpile",
			expr: "params.transpile || !params.transpile && !params.types && !params.compile",
			ctx:  &WhenContext{Params: map[string]any{"transpile": true}},
			want: true,
		},
		{
			name: "complex build condition - compile only excludes transpile",
			expr: "params.transpile || !params.transpile && !params.types && !params.compile",
			ctx:  &WhenContext{Params: map[string]any{"compile": true}},
			want: false,
		},
		{
			name: "step status match",
			expr: "steps.generate.status == 'success'",
			ctx: &WhenContext{
				Params: map[string]any{},
				Steps: map[string]*StepResult{
					"generate": {Status: "success", Data: map[string]any{"exports": true}},
				},
			},
			want: true,
		},
		{
			name: "step status no match",
			expr: "steps.generate.status == 'failed'",
			ctx: &WhenContext{
				Params: map[string]any{},
				Steps: map[string]*StepResult{
					"generate": {Status: "success", Data: map[string]any{"exports": true}},
				},
			},
			want: false,
		},
		{
			name: "parenthesized expression",
			expr: "(params.a)",
			ctx:  &WhenContext{Params: map[string]any{"a": true}},
			want: true,
		},
		{
			name: "deep param navigation match",
			expr: "params.build.target == 'linux/amd64'",
			ctx: &WhenContext{
				Params: map[string]any{
					"build": map[string]any{"target": "linux/amd64"},
				},
			},
			want: true,
		},
		{
			name: "deep param navigation no match",
			expr: "params.build.target == 'darwin/arm64'",
			ctx: &WhenContext{
				Params: map[string]any{
					"build": map[string]any{"target": "linux/amd64"},
				},
			},
			want: false,
		},
		{
			name: "undefined param equals null",
			expr: "params.missing == null",
			ctx:  &WhenContext{Params: map[string]any{}},
			want: true,
		},
		{
			name: "undefined param equals undefined",
			expr: "params.missing == undefined",
			ctx:  &WhenContext{Params: map[string]any{}},
			want: true,
		},
		{
			name: "top-level command is present",
			expr: "commands.build && commands.test",
			ctx:  &WhenContext{Commands: map[string]bool{"build": true, "test": true}},
			want: true,
		},
		{
			name: "absent top-level command is false",
			expr: "commands.build && commands.test",
			ctx:  &WhenContext{Commands: map[string]bool{"build": true}},
			want: false,
		},
		{
			name: "test-only race differs from build",
			expr: "params.race != commandParams.test.race",
			ctx: &WhenContext{
				Params:        ParamMap{"race": false},
				CommandParams: map[string]ParamMap{"test": {"race": true}},
			},
			want: true,
		},
		{
			name: "matching race settings compare equal",
			expr: "params.race != commandParams.test.race",
			ctx: &WhenContext{
				Params:        ParamMap{"race": true},
				CommandParams: map[string]ParamMap{"test": {"race": true}},
			},
			want: false,
		},
		{
			name:      "checked command parameter is valid",
			expr:      "commandParams.test.race == false",
			ctx:       &WhenContext{CommandParams: map[string]ParamMap{"test": {"race": false}}},
			checked:   true,
			want:      true,
			wantValid: true,
		},
		{
			name: "project classification matches",
			expr: "project.type == 'library'",
			ctx:  &WhenContext{ProjectType: &library},
			want: true,
		},
		{
			name:      "checked absent command is valid set membership",
			expr:      "commands.test",
			ctx:       &WhenContext{Commands: map[string]bool{"build": true}},
			checked:   true,
			want:      false,
			wantValid: true,
		},
		{
			name:    "checked missing planner context is invalid",
			expr:    "project.type == 'library' && commands.test",
			ctx:     &WhenContext{},
			checked: true,
			want:    false,
		},
		{
			name: "numeric equality match",
			expr: "params.workers == 4",
			ctx:  &WhenContext{Params: map[string]any{"workers": float64(4)}},
			want: true,
		},
		{
			name: "numeric equality no match",
			expr: "params.workers == 8",
			ctx:  &WhenContext{Params: map[string]any{"workers": float64(4)}},
			want: false,
		},
		{
			name: "chained AND all true",
			expr: "params.a && params.b && params.c",
			ctx:  &WhenContext{Params: map[string]any{"a": true, "b": true, "c": true}},
			want: true,
		},
		{
			name: "chained AND middle false",
			expr: "params.a && params.b && params.c",
			ctx:  &WhenContext{Params: map[string]any{"a": true, "b": false, "c": true}},
			want: false,
		},
		{
			name: "chained OR last true",
			expr: "params.a || params.b || params.c",
			ctx:  &WhenContext{Params: map[string]any{"a": false, "b": false, "c": true}},
			want: true,
		},
		{
			name: "chained OR all false",
			expr: "params.a || params.b || params.c",
			ctx:  &WhenContext{Params: map[string]any{"a": false, "b": false, "c": false}},
			want: false,
		},
		{
			name: "step data navigation",
			expr: "steps.compile.data.output == 'bin/app'",
			ctx: &WhenContext{
				Params: map[string]any{},
				Steps: map[string]*StepResult{
					"compile": {Status: "success", Data: map[string]any{"output": "bin/app"}},
				},
			},
			want: true,
		},
		{
			name: "missing step",
			expr: "steps.missing.status == 'success'",
			ctx:  &WhenContext{Params: map[string]any{}, Steps: map[string]*StepResult{}},
			want: false,
		},
		{
			name: "double negation of true",
			expr: "!!params.flag",
			ctx:  &WhenContext{Params: map[string]any{"flag": true}},
			want: true,
		},
		{
			name: "double negation of false",
			expr: "!!params.flag",
			ctx:  &WhenContext{Params: map[string]any{"flag": false}},
			want: false,
		},
		{
			name:    "missing comparison operand fails closed",
			expr:    "params.flag ==",
			ctx:     &WhenContext{Params: map[string]any{"flag": true}},
			checked: true,
			want:    false,
		},
		{
			name:    "unknown comparison operand fails closed",
			expr:    "params.flag == nope",
			ctx:     &WhenContext{Params: map[string]any{"flag": true}},
			checked: true,
			want:    false,
		},
		{
			name:    "unterminated string operand fails closed",
			expr:    "params.flag == '",
			ctx:     &WhenContext{Params: map[string]any{"flag": true}},
			checked: true,
			want:    false,
		},
		{
			name:      "checked explicit null operand is valid",
			expr:      "params.value == null",
			ctx:       presentNull,
			checked:   true,
			want:      true,
			wantValid: true,
		},
		{
			name:    "checked missing operands fail closed",
			expr:    "params.missing ==",
			ctx:     &WhenContext{},
			checked: true,
			want:    false,
		},
		{
			name:    "checked two missing paths fail closed",
			expr:    "params.missing == params.otherMissing",
			ctx:     &WhenContext{},
			checked: true,
			want:    false,
		},
		{
			name:    "checked negated missing path fails closed",
			expr:    "!params.missing",
			ctx:     &WhenContext{},
			checked: true,
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.checked {
				got, valid := EvaluateExpressionChecked(tt.expr, tt.ctx)
				if got != tt.want || valid != tt.wantValid {
					t.Errorf("EvaluateExpressionChecked(%q) = (%v, %v), want (%v, %v)",
						tt.expr, got, valid, tt.want, tt.wantValid)
				}
				return
			}
			got := EvaluateExpression(tt.expr, tt.ctx)
			if got != tt.want {
				t.Errorf("EvaluateExpression(%q) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestSplitOperator(t *testing.T) {
	tests := []struct {
		name     string
		expr     string
		op       string
		expected int
	}{
		{"simple OR", "a || b", "||", 2},
		{"triple AND", "a && b && c", "&&", 3},
		{"OR inside quotes", "a || 'hello || world'", "||", 2},
		{"parenthesized OR with AND", "(a || b) && c", "&&", 2},
		{"single term", "single", "||", 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts := splitOperator(tt.expr, tt.op)
			if len(parts) != tt.expected {
				t.Errorf("splitOperator(%q, %q) = %d parts, want %d", tt.expr, tt.op, len(parts), tt.expected)
			}
		})
	}
}

func TestIsTruthy(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		expected bool
	}{
		{"nil", nil, false},
		{"true", true, true},
		{"false", false, false},
		{"zero int", 0, false},
		{"nonzero int", 1, true},
		{"zero float", float64(0), false},
		{"nonzero float", float64(1), true},
		{"empty string", "", false},
		{"nonempty string", "hello", true},
		{"string false", "false", false},
		{"empty map", map[string]any{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isTruthy(tt.value); got != tt.expected {
				t.Errorf("isTruthy(%v) = %v, want %v", tt.value, got, tt.expected)
			}
		})
	}
}

func TestToString(t *testing.T) {
	tests := []struct {
		name     string
		value    any
		expected string
	}{
		{"nil", nil, ""},
		{"string", "hello", "hello"},
		{"bool true", true, "true"},
		{"bool false", false, "false"},
		{"int", 42, "42"},
		{"float", 3.14, "3.14"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := toString(tt.value); got != tt.expected {
				t.Errorf("toString(%v) = %q, want %q", tt.value, got, tt.expected)
			}
		})
	}
}
