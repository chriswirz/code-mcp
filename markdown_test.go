package main

import (
	"strings"
	"testing"
)

func TestSplitMarkdownSentences(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{
			"plain pair",
			"One thing happened. Another followed.",
			[]string{"One thing happened.", "Another followed."},
		},
		{
			"inline code is not a break",
			"Runs `go test ./...` before the commit. Then it pushes.",
			[]string{"Runs `go test ./...` before the commit.", "Then it pushes."},
		},
		{
			"abbreviations do not break",
			"Some tools, e.g. gofmt, rewrite in place. Others do not.",
			[]string{"Some tools, e.g. gofmt, rewrite in place.", "Others do not."},
		},
		{
			"lowercase after a period does not break",
			"See version 1.2 of the spec.",
			[]string{"See version 1.2 of the spec."},
		},
		{
			"a sentence may open with emphasis",
			"That is the default. **Nothing** overrides it.",
			[]string{"That is the default.", "**Nothing** overrides it."},
		},
		{
			"a sentence may open with inline code",
			"It is off by default. `--force` turns it on.",
			[]string{"It is off by default.", "`--force` turns it on."},
		},
		{
			"question and exclamation",
			"Is it ready? It is. Ship it!",
			[]string{"Is it ready?", "It is.", "Ship it!"},
		},
		{
			"no terminal punctuation",
			"A fragment with no full stop",
			[]string{"A fragment with no full stop"},
		},
		{
			"initials do not break",
			"Named for J. Random Hacker. He never existed.",
			[]string{"Named for J. Random Hacker.", "He never existed."},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitMarkdownSentences(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %d parts %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("part %d: got %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestReflowMarkdownStructure(t *testing.T) {
	t.Run("code fences are untouched", func(t *testing.T) {
		src := "Some prose. More prose.\n\n```go\nfmt.Println(\"a. b. c.\")\n// A comment. Another.\n```\n\nAfter. Done.\n"
		got := reflowMarkdown(src)
		if !strings.Contains(got, "fmt.Println(\"a. b. c.\")\n// A comment. Another.") {
			t.Fatalf("fence body was reflowed:\n%s", got)
		}
	})

	t.Run("tables and headings pass through", func(t *testing.T) {
		src := "## A heading. With a period.\n\n| a | b |\n| --- | --- |\n| one. two. | three. |\n"
		got := reflowMarkdown(src)
		if got != src {
			t.Fatalf("structure was altered:\n%s", got)
		}
	})

	t.Run("list items hang to the text column", func(t *testing.T) {
		src := "- **First.** One sentence. And a second.\n- Second item. With another.\n"
		want := "- **First.** One sentence.\n  And a second.\n- Second item.\n  With another.\n"
		if got := reflowMarkdown(src); got != want {
			t.Fatalf("got:\n%q\nwant:\n%q", got, want)
		}
	})

	t.Run("indented paragraphs keep their indent", func(t *testing.T) {
		// This is the case that detaches a continuation from its bullet.
		src := "- A bullet.\n\n  ```sh\n  run me\n  ```\n\n  A follow-up. With two sentences.\n"
		got := reflowMarkdown(src)
		if !strings.Contains(got, "\n  A follow-up.\n  With two sentences.\n") {
			t.Fatalf("indentation was lost:\n%q", got)
		}
	})

	t.Run("wrapped paragraphs are joined then split", func(t *testing.T) {
		src := "A sentence that was\nwrapped across lines. And a second\none also wrapped.\n"
		want := "A sentence that was wrapped across lines.\nAnd a second one also wrapped.\n"
		if got := reflowMarkdown(src); got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("front matter survives", func(t *testing.T) {
		src := "---\ntitle: A doc. With a period.\ntags: [a, b]\n---\n\nProse here. And more.\n"
		got := reflowMarkdown(src)
		if !strings.HasPrefix(got, "---\ntitle: A doc. With a period.\ntags: [a, b]\n---\n") {
			t.Fatalf("front matter was reflowed:\n%q", got)
		}
		if !strings.Contains(got, "Prose here.\nAnd more.") {
			t.Fatalf("prose after front matter was not reflowed:\n%q", got)
		}
	})

	t.Run("block quotes pass through", func(t *testing.T) {
		src := "> Quoted. Still quoted.\n"
		if got := reflowMarkdown(src); got != src {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("CRLF is preserved", func(t *testing.T) {
		src := "One. Two.\r\n\r\nThree.\r\n"
		got := reflowMarkdown(src)
		if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
			t.Fatalf("mixed line endings in output: %q", got)
		}
		if !strings.Contains(got, "One.\r\nTwo.\r\n") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("idempotent", func(t *testing.T) {
		src := "- A bullet. With two sentences.\n\nA paragraph that was\nwrapped. And another.\n\n```\ncode. here.\n```\n"
		once := reflowMarkdown(src)
		if twice := reflowMarkdown(once); twice != once {
			t.Fatalf("second pass changed the document:\n%q\n%q", once, twice)
		}
	})

	t.Run("no words are gained or lost", func(t *testing.T) {
		src := "A paragraph. With `code. spans.` and **emphasis**.\n\n- A list item. With a follow-up.\n  Wrapped here.\n\n| a | b |\n| --- | --- |\n"
		if got := reflowMarkdown(src); !sameWords(src, got) {
			t.Fatalf("word sequence changed:\n%q\n%q", src, got)
		}
	})
}

func TestSameWords(t *testing.T) {
	if !sameWords("a b  c\nd", "a\nb c d") {
		t.Error("whitespace differences should not matter")
	}
	if sameWords("a b c", "a b") {
		t.Error("a dropped word should be detected")
	}
	if sameWords("a b c", "a c b") {
		t.Error("reordering should be detected")
	}
	if sameWords("a b", "a b.") {
		t.Error("a changed word should be detected")
	}
}
