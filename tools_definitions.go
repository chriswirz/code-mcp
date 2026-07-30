package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// defaultDefinitionResults is how many definitions one call returns before it
// says it stopped. A name defined in two dozen places is a name worth
// narrowing rather than reading.
const defaultDefinitionResults = 20

// defaultDefinitionBodyLines caps one body, so asking about a name that turns
// out to be a thousand-line file does not spend the caller's whole context on
// it. The result says when it has cut one short.
const defaultDefinitionBodyLines = 400

// definitionResult is the structured content of a find_*_definition call.
type definitionResult struct {
	Name        string       `json:"name"`
	Kind        string       `json:"kind"`
	Definitions []definition `json:"definitions"`
	Count       int          `json:"count"`
	Truncated   bool         `json:"truncated,omitempty"`
}

// registerDefinitionTools adds find_method_definition and
// find_class_definition: where a name is defined, rather than everywhere it is
// mentioned, which is what grep answers and what a model then has to sift.
func (s *Server) registerDefinitionTools() {
	s.RegisterTool(definitionTool(
		"find_method_definition",
		"Find where a method or function is defined",
		"Find every place a method or function is defined, and return each one whole: its file, "+
			"the lines it spans, and its body with the doc comment above it. This is the question "+
			"grep answers badly - a search for the name matches every call site too, and returns "+
			"lines rather than the definition.",
	), s.definitionHandler(methodDefinition))

	s.RegisterTool(definitionTool(
		"find_class_definition",
		"Find where a class or type is defined",
		"Find every place a class, struct, interface, enum, trait or type is defined, and return "+
			"each one whole: its file, the lines it spans, and its body with the doc comment above "+
			"it. In Go this finds a type declaration; in Rust, the type and its impl blocks.",
	), s.definitionHandler(classDefinition))
}

func definitionTool(name, title, description string) Tool {
	return Tool{
		Name:  name,
		Title: title,
		Description: description + "\n\nThe language is taken from each file's extension, and " +
			"the search reads " + strings.Join(languageNames(), ", ") + ". Several definitions of " +
			"one name is the normal case - an interface and its implementations, a method on two " +
			"types, the same name in two packages - so the answer is always a list, ordered with " +
			"real definitions before declarations that have no body.",
		Annotations: &ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true},
		InputSchema: schema([]string{"name"}, map[string]any{
			"name": prop("string", "The identifier to find, exactly as it is spelled in the source. "+
				"Not a pattern: a name."),
			"path": propDefault("string",
				"File or directory to search, relative to the workspace root.", "."),
			"language": prop("string", "Restrict the search to one language ("+
				strings.Join(languageNames(), ", ")+"). Normally left out: the file extension decides."),
			"include_body": propDefault("boolean",
				"Return the body of each definition. Turn it off for a list of locations alone.", true),
			"max_body_lines": propDefault("integer",
				"Cap the lines of body returned per definition.", defaultDefinitionBodyLines),
			"max_results": propDefault("integer",
				"Stop after this many definitions.", defaultDefinitionResults),
		}),
	}
}

func (s *Server) definitionHandler(kind definitionKind) ToolHandler {
	return func(ctx context.Context, raw json.RawMessage) (*CallToolResult, *RPCError) {
		var args struct {
			Name         string `json:"name"`
			Path         string `json:"path"`
			Language     string `json:"language"`
			IncludeBody  *bool  `json:"include_body"`
			MaxBodyLines *int   `json:"max_body_lines"`
			MaxResults   int    `json:"max_results"`
		}
		if bad := decodeArgs(raw, &args); bad != nil {
			return bad, nil
		}
		if strings.TrimSpace(args.Name) == "" {
			return toolError("name is required: the identifier to find"), nil
		}

		ws := s.workspace()
		query := definitionQuery{
			Name:         strings.TrimSpace(args.Name),
			Kind:         kind,
			Path:         args.Path,
			MaxResults:   args.MaxResults,
			IncludeBody:  args.IncludeBody == nil || *args.IncludeBody,
			MaxBodyLines: defaultDefinitionBodyLines,
		}
		if args.MaxBodyLines != nil {
			query.MaxBodyLines = *args.MaxBodyLines
		}
		if query.MaxResults <= 0 {
			query.MaxResults = defaultDefinitionResults
		}
		if args.Language != "" {
			lang := languageByName(args.Language)
			if lang == nil {
				return toolError("no language called %q; this server reads %s",
					args.Language, strings.Join(languageNames(), ", ")), nil
			}
			query.Language = lang
		}

		defs, truncated, err := findDefinitions(ws, query)
		if err != nil {
			return pathToolError(ws, args.Path, err), nil
		}

		result := definitionResult{
			Name:        query.Name,
			Kind:        string(kind),
			Definitions: defs,
			Count:       len(defs),
			Truncated:   truncated,
		}
		return &CallToolResult{
			Content:           textContent(result.summarize(query)),
			StructuredContent: result,
		}, nil
	}
}

// summarize renders the definitions as the text block of the result: a
// heading per definition and then its body, which is what a model reads, with
// the structured content carrying the same thing in fields.
func (r definitionResult) summarize(q definitionQuery) string {
	if len(r.Definitions) == 0 {
		return r.nothingFound(q)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d definition(s) of %s %q\n", r.Count, r.Kind, r.Name)
	for _, def := range r.Definitions {
		fmt.Fprintf(&b, "\n%s:%d-%d  (%d lines, %s)",
			def.Path, def.StartLine, def.EndLine, def.TotalLines, def.Language)
		if !def.HasBody {
			b.WriteString("  [declaration only]")
		}
		b.WriteString("\n")
		if def.Body != "" {
			b.WriteString(def.Body)
			if !strings.HasSuffix(def.Body, "\n") {
				b.WriteString("\n")
			}
			if def.Truncated {
				fmt.Fprintf(&b, "... body cut off at %d lines; read %s from line %d for the rest\n",
					q.MaxBodyLines, def.Path, def.StartLine)
			}
		}
	}
	if r.Truncated {
		fmt.Fprintf(&b, "\n(stopped at %d definitions; narrow the path or raise max_results)\n", r.Count)
	}
	return b.String()
}

// nothingFound says why, which is usually one of three things, and each has a
// different next step.
func (r definitionResult) nothingFound(q definitionQuery) string {
	var b strings.Builder
	fmt.Fprintf(&b, "No %s named %q is defined under %s.\n", r.Kind, r.Name, displayPath(q.Path))
	b.WriteString("The name must be spelled exactly as it is in the source; this is not a pattern. ")
	if q.Language != nil {
		fmt.Fprintf(&b, "Only %s files were searched (%s). ", q.Language.Name, definitionExtensions(q.Language))
	}
	b.WriteString("If it is defined in a language this server does not read, or generated by a macro, " +
		"grep_files will still find it.")
	return b.String()
}

func displayPath(path string) string {
	if strings.TrimSpace(path) == "" || path == "." {
		return "the workspace"
	}
	return path
}
