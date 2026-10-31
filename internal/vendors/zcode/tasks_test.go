package zcode

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
)

// 套餐探测与领取的纯函数测试（不触网）。
//
// 覆盖三类容易静默出错的地方：
//   - 归一化：plan_id 大小写/空白、status 非 active 的过期套餐不该算「已持有」；
//   - 集合运算：已投放 ∩ 已持有 = 按钮置灰的依据；
//   - 业务码翻译：页面上的文案直接来自这里。

func TestBillingHeadersFallBackToAPIKey(t *testing.T) {
	cred := &Credential{APIKey: "sk-test", Profile: DeviceProfile{Platform: "win32", Arch: "x64"}}
	headers := billingHeaders(cred)
	if headers["x-api-key"] != "sk-test" || headers["Authorization"] != "" {
		t.Fatalf("API key headers = %#v", headers)
	}
}

func TestNormalizePlanIDIsCaseAndSpaceInsensitive(t *testing.T) {
	// 上游的 plan_id 大小写不统一（「ZCode-V3-Start-Plan」与「zcode-v3-start-plan」
	// 指同一个套餐）。不归一化会让已领套餐被再次列成可领。
	if got := normalizePlanID("  ZCode-V3-Start-Plan \n"); got != "zcode-v3-start-plan" {
		t.Fatalf("normalizePlanID = %q, want zcode-v3-start-plan", got)
	}
	if got := normalizePlanID("   "); got != "" {
		t.Fatalf("normalizePlanID(空白) = %q, want 空串", got)
	}
}

func TestParsePlanRequiresPlanID(t *testing.T) {
	// 缺 plan_id 的条目无法用于领取，必须直接丢掉而不是生成空 ID 的套餐。
	if got := parsePlan(map[string]any{"name": "无 ID 套餐"}); got != nil {
		t.Fatalf("parsePlan(无 plan_id) = %+v, want nil", got)
	}
	// planId 是 camelCase 别名，上游两个字段名都在用。
	if got := parsePlan(map[string]any{"planId": "p1", "name": "套餐一"}); got == nil || got.PlanID != "p1" {
		t.Fatalf("parsePlan(planId) = %+v, want plan_id=p1", got)
	}
}

func TestParsePlanGrantsFromEntitlements(t *testing.T) {
	plan := parsePlan(map[string]any{
		"plan_id":   "p1",
		"name":      "GLM 活动",
		"priority":  float64(7),
		"starts_at": float64(1700000000),
		"ends_at":   float64(1800000000),
		"entitlements": []any{
			map[string]any{
				"meter": "model_usage", "unit_type": "token",
				"show_name": "GLM-5.3", "grant_units": float64(250000), "period": "daily",
			},
			map[string]any{
				// 非 token 计量对页面无意义，应被过滤掉。
				"meter": "storage", "unit_type": "byte",
				"show_name": "网盘", "grant_units": float64(1),
			},
			map[string]any{
				"meter": "model_usage", "unit_type": "token",
				"show_name": "GLM-5.3-Flash", "grant_units": float64(0),
			},
		},
	})
	if plan == nil {
		t.Fatal("parsePlan 返回 nil")
	}
	if plan.Priority != 7 || plan.StartsAt != 1700000000000 || plan.EndsAt != 1800000000000 {
		t.Fatalf("套餐元数据解析错误: %+v", plan)
	}
	if len(plan.Grants) != 2 {
		t.Fatalf("授予项数 = %d, want 2（storage 项被过滤，零额度项保留展示）: %+v", len(plan.Grants), plan.Grants)
	}
	if plan.Grants[0].Model != "GLM-5.3" || plan.Grants[0].Units != 250000 || plan.Grants[0].Period != "daily" {
		t.Fatalf("首个授予项 = %+v", plan.Grants[0])
	}
}

func TestParsePlanGrantsFallsBackToGrantsField(t *testing.T) {
	// 上游也可能直接给已解析好的 grants[]。
	plan := parsePlan(map[string]any{
		"plan_id": "p1",
		"grants":  []any{map[string]any{"name": "GLM-5.3", "units": float64(1000), "period": "one_time"}},
	})
	if plan == nil || len(plan.Grants) != 1 || plan.Grants[0].Model != "GLM-5.3" {
		t.Fatalf("grants 回退解析失败: %+v", plan)
	}
}

func TestHeldPlanIDsExcludesInactivePlans(t *testing.T) {
	plans := planRows(map[string]any{"data": map[string]any{"plans": []any{
		map[string]any{"plan_id": "active-plan", "status": "active"},
		map[string]any{"plan_id": "expired-plan", "status": "expired"},
		map[string]any{"plan_id": "implicit-active"},
		map[string]any{"status": "active"}, // 无 ID：跳过
	}}})
	held := heldPlanIDs(plans, 0)
	if !held["active-plan"] || !held["implicit-active"] {
		t.Fatalf("生效套餐应计入持有: %+v", held)
	}
	// 过期套餐必须排除：否则活动下线后按钮会永远灰着。
	if held["expired-plan"] {
		t.Fatalf("过期套餐不应计入持有: %+v", held)
	}
}

