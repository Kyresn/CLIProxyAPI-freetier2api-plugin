package qoder

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginabi"
	"freetier2api-plugin/internal/hostbridge"
)

// Qoder 签到的行为测试。
//
// 这些用例锁住的是「什么才算签到成功」这个判据：上游的领取窗口每日
// 10:00（UTC+8）开启、需主动领取、错过不补领，因此
//   - 窗口未开启时不得显示成「今日已领取」（否则是假性成功）；
//   - 领取后必须回读确认发放，否则积分可能根本没到账。

// stubRequest 记录一次出站请求。
type stubRequest struct {
	method  string
	url     string
	headers map[string][]string
	body    string
}

// stubUpstream 是上游桩：按 method+url 匹配返回响应。
type stubUpstream struct {
	responses map[string]testhostResponse
	requests  []stubRequest
}

type testhostResponse struct {
	status int
	body   string
}

// installStubHost 装上只处理 host.http.do 的假宿主。
func installStubHost(t *testing.T, upstream *stubUpstream) {
	t.Helper()
	hostbridge.Configure(func(callbackID, method string, payload any) (json.RawMessage, error) {
		if method != pluginabi.MethodHostHTTPDo {
			return nil, fmt.Errorf("stub host: unsupported method %s", method)
		}
		raw, errMarshal := json.Marshal(payload)
		if errMarshal != nil {
			return nil, errMarshal
		}
		var req struct {
			Method  string              `json:"method"`
			URL     string              `json:"url"`
			Headers map[string][]string `json:"headers"`
			Body    []byte              `json:"body"`
		}
		if errDecode := json.Unmarshal(raw, &req); errDecode != nil {
			return nil, errDecode
		}
		upstream.requests = append(upstream.requests, stubRequest{
			method:  req.Method,
			url:     req.URL,
			headers: req.Headers,
			body:    string(req.Body),
		})
		response, okResponse := upstream.responses[req.Method+" "+req.URL]
		if !okResponse {
			response = testhostResponse{status: http.StatusNotFound, body: `{"code":"NOT_STUBBED"}`}
		}
		return json.Marshal(map[string]any{
			"StatusCode": response.status,
			"Headers":    map[string][]string{"Content-Type": {"application/json"}},
			"Body":       []byte(response.body),
		})
	})
	t.Cleanup(func() { hostbridge.Configure(nil) })
}

// campaignsBody 造一份活动列表响应。
func campaignsBody(campaigns ...string) string {
	return fmt.Sprintf(`{"showCampaign":true,"claimable":true,"campaigns":[%s]}`,
		strings.Join(campaigns, ","))
}

// testIdentity 返回一份「用户提供的」设备身份。
//
// 值本身无所谓：这里只驱动请求头组装与判定。上游对真值做精确校验，但那
// 属于服务端行为，单测只验证「有身份就发、没身份就不发」的契约。
func testIdentity() MachineIdentity {
	return ResolveIdentity(RegionGlobal, nil, map[string]string{
		"qoder_machine_token": "token-from-official-component",
		"qoder_machine_type":  "type-from-official-component",
		"qoder_machine_code":  "code-from-official-component",
	}, nil)
}

