package autoclaw

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/hostbridge/hostbridgetest"
)

// TestCheckinDetectsNotClaimed 锁住「code=0 但 data.success=false」不算成功。
//
// 上游会用这个形态表达「窗口未开 / 无资格」；旧实现只看 code，会把没领到
// 报成签到成功并写入 LastDate，当天不再重试。
func TestCheckinDetectsNotClaimed(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"success":false,"already_completed":false,"reward_points":0,"task_id":"daily_signin"}}`))
	}))
	defer server.Close()

	result, errCheckin := Checkin(context.Background(), server.URL, &Credential{AccessToken: "tok"})
	if errCheckin != nil {
		t.Fatalf("Checkin: %v", errCheckin)
	}
	if result.Claimed {
		t.Fatalf("success=false 不应算领取成功: %+v", result)
	}
	if result.Already {
		t.Fatalf("already_completed=false 不应算已签到: %+v", result)
	}
}

// TestCheckinDetectsRewardPoints 锁住 reward_points > 0 即视为领取成功。
func TestCheckinDetectsRewardPoints(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":{"success":true,"already_completed":false,"reward_points":500,"task_id":"daily_signin"}}`))
	}))
	defer server.Close()

	result, errCheckin := Checkin(context.Background(), server.URL, &Credential{AccessToken: "tok"})
	if errCheckin != nil {
		t.Fatalf("Checkin: %v", errCheckin)
	}
	if !result.Claimed || result.Points != 500 {
		t.Fatalf("应领取成功 +500: %+v", result)
	}
}

// TestCheckinTreatsAuthExpiredCodeAsSessionDead 锁住 400000 也是登录过期。
func TestCheckinTreatsAuthExpiredCodeAsSessionDead(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":400000,"msg":"login expired"}`))
	}))
	defer server.Close()

	_, errCheckin := Checkin(context.Background(), server.URL, &Credential{AccessToken: "tok"})
	if errCheckin == nil {
		t.Fatal("400000 应报错")
	}
	failure, okFailure := errCheckin.(*core.Failure)
	if !okFailure || failure.Kind != core.FailureKindSessionDead {
		t.Fatalf("应分类为 session_dead: %+v", errCheckin)
	}
}

// TestCheckinNonZeroBusinessCodeIsNotClaimed 锁住「非登录失效的业务码」不算失败
// 也不算成功：返回未领取，让上层记 NotRecorded。
func TestCheckinNonZeroBusinessCodeIsNotClaimed(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":12345,"msg":"task not available"}`))
	}))
	defer server.Close()

	result, errCheckin := Checkin(context.Background(), server.URL, &Credential{AccessToken: "tok"})
	if errCheckin != nil {
		t.Fatalf("业务码不应变成网络错误: %v", errCheckin)
	}
	if result.Claimed || result.Already {
		t.Fatalf("非零业务码不应算领取成功: %+v", result)
	}
	if result.Msg != "task not available" {
		t.Fatalf("Msg = %q", result.Msg)
	}
}

// TestApplyUserAPIHeadersUsesDesktopFingerprint 锁住官方桌面端指纹。
func TestApplyUserAPIHeadersUsesDesktopFingerprint(t *testing.T) {
	req, errNew := http.NewRequest(http.MethodPost, "https://example.test", nil)
	if errNew != nil {
		t.Fatalf("new request: %v", errNew)
	}
	ApplyUserAPIHeaders(req, "tok")
	if got := req.Header.Get("X-Client-Type"); got != "pc" {
		t.Errorf("X-Client-Type = %q, want pc", got)
	}
	if got := req.Header.Get("X-Harness-Type"); got != "zcode" {
		t.Errorf("X-Harness-Type = %q, want zcode", got)
	}
	if got := req.Header.Get("X-Lang"); got != "zh-CN" {
		t.Errorf("X-Lang = %q, want zh-CN", got)
	}
	if got := req.Header.Get("X-Tm"); got != "win" && got != "linux" {
		t.Errorf("X-Tm = %q, want win/linux", got)
	}
}

// TestTriggerLoginActivePostsDeviceIdentity 锁住登录活跃上报的端点、请求体与鉴权。
//
// 服务端按 source_id + device_id 匹配「哪台设备今天登录过」，缺任何一个都会
// 静默不发奖励，所以这两个字段必须与签到共用同一份设备身份。
func TestTriggerLoginActivePostsDeviceIdentity(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	var gotPath string
	var gotAuth string
	var gotBody map[string]string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	TriggerLoginActive(context.Background(), server.URL, &Credential{AccessToken: "tok", DeviceID: "dev-1"})

	if gotPath != UserInfoPath {
		t.Fatalf("path = %q, want %q", gotPath, UserInfoPath)
	}
	if gotAuth != bearer("tok") {
		t.Fatalf("Authorization = %q", gotAuth)
	}
	if gotBody["source_id"] != "autoclaw" || gotBody["device_id"] != "dev-1" {
		t.Fatalf("body = %v", gotBody)
	}
}

// TestTriggerLoginActiveSkipsWithoutDeviceID 锁住缺设备号时直接跳过（不发请求）。
func TestTriggerLoginActiveSkipsWithoutDeviceID(t *testing.T) {
	hostbridgetest.UseLoopbackHost(t)
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = w.Write([]byte(`{"code":0}`))
	}))
	defer server.Close()

	TriggerLoginActive(context.Background(), server.URL, &Credential{AccessToken: "tok"})
	if called {
		t.Fatal("没有设备号时不应上报登录活跃")
	}
}
