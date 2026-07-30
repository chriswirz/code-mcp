package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// registerDatabaseTools adds the PostgreSQL tools. It is only called when a
// connection was established from the credentials in config.json.
func (s *Server) registerDatabaseTools() {
	db := s.db

	s.RegisterTool(Tool{
		Name:  "db_query",
		Title: "Run a SQL query",
		Description: fmt.Sprintf("Run a read-only SQL query against the configured PostgreSQL database (%s) "+
			"and return the rows. Use $1, $2, ... placeholders with params rather than interpolating values.", db.Describe()),
		Annotations: &ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: true},
		InputSchema: schema([]string{"sql"}, map[string]any{
			"sql": prop("string", "A single SELECT, WITH, EXPLAIN or SHOW statement."),
			"params": map[string]any{
				"type":        "array",
				"description": "Values for the $1, $2, ... placeholders, in order.",
				"items":       map[string]any{},
			},
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			SQL    string `json:"sql"`
			Params []any  `json:"params"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if strings.TrimSpace(args.SQL) == "" {
			return toolError("sql is required"), nil
		}
		if !isReadOnlyStatement(args.SQL) {
			return toolError("db_query only runs a single read statement; use db_execute for anything that writes"), nil
		}
		res, err := db.Query(ctx, args.SQL, args.Params)
		if err != nil {
			return toolError("query failed: %v", err), nil
		}
		return &CallToolResult{
			Content:           textContent(renderTable(res)),
			StructuredContent: res,
		}, nil
	})

	s.RegisterTool(Tool{
		Name:        "db_tables",
		Title:       "List database tables",
		Description: "List the tables and views in the database, with their schema and estimated row count.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true},
		InputSchema: schema(nil, map[string]any{
			"schema": propDefault("string", "Only tables in this schema.", "public"),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Schema string `json:"schema"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Schema == "" {
			args.Schema = "public"
		}
		const sql = `
			SELECT c.relname AS name,
			       CASE c.relkind WHEN 'r' THEN 'table' WHEN 'v' THEN 'view'
			                      WHEN 'm' THEN 'materialized view' WHEN 'p' THEN 'partitioned table'
			                      ELSE c.relkind::text END AS kind,
			       c.reltuples::bigint AS estimated_rows
			  FROM pg_class c
			  JOIN pg_namespace n ON n.oid = c.relnamespace
			 WHERE n.nspname = $1 AND c.relkind IN ('r','v','m','p')
			 ORDER BY c.relname`
		res, err := db.Query(ctx, sql, []any{args.Schema})
		if err != nil {
			return toolError("%v", err), nil
		}
		if res.RowCount == 0 {
			return toolResult(fmt.Sprintf("No tables in schema %q.", args.Schema)), nil
		}
		return &CallToolResult{Content: textContent(renderTable(res)), StructuredContent: res}, nil
	})

	s.RegisterTool(Tool{
		Name:        "db_describe_table",
		Title:       "Describe a table",
		Description: "Show the columns, types, nullability, defaults and indexes of one table.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, OpenWorldHint: true},
		InputSchema: schema([]string{"table"}, map[string]any{
			"table":  prop("string", "Table name, optionally schema-qualified."),
			"schema": propDefault("string", "Schema, when the table name is not qualified.", "public"),
		}),
	}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Table  string `json:"table"`
			Schema string `json:"schema"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if args.Table == "" {
			return toolError("table is required"), nil
		}
		schemaName, tableName := args.Schema, args.Table
		if parts := strings.SplitN(args.Table, ".", 2); len(parts) == 2 {
			schemaName, tableName = parts[0], parts[1]
		}
		if schemaName == "" {
			schemaName = "public"
		}
		const columnsSQL = `
			SELECT column_name, data_type, is_nullable, column_default
			  FROM information_schema.columns
			 WHERE table_schema = $1 AND table_name = $2
			 ORDER BY ordinal_position`
		cols, err := db.Query(ctx, columnsSQL, []any{schemaName, tableName})
		if err != nil {
			return toolError("%v", err), nil
		}
		if cols.RowCount == 0 {
			return toolError("no table %s.%s", schemaName, tableName), nil
		}
		const indexSQL = `
			SELECT indexname AS name, indexdef AS definition
			  FROM pg_indexes WHERE schemaname = $1 AND tablename = $2 ORDER BY indexname`
		idx, err := db.Query(ctx, indexSQL, []any{schemaName, tableName})
		if err != nil {
			return toolError("%v", err), nil
		}
		text := fmt.Sprintf("%s.%s\n\nColumns:\n%s\n\nIndexes:\n%s",
			schemaName, tableName, renderTable(cols), renderTable(idx))
		return &CallToolResult{
			Content: textContent(text),
			StructuredContent: map[string]any{
				"schema":  schemaName,
				"table":   tableName,
				"columns": cols,
				"indexes": idx,
			},
		}, nil
	})

	if db.allowWrite {
		s.RegisterTool(Tool{
			Name:  "db_execute",
			Title: "Execute a SQL statement",
			Description: "Run a writing SQL statement (INSERT, UPDATE, DELETE, DDL) against the configured database " +
				"and report the rows affected. This changes real data.",
			Annotations: &ToolAnnotations{DestructiveHint: true, OpenWorldHint: true},
			InputSchema: schema([]string{"sql"}, map[string]any{
				"sql": prop("string", "The statement to run."),
				"params": map[string]any{
					"type":        "array",
					"description": "Values for the $1, $2, ... placeholders, in order.",
					"items":       map[string]any{},
				},
			}),
		}, func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
			var args struct {
				SQL    string `json:"sql"`
				Params []any  `json:"params"`
			}
			if bad := decodeArgs(raw, &args); bad != nil {
				return bad, nil
			}
			if strings.TrimSpace(args.SQL) == "" {
				return toolError("sql is required"), nil
			}
			tag, err := db.Exec(ctx, args.SQL, args.Params)
			if err != nil {
				return toolError("statement failed: %v", err), nil
			}
			return toolResult(tag), nil
		})
	}
}