// TestCheckinGlobalSendsNativeMachineHeaders 覆盖：拿到真身份后，活动查询、
// 领取与发放回查都要带同一组机器头（上游在三条链路上都按身份过滤）。
func TestCheckinGlobalSendsNativeMachineHeaders(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	const campaignID = "camp-global-identity"
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.sh/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimableCampaign(campaignID, "act-global", startAt, endAt)),
	}
	upstream.responses["POST https://openapi.qoder.sh/sash/api/v1/me/campaigns/"+campaignID+"/claim"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"grantId":"g1","status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`,
	}
	upstream.responses["GET https://openapi.qoder.sh/sash/api/v1/me/campaigns/"+campaignID+"/reward"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"grantId":"g1","status":"CLAIMED","grantedAt":"2026-10-22T04:16:34Z","benefit":{"kind":"CREDITS","amount":100}}`,
	}
	upstream.responses["GET https://openapi.qoder.sh/api/v2/quota/usage"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"addOnQuota":{"total":2700}}`,
	}
	upstream.responses["GET https://openapi.qoder.sh/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusNotFound,
		body:   `{"errorCode":"NotFound"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionGlobal}, testIdentity())
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status != CheckinClaimed {
		t.Fatalf("状态 = %q, 期望 %q（message=%s）", result.Status, CheckinClaimed, result.Message)
	}

	identity := testIdentity()
	want := map[string]string{
		"Cosy-Machinetoken": identity.Token,
		"Cosy-Machinetype":  identity.Type,
		"Cosy-Machinecode":  identity.Code,
		"Cosy-Version":      identity.Version,
	}
	// 派生机器头（machineid / machineos / hostname）一律不发：上游只认官方
	// 组件签发的精确值，别的机器头既解锁不了活动，还会让请求形态偏离官方客户端。
	forbidden := []string{"Cosy-Machineid", "Cosy-Machineos", "Cosy-Machinehostname"}
	checked := 0
	for _, request := range upstream.requests {
		if !strings.Contains(request.url, "/sash/") {
			continue
		}
		checked++
		for key, value := range want {
			if got := request.headers[key]; len(got) == 0 || got[0] != value {
				t.Errorf("%s %s 的头 %s = %v, 期望 [%s]", request.method, request.url, key, got, value)
			}
		}
		for _, key := range forbidden {
			if got := request.headers[key]; len(got) != 0 {
				t.Errorf("%s %s 不应发送 %s = %v", request.method, request.url, key, got)
			}
		}
	}
	// 活动查询、领取、reward 回查三条链路都必须带身份。
	if checked < 3 {
		t.Errorf("只检查到 %d 个 /sash/ 请求，期望至少 3 个", checked)
	}
}

// claimableCampaign 造一条可领取活动。
func claimableCampaign(id, key string, startAt, endAt int64) string {
	return fmt.Sprintf(`{"campaignId":%q,"campaignKey":%q,"actionType":"CLAIM_BENEFIT",`+
		`"claimStatus":"CLAIMABLE","startAt":%d,"endAt":%d,`+
		`"benefit":{"kind":"CREDITS","amount":100}}`, id, key, startAt, endAt)
}

// claimedCampaign 造一条已领取活动。
func claimedCampaign(id, key string, startAt, endAt int64) string {
	return fmt.Sprintf(`{"campaignId":%q,"campaignKey":%q,"actionType":"CLAIM_BENEFIT",`+
		`"claimStatus":"CLAIMED","startAt":%d,"endAt":%d,`+
		`"benefit":{"kind":"CREDITS","amount":100}}`, id, key, startAt, endAt)
}

// pinCheckinNow 把签到判定时钟固定到 at，并在用例结束时恢复。
func pinCheckinNow(t *testing.T, at time.Time) time.Time {
	t.Helper()
	restore := checkinNow
	checkinNow = func() time.Time { return at }
	t.Cleanup(func() { checkinNow = restore })
	return at
}

// checkinOpenNow 返回「当日窗口已开启」的固定时刻（UTC+8 15:00）。
//
// 这批用例按返回的 now 构造上游活动窗口，必须与签到判定共用同一个时钟。
// 直接取真实时间时，本地 10:00 之前构造出来的是上一轮窗口，领取逻辑按
// 设计返回 window_not_open，用例于是每天有 8 小时必然失败（CI 跑在 UTC，
// 北京时间的凌晨正好落在窗口开启前）。签到正确性只取决于相对时刻，把时钟
// 与窗口一起钉死即可稳定覆盖。
func checkinOpenNow(t *testing.T) time.Time {
	t.Helper()
	return pinCheckinNow(t, time.Date(2026, 10, 22, 15, 0, 0, 0, campaignZone))
}

// checkinBeforeOpenNow 返回「当日窗口尚未开启」的固定时刻（UTC+8 09:00）。
func checkinBeforeOpenNow(t *testing.T) time.Time {
	t.Helper()
	return pinCheckinNow(t, time.Date(2026, 10, 22, 9, 0, 0, 0, campaignZone))
}

// roundWindow 返回当前轮与上一轮的时间窗口（UTC+8 秒级时间戳）。
//
// 活动 startAt 落在当天 10:00、endAt 落在次日 10:00 之前，与上游实测一致。
func roundWindow(t *testing.T, now time.Time) (startAt, endAt int64, prevStart, prevEnd int64) {
	t.Helper()
	local := now.In(campaignZone)
	roundStart := time.Date(local.Year(), local.Month(), local.Day(), dailyRoundHour, 0, 0, 0, campaignZone)
	if local.Hour() < dailyRoundHour {
		roundStart = roundStart.AddDate(0, 0, -1)
	}
	roundEnd := roundStart.AddDate(0, 0, 1)
	return roundStart.Unix(), roundEnd.Unix(), roundStart.AddDate(0, 0, -1).Unix(), roundStart.Unix()
}

// TestCheckinClaimsCurrentRound 覆盖：当前轮可领取时应真正领取并确认到账。
func TestCheckinClaimsCurrentRound(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	const campaignID = "camp-current"
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimableCampaign(campaignID, "act-today", startAt, endAt)),
	}
	upstream.responses["POST https://openapi.qoder.com.cn/sash/api/v1/me/campaigns/"+campaignID+"/claim"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"grantId":"g1","status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns/"+campaignID+"/reward"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"grantId":"g1","status":"CLAIMED","grantedAt":"2026-10-21T02:46:36Z","benefit":{"kind":"CREDITS","amount":100}}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/api/v2/quota/usage"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"addOnQuota":{"total":2300,"used":0,"remaining":2300}}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED","currentStreakDays":0,"totalClaimDays":0,"totalRewardCredits":0}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status != CheckinClaimed {
		t.Fatalf("状态 = %q, 期望 %q（message=%s）", result.Status, CheckinClaimed, result.Message)
	}
	if result.Amount != 100 {
		t.Errorf("积分 = %d, 期望 100", result.Amount)
	}
	if result.NotRecorded {
		t.Error("领取成功后不应标记 NotRecorded")
	}

	// 领取请求必须是 POST 且带桌面端头、空 body。
	var claimRequest *stubRequest
	for index := range upstream.requests {
		if upstream.requests[index].method == http.MethodPost {
			claimRequest = &upstream.requests[index]
		}
	}
	if claimRequest == nil {
		t.Fatal("没有发出领取请求")
	}
	if got := claimRequest.headers["Cosy-Clienttype"]; len(got) == 0 || got[0] != "10" {
		t.Errorf("cosy-clienttype = %v, 期望 [10]", got)
	}
	if claimRequest.body != "" {
		t.Errorf("领取请求 body = %q, 期望空", claimRequest.body)
	}
}

// TestCheckinBeforeWindowIsNotRecorded 覆盖核心回归：窗口未开启时不得显示成功。
//
// 上游每日 10:00（UTC+8）开启新一轮。9 点那一轮只能看到**上一轮**已领取
// 的活动——把它当成「今日已领取」就是 issue 报告的假性成功。
func TestCheckinBeforeWindowIsNotRecorded(t *testing.T) {
	now := checkinBeforeOpenNow(t)
	_, _, prevStart, prevEnd := roundWindow(t, now)
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimedCampaign("camp-prev", "act-yesterday", prevStart, prevEnd)),
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status == CheckinAlreadyClaimed {
		t.Errorf("上一轮已领取不应被判定为「今日已领取」（message=%s）", result.Message)
	}
	if !result.NotRecorded {
		t.Error("窗口未开启时应标记 NotRecorded，避免落库成假性成功")
	}
	for _, request := range upstream.requests {
		if request.method == http.MethodPost {
			t.Error("没有可领取活动时不应发出领取请求")
		}
	}
}

// TestCheckinCurrentRoundAlreadyClaimed 覆盖：当前轮已领取才算「今日已领取」。
func TestCheckinCurrentRoundAlreadyClaimed(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimedCampaign("camp-done", "act-today", startAt, endAt)),
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status != CheckinAlreadyClaimed {
		t.Fatalf("状态 = %q, 期望 %q", result.Status, CheckinAlreadyClaimed)
	}
	if result.NotRecorded {
		t.Error("当前轮已领取应落库（表示今日确实完成了）")
	}
}

// TestCheckinBlockedByUpstream 覆盖：上游风控去重不得当成成功。
func TestCheckinBlockedByUpstream(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	const campaignID = "camp-blocked"
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimableCampaign(campaignID, "act-today", startAt, endAt)),
	}
	upstream.responses["POST https://openapi.qoder.com.cn/sash/api/v1/me/campaigns/"+campaignID+"/claim"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"BLOCKED","failureCode":"SAME_PERSON_ALREADY_CLAIMED"}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/api/v2/quota/usage"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"addOnQuota":{"total":2200}}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status != CheckinBlocked {
		t.Errorf("状态 = %q, 期望 %q", result.Status, CheckinBlocked)
	}
	if !result.NotRecorded {
		t.Error("风控拒绝发放时不应落库")
	}
	if !strings.Contains(result.Message, "SAME_PERSON_ALREADY_CLAIMED") {
		t.Errorf("消息应含上游给出的失败码，实际 = %s", result.Message)
	}
}

// TestCheckinNotConfirmedWithoutGrant 覆盖：领取 200 但回读不到发放记录。
func TestCheckinNotConfirmedWithoutGrant(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	const campaignID = "camp-unconfirmed"
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimableCampaign(campaignID, "act-today", startAt, endAt)),
	}
	upstream.responses["POST https://openapi.qoder.com.cn/sash/api/v1/me/campaigns/"+campaignID+"/claim"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`,
	}
	// reward 回读 404：上游没有给出发放记录。
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns/"+campaignID+"/reward"] = testhostResponse{
		status: http.StatusNotFound,
		body:   `{"errorCode":"NotFound"}`,
	}
	// 额度总额领取前后不变：没有真的到账。
	upstream.responses["GET https://openapi.qoder.com.cn/api/v2/quota/usage"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"addOnQuota":{"total":2200}}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status != CheckinNotConfirmed {
		t.Errorf("状态 = %q, 期望 %q", result.Status, CheckinNotConfirmed)
	}
	if !result.NotRecorded {
		t.Error("未确认发放时不应落库")
	}
}

