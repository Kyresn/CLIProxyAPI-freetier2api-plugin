package tasks

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/auth"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/state"
)

// 签到落库的行为测试。
//
// 落库语义是「假性成功」的最后一道闸：只要签到结果被记成「今日已签到」，
// 控制台就会显示已签到，而上游窗口错过不补领——积分就此永久丢失。
// 因此「本次没真正领到」必须不落库。

// fakeDeps 是只实现落库所需能力的最小 Deps。
type fakeDeps struct {
	now   time.Time
	state state.State
	cfg   Config
	// mutations 统计落盘次数：一次签到只应产生一次状态变更。
	mutations int
}

func (f *fakeDeps) Registry() *core.Registry { return nil }
func (f *fakeDeps) Pool() *auth.Pool         { return nil }
func (f *fakeDeps) Config() Config           { return f.cfg }
func (f *fakeDeps) State() state.State       { return f.state }
func (f *fakeDeps) Now() time.Time           { return f.now }

func (f *fakeDeps) MutateState(mutate func(*state.State)) {
	f.mutations++
	mutate(&f.state)
}

func (f *fakeDeps) SaveRefreshedCredential(
	ctx context.Context, callbackID string, vendor core.Vendor,
	entry core.HostAuth, original []byte, updated *core.Credential, filePath string,
) {
}

// newFakeDeps 构造一个带好 map 的最小 Deps。
func newFakeDeps(now time.Time) *fakeDeps {
	return &fakeDeps{now: now, state: *state.New()}
}