func TestHeldPlanIDsTreatsPastEndsAtAsExpired(t *testing.T) {
	// 上游可能把已结束的活动仍标成 active（官方客户端按服务端时间归一成 expired）。
	// 不归一的话活动下线后按钮会一直灰在「已领取」。
	plans := planRows(map[string]any{"data": map[string]any{"plans": []any{
		map[string]any{"plan_id": "ended", "status": "active", "ends_at": float64(1000)},
		map[string]any{"plan_id": "running", "status": "active", "ends_at": float64(3000)},
		map[string]any{"plan_id": "no-end", "status": "active"},
	}}})
	held := heldPlanIDs(plans, 2000)
	if held["ended"] {
		t.Fatalf("ends_at 已过的 active 套餐不应计入持有: %+v", held)
	}
	if !held["running"] || !held["no-end"] {
		t.Fatalf("未到期/无到期时间的套餐应计入持有: %+v", held)
	}
	// 服务端时间缺失（0）时不做过期推断，保持保守。
	if !heldPlanIDs(plans, 0)["ended"] {
		t.Fatal("缺少服务端时间时不应按本机时钟推断过期")
	}
}

func TestPlanSnapshotClaimableSubtractsHeld(t *testing.T) {
	snapshot := planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}, {PlanID: "b"}, {PlanID: "c"}},
		held:    map[string]bool{"b": true},
	}
	claimable := snapshot.claimable()
	if len(claimable) != 2 {
		t.Fatalf("可领数 = %d, want 2: %+v", len(claimable), claimable)
	}
	for _, plan := range claimable {
		if plan.PlanID == "b" {
			t.Fatal("已持有的套餐不应出现在可领列表里")
		}
	}
}

func TestMergeOfferedPlansCombinesBothChannels(t *testing.T) {
	// 国内版实测：billing/preview 为空，活动只在 marketing/touch 里。合并后
	// 必须仍能看到可领套餐，否则按钮永远显示「已领取」。
	merged := mergeOfferedPlans(
		[]core.PlanInfo{{PlanID: "a", Priority: 10}},
		[]core.PlanInfo{{PlanID: "wk", Priority: 101}, {PlanID: "a", Priority: 5}},
	)
	if len(merged) != 2 {
		t.Fatalf("合并结果 = %d 个，want 2: %+v", len(merged), merged)
	}
	// 计费通道优先：同 plan_id 保留它的条目（带完整授予明细）。
	if merged[0].PlanID != "wk" || merged[0].Priority != 101 {
		t.Fatalf("应按优先级降序且营销投放排最前: %+v", merged)
	}
	if merged[1].PlanID != "a" || merged[1].Priority != 10 {
		t.Fatalf("同 plan_id 应以计费通道条目为准: %+v", merged)
	}
	if got := mergeOfferedPlans(nil, nil); len(got) != 0 {
		t.Fatalf("空合并 = %+v", got)
	}
}

// TestPlanGrantQuotaFallsBackToEntitlements 锁住「活动额度未生效时也有数字」：
// 上游在 effective_at 之前不生成 balances 桶，只能拿授予明细兜底。
func TestPlanGrantQuotaFallsBackToEntitlements(t *testing.T) {
	entitlement := map[string]any{
		"entitlement_id": "ent-1",
		"show_name":      "GLM-5.3-Flash",
		"meter":          "model_usage",
		"unit_type":      "token",
		"grant_units":    float64(500000000),
		"effective_at":   float64(200),
	}
	plans := []map[string]any{{
		"status": "active",
		// 同一份权益出现两次：必须按 entitlement_id 去重，不能重复累加。
		"entitlements": []any{entitlement, entitlement},
	}}

	metrics, groups, effectiveAt := planGrantQuota(plans, 100)
	if effectiveAt != 200 {
		t.Fatalf("生效时刻 = %d, want 200", effectiveAt)
	}
	if len(metrics) != 2 || metrics[1].Value != 500000000 {
		t.Fatalf("总额指标错误: %+v", metrics)
	}
	if len(groups) != 1 || len(groups[0].Buckets) != 1 {
		t.Fatalf("窗口组错误: %+v", groups)
	}
	if !strings.Contains(groups[0].Buckets[0].Description, "生效") {
		t.Fatalf("未生效的授予应在描述里注明生效时刻: %q", groups[0].Buckets[0].Description)
	}

	// 非模型用量类授予（功能开关）不参与额度。
	other := []map[string]any{{
		"status": "active",
		"entitlements": []any{map[string]any{
			"entitlement_id": "ent-2", "meter": "feature", "unit_type": "token", "grant_units": float64(1),
		}},
	}}
	if m, _, _ := planGrantQuota(other, 100); len(m) != 0 {
		t.Fatalf("功能类授予不应计入额度: %+v", m)
	}

	// 已领取但额度未生效的套餐可能被上游标成 pending：仍要按授予兜底，
	// 否则页面会把它说成「尚未领取体验套餐」。
	pending := []map[string]any{{
		"status": "pending",
		"entitlements": []any{map[string]any{
			"entitlement_id": "ent-3", "show_name": "GLM-5.3", "meter": "model_usage",
			"unit_type": "token", "grant_units": float64(10), "effective_at": float64(200),
		}},
	}}
	if m, _, at := planGrantQuota(pending, 100); len(m) == 0 || at != 200 {
		t.Fatalf("pending 套餐应参与兜底: metrics=%+v effectiveAt=%d", m, at)
	}
}