// TestCheckinAchievementGate 覆盖：需先完成成就的活动不能当成已领。
func TestCheckinAchievementGate(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	gated := fmt.Sprintf(`{"campaignId":"camp-gated","campaignKey":"act-milktea","actionType":"CLAIM_BENEFIT",`+
		`"claimStatus":"ACHIEVEMENT_NOT_COMPLETED","startAt":%d,"endAt":%d,`+
		`"requiredAchievementKey":"sites_first_use","achievementCompleted":false,`+
		`"benefit":{"kind":"CREDITS","amount":100}}`, startAt, endAt)
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(gated),
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status == CheckinAlreadyClaimed {
		t.Error("成就未完成的活动不应被当成已领取")
	}
	if !result.NotRecorded {
		t.Error("成就未完成时不应落库")
	}
	if !strings.Contains(result.Message, "sites_first_use") {
		t.Errorf("消息应含所需成就键，实际 = %s", result.Message)
	}
}

// TestCheckinSkipsGatedCampaignAfterDailyClaimed 覆盖真实报障：
// 今日每日活动已领，列表里同时还有一条「可领但成就未完成」的活动。
// 后者不能被选中去领取，否则「今日已领取」会被误报成成就未完成。
func TestCheckinSkipsGatedCampaignAfterDailyClaimed(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	daily := claimedCampaign("camp-daily", "act-20261027-334", startAt, endAt)
	gated := fmt.Sprintf(`{"campaignId":"camp-gated","campaignKey":"act-milktea","actionType":"CLAIM_BENEFIT",`+
		`"claimStatus":"CLAIMABLE","startAt":%d,"endAt":%d,`+
		`"requiredAchievementKey":"sites_first_use","achievementCompleted":false,`+
		`"benefit":{"kind":"CREDITS","amount":100}}`, startAt, endAt)
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(daily, gated),
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status != CheckinAlreadyClaimed {
		t.Fatalf("状态 = %q, 期望 %q（消息：%s）", result.Status, CheckinAlreadyClaimed, result.Message)
	}
	for _, request := range upstream.requests {
		if request.method == http.MethodPost && strings.Contains(request.url, "/claim") {
			t.Fatalf("今日已领取时不应再发 claim，实际请求 %s", request.url)
		}
	}
}