// TestRecordCheckinResultSkipsUnfinished 验证：未完成的签到不落库。
func TestRecordCheckinResultSkipsUnfinished(t *testing.T) {
	credential := &core.Credential{FileID: "qodercn-user"}
	deps := newFakeDeps(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	service := &Service{deps: deps}

	// 上游窗口未开启：本次什么都没领到。
	service.recordCheckinResult(&fakeCheckinVendor{id: "qodercn"}, credential, CheckinTriggerSchedule, &core.CheckinResult{
		Message:     "上游领取窗口 10:00（UTC+8）开启，当前未开启",
		NotRecorded: true,
	})
	if len(deps.state.Checkin) != 0 {
		t.Fatalf("未完成的签到不应落库，实际 = %+v", deps.state.Checkin)
	}
}

// TestRecordCheckinResultRecordsCompleted 验证：真正领到的签到照常落库。
func TestRecordCheckinResultRecordsCompleted(t *testing.T) {
	credential := &core.Credential{FileID: "qodercn-user"}
	deps := newFakeDeps(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	service := &Service{deps: deps}

	service.recordCheckinResult(&fakeCheckinVendor{id: "qodercn"}, credential, CheckinTriggerSchedule, &core.CheckinResult{Credit: 100})
	record, ok := deps.state.Checkin["qodercn-user"]
	if !ok {
		t.Fatal("已完成的签到应落库")
	}
	if record.LastDate != "2026-10-21" {
		t.Errorf("last_date = %q, 期望 2026-10-21", record.LastDate)
	}
	if record.Credit != 100 {
		t.Errorf("credit = %d, 期望 100", record.Credit)
	}
}

// TestRecordCheckinResultKeepsVendorDefault 验证：未声明 NotRecorded 的厂商
// 行为不变（零值兼容，其它厂商的签到记录照常写入）。
func TestRecordCheckinResultKeepsVendorDefault(t *testing.T) {
	credential := &core.Credential{FileID: "workbuddycn-user"}
	deps := newFakeDeps(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	service := &Service{deps: deps}

	// Already=true、Credit=0：既有厂商「已领取但本次无新积分」的形态。
	service.recordCheckinResult(&fakeCheckinVendor{id: "qodercn"}, credential, CheckinTriggerSchedule, &core.CheckinResult{Already: true})
	record, ok := deps.state.Checkin["workbuddycn-user"]
	if !ok {
		t.Fatal("未声明 NotRecorded 的结果应照常落库（其它厂商行为不得改变）")
	}
	if record.LastDate != "2026-10-21" {
		t.Errorf("last_date = %q, 期望 2026-10-21", record.LastDate)
	}
}

// fakeCheckinVendor 是只实现签到能力的最小 Vendor：历史落库只依赖 ID 与
// CheckinProvider，其余方法按契约返回零值。
type fakeCheckinVendor struct {
	id     string
	result *core.CheckinResult
	err    error
}

func (v *fakeCheckinVendor) ID() string                                     { return v.id }
func (v *fakeCheckinVendor) Name() string                                   { return v.id }
func (v *fakeCheckinVendor) Region() string                                 { return "cn" }
func (v *fakeCheckinVendor) ModelPrefix() string                            { return "" }
func (v *fakeCheckinVendor) Match(string, string, map[string]any) bool      { return false }
func (v *fakeCheckinVendor) Parse([]byte, string) (*core.Credential, error) { return nil, nil }
func (v *fakeCheckinVendor) StaticModels(context.Context) ([]pluginapi.ModelInfo, error) {
	return nil, nil
}
func (v *fakeCheckinVendor) Execute(context.Context, *core.Credential, *core.ExecuteRequest) (*pluginapi.ExecutorResponse, error) {
	return nil, nil
}
func (v *fakeCheckinVendor) ExecuteStream(context.Context, *core.Credential, *core.ExecuteRequest, core.StreamSink) error {
	return nil
}
func (v *fakeCheckinVendor) SupportsCheckin() bool { return true }
func (v *fakeCheckinVendor) Checkin(context.Context, *core.Credential) (*core.CheckinResult, error) {
	return v.result, v.err
}

// TestCheckinRecordsHistoryForEveryPath 验证三条结果路径都进历史。
//
// 失败路径过去完全不落库：横幅一消失就再也查不到「哪个账号、为什么失败」，
// 这正是本 issue 要解决的问题。
func TestCheckinRecordsHistoryForEveryPath(t *testing.T) {
	credential := &core.Credential{FileID: "qodercn-user"}
	key := state.CheckinHistoryKey("qodercn", "qodercn-user")

	t.Run("success", func(t *testing.T) {
		deps := newFakeDeps(time.Date(2026, 10, 25, 10, 0, 1, 0, time.UTC))
		service := &Service{deps: deps}
		vendor := &fakeCheckinVendor{id: "qodercn", result: &core.CheckinResult{Credit: 100, Message: "签到成功"}}

		if _, errRun := service.Checkin(context.Background(), vendor, credential, CheckinTriggerManual); errRun != nil {
			t.Fatalf("Checkin: %v", errRun)
		}
		history := deps.state.CheckinHistory[key]
		if len(history) != 1 {
			t.Fatalf("历史条数 = %d, 期望 1", len(history))
		}
		entry := history[0]
		if entry.Status != "success" || entry.Credit != 100 || entry.Message != "签到成功" {
			t.Errorf("历史条目 = %+v, 期望成功且 credit=100", entry)
		}
		if entry.Trigger != string(CheckinTriggerManual) {
			t.Errorf("trigger = %q, 期望 manual", entry.Trigger)
		}
		if entry.RunAt != "2026-10-25T10:00:01Z" {
			t.Errorf("run_at = %q, 期望本次执行时间", entry.RunAt)
		}
		if deps.state.Checkin["qodercn-user"].LastDate != "2026-10-25" {
			t.Error("成功签到仍要更新「最近一次」记录")
		}
		if deps.mutations != 1 {
			t.Errorf("落盘次数 = %d, 期望 1（最近一次与历史在同一次 Mutate 里写）", deps.mutations)
		}
	})

	t.Run("already", func(t *testing.T) {
		deps := newFakeDeps(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC))
		service := &Service{deps: deps}
		vendor := &fakeCheckinVendor{id: "qodercn", result: &core.CheckinResult{Already: true, Message: "今日已完成签到"}}

		if _, errRun := service.Checkin(context.Background(), vendor, credential, CheckinTriggerSchedule); errRun != nil {
			t.Fatalf("Checkin: %v", errRun)
		}
		if status := deps.state.CheckinHistory[key][0].Status; status != "already" {
			t.Errorf("status = %q, 期望 already", status)
		}
		if trigger := deps.state.CheckinHistory[key][0].Trigger; trigger != string(CheckinTriggerSchedule) {
			t.Errorf("trigger = %q, 期望 schedule", trigger)
		}
	})

	t.Run("error", func(t *testing.T) {
		deps := newFakeDeps(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC))
		service := &Service{deps: deps}
		vendor := &fakeCheckinVendor{id: "qodercn", err: &core.Failure{Kind: core.FailureKindServer, Msg: "上游 500"}}

		if _, errRun := service.Checkin(context.Background(), vendor, credential, CheckinTriggerSchedule); errRun == nil {
			t.Fatal("上游失败必须把错误透传给调用方")
		}
		history := deps.state.CheckinHistory[key]
		if len(history) != 1 || history[0].Status != "error" {
			t.Fatalf("失败也要进历史，实际 = %+v", history)
		}
		if !strings.Contains(history[0].Message, "上游 500") {
			t.Errorf("历史必须保留报错原文，实际 = %q", history[0].Message)
		}
		if len(deps.state.Checkin) != 0 {
			t.Error("失败的签到不得写成「今日已签到」")
		}
	})

	t.Run("not-recorded", func(t *testing.T) {
		deps := newFakeDeps(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC))
		service := &Service{deps: deps}
		vendor := &fakeCheckinVendor{id: "qodercn", result: &core.CheckinResult{
			Message: "无可用签到活动", NotRecorded: true,
		}}

		if _, errRun := service.Checkin(context.Background(), vendor, credential, CheckinTriggerManual); errRun != nil {
			t.Fatalf("Checkin: %v", errRun)
		}
		if status := deps.state.CheckinHistory[key][0].Status; status != "error" {
			t.Errorf("status = %q, 期望 error（本次没领到不等于成功）", status)
		}
		if len(deps.state.Checkin) != 0 {
			t.Error("NotRecorded 的结果不得写入「最近一次」记录")
		}
	})
}

