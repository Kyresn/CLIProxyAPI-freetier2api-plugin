package autoclaw

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestCheckinDetectsNotClaimed 锁住「code=0 但 data.success=false」不算成功。
//
// 上游会用这个形态表达「窗口未开 / 无资格」；旧实现只看 code，会把没领到
// 报成签到成功并写入 LastDate，当天不再重试。
func TestCheckinDetectsNotClaimed(t *testing.T) {
	useLoopbackHost(t)
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
	useLoopbackHost(t)
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
	useLoopbackHost(t)
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
