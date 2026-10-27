package tests

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/app"
)

// Exercise the real plugin RPC -> vendor -> HTTP bridge -> host stream path.
// The fake host records executor payloads, not public HTTP responses or usage
// database rows. CPA observes these counters before translating protocols.
func TestZCodeExecutorStreamUsage(t *testing.T) {
	for _, option := range []string{"", `,"stream_options":{"include_usage":true}`, `,"stream_options":{"include_usage":false}`} {
		for _, collapsed := range []bool{false, true} {
			t.Run(fmt.Sprintf("option=%s/collapsed=%v", option, collapsed), func(t *testing.T) {
				host := installFakeHost(t)
				setupTestPlugin(t)
				host.addAccount("1", "zcode-test.json", `{"vendor":"zcode","api_key":"k.s"}`)
				host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
					if method != http.MethodPost || !strings.HasSuffix(url, "/messages") {
						return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{}`}
					}
					// Client stream_options is a host concern, not Anthropic input.
					if strings.Contains(body, "stream_options") {
						t.Errorf("OpenAI-only options leaked upstream: %s", body)
					}
					if collapsed {
						return fakeUpstreamResponse{Status: 200, Header: map[string][]string{"Content-Type": {"application/json"}},
							Body: `{"id":"msg_1","model":"GLM-5.3-Flash","stop_reason":"end_turn","content":[{"type":"text","text":"OK"}],"usage":{"input_tokens":100,"cache_read_input_tokens":80,"cache_creation_input_tokens":20,"output_tokens":9}}`}
					}
					return fakeUpstreamResponse{Status: 200, Header: map[string][]string{"Content-Type": {"text/event-stream"}},
						Body: strings.Join([]string{
							`data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":100,"cache_read_input_tokens":80,"cache_creation_input_tokens":20}}}`,
							`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"OK"}}`,
							`data: {"type":"message_delta","delta":{},"usage":{"output_tokens":4}}`,
							`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9}}`,
							`data: {"type":"message_stop"}`, "",
						}, "\n\n")}
				})
				callMethod(t, pluginabi.MethodExecutorExecuteStream, app.ExecutorRPCRequest{
					ExecutorRequest: pluginapi.ExecutorRequest{
						AuthID: "1", Model: "zcode:GLM-5.3-Flash", Format: app.FormatChatCompletions, Stream: true,
						Payload: []byte(`{"model":"GLM-5.3-Flash","stream":true,"messages":[{"role":"user","content":"hi"}]` + option + `}`),
					}, StreamID: "usage-stream",
				})
				if !waitFor(t, 3*time.Second, func() bool { return len(host.closedStreams()) == 1 }) {
					t.Fatal("stream did not close")
				}
				if errs := host.streamErrors(); len(errs) != 0 {
					t.Fatalf("stream errors: %v", errs)
				}
				chunks := host.emittedChunks()
				if len(chunks) != 3 {
					t.Fatalf("expected role/content/finish, got %q", chunks)
				}
				usageCount, finishCount := 0, 0
				for index, raw := range chunks {
					var chunk struct {
						Choices []struct {
							FinishReason *string `json:"finish_reason"`
						} `json:"choices"`
						Usage *struct {
							Input  int `json:"prompt_tokens"`
							Output int `json:"completion_tokens"`
							Total  int `json:"total_tokens"`
						} `json:"usage"`
					}
					if err := json.Unmarshal([]byte(raw), &chunk); err != nil {
						t.Fatal(err)
					}
					if len(chunk.Choices) != 1 {
						t.Fatalf("empty-choice snapshot: %s", raw)
					}
					if chunk.Choices[0].FinishReason != nil {
						finishCount++
						if index != len(chunks)-1 || *chunk.Choices[0].FinishReason != "stop" {
							t.Fatalf("premature/incorrect finish: %s", raw)
						}
					}
					if chunk.Usage != nil {
						usageCount++
						if index != len(chunks)-1 || chunk.Usage.Input != 200 || chunk.Usage.Output != 9 || chunk.Usage.Total != 209 {
							t.Fatalf("unexpected usage: %s", raw)
						}
					}
				}
				if usageCount != 1 || finishCount != 1 {
					t.Fatalf("usage=%d finish=%d, want one each", usageCount, finishCount)
				}
			})
		}
	}
}
