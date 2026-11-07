package codearts

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/hostbridge/hostbridgetest"
)

// pastedCallback 是浏览器授权后停在 127.0.0.1 上的真实回调链接样本。
//
// 它是整个 bug 的关键证据：外层 secret 与 redirect 里的 ticket_id / secret 都是
// **华为云签发**的，不是 LoginStart 送进 /authorize 的那一对。插件此前一直拿自造
// 值轮询，所以控制台永远停在「等待授权中…」。
const pastedCallback = "http://127.0.0.1/oauth/callback?secret=d55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280" +
	"&redirect=https%3A%2F%2Fcodearts.huaweicloud.com%2Fportal%2Flogin%3Ftheme%3Dlight%26locale%3Dzh-cn" +
	"%26ticket_id%3D657abbb247eab363d1a461285fc50145%26authorizationAvailable%3Dfalse" +
	"%26secret%3Dd55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280"

const (
	upstreamTicketID = "657abbb247eab363d1a461285fc50145"
	upstreamSecret   = "d55df9259e688e2a71b93f058c748349782ca1a30440473980961ef6833ee280"
)

// TestParseCallbackTakesUpstreamTicket 锁定解析口径：ticket_id / secret 取自回调
// （含 redirect 里的门户登录地址），而不是插件自造的那一对。
func TestParseCallbackTakesUpstreamTicket(t *testing.T) {
	info, errParse := ParseCallback(pastedCallback)
	if errParse != nil {
		t.Fatalf("parse callback: %v", errParse)
	}
	if info.TicketID != upstreamTicketID {
		t.Fatalf("ticket id = %q, want %q", info.TicketID, upstreamTicketID)
	}
	if info.Secret != upstreamSecret {
		t.Fatalf("secret = %q, want %q", info.Secret, upstreamSecret)
	}
	if info.Redirect == "" {
		t.Fatal("redirect 未保留：票据没换到凭证时用户还要靠它继续授权")
	}
}

// TestParseCallbackRejectsLinkWithoutTicket 守住解析下限：没有票据的链接不能被
// 当成有效回调（否则用户只会看到一句莫名其妙的上游报错）。
func TestParseCallbackRejectsLinkWithoutTicket(t *testing.T) {
	if _, errParse := ParseCallback("http://127.0.0.1/oauth/callback?secret=only-secret"); errParse == nil {
		t.Fatal("只有 secret、没有 code/redirect 的链接必须解析失败")
	}
}

