package main

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// preferredNames maps the camelCase spellings that differ from their canonical
// parameter by more than case. oldText is read as old_string, not old_text, so
// the warning has to name the parameter that is actually documented.
var preferredNames = map[string]string{
	"oldText":   "old_string",
	"newText":   "new_string",
	"oldString": "old_string",
	"newString": "new_string",
}

// namingWarning explains the camelCase keys a call used in place of the
// snake_case parameters the tool declares. decodeArgs already accepts them, so
// the call still runs; the warning is what stops a model from repeating the
// spelling on the next call, where a key with no alias would be silently lost.
//
// Only keys whose snake_case form is a parameter in the tool's schema are
// reported. Free-form values - environment maps, query parameters, request
// bodies - are data, and their capitals are nobody's mistake.
func namingWarning(inputSchema map[string]any, raw json.RawMessage) string {
	if len(raw) == 0 || !hasUpper(raw) {
		return ""
	}
	var generic any
	if err := json.Unmarshal(raw, &generic); err != nil {
		return ""
	}
	declared := map[string]bool{}
	collectSchemaProperties(inputSchema, declared)
	if len(declared) == 0 {
		return ""
	}

	found := map[string]string{}
	collectCamelKeys(generic, declared, found)
	if len(found) == 0 {
		return ""
	}
	keys := make([]string, 0, len(found))
	for k := range found {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	lines := make([]string, 0, len(keys))
	for _, k := range keys {
		lines = append(lines, found[k])
	}
	return "Warning: this server's parameters are snake_case. The call was performed, but " +
		"use the snake_case names in future calls:\n" + strings.Join(lines, "\n")
}

// collectCamelKeys walks the arguments and records, per camelCase key, how it
// was treated.
func collectCamelKeys(v any, declared map[string]bool, found map[string]string) {
	switch t := v.(type) {
	case map[string]any:
		for k, val := range t {
			collectCamelKeys(val, declared, found)
			// oldText and newText are declared as aliases, but they are still
			// the spelling to steer away from.
			snake, ok := preferredNames[k]
			if declared[k] && !ok {
				continue
			}
			if !ok {
				snake = camelToSnake(k)
			}
			if snake == k || !declared[snake] {
				continue
			}
			if _, both := t[snake]; both {
				found[k] = fmt.Sprintf("- %q was ignored because %q was also given", k, snake)
			} else if _, done := found[k]; !done {
				found[k] = fmt.Sprintf("- %q was read as %q", k, snake)
			}
		}
	case []any:
		for _, item := range t {
			collectCamelKeys(item, declared, found)
		}
	}
}

// collectSchemaProperties gathers every property name declared anywhere in a
// JSON schema, including those of array items and nested objects.
func collectSchemaProperties(v any, into map[string]bool) {
	switch t := v.(type) {
	case map[string]any:
		if props, ok := t["properties"].(map[string]any); ok {
			for name := range props {
				into[name] = true
			}
		}
		for _, val := range t {
			collectSchemaProperties(val, into)
		}
	case []any:
		for _, item := range t {
			collectSchemaProperties(item, into)
		}
	}
}

// withNamingWarning appends the warning to a result's text, error or not: a
// failed call is exactly when the model most needs to know a key was misnamed.
func withNamingWarning(res *CallToolResult, warning string) *CallToolResult {
	if warning == "" || res == nil {
		return res
	}
	for i := range res.Content {
		if res.Content[i].Type == "text" {
			res.Content[i].Text += "\n\n" + warning
			return res
		}
	}
	res.Content = append(res.Content, textContent(warning)...)
	return res
}
