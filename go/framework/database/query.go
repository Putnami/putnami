package database

import (
	"fmt"
	"strings"
)

// QueryBuilder constructs SQL queries with a fluent API.
// It produces parameterized queries to prevent SQL injection.
//
//	q := database.Select("users").
//	    Columns("id", "name", "email").
//	    Where("active = $1", true).
//	    OrderBy("name ASC").
//	    Limit(10)
//
//	query, args := q.Build()
//	rows, err := pool.Query(ctx, query, args...)
type QueryBuilder struct {
	operation string
	table     string
	columns   []string
	values    [][]any
	sets      []string
	wheres    []string
	args      []any
	orderBy   string
	limit     int
	offset    int
	groupBy   string
	having    string
	returning []string
}

// Select starts a SELECT query on the given table.
// Panics if the table name contains invalid characters.
func Select(table string) *QueryBuilder {
	if !validIdentifier(table) {
		panic(fmt.Sprintf("database.Select: invalid table name %q", table))
	}
	return &QueryBuilder{
		operation: "SELECT",
		table:     quoteIdentifier(table),
		limit:     -1,
		offset:    -1,
	}
}

// Insert starts an INSERT query on the given table.
// Panics if the table name contains invalid characters.
func Insert(table string) *QueryBuilder {
	if !validIdentifier(table) {
		panic(fmt.Sprintf("database.Insert: invalid table name %q", table))
	}
	return &QueryBuilder{
		operation: "INSERT",
		table:     quoteIdentifier(table),
		limit:     -1,
		offset:    -1,
	}
}

// Update starts an UPDATE query on the given table.
// Panics if the table name contains invalid characters.
func Update(table string) *QueryBuilder {
	if !validIdentifier(table) {
		panic(fmt.Sprintf("database.Update: invalid table name %q", table))
	}
	return &QueryBuilder{
		operation: "UPDATE",
		table:     quoteIdentifier(table),
		limit:     -1,
		offset:    -1,
	}
}

// Delete starts a DELETE query on the given table.
// Panics if the table name contains invalid characters.
func Delete(table string) *QueryBuilder {
	if !validIdentifier(table) {
		panic(fmt.Sprintf("database.Delete: invalid table name %q", table))
	}
	return &QueryBuilder{
		operation: "DELETE",
		table:     quoteIdentifier(table),
		limit:     -1,
		offset:    -1,
	}
}

// Columns specifies the columns for SELECT or INSERT.
// Simple identifiers are validated and quoted. Expressions containing
// special characters (parentheses, spaces, asterisks) are passed through as-is.
func (q *QueryBuilder) Columns(cols ...string) *QueryBuilder {
	q.columns = quoteColumns(cols)
	return q
}

// Values adds a row of values for INSERT. Call multiple times for multi-row insert.
func (q *QueryBuilder) Values(vals ...any) *QueryBuilder {
	q.values = append(q.values, vals)
	return q
}

// Set adds a SET clause for UPDATE. Positional parameters ($1, $2, ...) are
// rewritten to account for previously added arguments, like Where.
//
//	q.Set("name = $1", "Alice").Set("age = $1", 30)
func (q *QueryBuilder) Set(expr string, args ...any) *QueryBuilder {
	// Rewrite positional params to account for existing args
	offset := len(q.args)
	rewritten := rewriteParams(expr, offset)
	q.sets = append(q.sets, rewritten)
	q.args = append(q.args, args...)
	return q
}

// Where adds a WHERE condition. Multiple calls are ANDed together.
// Positional parameters ($1, $2, ...) are rewritten to account for
// previously added arguments.
//
//	q.Where("age > $1", 18).Where("active = $1", true)
func (q *QueryBuilder) Where(condition string, args ...any) *QueryBuilder {
	offset := len(q.args)
	rewritten := rewriteParams(condition, offset)
	q.wheres = append(q.wheres, rewritten)
	q.args = append(q.args, args...)
	return q
}