func TestClaimStatusCountsAndErrors(t *testing.T) {
	clean := claimStatusOf(planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}, {PlanID: "b"}},
		held:    map[string]bool{"b": true},
	})
	if clean.Available != 2 || clean.Claimable != 1 || clean.Error != "" {
		t.Fatalf("领取状态错误: %+v", clean)
	}
	// 全领完 → Claimable=0，页面据此把按钮置灰成「已领取」。
	all := claimStatusOf(planSnapshot{
		offered: []core.PlanInfo{{PlanID: "a"}},
		held:    map[string]bool{"a": true},
	})
	if all.Claimable != 0 {
		t.Fatalf("全部领取后 claimable = %d, want 0", all.Claimable)
	}
	// preview 失败时必须带 error：页面据此保持按钮可点，而不是误显示「已领取」。
	failed := claimStatusOf(planSnapshot{
		held:       map[string]bool{},
		errPreview: core.NewPluginError("x", "preview 失败", 502),
	})
	if failed.Error == "" || failed.Claimable != 0 {
		t.Fatalf("preview 失败应带 error: %+v", failed)
	}

	// 活跃上报失败且活动列表为空：不能说成「已领取」——空列表本来就不代表领完。
	activationFailed := claimStatusOf(planSnapshot{
		held:          map[string]bool{},
		errActivation: "激活上报 HTTP 500",
	})
	if activationFailed.Error == "" {
		t.Fatalf("活跃上报失败时空列表必须带 error: %+v", activationFailed)
	}
	// 反过来：有投放时活跃上报的失败不影响按钮可用性，就不该打扰用户。
	withPlans := claimStatusOf(planSnapshot{
		offered:       []core.PlanInfo{{PlanID: "a"}},
		held:          map[string]bool{},
		errActivation: "激活上报 HTTP 500",
	})
	if withPlans.Error != "" || withPlans.Claimable != 1 {
		t.Fatalf("有可领套餐时不应带 error: %+v", withPlans)
	}
}

func TestParseCaptchaConfig(t *testing.T) {
	body := "{\"data\":{\"configs\":{\"captcha\":{\"enabled\":true,\"sceneId\":\"sid\",\"region\":\"cn\",\"prefix\":\"pfx\"}}}}"
	config := parseCaptchaConfig([]byte(body))
	if config.SceneID != "sid" || config.Region != "cn" || config.Prefix != "pfx" || !config.Enabled {
		t.Fatalf("验证码配置解析错误: %+v", config)
	}
	if got := parseCaptchaConfig([]byte("{\"data\":{}}")); got != (core.CaptchaConfig{}) {
		t.Fatalf("缺失字段应返回零值, got %+v", got)
	}
	// 兜底常量必须齐备（configs 不可用时靠它渲染滑块）。
	if captchaFallbackScene == "" || captchaFallbackRegion == "" || captchaFallbackPrefix == "" {
		t.Fatal("验证码兜底常量不应为空")
	}
}

func TestClaimFailureMessages(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{codePlanClaimed, "已经领取过"},
		{codePlanNotFound, "套餐不存在"},
		{codeDailyQuotaUsed, "名额已用完"},
		{codeCaptchaFailed, "验证码校验失败"},
	}
	for _, item := range cases {
		message := claimFailure(item.code, map[string]any{}).Error()
		if !strings.Contains(message, item.want) {
			t.Fatalf("code=%d 文案 = %q, want 包含 %q", item.code, message, item.want)
		}
	}
}

