package tasks

import (
	"context"
	"testing"

	"freetier2api-plugin/internal/core"
)

// TestBuildQueueItemsCollapsesAggregatedTasks 验证聚合任务（RunCode）每个账号只入队一次，
// 且队列里使用的是 RunCode 而不是明细任务码。
func TestBuildQueueItemsCollapsesAggregatedTasks(t *testing.T) {
	service := &Service{}
	scans := []accountScan{
		{
			UID: "u1", Label: "one",
			Tasks: []taskItem{
				{Kind: "growth", Code: "chat_5"},
				{Kind: "school", Code: "share_invite", RunCode: "school_daily"},
				{Kind: "school", Code: "chat_3_times", RunCode: "school_daily"},
			},
		},
		{
			UID: "u2", Label: "two",
			Tasks: []taskItem{
				{Kind: "school", Code: "expert_use", RunCode: "school_daily"},
			},
		},
	}

	items := service.buildQueueItems(scans, nil)
	if len(items) != 3 {
		t.Fatalf("buildQueueItems returned %d items, want 3: %#v", len(items), items)
	}
	want := map[string][]string{
		"u1": {"chat_5", "school_daily"},
		"u2": {"school_daily"},
	}
	for _, item := range items {
		found := false
		for _, code := range want[item.UID] {
			if item.Code == code {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("unexpected queue item for %s: %#v", item.UID, item)
		}
	}
}

// TestBuildQueueItemsFiltersByKind 验证旧控制台的 kind 筛选仍然生效。
func TestBuildQueueItemsFiltersByKind(t *testing.T) {
	service := &Service{}
	scans := []accountScan{{
		UID: "u1",
		Tasks: []taskItem{
			{Kind: "growth", Code: "chat_5"},
			{Kind: "school", Code: "share_invite", RunCode: "school_daily"},
		},
	}}
	items := service.buildQueueItems(scans, map[string]bool{"school": true})
	if len(items) != 1 || items[0].Code != "school_daily" {
		t.Fatalf("filtered queue = %#v, want only school_daily", items)
	}
}

// fakeScheduledVendor 声明若干任务族并记录实际被调用的那些。
type fakeScheduledVendor struct {
	fakeCheckinVendor
	kinds []core.TaskSpec
	calls []string
}

func (v *fakeScheduledVendor) TaskSpecs() []core.TaskSpec { return v.kinds }

func (v *fakeScheduledVendor) RunScheduled(_ context.Context, _ *core.Credential, kind string) (string, error) {
	v.calls = append(v.calls, kind)
	return kind + " done", nil
}

// TestRunScheduledForAccountSkipsUndeclaredKind 验证全局排程被供应商能力过滤。
//
// issue #58：task_schedules 里的 dailybonus 只对国际版有意义，国内账号执行时
// 抛 "unknown scheduled task kind" 并记 ERROR。调度器必须跳过未声明的族。
func TestRunScheduledForAccountSkipsUndeclaredKind(t *testing.T) {
	vendor := &fakeScheduledVendor{
		fakeCheckinVendor: fakeCheckinVendor{id: "workbuddycn"},
		kinds:             []core.TaskSpec{{Kind: "travel"}},
	}
	service := &Service{}
	account := &accountContext{vendor: vendor, credential: &core.Credential{FileID: "u1"}}

	if message := service.runScheduledForAccount(context.Background(), account, "dailybonus"); message != "" {
		t.Fatalf("未声明的任务族应跳过，实际返回 %q", message)
	}
	if len(vendor.calls) != 0 {
		t.Fatalf("未声明的任务族不该下发到供应商，实际调用 %v", vendor.calls)
	}

	if message := service.runScheduledForAccount(context.Background(), account, "travel"); message != "travel done" {
		t.Fatalf("已声明的任务族应正常执行，返回 %q", message)
	}
	if len(vendor.calls) != 1 || vendor.calls[0] != "travel" {
		t.Fatalf("调用记录 = %v, want [travel]", vendor.calls)
	}
}
