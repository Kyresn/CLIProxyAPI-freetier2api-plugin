package codearts

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestClampMaxTokens 锁定请求侧的输出上限夹取。
//
// 元数据侧把目录上限夹到 65536 只解决一半：客户端自己带 max_tokens 时仍会
// 触发上游平台级校验（65537 起整条请求 400）。这里覆盖两种键名、边界值，
// 以及「不该动」的输入。
func TestClampMaxTokens(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{name: "超限回夹", in: `{"model":"glm-5.2","max_tokens":131072}`, want: maxOutputTokenCap},
		{name: "completion 键同样夹", in: `{"max_completion_tokens":393216}`, want: maxOutputTokenCap},
		{name: "浮点形态也夹", in: `{"max_tokens":131072.0}`, want: maxOutputTokenCap},
		{name: "数字字符串也夹", in: `{"max_tokens":"131072"}`, want: maxOutputTokenCap},
		{name: "两键同时超限", in: `{"max_tokens":200000,"max_completion_tokens":300000}`, want: maxOutputTokenCap},
		{name: "恰好在顶保留", in: `{"max_tokens":65536}`, want: 65536},
		{name: "未超限原样", in: `{"max_tokens":4096}`, want: 4096},
	}
	v := &adapter{}
	for _, tc := range cases {
		out := v.prepareRequestBody([]byte(tc.in))
		var payload map[string]any
		if err := json.Unmarshal(out, &payload); err != nil {
			t.Fatalf("%s: 输出不是合法 JSON: %v (%s)", tc.name, err, out)
		}
		got, ok := payload["max_tokens"]
		if !ok {
			got = payload["max_completion_tokens"]
		}
		num, ok := got.(float64)
		if !ok || int(num) != tc.want {
			t.Fatalf("%s: max_tokens = %v, want %d", tc.name, got, tc.want)
		}
	}
}

// TestClampMaxTokensPassesThroughUntouched 验证不该改写的情况原样放行。
func TestClampMaxTokensPassesThroughUntouched(t *testing.T) {
	for _, body := range []string{
		`not json at all`,
		`{"model":"glm-5.2"}`,
		`{"max_tokens":true}`,
		`{"max_tokens":{}}`,
		`{"max_tokens":null}`,
		`[]`,
		`null`,
		`{"max_tokens":4096}`,
	} {
		if got := string((&adapter{}).prepareRequestBody([]byte(body))); got != body {
			t.Fatalf("body 应原样返回，实际 %q", got)
		}
	}
}

// TestForwardStreamConvertsBusinessErrorFrame 验证 data: 帧里的业务错误被翻成
// OpenAI 错误帧，而不是原样交给宿主（宿主认不出就会被「流提前结束」覆盖）。
func TestForwardStreamConvertsBusinessErrorFrame(t *testing.T) {
	body := "data: " + streamErrorFrame + "\n\ndata: [DONE]\n\n"
	var emitted []string
	errForward := ForwardStream(strings.NewReader(body), func(payload []byte) error {
		emitted = append(emitted, string(payload))
		return nil
	})
	if errForward != nil {
		t.Fatalf("ForwardStream: %v", errForward)
	}
	if len(emitted) != 1 {
		t.Fatalf("只应下发一帧转换后的错误，实际 %d 帧: %v", len(emitted), emitted)
	}
	for _, want := range []string{`"upstream_error"`, "benefit not found", "req-fixture-0001"} {
		if !strings.Contains(emitted[0], want) {
			t.Fatalf("错误帧缺少 %q: %s", want, emitted[0])
		}
	}
	if strings.Contains(emitted[0], "error_code") {
		t.Fatalf("下发帧必须已是 OpenAI 形态: %s", emitted[0])
	}
}

// TestForwardStreamPassesNormalFrames 验证正常帧与注释行原样透传。
func TestForwardStreamPassesNormalFrames(t *testing.T) {
	body := ": keep-alive\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	var emitted []string
	if errForward := ForwardStream(strings.NewReader(body), func(payload []byte) error {
		emitted = append(emitted, string(payload))
		return nil
	}); errForward != nil {
		t.Fatalf("ForwardStream: %v", errForward)
	}
	if len(emitted) != 2 || emitted[0] != ": keep-alive" || !strings.Contains(emitted[1], `"content":"hi"`) {
		t.Fatalf("正常帧应原样透传，实际 %v", emitted)
	}
}

// TestChatRejectsBusinessErrorIn200Body 验证非流式 200 包体里的业务错误被转成
// 分类错误，而不是原样透传给客户端。
func TestChatRejectsBusinessErrorIn200Body(t *testing.T) {
	stubHostWithBody(t, `{"error_code":"4004","error_msg":"benefit not found","result":null}`)
	_, errChat := Chat(context.Background(), "http://codearts.test", ChatRequest{
		Credential: &Credential{AccessKeyID: "ak", SecretAccessKey: "sk"},
		Body:       []byte(`{"model":"glm-5.3","messages":[]}`),
	})
	if errChat == nil {
		t.Fatal("200 包体里的业务错误必须报错")
	}
	failure, okFailure := errChat.(*core.Failure)
	if !okFailure {
		t.Fatalf("应返回 *core.Failure，实际 %T: %v", errChat, errChat)
	}
	if !strings.Contains(failure.Msg, "benefit not found") || !strings.Contains(failure.Msg, benefitHint) {
		t.Fatalf("message 应保留上游原文并给出可操作提示: %s", failure.Msg)
	}
}