// OrderBy sets the ORDER BY clause.
// Simple identifiers (optionally followed by ASC/DESC) are validated and quoted.
// Complex expressions are passed through as-is.
func (q *QueryBuilder) OrderBy(expr string) *QueryBuilder {
	q.orderBy = quoteOrderByExpr(expr)
	return q
}

// Limit sets the LIMIT clause.
func (q *QueryBuilder) Limit(n int) *QueryBuilder {
	q.limit = n
	return q
}

// Offset sets the OFFSET clause.
func (q *QueryBuilder) Offset(n int) *QueryBuilder {
	q.offset = n
	return q
}

// GroupBy sets the GROUP BY clause.
// Simple identifiers are validated and quoted.
func (q *QueryBuilder) GroupBy(expr string) *QueryBuilder {
	q.groupBy = quoteColumnExpr(expr)
	return q
}

// Having sets the HAVING clause (used with GroupBy).
// Use $1, $2, etc. for parameter placeholders, same as Where().
func (q *QueryBuilder) Having(expr string, args ...any) *QueryBuilder {
	offset := len(q.args)
	q.having = rewriteParams(expr, offset)
	q.args = append(q.args, args...)
	return q
}

// Returning adds a RETURNING clause (for INSERT/UPDATE/DELETE).
// Simple identifiers are validated and quoted. "*" is passed through.
func (q *QueryBuilder) Returning(cols ...string) *QueryBuilder {
	q.returning = quoteColumns(cols)
	return q
}

// Build produces the final SQL query string and its arguments. It is a
// pure projection of accumulated builder state: calling Build more than
// once on the same builder returns identical results and never mutates
// the receiver.
func (q *QueryBuilder) Build() (string, []any) {
	var sb strings.Builder

	args := q.args
	switch q.operation {
	case "SELECT":
		q.buildSelect(&sb)
	case "INSERT":
		args = q.buildInsert(&sb)
	case "UPDATE":
		q.buildUpdate(&sb)
	case "DELETE":
		q.buildDelete(&sb)
	}

	return sb.String(), args
}

func (q *QueryBuilder) buildSelect(sb *strings.Builder) {
	sb.WriteString("SELECT ")
	if len(q.columns) > 0 {
		sb.WriteString(strings.Join(q.columns, ", "))
	} else {
		sb.WriteString("*")
	}
	sb.WriteString(" FROM ")
	sb.WriteString(q.table)

	q.appendWhere(sb)
	q.appendGroupBy(sb)
	q.appendOrderBy(sb)
	q.appendLimit(sb)
}

// buildInsert writes the INSERT statement and returns its argument list.
// It builds the args into a fresh slice (seeded with any pre-existing
// q.args) rather than appending to the receiver, so Build stays
// idempotent.
func (q *QueryBuilder) buildInsert(sb *strings.Builder) []any {
	sb.WriteString("INSERT INTO ")
	sb.WriteString(q.table)

	if len(q.columns) > 0 {
		sb.WriteString(" (")
		sb.WriteString(strings.Join(q.columns, ", "))
		sb.WriteString(")")
	}

	sb.WriteString(" VALUES ")

	args := append([]any(nil), q.args...)
	argIdx := 1
	for i, row := range q.values {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString("(")
		for j := range row {
			if j > 0 {
				sb.WriteString(", ")
			}
			fmt.Fprintf(sb, "$%d", argIdx)
			argIdx++
		}
		sb.WriteString(")")
		args = append(args, row...)
	}

	q.appendReturning(sb)
	return args
}

func (q *QueryBuilder) buildUpdate(sb *strings.Builder) {
	sb.WriteString("UPDATE ")
	sb.WriteString(q.table)
	sb.WriteString(" SET ")
	sb.WriteString(strings.Join(q.sets, ", "))

	q.appendWhere(sb)
	q.appendReturning(sb)
}

func (q *QueryBuilder) buildDelete(sb *strings.Builder) {
	sb.WriteString("DELETE FROM ")
	sb.WriteString(q.table)

	q.appendWhere(sb)
	q.appendReturning(sb)
}

