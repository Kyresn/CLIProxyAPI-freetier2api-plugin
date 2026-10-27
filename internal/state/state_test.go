package state

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestStoreRoundTripPreservesFields(t *testing.T) {
	dir := t.TempDir()
	store := NewStore()
	store.Configure(dir)
	if _, err := store.Load(); err != nil {
		t.Fatalf("load: %v", err)
	}
	enabled := true
	store.Mutate(func(s *State) {
		s.AutoCheckin = &enabled
		s.PromptMode = "append"
		s.DisabledModels = []string{"vendor:model"}
		s.ModelAliases = map[string]string{"vendor:model": "alias"}
		s.AccountAliases = map[string]string{"workbuddycn-u1": "工作号"}
		s.Checkin["u1"] = CheckinRecord{LastDate: "2026-10-18", Streak: 3}
		s.Tasks["u1\x1fchat_5"] = TaskRecord{LastStatus: "done", Credit: 10}
	})

	reloaded := NewStore()
	reloaded.Configure(dir)
	got, err := reloaded.Load()
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if got.AutoCheckin == nil || !*got.AutoCheckin || got.PromptMode != "append" {
		t.Fatalf("pointer fields lost: %#v", got)
	}
	if len(got.DisabledModels) != 1 || got.ModelAliases["vendor:model"] != "alias" {
		t.Fatalf("model fields lost: %#v", got)
	}
	if got.AccountAliases["workbuddycn-u1"] != "工作号" {
		t.Fatalf("account aliases lost: %#v", got.AccountAliases)
	}
	if got.Checkin["u1"].Streak != 3 || got.Tasks["u1\x1fchat_5"].Credit != 10 {
		t.Fatalf("records lost: %#v", got)
	}
	raw, errRead := os.ReadFile(filepath.Join(dir, "state.json"))
	if errRead != nil || len(raw) == 0 {
		t.Fatalf("state file missing: %v", errRead)
	}
}

func TestStoreCorruptFileStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	store.Configure(dir)
	got, err := store.Load()
	if err != nil {
		t.Fatalf("load corrupt: %v", err)
	}
	if got.Checkin == nil || got.Tasks == nil || got.ModelAliases == nil || got.AccountAliases == nil {
		t.Fatalf("empty state not normalized: %#v", got)
	}
}

func TestSnapshotIsDeepCopy(t *testing.T) {
	store := NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	store.Mutate(func(s *State) {
		s.DisabledModels = []string{"a"}
		s.Checkin["u"] = CheckinRecord{Streak: 1}
		s.AccountAliases["f-1"] = "工作号"
	})
	snap := store.Snapshot()
	snap.DisabledModels[0] = "changed"
	snap.Checkin["u"] = CheckinRecord{Streak: 99}
	snap.AccountAliases["f-1"] = "changed"
	if got := store.Snapshot(); got.DisabledModels[0] != "a" || got.Checkin["u"].Streak != 1 || got.AccountAliases["f-1"] != "工作号" {
		t.Fatalf("snapshot aliases internal state: %#v", got)
	}
}

func TestMutateIsConcurrentSafe(t *testing.T) {
	store := NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Mutate(func(s *State) { s.Tasks["k"] = TaskRecord{LastStatus: "done"} })
			_ = store.Snapshot()
		}()
	}
	wg.Wait()
}

func TestCheckinHistoryKeyRequiresVendorAndAccount(t *testing.T) {
	if got := CheckinHistoryKey("qodercn", "qodercn-user"); got != "qodercn\x00qodercn-user" {
		t.Fatalf("key = %q, 期望 vendor + 账号身份", got)
	}
	// 身份不完整时返回空键：调用方据此放弃落库，而不是写出一个会被互相覆盖的键。
	if key := CheckinHistoryKey("", "qodercn-user"); key != "" {
		t.Fatalf("缺供应商时 key = %q, 期望空", key)
	}
	if key := CheckinHistoryKey("qodercn", "  "); key != "" {
		t.Fatalf("缺账号身份时 key = %q, 期望空", key)
	}
}

func TestSnapshotCopiesCheckinHistory(t *testing.T) {
	store := NewStore()
	store.Configure(t.TempDir())
	if _, err := store.Load(); err != nil {
		t.Fatal(err)
	}
	key := CheckinHistoryKey("qodercn", "u1")
	store.Mutate(func(s *State) {
		s.CheckinHistory[key] = []CheckinHistoryEntry{{RunAt: "2026-10-25T10:00:01Z", Status: "success", Credit: 100}}
	})

	snap := store.Snapshot()
	snap.CheckinHistory[key][0].Status = "error"
	got := store.Snapshot().CheckinHistory[key]
	if len(got) != 1 || got[0].Status != "success" {
		t.Fatalf("snapshot 与内部状态共享了历史切片: %#v", got)
	}
}