// TestCheckinHistoryTrimsOldest 验证超出上限时淘汰最旧，且始终新→旧。
func TestCheckinHistoryTrimsOldest(t *testing.T) {
	credential := &core.Credential{FileID: "qodercn-user"}
	key := state.CheckinHistoryKey("qodercn", "qodercn-user")
	deps := newFakeDeps(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC))
	deps.cfg.CheckinHistoryLimit = 2
	service := &Service{deps: deps}

	for index := 1; index <= 3; index++ {
		deps.now = time.Date(2026, 10, 25, 10, 0, index, 0, time.UTC)
		vendor := &fakeCheckinVendor{id: "qodercn", result: &core.CheckinResult{Message: fmt.Sprintf("第 %d 次", index)}}
		if _, errRun := service.Checkin(context.Background(), vendor, credential, CheckinTriggerManual); errRun != nil {
			t.Fatalf("Checkin: %v", errRun)
		}
	}

	history := deps.state.CheckinHistory[key]
	if len(history) != 2 {
		t.Fatalf("历史条数 = %d, 期望按上限截断到 2", len(history))
	}
	if history[0].Message != "第 3 次" || history[1].Message != "第 2 次" {
		t.Fatalf("历史顺序 = %+v, 期望新→旧且淘汰最旧", history)
	}
}

// TestCheckinHistoryKeySeparatesVendors 验证历史按「供应商 + 账号」隔离：
// 两个渠道的同名文件不得互相串记录（额度缓存踩过同一个坑）。
func TestCheckinHistoryKeySeparatesVendors(t *testing.T) {
	credential := &core.Credential{FileID: "same-file"}
	deps := newFakeDeps(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC))
	service := &Service{deps: deps}

	for _, vendorID := range []string{"qodercn", "qoderglobal"} {
		vendor := &fakeCheckinVendor{id: vendorID, result: &core.CheckinResult{Message: vendorID}}
		if _, errRun := service.Checkin(context.Background(), vendor, credential, CheckinTriggerManual); errRun != nil {
			t.Fatalf("Checkin %s: %v", vendorID, errRun)
		}
	}
	for _, vendorID := range []string{"qodercn", "qoderglobal"} {
		history := deps.state.CheckinHistory[state.CheckinHistoryKey(vendorID, "same-file")]
		if len(history) != 1 || history[0].Message != vendorID {
			t.Fatalf("%s 的历史 = %+v, 期望只有自己那一条", vendorID, history)
		}
	}
}