func TestClaimFailureQuotaRecoveryCarriesTime(t *testing.T) {
	// 1005 会带上名额恢复时间，页面要靠它提示何时再试。
	err := claimFailure(codeDailyQuotaUsed, map[string]any{
		"data": map[string]any{"plan": map[string]any{"ends_at": float64(1800000000)}},
	})
	if !strings.Contains(err.Error(), "名额恢复时间") {
		t.Fatalf("1005 应带名额恢复时间, got %q", err.Error())
	}
}

func TestClaimEndpointsUseBillingBase(t *testing.T) {
	// 领取与预览都挂在 billing 族下：路径写错会 404，而 404 在页面上只表现为
	// 「查询失败」，很难定位。
	if got := BillingBase + PathBillingPreview; got != "https://zcode.z.ai/api/v1/zcode-plan/billing/preview" {
		t.Fatalf("preview 端点 = %q", got)
	}
	if got := BillingBase + PathBillingClaim; got != "https://zcode.z.ai/api/v1/zcode-plan/billing/claim" {
		t.Fatalf("claim 端点 = %q", got)
	}
}

func TestClaimThrottleBlocksAndHonorsCancellation(t *testing.T) {
	// 节流的语义是「阻塞到允许为止」，不是「拒绝」：调用方在同一次领取里等
	// 一下即可，不该把等待当成失败。
	gate := newClaimGate()
	gate.markIssued()
	gate.setInterval(0)
	if !gate.wait(context.Background()) {
		t.Fatal("间隔为 0 时不应节流")
	}

	// 间隔远大于 context 剩余时间时应放弃，而不是把请求拖死。
	gate.setInterval(time.Hour)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if gate.wait(ctx) {
		t.Fatal("context 取消后应放弃本次领取")
	}

	// 短间隔应等到点后放行。
	gate.setInterval(60 * time.Millisecond)
	gate.markIssued()
	start := time.Now()
	if !gate.wait(context.Background()) {
		t.Fatal("短间隔应等待后放行")
	}
	if waited := time.Since(start); waited < 40*time.Millisecond {
		t.Fatalf("实际等待 %s，短于节流间隔", waited)
	}
}

// TestClaimGateIsPerInstance 锁定「节流窗口属于供应商实例」。
//
// 回归的是重构前的缺陷：间隔与计时器是包级变量，一个 App 的领取会
// 阻塞另一个 App 的领取，与 README 声明的实例隔离相冲突。
func TestClaimGateIsPerInstance(t *testing.T) {
	busy := newClaimGate()
	busy.setInterval(time.Hour)
	busy.markIssued()

	idle := newClaimGate()
	idle.setInterval(time.Hour)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if !idle.wait(ctx) {
		t.Fatal("另一个实例不应被本实例的领取节流阻塞")
	}
	if busy.wait(ctx) {
		t.Fatal("本实例刚发过领取，应当节流")
	}
}

// 活跃上报的纯函数测试（不触网）。
//
// 覆盖三件会静默出错、且出错表现都是「活动列表恒空」的事：
//   - 事件体字段集必须与官方 sendReport 一致（少字段上游可能静默忽略）；
//   - 每账号每日只报一次（重复上报没有收益，只会多打上游）；
//   - 日期按账号档案时区算（官方 reportAppDailyActive 的 toLocalDateKey 同义）。

func TestActivationEventBodyMatchesOfficialFields(t *testing.T) {
	cred := &Credential{
		JWTToken: "jwt",
		Profile: DeviceProfile{
			Platform: "win32", Arch: "x64", OSVersion: "10.0.22631",
			Language: "zh-CN", Timezone: "Asia/Shanghai",
			Screen: "1920x1080", DeviceMID: "mid-1",
		},
	}
	body := activationEventBody("app_launch", cred, "user-1")

	// 官方 sendReport 的 16 个键，一个都不能少（多一个不会被拒，少一个可能被忽略）。
	want := []string{
		"event_id", "client_timezone", "client_language", "element_name",
		"event_region", "event_type", "event_text", "event_extra_detail",
		"user_id", "screen_resolution", "app_version", "device_os_category",
		"device_os_version", "device_mid", "mac_id", "marketing_params",
	}
	for _, key := range want {
		if _, ok := body[key]; !ok {
			t.Fatalf("事件体缺少字段 %q: %+v", key, body)
		}
	}
	if len(body) != len(want) {
		t.Fatalf("事件体字段数 = %d, want %d: %+v", len(body), len(want), body)
	}

	if body["element_name"] != "app_launch" || body["event_region"] != "app" || body["event_type"] != "view" {
		t.Fatalf("事件语义字段错误: %+v", body)
	}
	// 设备字段必须取自账号档案，否则多账号会聚成同一台机器。
	if body["device_mid"] != "mid-1" || body["device_os_category"] != "windows" ||
		body["screen_resolution"] != "1920x1080" || body["client_timezone"] != "Asia/Shanghai" {
		t.Fatalf("设备字段未取自账号档案: %+v", body)
	}
	if body["marketing_params"] != "{}" || body["mac_id"] != "" {
		t.Fatalf("固定字段错误: %+v", body)
	}
	if body["event_id"] == "" || body["event_id"] == activationEventBody("app_launch", cred, "user-1")["event_id"] {
		t.Fatal("event_id 每次必须新生成")
	}
}

