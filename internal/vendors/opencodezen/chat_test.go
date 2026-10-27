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

func TestChatEndpointUsesInferenceLane(t *testing.T) {
	// 免费与付费共用同一条 inference 车道（官方 v2 靠凭证区分，不靠路径）。
	if got, want := chatEndpoint(""), "https://opencode.ai/inference/openai"+ChatPath; got != want {
		t.Fatalf("出口 = %q，期望 %q", got, want)
	}
	// 配置了自建/自托管网关时走覆盖值，便于用户自救。
	if got, want := chatEndpoint("https://gateway.example/zen/"), "https://gateway.example/zen"+ChatPath; got != want {
		t.Fatalf("覆盖后的出口 = %q，期望 %q", got, want)
	}
}

// TestModelsEndpointStripsOpenAILayer 锁定清单出口比 chat 少一层 /openai。
//
// /inference/openai/v1/models 实测 404，清单在 /inference/v1/models；
// 覆盖值只写一层时也要能退回去，否则自建网关拿不到模型清单。
func TestModelsEndpointStripsOpenAILayer(t *testing.T) {
	if got, want := ModelsBaseURLFor(""), "https://opencode.ai/inference"; got != want {
		t.Fatalf("清单出口 = %q，期望 %q", got, want)
	}
	if got, want := ModelsBaseURLFor("https://gw.example/inference/openai/"), "https://gw.example/inference"; got != want {
		t.Fatalf("覆盖后的清单出口 = %q，期望 %q", got, want)
	}
}

func TestNewChatRequestFreeTierUsesAnonymousCredential(t *testing.T) {
	// mimo-v2.6-flash-free 在目录里是零价，按单价判定应进免费层。
	catalog := modelsDevCatalog{
		"mimo-v2.6-flash-free": {Cost: &modelsDevCost{}},
	}
	body := []byte(`{"model":"mimo-v2.6-flash-free","messages":[{"role":"user","content":"hi"}]}`)
	req, errReq := newChatRequest(context.Background(), "", ChatRequest{APIKey: "oc_sk_secret", Body: body}, catalog)
	if errReq != nil {
		t.Fatalf("newChatRequest: %v", errReq)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer "+freeTierCredential {
		t.Fatalf("免费层凭证 = %q，期望匿名凭证", got)
	}
	if got := req.URL.String(); got != DefaultBaseURL+ChatPath {
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
	// 官方客户端只透传会话自有工具，不按模型补一份；这里确认我们不注入。
	if _, ok := payload["tools"]; ok {
		t.Fatal("免费层不应注入工具定义")
	}
}

// TestFreeTierDetectionUsesPriceNotName 锁定免费判定口径。
//
// 官方用「单价为 0」判定免费（packages/core/src/plugin/provider/opencode.ts），
// 目录里 big-pickle 零价但不带 "-free" 后缀，还是官方登出状态的默认模型。
// 按名字判定会把它错推上付费车道。
func TestFreeTierDetectionUsesPriceNotName(t *testing.T) {
	catalog := modelsDevCatalog{
		"big-pickle":           {Cost: &modelsDevCost{}},
		"glm-5.3":              {Cost: &modelsDevCost{Input: 1.5, Output: 6}},
		"payment-free-in-name": {Cost: &modelsDevCost{Input: 2, Output: 4}},
	}

	if !isFreeTierModel("big-pickle", catalog) {
		t.Fatal("零价但无 free 后缀的模型应判定为免费")
	}
	if isFreeTierModel("glm-5.3", catalog) {
		t.Fatal("有价格的模型不能判定为免费")
	}
	if isFreeTierModel("payment-free-in-name", catalog) {
		t.Fatal("目录说它收费时，名字里的 free 不能覆盖价格")
	}
	// 目录缺失时退回名字判定，不能让免费请求误用用户 key。
	if !isFreeTierModel("some-model-free", nil) {
		t.Fatal("目录不可用时应退回按名字判定")
	}
	if isFreeTierModel("glm-5.3", nil) {
		t.Fatal("目录不可用且名字无 free 时应按付费处理")
	}
}

func TestNewChatRequestPaidKeepsUserCredential(t *testing.T) {
	catalog := modelsDevCatalog{
		"glm-5.3": {Cost: &modelsDevCost{Input: 1.5, Output: 6}},
	}
	body := []byte(`{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`)
	req, errReq := newChatRequest(context.Background(), "", ChatRequest{APIKey: "oc_sk_secret", Body: body}, catalog)
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

// TestMaxTokensNormalizedToCompletionTokens 锁定上限字段名。
//
// 官方 2.1.4 对 opencode 家族一律发 max_completion_tokens
// （packages/ai/src/protocols/openai-chat.ts 的 detectMaxTokensField 落默认分支）。
func TestMaxTokensNormalizedToCompletionTokens(t *testing.T) {
	out := decodeJSON(t, normalizeMaxTokens([]byte(`{"model":"glm-5.3","max_tokens":256}`)))
	if got := out["max_completion_tokens"]; got != float64(256) {
		t.Fatalf("max_completion_tokens = %v，期望 256", got)
	}
	if _, exists := out["max_tokens"]; exists {
		t.Fatal("max_tokens 应被移除，避免同时出现两个上限字段")
	}

	// 已经只给 max_completion_tokens 时保持原样。
	same := decodeJSON(t, normalizeMaxTokens([]byte(`{"model":"glm-5.3","max_completion_tokens":64}`)))
	if got := same["max_completion_tokens"]; got != float64(64) {
		t.Fatalf("max_completion_tokens = %v，期望 64", got)
	}

	// 两个都给时以 max_completion_tokens 为准。
	both := decodeJSON(t, normalizeMaxTokens([]byte(`{"max_tokens":10,"max_completion_tokens":99}`)))
	if got := both["max_completion_tokens"]; got != float64(99) {
		t.Fatalf("max_completion_tokens = %v，期望保留 99", got)
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
