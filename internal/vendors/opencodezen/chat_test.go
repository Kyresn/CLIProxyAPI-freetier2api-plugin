package opencodezen

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"testing"
)

// 本文件锁定 ZEN session ID 的编码规则。
//
// 这段编码是免费层的硬门槛：早期实现用随机十六进制填时间部分，形态对但
// 编码错，上游一律 403 FreeTierError，且错误信息完全看不出是编码问题。
// 因此这里对「形态」和「时间部分编码」分别断言。

func TestSessionIDShape(t *testing.T) {
	body := []byte(`{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	id := sessionIDFor(body)

	if !canonicalSessionPattern.MatchString(id) {
		t.Fatalf("session %q 不符合 canonical 形态", id)
	}
	if !descendingTimePartPattern.MatchString(id[4:16]) {
		t.Fatalf("session %q 的时间部分 %q 不是 descending 编码（免费层会 403）", id, id[4:16])
	}
	// 随机信号也必须落在同一形态内（对话取不到种子时的兜底路径）。
	if got := sessionIDFor([]byte(`{"messages":[]}`)); !canonicalSessionPattern.MatchString(got) ||
		!descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("兜底 session %q 不合规", got)
	}
	// 非 JSON 请求体同样不能 panic，且要产出合规 session。
	if got := sessionIDFor([]byte("not json")); !canonicalSessionPattern.MatchString(got) ||
		!descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("非 JSON 请求体的 session %q 不合规", got)
	}
}

// TestSessionIDStablePerConversation 验证同一段对话得到同一个 session。
//
// 用整段历史做种子的话，多轮对话每轮都会换 session，上游 prompt cache
// 永远命中不了；这里钉住「只看首条 user 消息」。
func TestSessionIDStablePerConversation(t *testing.T) {
	first := []byte(`{"messages":[{"role":"user","content":"讲个笑话"},{"role":"assistant","content":"好的"}]}`)
	grown := []byte(`{"messages":[{"role":"user","content":"讲个笑话"},{"role":"assistant","content":"好的"},` +
		`{"role":"user","content":"再来一个"},{"role":"assistant","content":"好"}]}`)
	if sessionIDFor(first) != sessionIDFor(grown) {
		t.Fatal("同一段对话在历史增长后 session 变了，prompt cache 会失效")
	}

	other := []byte(`{"messages":[{"role":"user","content":"讲个别的"}]}`)
	if sessionIDFor(first) == sessionIDFor(other) {
		t.Fatal("不同对话得到了相同 session")
	}
}

// TestCanonicalSessionIDPreservesOfficial 验证已经是官方铸造的 session 原样保留。
func TestCanonicalSessionIDPreservesOfficial(t *testing.T) {
	// descendingTimePart 对任意哈希都产出以 ffe 收尾的时间部分。
	official := canonicalSessionID("any-signal")
	if got := canonicalSessionID(official); got != official {
		t.Fatalf("官方形态的 session 被重编码了：%q -> %q", official, got)
	}
	// 形态对但编码错（随机十六进制）时必须重编码，否则上游 403。
	randomHex := "ses_" + "0123456789ab" + "ABCDEFGHIJKLMN"
	if !canonicalSessionPattern.MatchString(randomHex) {
		t.Fatal("测试样本本身不合法")
	}
	if got := canonicalSessionID(randomHex); !descendingTimePartPattern.MatchString(got[4:16]) {
		t.Fatalf("随机时间部分未被重编码：%q", got)
	}
}

// TestDescendingTimePartEndsWithFFE 验证时间部分的低 12 位落在 ffe。
func TestDescendingTimePartEndsWithFFE(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{9}ffe$`)
	for _, seed := range []string{"a", "b", "hello world", "\x00\x01", "讲个笑话"} {
		id := canonicalSessionID(seed)
		if !pattern.MatchString(id[4:16]) {
			t.Errorf("seed %q 的时间部分 %q 未以 ffe 收尾", seed, id[4:16])
		}
	}
}

// TestUserAgentMatchesV2Shape 锁定 v2 客户端标识。
//
// 1.x 的两段式 UA（opencode/1.18.31 (os arch; go)）已被上游拒绝；v2 形态取自
// 官方 packages/core/src/app.ts 的 App.useragent，版本取自官方
// packages/cli/package.json。
func TestUserAgentMatchesV2Shape(t *testing.T) {
	if got := UserAgent(); got != "opencode/latest/2.1.4/cli" {
		t.Fatalf("UserAgent = %q，期望 opencode/latest/2.1.4/cli", got)
	}
}

// TestApplyChatHeadersCarriesSessionMirrors 锁定会话头的镜像关系。
//
// 官方 v2 客户端同时写 x-opencode-session-id / x-opencode-session /
// x-session-affinity / X-Session-Id；缺 x-opencode-session-id 会被判成非官方客户端。
func TestApplyChatHeadersCarriesSessionMirrors(t *testing.T) {
	req, errNew := http.NewRequest(http.MethodPost, "https://example.invalid/chat", nil)
	if errNew != nil {
		t.Fatalf("new request: %v", errNew)
	}
	ApplyChatHeaders(req, "key", "ses_abc")

	for _, name := range []string{"x-opencode-session", "x-opencode-session-id", "x-session-affinity", "x-session-id"} {
		if got := req.Header.Get(name); got != "ses_abc" {
			t.Fatalf("%s = %q，期望 ses_abc", name, got)
		}
	}
	if got := req.Header.Get("x-opencode-client"); got != "cli" {
		t.Fatalf("x-opencode-client = %q，期望 cli", got)
	}
	if got := req.Header.Get("User-Agent"); got != UserAgent() {
		t.Fatalf("User-Agent = %q", got)
	}
}