func TestActivationElementsAreOfficialPair(t *testing.T) {
	// 两条缺一不可：app_launch 是启动，app_daily_active 是日活。
	want := []string{"app_launch", "app_daily_active"}
	if len(activationElements) != len(want) {
		t.Fatalf("activationElements = %v", activationElements)
	}
	for index, element := range want {
		if activationElements[index] != element {
			t.Fatalf("activationElements[%d] = %q, want %q", index, activationElements[index], element)
		}
	}
}

func TestActivationStateDedupesPerUserPerDay(t *testing.T) {
	state := newActivationState()
	if !state.shouldReport("u1", "2026-10-23") {
		t.Fatal("首次上报应放行")
	}
	if state.shouldReport("u1", "2026-10-23") {
		t.Fatal("同一账号同一天不应重复上报")
	}
	// 账号之间互不影响。
	if !state.shouldReport("u2", "2026-10-23") {
		t.Fatal("不同账号应各自上报")
	}
	// 跨日重新放行。
	if !state.shouldReport("u1", "2026-10-24") {
		t.Fatal("跨日应重新上报")
	}
}

func TestActivationStateForgetsOnFailure(t *testing.T) {
	state := newActivationState()
	state.shouldReport("u1", "2026-10-23")
	// 上报失败必须撤销占位，否则整个自然日都拿不到活动。
	state.forget("u1")
	if !state.shouldReport("u1", "2026-10-23") {
		t.Fatal("失败后应允许重试")
	}
}

func TestLocalDateFollowsProfileTimezone(t *testing.T) {
	// 2026-10-23T20:00:00Z：上海已是 10-24，纽约仍是 10-23。
	at := time.Date(2026, 10, 23, 20, 0, 0, 0, time.UTC)
	if got := localDate(DeviceProfile{Timezone: "Asia/Shanghai"}, at); got != "2026-10-24" {
		t.Fatalf("上海日期 = %q, want 2026-10-24", got)
	}
	if got := localDate(DeviceProfile{Timezone: "America/New_York"}, at); got != "2026-10-23" {
		t.Fatalf("纽约日期 = %q, want 2026-10-23", got)
	}
	// 时区缺失/非法时退回 UTC，而不是 panic 或空串。
	if got := localDate(DeviceProfile{}, at); got != "2026-10-23" {
		t.Fatalf("无时区日期 = %q, want 2026-10-23", got)
	}
	if got := localDate(DeviceProfile{Timezone: "Not/AZone"}, at); got != "2026-10-23" {
		t.Fatalf("非法时区日期 = %q, want 2026-10-23", got)
	}
}

func TestEnsureActivationExplainsMissingUserID(t *testing.T) {
	// 无 user_id 时不上报，但必须给出原因：否则用户只会看到永远空的活动列表。
	v := Instances(core.Env{})[0].(*adapter)
	reason := v.ensureActivation(context.Background(), &Credential{JWTToken: "not-a-jwt"})
	if reason == "" {
		t.Fatal("缺少 user_id 时必须返回原因")
	}
	if reason := v.ensureActivation(context.Background(), nil); reason != "" {
		t.Fatalf("nil 凭证应静默, got %q", reason)
	}
}

func TestMarketingHeadersCarryCredential(t *testing.T) {
	// JWT 优先：同一凭证同时有 JWT 与 API Key 时，只能带 JWT 的 Bearer。
	h := marketingHeaders(&Credential{
		JWTToken: "jwt-token",
		APIKey:   "sk-test",
		Profile:  DeviceProfile{Platform: "win32", Arch: "x64", Language: "en-US"},
	})
	if h["Authorization"] != "Bearer jwt-token" || h["x-api-key"] != "" {
		t.Fatalf("JWT headers = %#v", h)
	}
	if h["Accept"] != "application/json" || h["x-request-id"] == "" {
		t.Fatalf("missing request headers: %#v", h)
	}
	// 语言必须来自账号档案，而不是被硬编码的 zh-CN 覆盖。
	if got := h["X-Client-Language"]; got != "en-US" {
		t.Fatalf("X-Client-Language = %q, want en-US", got)
	}

	// 纯 API Key 凭证（控制台添加的形态）必须带 x-api-key，否则上游按匿名请求处理。
	h = marketingHeaders(&Credential{APIKey: "sk-test", Profile: DeviceProfile{Platform: "win32", Arch: "x64"}})
	if h["x-api-key"] != "sk-test" || h["Authorization"] != "" {
		t.Fatalf("API key headers = %#v", h)
	}
}

