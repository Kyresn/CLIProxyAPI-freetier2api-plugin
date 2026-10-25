package app

import (
	"encoding/json"
	"testing"
)

// TestAlignUIDWithFileName 验证 uid 与文件名的大小写对齐：
// 仅大小写不一致的同值改写，其它取值（含供应商自有 uid）不碰。
func TestAlignUIDWithFileName(t *testing.T) {
	// 大小写不一致的同值：对齐到文件名拼写（按键语义比较，不比字节序）。
	aligned := alignUIDWithFileName([]byte(`{"uid":"zcode-eyjh---1kc8","jwt_token":"t"}`), "zcode-eyJh---1KC8.json")
	var m map[string]any
	if errParse := json.Unmarshal(aligned, &m); errParse != nil {
		t.Fatalf("aligned payload is not json: %v", errParse)
	}
	if m["uid"] != "zcode-eyJh---1KC8" || m["jwt_token"] != "t" {
		t.Fatalf("aligned = %v", m)
	}

	// 已一致：内容不变。
	same := []byte(`{"uid":"zcode-eyJh---1KC8"}`)
	var before, after map[string]any
	_ = json.Unmarshal(same, &before)
	_ = json.Unmarshal(alignUIDWithFileName(same, "zcode-eyJh---1KC8.json"), &after)
	if before["uid"] != after["uid"] {
		t.Fatalf("already aligned payload must be untouched")
	}

	// uid 是完全不同的值（供应商自有上游 uid，非文件名的大小写变体）：不碰。
	own := []byte(`{"uid":"a707ecea-6211-4e4e-upstream"}`)
	var ownAfter map[string]any
	_ = json.Unmarshal(alignUIDWithFileName(own, "workbuddycn-A707ECEA.json"), &ownAfter)
	if ownAfter["uid"] != "a707ecea-6211-4e4e-upstream" {
		t.Fatalf("non-matching uid must be untouched, got %v", ownAfter["uid"])
	}

	// 无 uid 字段 / 非 JSON：原样返回。
	noUID := []byte(`{"jwt_token":"t"}`)
	var noUIDAfter map[string]any
	_ = json.Unmarshal(alignUIDWithFileName(noUID, "x.json"), &noUIDAfter)
	if noUIDAfter["uid"] != nil {
		t.Fatalf("payload without uid must stay without uid")
	}
	bad := []byte(`not json`)
	if got := alignUIDWithFileName(bad, "x.json"); string(got) != string(bad) {
		t.Fatalf("invalid json must pass through, got %s", got)
	}
}
