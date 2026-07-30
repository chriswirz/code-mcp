package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the optional PostgreSQL connection. It exists only when config.json
// carries credentials; every database tool is registered only alongside it.
type DB struct {
	pool             *pgxpool.Pool
	dsn              string
	maxRows          int
	allowWrite       bool
	statementTimeout time.Duration
}

// OpenDB connects using the configured credentials. It returns (nil, nil) when
// the database section is disabled or incomplete, which is not an error: the
// server simply comes up without the database tools.
func OpenDB(ctx context.Context, cfg DatabaseConfig) (*DB, error) {
	dsn := cfg.DSN()
	if dsn == "" {
		return nil, nil
	}
	poolCfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
	if err != nil {
		return nil, fmt.Errorf("database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("database: could not connect to %s: %w", RedactDSN(dsn), err)
	}
	return &DB{
		pool:             pool,
		dsn:              dsn,
		maxRows:          cfg.MaxRows,
		allowWrite:       cfg.AllowWrite,
		statementTimeout: time.Duration(cfg.StatementTimeoutSeconds) * time.Second,
	}, nil
}

// Close releases the pool.
func (d *DB) Close() {
	if d != nil && d.pool != nil {
		d.pool.Close()
	}
}

// Describe returns the connection string with the password redacted.
func (d *DB) Describe() string { return RedactDSN(d.dsn) }

// queryResult is the structured content of a successful query.
type queryResult struct {
	Columns   []string `json:"columns"`
	Rows      [][]any  `json:"rows"`
	RowCount  int      `json:"row_count"`
	Truncated bool     `json:"truncated,omitempty"`
}

// Query runs a read query and materialises at most maxRows rows.
func (d *DB) Query(ctx context.Context, sql string, args []any) (*queryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, d.statementTimeout)
	defer cancel()

	rows, err := d.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := &queryResult{}
	for _, fd := range rows.FieldDescriptions() {
		out.Columns = append(out.Columns, string(fd.Name))
	}
	for rows.Next() {
		if len(out.Rows) >= d.maxRows {
			out.Truncated = true
			break
		}
		values, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out.Rows = append(out.Rows, normalizeRow(values))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out.RowCount = len(out.Rows)
	return out, nil
}

// Exec runs a statement and reports the rows affected.
func (d *DB) Exec(ctx context.Context, sql string, args []any) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d.statementTimeout)
	defer cancel()
	tag, err := d.pool.Exec(ctx, sql, args...)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%s (%d rows affected)", tag.String(), tag.RowsAffected()), nil
}

// normalizeRow renders values JSON can carry. pgx hands back a few Go types
// that marshal poorly, notably []byte and time.Time.
func normalizeRow(values []any) []any {
	out := make([]any, len(values))
	for i, v := range values {
		switch typed := v.(type) {
		case []byte:
			out[i] = fmt.Sprintf("\\x%x", typed)
		case time.Time:
			out[i] = typed.Format(time.RFC3339Nano)
		default:
			out[i] = v
		}
	}
	return out
}

// isReadOnlyStatement is a conservative check used when database.allow_write
// is false. It only lets through statements that begin with a read verb, and
// refuses anything carrying a second statement.
func isReadOnlyStatement(sql string) bool {
	trimmed := strings.TrimSpace(sql)
	trimmed = strings.TrimSuffix(trimmed, ";")
	if strings.Contains(trimmed, ";") {
		return false
	}
	lower := strings.ToLower(trimmed)
	for _, verb := range []string{"select ", "select\n", "with ", "explain ", "show ", "table "} {
		if strings.HasPrefix(lower, verb) {
			return true
		}
	}
	return false
}

// renderTable formats a query result as a plain text table for the model.
func renderTable(q *queryResult) string {
	if len(q.Columns) == 0 {
		return "(no columns)"
	}
	widths := make([]int, len(q.Columns))
	for i, c := range q.Columns {
		widths[i] = len(c)
	}
	cells := make([][]string, len(q.Rows))
	for r, row := range q.Rows {
		cells[r] = make([]string, len(q.Columns))
		for c := range q.Columns {
			var text string
			if c < len(row) {
				text = renderValue(row[c])
			}
			cells[r][c] = text
			if len(text) > widths[c] {
				widths[c] = len(text)
			}
		}
	}
	var b strings.Builder
	writeRow := func(fields []string) {
		for i, f := range fields {
			if i > 0 {
				b.WriteString(" | ")
			}
			b.WriteString(f)
			if i < len(fields)-1 {
				b.WriteString(strings.Repeat(" ", widths[i]-len(f)))
			}
		}
		b.WriteString("\n")
	}
	writeRow(q.Columns)
	seps := make([]string, len(q.Columns))
	for i := range seps {
		seps[i] = strings.Repeat("-", widths[i])
	}
	writeRow(seps)
	for _, row := range cells {
		writeRow(row)
	}
	fmt.Fprintf(&b, "\n%d row(s)", q.RowCount)
	if q.Truncated {
		b.WriteString(" (truncated at the configured max_rows)")
	}
	return b.String()
}

func renderValue(v any) string {
	if v == nil {
		return "NULL"
	}
	return fmt.Sprint(v)
}