func TestParseMarketingTouchNormalizesPlans(t *testing.T) {
	raw := []byte(`{"code":0,"data":{"deliveries":[
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"low","name":"Low","priority":1,"entitlements":[{"show_name":"GLM-5.3-Flash","grant_units":100,"period":"one_time"}]}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"high","name":"High","priority":9}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"plan_id":"HIGH","name":"High 重复","priority":9}}}}},
		{"banner":{"background":{"args":{"button":{"text":"learn more"}}}}},
		{"banner":{"background":{"args":{"zcode_plan":{"name":"没有 plan_id"}}}}}
	]}}`)

	plans, err := parseMarketingTouch(http.StatusOK, raw)
	if err != nil {
		t.Fatalf("parseMarketingTouch: %v", err)
	}
	// 没有 plan_id 的投递与重复 plan_id（大小写不同）都要被丢弃。
	if len(plans) != 2 {
		t.Fatalf("plans = %+v, want 2", plans)
	}
	// 优先级降序，同优先级按 plan_id 升序。
	if plans[0].PlanID != "high" || plans[1].PlanID != "low" {
		t.Fatalf("order = %q, %q", plans[0].PlanID, plans[1].PlanID)
	}
	if len(plans[1].Grants) != 1 || plans[1].Grants[0].Model != "GLM-5.3-Flash" {
		t.Fatalf("grants = %+v", plans[1].Grants)
	}
}

func TestParseMarketingTouchErrors(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantCode   string
		wantDetail string
	}{
		{
			name:       "业务码非零",
			status:     http.StatusOK,
			body:       `{"code":4001,"msg":"today not active"}`,
			wantCode:   "zcode_activity_business",
			wantDetail: "today not active",
		},
		{
			name:       "HTTP 非 2xx",
			status:     http.StatusUnauthorized,
			body:       `{"code":401,"msg":"unauthorized"}`,
			wantCode:   "zcode_activity_http",
			wantDetail: "HTTP 401",
		},
		{
			name:       "响应不是 JSON",
			status:     http.StatusOK,
			body:       `<html>login</html>`,
			wantCode:   "zcode_activity_response",
			wantDetail: "login",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseMarketingTouch(test.status, []byte(test.body))
			var pluginErr *core.PluginError
			if !errors.As(err, &pluginErr) {
				t.Fatalf("err = %v, want *core.PluginError", err)
			}
			if pluginErr.Code != test.wantCode || pluginErr.HTTPStatus != http.StatusBadGateway {
				t.Fatalf("err = %+v", pluginErr)
			}
			if !strings.Contains(err.Error(), test.wantDetail) {
				t.Fatalf("err = %q, want detail %q", err, test.wantDetail)
			}
		})
	}
}

// ---- billing/balance 额度折算 ----

// balanceFixture 返回 2026-10-25 实测 billing/balance 的裁剪响应（字段结构一致）。
// 每次返回新副本，避免用例间共享可变包级状态。
func balanceFixture() map[string]any {
	return map[string]any{
		"data": map[string]any{
			"server_time": float64(1792900000),
			"plans": []any{
				map[string]any{
					"plan_id": "zcode-v3-start-plan-1024",
					"name":    "ZCode 1024 Dev Day",
					"status":  "active",
					"ends_at": float64(1792976400),
				},
				map[string]any{
					"plan_id": "zcode-basic",
					"name":    "Basic",
					"status":  "",
					"ends_at": float64(1795219200),
				},
				map[string]any{
					"plan_id": "zcode-expired",
					"name":    "Expired",
					"status":  "active",
					"ends_at": float64(1792800000),
				},
			},
			"balances": []any{
				map[string]any{
					"show_name":       "GLM-5.3-Flash",
					"total_units":     float64(300000000),
					"used_units":      float64(169395514),
					"remaining_units": float64(130604486),
					"expires_at":      float64(1792976400),
				},
				map[string]any{
					"show_name":       "GLM-5.3",
					"total_units":     float64(200000000),
					"used_units":      float64(15638817),
					"remaining_units": float64(184361183),
					"expires_at":      float64(1792976400),
				},
			},
		},
	}
}

// balanceRowsFixture 取实测 fixture 里的额度桶条目（balanceQuota 只吃条目）。
func balanceRowsFixture() []map[string]any {
	return objectRows(dataMap(balanceFixture())["balances"])
}

// metricValue 取摘要里指定 key 的指标值。
func metricValue(metrics []pluginapi.QuotaMetric, key string) (float64, bool) {
	for _, metric := range metrics {
		if metric.Key == key {
			return metric.Value, true
		}
	}
	return 0, false
}