// TestCheckinClaimRejectedByAchievement 覆盖：领取响应直接返回成就未完成。
func TestCheckinClaimRejectedByAchievement(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	const campaignID = "camp-achievement-claim"
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimableCampaign(campaignID, "act-milktea", startAt, endAt)),
	}
	upstream.responses["POST https://openapi.qoder.com.cn/sash/api/v1/me/campaigns/"+campaignID+"/claim"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"ACHIEVEMENT_NOT_COMPLETED","failureCode":"ACHIEVEMENT_NOT_COMPLETED"}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if !result.NotRecorded {
		t.Error("领取被成就拒绝时不应落库")
	}
	if !strings.Contains(result.Message, "成就") {
		t.Errorf("消息应说明成就门槛，实际 = %s", result.Message)
	}
}

// TestCheckinGlobalRegionUsesGlobalEndpoint 覆盖：国际版走国际版域名。
//
// 国际版与国内版共用同一套 campaigns 活动（官方文档与官方活动页均确认），
// 因此不能对国际版做静态跳过。
func TestCheckinGlobalRegionUsesGlobalEndpoint(t *testing.T) {
	now := checkinOpenNow(t)
	startAt, endAt, _, _ := roundWindow(t, now)
	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.sh/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimableCampaign("camp-global", "act-global", startAt, endAt)),
	}
	upstream.responses["GET https://openapi.qoder.sh/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionGlobal}, testIdentity())
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status == CheckinNoCampaign && strings.Contains(result.Message, "国际版没有签到计划") {
		t.Errorf("国际版不应被静态判定为无签到计划，实际消息 = %s", result.Message)
	}
	found := false
	for _, request := range upstream.requests {
		if strings.HasPrefix(request.url, "https://openapi.qoder.sh/") {
			found = true
		}
		if strings.HasPrefix(request.url, "https://openapi.qoder.com.cn/") {
			t.Errorf("国际版请求不应打到国内域名: %s", request.url)
		}
	}
	if !found {
		t.Error("没有向国际版域名发出请求")
	}
}