func (q *QueryBuilder) appendWhere(sb *strings.Builder) {
	if len(q.wheres) > 0 {
		sb.WriteString(" WHERE ")
		sb.WriteString(strings.Join(q.wheres, " AND "))
	}
}

func (q *QueryBuilder) appendOrderBy(sb *strings.Builder) {
	if q.orderBy != "" {
		sb.WriteString(" ORDER BY ")
		sb.WriteString(q.orderBy)
	}
}

func (q *QueryBuilder) appendGroupBy(sb *strings.Builder) {
	if q.groupBy != "" {
		sb.WriteString(" GROUP BY ")
		sb.WriteString(q.groupBy)
	}
	if q.having != "" {
		sb.WriteString(" HAVING ")
		sb.WriteString(q.having)
	}
}

func (q *QueryBuilder) appendLimit(sb *strings.Builder) {
	if q.limit >= 0 {
		fmt.Fprintf(sb, " LIMIT %d", q.limit)
	}
	if q.offset >= 0 {
		fmt.Fprintf(sb, " OFFSET %d", q.offset)
	}
}

func (q *QueryBuilder) appendReturning(sb *strings.Builder) {
	if len(q.returning) > 0 {
		sb.WriteString(" RETURNING ")
		sb.WriteString(strings.Join(q.returning, ", "))
	}
}

// isExpression returns true if s contains characters indicating a SQL expression
// rather than a simple identifier (parentheses, asterisks).
func isExpression(s string) bool {
	return strings.ContainsAny(s, "()*")
}

// quoteColumnExpr validates and quotes a simple identifier, or passes through
// expressions containing special characters.
func quoteColumnExpr(s string) string {
	if isExpression(s) {
		return s
	}
	if !validIdentifier(s) {
		panic(fmt.Sprintf("database: invalid identifier %q", s))
	}
	return quoteIdentifier(s)
}

// quoteColumns validates and quotes each column name. Simple identifiers are
// quoted; expressions (containing parentheses, spaces, etc.) pass through.
func quoteColumns(cols []string) []string {
	result := make([]string, len(cols))
	for i, c := range cols {
		result[i] = quoteColumnExpr(c)
	}
	return result
}

// quoteOrderByExpr validates and quotes the column part of an ORDER BY expression.
// Handles "column [ASC|DESC]" patterns. Complex expressions pass through.
func quoteOrderByExpr(expr string) string {
	if isExpression(expr) {
		return expr
	}
	// Check for "column ASC" or "column DESC" suffix
	upper := strings.ToUpper(strings.TrimSpace(expr))
	for _, suffix := range []string{" ASC", " DESC"} {
		if strings.HasSuffix(upper, suffix) {
			col := strings.TrimSpace(expr[:len(expr)-len(suffix)])
			if !validIdentifier(col) {
				panic(fmt.Sprintf("database: invalid identifier in ORDER BY %q", col))
			}
			return quoteIdentifier(col) + suffix
		}
	}
	// No direction suffix — treat as plain identifier
	if !validIdentifier(expr) {
		panic(fmt.Sprintf("database: invalid identifier in ORDER BY %q", expr))
	}
	return quoteIdentifier(expr)
}

// rewriteParams rewrites $1, $2, ... in expr to $1+offset, $2+offset, ...
func rewriteParams(expr string, offset int) string {
	if offset == 0 {
		return expr
	}

	var sb strings.Builder
	i := 0
	for i < len(expr) {
		if expr[i] == '$' && i+1 < len(expr) && expr[i+1] >= '1' && expr[i+1] <= '9' {
			// Parse the number
			j := i + 1
			for j < len(expr) && expr[j] >= '0' && expr[j] <= '9' {
				j++
			}
			num := 0
			for _, c := range expr[i+1 : j] {
				num = num*10 + int(c-'0')
			}
			fmt.Fprintf(&sb, "$%d", num+offset)
			i = j
		} else {
			sb.WriteByte(expr[i])
			i++
		}
	}
	return sb.String()
}