// TestBalanceQuotaExposeTotalsFamily 验证 default 族汇总：账号表的
// 「剩余 / 总额」列靠它们算比例，逐模型指标保持独立键。
func TestBalanceQuotaExposeTotalsFamily(t *testing.T) {
	metrics, groups := balanceQuota(balanceRowsFixture())
	for key, want := range map[string]float64{
		"remaining": 130604486 + 184361183,
		"total":     500000000,
		"used":      169395514 + 15638817,
	} {
		if got, ok := metricValue(metrics, key); !ok || got != want {
			t.Fatalf("metric %q = %v (found %v), want %v", key, got, ok, want)
		}
	}
	// 模型额度组：每桶一个，分数与到期时间齐备。
	if len(groups) != 1 || len(groups[0].Buckets) != 2 {
		t.Fatalf("groups = %+v", groups)
	}
	first := groups[0].Buckets[0]
	want := 130604486.0 / 300000000.0
	if first.RemainingFraction < want-0.0001 || first.RemainingFraction > want+0.0001 {
		t.Fatalf("fraction = %v, want ~%v", first.RemainingFraction, want)
	}
	if first.ResetTime != "2026-10-26T01:00:00Z" {
		t.Fatalf("ResetTime = %q, want 2026-10-26T01:00:00Z (UTC)", first.ResetTime)
	}
}

// TestBalanceQuotaEdgeCases 覆盖边界：空 balances、total=0、越界分数 clamp、
// 全部条目解析失败不给假汇总。
func TestBalanceQuotaEdgeCases(t *testing.T) {
	if m, g := balanceQuota(nil); m != nil || g != nil {
		t.Fatalf("empty balances must yield nil/nil, got %+v %+v", m, g)
	}

	// total=0：分数按「仍有余额 = 满额」展示，remaining 仍计入汇总。
	metrics, groups := balanceQuota([]map[string]any{
		{"show_name": "X", "total_units": float64(0), "remaining_units": float64(5)},
	})
	if got, ok := metricValue(metrics, "remaining"); !ok || got != 5 {
		t.Fatalf("total=0 metrics = %+v", metrics)
	}
	if got := groups[0].Buckets[0].RemainingFraction; got != 1 {
		t.Fatalf("total=0 fraction = %v, want 1 (unknown total but positive remaining)", got)
	}

	// remaining 超过 total：clamp 到 1。
	_, groups = balanceQuota([]map[string]any{
		{"show_name": "X", "total_units": float64(10), "remaining_units": float64(50)},
	})
	if groups[0].Buckets[0].RemainingFraction != 1 {
		t.Fatalf("clamp fraction = %v", groups[0].Buckets[0].RemainingFraction)
	}

	// 条目全部缺 show_name：不给「剩余 0」的假汇总。
	metrics, _ = balanceQuota([]map[string]any{
		{"total_units": float64(10)},
	})
	if metrics != nil {
		t.Fatalf("unparsable entries must yield nil metrics, got %+v", metrics)
	}
}

// TestPlanExpiryGroupNearestActive 验证有效期口径：空 status 视作 active、
// active 但已过 ends_at 排除、多套餐取最近未过期；到期走 Description。
func TestPlanExpiryGroupNearestActive(t *testing.T) {
	body := balanceFixture()
	plans := planRows(body)
	serverTime := serverSeconds(body)
	group := planExpiryGroup(plans, serverTime)
	if group == nil {
		t.Fatal("expiry group missing")
	}
	bucket := group.Buckets[0]
	// 最近未过期的是 1792976400（1024 礼包），不是更晚的 basic（空 status 也算 active）。
	if bucket.ResetTime != "" {
		t.Fatalf("ResetTime must stay empty for one-time plans, got %q", bucket.ResetTime)
	}
	if bucket.RemainingFraction != 0 {
		t.Fatalf("expiry bucket must not render a progress bar, got %v", bucket.RemainingFraction)
	}
	want := "套餐到期 2026-10-26T01:00:00Z"
	if bucket.Description != want {
		t.Fatalf("Description = %q, want %q", bucket.Description, want)
	}
}

// TestActivePlansSharedSemantics 验证 activePlans 的共用口径被
// heldPlanIDs 与 planExpiryGroup 一致消费。
func TestActivePlansSharedSemantics(t *testing.T) {
	plans := planRows(balanceFixture())
	active := activePlans(plans, serverSeconds(balanceFixture()))
	if len(active) != 2 {
		t.Fatalf("active plans = %d, want 2 (1024 礼包 + 空 status basic；过期项排除)", len(active))
	}
	held := heldPlanIDs(plans, serverSeconds(balanceFixture()))
	if len(held) != 2 || held["zcode-expired"] {
		t.Fatalf("held = %+v", held)
	}
}