// TestCheckinAtNineAMDoesNotClaimToday 覆盖 issue 的核心场景。
//
// 插件的签到排程是 9 点 / 21 点（internal/tasks/catalog.go）。9 点那一轮
// 跑在当日窗口开启（10:00 UTC+8）之前 1 小时，上游此刻下发的仍是**上一轮**
// 已领取的活动。此时若判定为「今日已领取」，控制台会显示假性成功；而当日
// 窗口随后开启时已被错过——官方不补领，积分就此永久丢失。
func TestCheckinAtNineAMDoesNotClaimToday(t *testing.T) {
	// 固定在 2026-10-21 09:00（UTC+8）。
	fixedNow := time.Date(2026, 10, 21, 9, 0, 0, 0, campaignZone)
	restore := checkinNow
	checkinNow = func() time.Time { return fixedNow }
	t.Cleanup(func() { checkinNow = restore })

	// 上游此时下发的是上一轮（10-20 10:00 → 10-21 10:00）已领取的活动。
	prevStart := time.Date(2026, 10, 20, dailyRoundHour, 0, 0, 0, campaignZone)
	prevEnd := time.Date(2026, 10, 21, dailyRoundHour, 0, 0, 0, campaignZone)

	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimedCampaign("camp-prev", "act-20261019-791", prevStart.Unix(), prevEnd.Unix())),
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status == CheckinAlreadyClaimed {
		t.Errorf("09:00 时上一轮已领不应被判为「今日已领取」（message=%s）", result.Message)
	}
	if result.Status != CheckinWindowNotOpen {
		t.Errorf("状态 = %q, 期望 %q", result.Status, CheckinWindowNotOpen)
	}
	if !result.NotRecorded {
		t.Error("09:00 时不得落库成「今日已签到」")
	}
	if result.WindowDate != "2026-10-20" {
		t.Errorf("窗口日期 = %s, 期望 2026-10-20（上一轮）", result.WindowDate)
	}
	for _, request := range upstream.requests {
		if request.method == http.MethodPost {
			t.Error("09:00 时不应发出领取请求")
		}
	}
}

