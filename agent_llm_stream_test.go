package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestScanSSESkipsNonDataLinesAndStopsOnDone(t *testing.T) {
	input := "event: message_start\n" +
		"data: {\"n\":1}\n" +
		"\n" +
		": this is a comment\n" +
		"data: {\"n\":2}\n" +
		"data: [DONE]\n" +
		"data: {\"n\":3}\n" // must never be reached
	var got []string
	err := scanSSE(strings.NewReader(input), func(data string) error {
		got = append(got, data)
		return nil
	})
	if err != nil {
		t.Fatalf("scanSSE: %v", err)
	}
	want := []string{`{"n":1}`, `{"n":2}`}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d = %q, want %q", i, got[i], want[i])
		}
	}
}

// openAIToolCallChunk builds one streamed tool-call delta line, using
// json.Marshal rather than a hand-escaped literal so the embedded arguments
// fragment - itself a piece of JSON text - cannot be mis-escaped by hand.
func openAIToolCallChunk(index int, id, name, argsFragment string) string {
	data, _ := json.Marshal(map[string]any{
		"choices": []any{map[string]any{"delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": index, "id": id,
				"function": map[string]any{"name": name, "arguments": argsFragment},
			}},
		}}},
	})
	return "data: " + string(data)
}

func TestReadOpenAIStreamAccumulatesTextReasoningAndToolCalls(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"let me "}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"think"}}]}`,
		`data: {"choices":[{"delta":{"content":"The "}}]}`,
		`data: {"choices":[{"delta":{"content":"answer"}}]}`,
		openAIToolCallChunk(0, "call_1", "echo", ""),
		openAIToolCallChunk(0, "", "", `{"x":`),
		openAIToolCallChunk(0, "", "", `1}`),
		`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5}}`,
		`data: [DONE]`,
		"",
	}, "\n")

	var texts, reasons []string
	reply, err := readOpenAIStream(strings.NewReader(stream), func(kind, delta string) {
		if kind == "text" {
			texts = append(texts, delta)
		} else {
			reasons = append(reasons, delta)
		}
	})
	if err != nil {
		t.Fatalf("readOpenAIStream: %v", err)
	}
	if reply.Text != "The answer" {
		t.Errorf("Text = %q", reply.Text)
	}
	if reply.Reasoning != "let me think" {
		t.Errorf("Reasoning = %q", reply.Reasoning)
	}
	if strings.Join(texts, "") != "The answer" {
		t.Errorf("streamed text = %v", texts)
	}
	if strings.Join(reasons, "") != "let me think" {
		t.Errorf("streamed reasoning = %v", reasons)
	}
	if reply.InTokens != 10 || reply.OutTokens != 5 {
		t.Errorf("tokens = %d/%d, want 10/5", reply.InTokens, reply.OutTokens)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].Name != "echo" || reply.ToolCalls[0].ID != "call_1" {
		t.Fatalf("ToolCalls = %+v", reply.ToolCalls)
	}
	if string(reply.ToolCalls[0].Arguments) != `{"x":1}` {
		t.Errorf("tool call arguments = %s, want {\"x\":1}", reply.ToolCalls[0].Arguments)
	}
}

func TestReadOpenAIStreamIgnoresUnparsableChunks(t *testing.T) {
	stream := "data: not json at all\n" +
		`data: {"choices":[{"delta":{"content":"ok"}}]}` + "\n" +
		"data: [DONE]\n"
	reply, err := readOpenAIStream(strings.NewReader(stream), func(string, string) {})
	if err != nil {
		t.Fatalf("readOpenAIStream: %v", err)
	}
	if reply.Text != "ok" {
		t.Errorf("Text = %q, want ok despite the earlier garbage chunk", reply.Text)
	}
}

func TestReadAnthropicStreamAccumulatesTextThinkingAndToolUse(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":7}}}`,
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		`data: {"type":"content_block_stop","index":0}`,
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Hi "}}`,
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"there"}}`,
		`data: {"type":"content_block_stop","index":1}`,
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_1","name":"echo"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`,
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"2}"}}`,
		`data: {"type":"content_block_stop","index":2}`,
		`data: {"type":"message_delta","usage":{"output_tokens":9}}`,
		`data: {"type":"message_stop"}`,
		"",
	}, "\n")

	var texts, reasons []string
	reply, err := readAnthropicStream(strings.NewReader(stream), func(kind, delta string) {
		if kind == "text" {
			texts = append(texts, delta)
		} else {
			reasons = append(reasons, delta)
		}
	})
	if err != nil {
		t.Fatalf("readAnthropicStream: %v", err)
	}
	if reply.Text != "Hi there" {
		t.Errorf("Text = %q", reply.Text)
	}
	if reply.Reasoning != "hmm" {
		t.Errorf("Reasoning = %q", reply.Reasoning)
	}
	if strings.Join(texts, "") != "Hi there" || strings.Join(reasons, "") != "hmm" {
		t.Errorf("streamed text/reasoning = %v / %v", texts, reasons)
	}
	if reply.InTokens != 7 || reply.OutTokens != 9 {
		t.Errorf("tokens = %d/%d, want 7/9", reply.InTokens, reply.OutTokens)
	}
	if len(reply.ToolCalls) != 1 || reply.ToolCalls[0].ID != "toolu_1" || reply.ToolCalls[0].Name != "echo" {
		t.Fatalf("ToolCalls = %+v", reply.ToolCalls)
	}
	if string(reply.ToolCalls[0].Arguments) != `{"x":2}` {
		t.Errorf("tool call arguments = %s", reply.ToolCalls[0].Arguments)
	}
}

func TestReadOllamaStreamAccumulatesContentThinkingAndUsage(t *testing.T) {
	stream := strings.Join([]string{
		`{"message":{"role":"assistant","thinking":"pondering"},"done":false}`,
		`{"message":{"role":"assistant","content":"Hel"},"done":false}`,
		`{"message":{"role":"assistant","content":"lo"},"done":false}`,
		`{"message":{"role":"assistant","content":""},"done":true,"prompt_eval_count":4,"eval_count":2}`,
		"",
	}, "\n")

	var texts, reasons []string
	reply, err := readOllamaStream(strings.NewReader(stream), func(kind, delta string) {
		if kind == "text" {
			texts = append(texts, delta)
		} else {
			reasons = append(reasons, delta)
		}
	})
	if err != nil {
		t.Fatalf("readOllamaStream: %v", err)
	}
	if reply.Text != "Hello" {
		t.Errorf("Text = %q", reply.Text)
	}
	if reply.Reasoning != "pondering" {
		t.Errorf("Reasoning = %q", reply.Reasoning)
	}
	if reply.InTokens != 4 || reply.OutTokens != 2 {
		t.Errorf("tokens = %d/%d, want 4/2", reply.InTokens, reply.OutTokens)
	}
	if strings.Join(texts, "") != "Hello" {
		t.Errorf("streamed text = %v", texts)
	}
}