func TestChatEndpointSplitsFreeAndPaidLanes(t *testing.T) {
	pay := chatEndpoint("", false)
	if want := DefaultBaseURL + ChatPath; pay != want {
		t.Fatalf("付费出口 = %q，期望 %q", pay, want)
	}
	free := chatEndpoint("", true)
	if want := FreeTierBaseURL + ChatPath; free != want {
		t.Fatalf("免费出口 = %q，期望 %q", free, want)
	}
	// 配置了自建/自托管网关时两层都走覆盖值，便于用户自救。
	if got, want := chatEndpoint("https://gateway.example/zen/", true), "https://gateway.example/zen"+ChatPath; got != want {
		t.Fatalf("覆盖后的免费出口 = %q，期望 %q", got, want)
	}
}

func TestNewChatRequestFreeTierUsesAnonymousCredentialAndOfficialTools(t *testing.T) {
	body := []byte(`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	req, errReq := newChatRequest(context.Background(), "", ChatRequest{APIKey: "oc_sk_secret", Body: body})
	if errReq != nil {
		t.Fatalf("newChatRequest: %v", errReq)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+freeTierCredential {
		t.Fatalf("免费层凭证 = %q，期望匿名凭证", got)
	}
	if got := req.URL.String(); got != FreeTierBaseURL+ChatPath {
		t.Fatalf("免费层出口 = %q", got)
	}

	payload := decodePayload(t, req)
	if payload["stream"] != true {
		t.Fatal("免费层未强制 stream")
	}
	opt, _ := payload["stream_options"].(map[string]any)
	if opt == nil || opt["include_usage"] != true {
		t.Fatal("免费层未要求 stream_options.include_usage")
	}

	tools, _ := payload["tools"].([]any)
	declared := make(map[string]map[string]any, len(tools))
	for _, item := range tools {
		toolMap, _ := item.(map[string]any)
		fn, _ := toolMap["function"].(map[string]any)
		name, _ := fn["name"].(string)
		declared[name] = fn
	}
	for _, name := range []string{"glob", "grep", "read", "shell"} {
		fn, ok := declared[name]
		if !ok {
			t.Fatalf("免费层缺少官方工具 %q", name)
		}
		params, _ := fn["parameters"].(map[string]any)
		props, _ := params["properties"].(map[string]any)
		if len(props) == 0 {
			t.Fatalf("工具 %q 的 parameters 是空壳，上游会判成伪造客户端", name)
		}
	}
}

func TestNewChatRequestPaidKeepsUserCredential(t *testing.T) {
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	req, errReq := newChatRequest(context.Background(), "", ChatRequest{APIKey: "oc_sk_secret", Body: body})
	if errReq != nil {
		t.Fatalf("newChatRequest: %v", errReq)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer oc_sk_secret" {
		t.Fatalf("付费凭证 = %q", got)
	}
	if got := req.URL.String(); got != DefaultBaseURL+ChatPath {
		t.Fatalf("付费出口 = %q", got)
	}
	if _, ok := decodePayload(t, req)["tools"]; ok {
		t.Fatal("付费请求不应被注入免费层工具")
	}
}

func TestPrepareFreeTierBodyKeepsCallerTools(t *testing.T) {
	body := []byte(`{"model":"beam-free","tools":[{"type":"function","function":{"name":"read","description":"caller","parameters":{"type":"object","properties":{"p":{"type":"string"}}}}}],"messages":[]}`)
	payload := decodeJSON(t, prepareFreeTierBody(body))
	tools, _ := payload["tools"].([]any)
	seen := map[string]int{}
	for _, item := range tools {
		toolMap, _ := item.(map[string]any)
		fn, _ := toolMap["function"].(map[string]any)
		name, _ := fn["name"].(string)
		seen[name]++
		if name == "read" {
			if desc, _ := fn["description"].(string); desc != "caller" {
				t.Fatalf("调用方自带的 read 被覆盖：%q", desc)
			}
		}
	}
	if seen["read"] != 1 {
		t.Fatalf("read 出现了 %d 次", seen["read"])
	}
	if seen["shell"] != 1 {
		t.Fatalf("缺少补齐的 shell 工具：%d", seen["shell"])
	}
}

// decodePayload 取出请求体并解码。
func decodePayload(t *testing.T, req *http.Request) map[string]any {
	t.Helper()
	raw, errRead := io.ReadAll(req.Body)
	if errRead != nil {
		t.Fatalf("read body: %v", errRead)
	}
	return decodeJSON(t, raw)
}

func decodeJSON(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var payload map[string]any
	if errUnmarshal := json.Unmarshal(raw, &payload); errUnmarshal != nil {
		t.Fatalf("unmarshal body: %v", errUnmarshal)
	}
	return payload
}
