package tests

// 本文件锁定「签到历史可回溯」的端到端契约。
//
// 真实痛点：一键签到失败时只有几秒钟的横幅，事后无法回答「哪个账号、什么
// 报错、哪次调度触发的」。历史必须落盘并在 /status 里回给控制台，否则
// 账号概览的展开行没有数据源。

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestCheckinHistoryIsVisibleInStatus 验证一键签到写入的历史能从 /status 读回。
func TestCheckinHistoryIsVisibleInStatus(t *testing.T) {
	host := installFakeHost(t)
	setupTestPlugin(t)
	host.addAccount("1", "cn-account", personalCredentialJSON("cn", "cn1"))
	host.setUpstream(func(method, url, body string) fakeUpstreamResponse {
		if strings.Contains(url, "daily-checkin") {
			return fakeUpstreamResponse{
				Status: http.StatusOK,
				Body:   `{"code":0,"data":{"credit":10,"energy":5,"streak":3}}`,
			}
		}
		return fakeUpstreamResponse{Status: http.StatusNotFound, Body: `{"code":404}`}
	})

	resp := managementCallWithBody(t, "POST", "/checkin", map[string]any{})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/checkin = %d: %s", resp.StatusCode, resp.Body)
	}

	status := managementCall(t, "GET", "/status")
	var payload struct {
		Accounts []struct {
			AuthID  string `json:"auth_id"`
			History []struct {
				RunAt   string `json:"run_at"`
				Trigger string `json:"trigger"`
				Status  string `json:"status"`
				Message string `json:"message"`
				Credit  int64  `json:"credit"`
			} `json:"checkin_history"`
		} `json:"accounts"`
	}
	if errUnmarshal := json.Unmarshal(status.Body, &payload); errUnmarshal != nil {
		t.Fatalf("decode status: %v (%s)", errUnmarshal, status.Body)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("want 1 account, got %d: %s", len(payload.Accounts), status.Body)
	}
	history := payload.Accounts[0].History
	if len(history) == 0 {
		t.Fatalf("一键签到后 /status 必须带回签到历史: %s", status.Body)
	}
	entry := history[0]
	if entry.Status != "success" || entry.Credit != 10 {
		t.Fatalf("历史条目 = %+v, 期望成功且 credit=10", entry)
	}
	// 触发方式必须能区分定时与手动：排查「哪次调度触发的」靠这个字段。
	if entry.Trigger != "manual" {
		t.Fatalf("trigger = %q, 期望 manual（一键签到是手动触发）", entry.Trigger)
	}
	if entry.RunAt == "" {
		t.Fatal("历史条目必须带执行时间")
	}
}
