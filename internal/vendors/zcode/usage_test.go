package zcode

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStreamUsageCumulativeAndTerminalOnly(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":100,"output_tokens":0,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}`,
		`{"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`,
		`{"type":"message_delta","delta":{},"usage":{"output_tokens":4}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
		`{"type":"message_stop"}`,
	})
	// Role, content, terminal choice: no intermediate empty-choice snapshots
	// or premature finish_reason on a usage-only message_delta.
	if len(chunks) != 3 {
		t.Fatalf("expected role/content/finish, got %v", chunks)
	}
	for i, chunk := range chunks {
		choices := chunk["choices"].([]any)
		if len(choices) != 1 {
			t.Fatalf("chunk %d has no choice: %v", i, chunk)
		}
		if i < len(chunks)-1 {
			if _, ok := chunk["usage"]; ok {
				t.Fatalf("interim usage on chunk %d: %v", i, chunk)
			}
			if choices[0].(map[string]any)["finish_reason"] != nil {
				t.Fatalf("premature finish: %v", chunk)
			}
		}
	}
	u := chunkUsage(t, chunks[len(chunks)-1])
	assertUsage(t, u, 200, 9, 209)
	details := u["prompt_tokens_details"].(map[string]any)
	if details["cached_tokens"] != float64(80) || details["cache_creation_tokens"] != float64(20) {
		t.Fatalf("cache detail = %v", details)
	}
}

func TestStreamUsageExplicitZeroAndAbsent(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":12,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"cache_read_input_tokens":0,"output_tokens":3}}`,
	})
	assertUsage(t, chunkUsage(t, chunks[len(chunks)-1]), 2, 3, 5)
	for _, chunk := range collectChunks(t, []string{`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`}) {
		if _, ok := chunk["usage"]; ok {
			t.Fatalf("invented usage: %v", chunk)
		}
	}
}

func TestStreamUsageToolCall(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`{"type":"content_block_start","content_block":{"type":"tool_use","id":"tool_1","name":"bash"}}`,
		`{"type":"content_block_delta","delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":7}}`,
	})
	last := chunks[len(chunks)-1]
	if last["choices"].([]any)[0].(map[string]any)["finish_reason"] != "tool_calls" {
		t.Fatalf("tool finish lost: %v", last)
	}
	assertUsage(t, chunkUsage(t, last), 10, 7, 17)
}

func TestNonstreamAndCollapsedUsage(t *testing.T) {
	raw := []byte(`{"id":"msg_cache","model":"GLM-5.3-Flash","stop_reason":"end_turn","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":100,"output_tokens":9,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}`)
	converted, err := anthropicToOpenAICompletion(raw, "GLM-5.3-Flash")
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(converted, &document); err != nil {
		t.Fatal(err)
	}
	assertUsage(t, chunkUsage(t, document), 200, 9, 209)
	var chunks []map[string]any
	err = emitCollapsedCompletion(converted, "GLM-5.3-Flash", func(b []byte) error {
		var chunk map[string]any
		if err := json.Unmarshal(b, &chunk); err != nil {
			return err
		}
		chunks = append(chunks, chunk)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks[:len(chunks)-1] {
		if _, ok := chunk["usage"]; ok {
			t.Fatalf("collapsed interim usage: %v", chunk)
		}
	}
	assertUsage(t, chunkUsage(t, chunks[len(chunks)-1]), 200, 9, 209)
}

func TestStreamUsageZeroSnapshot(t *testing.T) {
	chunks := collectChunks(t, []string{
		`{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":4}}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":0,"output_tokens":0}}`,
	})
	assertUsage(t, chunkUsage(t, chunks[len(chunks)-1]), 0, 0, 0)
}

func TestStreamUsageErrorAndSinkFailure(t *testing.T) {
	converter := newStreamConverter("glm")
	var chunks [][]byte
	emit := func(b []byte) error { chunks = append(chunks, b); return nil }
	input := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\ndata: {\"type\":\"error\",\"error\":{\"message\":\"interrupted\"}}\n\n"
	err := parseAnthropicSSE(strings.NewReader(input), converter, emit)
	if err == nil || !strings.Contains(err.Error(), "interrupted") || len(chunks) != 0 {
		t.Fatalf("error must propagate without a success/usage chunk: chunks=%q err=%v", chunks, err)
	}
	want := errors.New("client disconnected")
	for _, collapsed := range []bool{false, true} {
		calls := 0
		sink := func([]byte) error { calls++; return want }
		if collapsed {
			err = emitCollapsedCompletion([]byte(`{"choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],"usage":{"total_tokens":12}}`), "glm", sink)
		} else {
			err = converter.feed(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}}, sink)
		}
		if !errors.Is(err, want) || calls != 1 {
			t.Fatalf("sink error lost or retried: collapsed=%v calls=%d err=%v", collapsed, calls, err)
		}
	}
}

func assertUsage(t *testing.T, usage map[string]any, input, output, total float64) {
	t.Helper()
	if usage["prompt_tokens"] != input || usage["completion_tokens"] != output || usage["total_tokens"] != total {
		t.Fatalf("usage = %v, want input=%v output=%v total=%v", usage, input, output, total)
	}
}

func chunkUsage(t *testing.T, chunk map[string]any) map[string]any {
	t.Helper()
	usage, ok := chunk["usage"].(map[string]any)
	if !ok {
		t.Fatalf("missing usage: %v", chunk)
	}
	return usage
}