// TestSubscriptionPlanSelection 验证订阅名选择：优先取与额度桶同到期的套餐，
// 没有关联证据时退回最早的生效套餐，一个生效套餐都没有时保留默认名。
//
// 数组顺序不可信：并存「促销礼包 + 基础套餐」时，第一个 active 未必是账号
// 正在消耗的那份，展示错套餐名会让用户按错误的额度预期用模型。
func TestSubscriptionPlanSelection(t *testing.T) {
	const serverTime = 1792900000
	matched := []map[string]any{
		{"plan_id": "promo", "name": "Promo", "status": "active", "ends_at": float64(1800000000)},
		{"plan_id": "in-use", "name": "In Use", "status": "active", "ends_at": float64(1792976400)},
		{"plan_id": "pending", "name": "Pending", "status": "pending", "ends_at": float64(1792976400)},
	}
	ended := []map[string]any{
		{"plan_id": "ended", "name": "Ended", "status": "active", "ends_at": float64(1792800000)},
		{"plan_id": "running", "name": "Running", "status": "", "ends_at": float64(1800000000)},
	}
	tests := []struct {
		name     string
		plans    []map[string]any
		balances []map[string]any
		wantPlan string
		wantTier string
	}{
		{
			name:  "按桶到期时刻命中",
			plans: matched,
			// 两个桶同属 1024 礼包；promo 到期更晚但没有任何桶指向它。
			balances: []map[string]any{
				{"show_name": "GLM-5.3", "expires_at": float64(1792976400)},
				{"show_name": "GLM-5.3-Flash", "expires_at": float64(1792976400)},
			},
			wantPlan: "In Use", wantTier: "in-use",
		},
		{
			name:     "桶到期对不上时退回第一个生效套餐",
			plans:    ended,
			balances: []map[string]any{{"show_name": "X", "expires_at": float64(9999)}},
			wantPlan: "Running", wantTier: "running",
		},
		{
			name:     "没有额度桶时退回第一个生效套餐",
			plans:    ended,
			wantPlan: "Running", wantTier: "running",
		},
		{
			name: "没有生效套餐时保留默认订阅名",
			plans: []map[string]any{
				{"plan_id": "gone", "name": "Gone", "status": "expired", "ends_at": float64(1000)},
			},
			balances: []map[string]any{{"show_name": "X"}},
			wantPlan: "ZCode Coding Plan", wantTier: "Standard",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := &pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{
				Plan: "ZCode Coding Plan", TierName: "Standard", TierID: "zcode",
			}}
			applySubscription(planSnapshot{
				plans: test.plans, balances: test.balances, serverTime: serverTime,
			}, response)
			if response.Subscription.Plan != test.wantPlan || response.Subscription.TierName != test.wantTier {
				t.Fatalf("subscription = %q / %q, want %q / %q",
					response.Subscription.Plan, response.Subscription.TierName, test.wantPlan, test.wantTier)
			}
		})
	}
}

// TestBillingEnvelopeComplete 区分「上游没给 balances 数组（改版/截断）」与
// 「账号确实没有额度桶（空数组）」。前者必须报错，后者是合法答案。
func TestBillingEnvelopeComplete(t *testing.T) {
	tests := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"真空账号：数组齐全但为空", map[string]any{"data": map[string]any{
			"plans": []any{}, "balances": []any{},
		}}, true},
		{"缺 balances", map[string]any{"data": map[string]any{"plans": []any{}}}, false},
		{"balances 不是数组", map[string]any{"data": map[string]any{"balances": "oops"}}, false},
		{"缺 data", map[string]any{"code": codeSuccess}, false},
		{"空响应", nil, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := billingEnvelopeComplete(test.body); got != test.want {
				t.Fatalf("billingEnvelopeComplete = %v, want %v", got, test.want)
			}
		})
	}
}

// TestActivePlansNormalizesMillisecondTimestamps 验证 13 位毫秒时间戳不会把
// 生效套餐整批判成过期：上游混用秒/毫秒时，比较前必须先归一。
func TestActivePlansNormalizesMillisecondTimestamps(t *testing.T) {
	plans := []map[string]any{
		{"plan_id": "ms", "status": "active", "ends_at": float64(1792976400000)},
		{"plan_id": "s", "status": "active", "ends_at": float64(1800000000)},
	}
	if active := activePlans(plans, 1792900000); len(active) != 2 {
		t.Fatalf("active plans = %+v, want both entries", active)
	}
	body := map[string]any{"data": map[string]any{"server_time": float64(1792900000000)}}
	if got := serverSeconds(body); got != 1792900000 {
		t.Fatalf("serverSeconds = %d, want 1792900000", got)
	}
}