// TestCheckinAfterOpenClaimsToday 覆盖：10:00 之后触发应真正领取当日窗口。
func TestCheckinAfterOpenClaimsToday(t *testing.T) {
	fixedNow := time.Date(2026, 10, 21, 10, 30, 0, 0, campaignZone)
	restore := checkinNow
	checkinNow = func() time.Time { return fixedNow }
	t.Cleanup(func() { checkinNow = restore })

	roundStart := time.Date(2026, 10, 21, dailyRoundHour, 0, 0, 0, campaignZone)
	roundEnd := time.Date(2026, 10, 22, dailyRoundHour, 0, 0, 0, campaignZone)
	const campaignID = "camp-today"

	upstream := &stubUpstream{responses: map[string]testhostResponse{}}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns"] = testhostResponse{
		status: http.StatusOK,
		body:   campaignsBody(claimableCampaign(campaignID, "act-20261020-791", roundStart.Unix(), roundEnd.Unix())),
	}
	upstream.responses["POST https://openapi.qoder.com.cn/sash/api/v1/me/campaigns/"+campaignID+"/claim"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"grantId":"g1","status":"CLAIMED","replayed":false,"benefit":{"kind":"CREDITS","amount":100}}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/campaigns/"+campaignID+"/reward"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"grantId":"g1","status":"CLAIMED","grantedAt":"2026-10-21T02:46:36Z","benefit":{"kind":"CREDITS","amount":100}}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/api/v2/quota/usage"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"addOnQuota":{"total":2300}}`,
	}
	upstream.responses["GET https://openapi.qoder.com.cn/sash/api/v1/me/daily-check-in/status"] = testhostResponse{
		status: http.StatusOK,
		body:   `{"status":"DISABLED"}`,
	}
	installStubHost(t, upstream)

	result, errCheckin := Checkin(context.Background(), &Credential{Token: "dt-test", Region: RegionCN}, MachineIdentity{})
	if errCheckin != nil {
		t.Fatalf("Checkin 返回错误: %v", errCheckin)
	}
	if result.Status != CheckinClaimed {
		t.Fatalf("状态 = %q, 期望 %q（message=%s）", result.Status, CheckinClaimed, result.Message)
	}
	if result.Amount != 100 {
		t.Errorf("积分 = %d, 期望 100", result.Amount)
	}
	if result.WindowDate != "2026-10-21" {
		t.Errorf("窗口日期 = %s, 期望 2026-10-21", result.WindowDate)
	}
	if result.NotRecorded {
		t.Error("领取成功后不应标记 NotRecorded")
	}
}

// TestIsTodayRoundWindowBoundaries 覆盖轮次边界：10:00 前上游下发的仍是上一轮。
func TestIsTodayRoundWindowBoundaries(t *testing.T) {
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, campaignZone)
	beforeOpen := day.Add(9*time.Hour + 59*time.Minute)
	afterOpen := day.Add(10 * time.Hour)

	// 上一轮：10-20 10:00 开启、10-21 10:00 关闭。
	prevStart := time.Date(2026, 10, 20, dailyRoundHour, 0, 0, 0, campaignZone)
	prevEnd := time.Date(2026, 10, 21, dailyRoundHour, 0, 0, 0, campaignZone)
	prev := &campaignInfo{StartAt: prevStart.Unix(), EndAt: prevEnd.Unix()}
	// 今天这一轮：10-21 10:00 开启、10-22 10:00 关闭。
	todayStart := time.Date(2026, 10, 21, dailyRoundHour, 0, 0, 0, campaignZone)
	todayEnd := time.Date(2026, 10, 22, dailyRoundHour, 0, 0, 0, campaignZone)
	today := &campaignInfo{StartAt: todayStart.Unix(), EndAt: todayEnd.Unix()}

	if isTodayRoundWindow(prev, beforeOpen) {
		t.Error("09:59 时上一轮不应被判定为今天这一轮")
	}
	if !isTodayRoundWindow(today, afterOpen) {
		t.Error("10:00 之后今天这一轮应被识别")
	}
}

// TestCampaignWindowDateUsesUTC8 覆盖：窗口日期按 UTC+8 计算，不受宿主时区影响。
func TestCampaignWindowDateUsesUTC8(t *testing.T) {
	// 2026-10-21 02:00 UTC == 2026-10-21 10:00 UTC+8。
	start := time.Date(2026, 10, 21, 2, 0, 0, 0, time.UTC)
	campaign := &campaignInfo{StartAt: start.Unix()}
	if got := campaignWindowDate(campaign); got != "2026-10-21" {
		t.Errorf("窗口日期 = %s, 期望 2026-10-21", got)
	}
}