// TestSubmitCallbackKeepsSessionTicket 锁定票据通道最容易错的一点：
//
// 回调里那个 ticket_id 是门户的中间票，拿它去查票据接口必然得到「无效ticketId」
// （实测 TM.00001001）。真正要换的只有 secret——ticket_id 必须仍是本会话自己那个。
func TestSubmitCallbackKeepsSessionTicket(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	var gotTicket, gotSecret string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTicket = r.URL.Query().Get("ticket_id")
		gotSecret = r.URL.Query().Get("secret")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"user_id":"user-1","user_name":"tester","refresh_token":"refresh-1",` +
			`"credentials":{"access_key_id":"AK-test","secret_access_key":"SK-test",` +
			`"security_token":"ST-test","expiration":"2026-10-19T18:00:00Z"}}`))
	}))
	defer server.Close()

	// 本会话的票据号必须与回调 redirect 里的一致，这里就取回调里那个当会话票据号。
	info, errParse := ParseCallback(pastedCallback)
	if errParse != nil {
		t.Fatalf("parse callback: %v", errParse)
	}
	logins := auth.NewSessionRegistry()
	logins.Register(VendorID, "codearts_paste", 15*time.Minute, 0, &pendingLogin{
		TicketID:  info.TicketID,
		Secret:    "self-made-secret",
		TicketURL: server.URL,
	})

	result, errSubmit := SubmitCallback(context.Background(), logins, "codearts_paste", pastedCallback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusSuccess {
		t.Fatalf("status = %q, want success (message=%q)", result.Status, result.Message)
	}
	if gotTicket != info.TicketID {
		t.Fatalf("票据查询用了 %q，want 本会话的 %q（不能换成门户的中间票）", gotTicket, info.TicketID)
	}
	if gotSecret != upstreamSecret {
		t.Fatalf("票据查询用了 secret %q，want 门户下发的 %q", gotSecret, upstreamSecret)
	}
	if result.Auth.FileName != "codearts-user-1.json" {
		t.Fatalf("file name = %q, want codearts-user-1.json", result.Auth.FileName)
	}

	var storage map[string]any
	if err := json.Unmarshal(result.Auth.StorageJSON, &storage); err != nil {
		t.Fatalf("decode storage json: %v", err)
	}
	if storage["access_key_id"] != "AK-test" {
		t.Fatalf("凭证未落盘: %v", storage["access_key_id"])
	}
}

// TestSubmitCallbackRejectsForeignTicket 守住归属：回调里的票据号与本会话不一致时
// 不能改会话状态，否则一次误粘贴就能把凭证写进别人的登录会话。
func TestSubmitCallbackRejectsForeignTicket(t *testing.T) {
	logins := auth.NewSessionRegistry()
	logins.Register(VendorID, "codearts_foreign", 15*time.Minute, 0, &pendingLogin{
		TicketID: "00000000000000000000000000000000",
		Secret:   "self-made-secret",
	})

	_, errSubmit := SubmitCallback(context.Background(), logins, "codearts_foreign", pastedCallback)
	if errSubmit == nil {
		t.Fatal("票据号不属于本会话时必须报错")
	}
}

// TestSubmitCallbackKeepsPortalURLWhilePending 锁定「还没换到凭证」这一步：把回调里
// 的门户登录地址带回去，让用户能在能联网的浏览器里继续完成授权，插件则继续轮询。
func TestSubmitCallbackKeepsPortalURLWhilePending(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	logins := auth.NewSessionRegistry()
	logins.Register(VendorID, "codearts_pending", 15*time.Minute, 0, &pendingLogin{TicketURL: server.URL})

	result, errSubmit := SubmitCallback(context.Background(), logins, "codearts_pending", pastedCallback)
	if errSubmit != nil {
		t.Fatalf("submit callback: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", result.Status)
	}
	if result.RedirectURL == "" {
		t.Fatal("pending 时必须带回门户登录地址，否则用户没有下一步")
	}
}

// TestSubmitCallbackRejectsUnknownSession 守住会话边界：没有登录会话时不能凭一条
// 链接就换出凭证。
func TestSubmitCallbackRejectsUnknownSession(t *testing.T) {
	if _, errSubmit := SubmitCallback(context.Background(), auth.NewSessionRegistry(), "codearts_missing", pastedCallback); errSubmit == nil {
		t.Fatal("未知会话必须报错")
	}
}

// TestSubmitCallbackKeepsWaitingOnInvalidTicket 锁定一个反直觉但必须如此的行为：
//
// 上游对「票据还没生效」和「票据号不存在」回的是同一句 TM.00001001 无效ticketId
// （实测：随便编一个 ticket_id 也是这句）。所以这句话**不能**当终态——判成失败
// 就等于把「用户还没点完授权」永远钉死成「票据已失效，请重新发起登录」。
func TestSubmitCallbackKeepsWaitingOnInvalidTicket(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error_code":"TM.00001001","error_msg":"无效ticketId: ` + upstreamTicketID + `"}`))
	}))
	defer server.Close()

	info, errParse := ParseCallback(pastedCallback)
	if errParse != nil {
		t.Fatalf("parse callback: %v", errParse)
	}
	logins := auth.NewSessionRegistry()
	logins.Register(VendorID, "codearts_wait", 15*time.Minute, 0, &pendingLogin{
		TicketID: info.TicketID, TicketURL: server.URL,
	})

	result, errSubmit := SubmitCallback(context.Background(), logins, "codearts_wait", pastedCallback)
	if errSubmit != nil {
		t.Fatalf("票据未生效属于等待，不该报错: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", result.Status)
	}
	if !strings.Contains(result.Message, "TM.00001001") {
		t.Fatalf("等待提示里没有上游原因: %q", result.Message)
	}
	if result.RedirectURL == "" {
		t.Fatal("等待时必须带回门户登录页，否则用户没有下一步")
	}
}

// TestSubmitCallbackSurfacesUpstreamNote 锁定诊断信息透传：还在等的时候，页面必须
// 能看到上游到底说了什么，否则「等待中」既没法自查也没法上报。
func TestSubmitCallbackSurfacesUpstreamNote(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error_code":"TM.00001002","error_msg":"用户尚未完成授权"}`))
	}))
	defer server.Close()

	logins := auth.NewSessionRegistry()
	info, errParse := ParseCallback(pastedCallback)
	if errParse != nil {
		t.Fatalf("parse callback: %v", errParse)
	}
	logins.Register(VendorID, "codearts_waiting", 15*time.Minute, 0,
		&pendingLogin{TicketID: info.TicketID, TicketURL: server.URL})

	result, errSubmit := SubmitCallback(context.Background(), logins, "codearts_waiting", pastedCallback)
	if errSubmit != nil {
		t.Fatalf("未授权属于等待，不该报错: %v", errSubmit)
	}
	if result.Status != pluginapi.AuthLoginStatusPending {
		t.Fatalf("status = %q, want pending", result.Status)
	}
	if !strings.Contains(result.Message, "TM.00001002") {
		t.Fatalf("pending 提示里没有上游原因: %q", result.Message)
	}
	if result.RedirectURL == "" {
		t.Fatal("pending 时仍需带回门户登录地址")
	}
}
