package opencodezen

import (
	"testing"
)

// sampleModelsDevCatalog 是 models.dev 响应的最小还原（provider "opencode"）。
const sampleModelsDevCatalog = `{
  "opencode": {
    "models": {
      "glm-5.3-flash-free": {
        "name": "GLM-5.3-Flash Free",
        "description": "Native multimodal GLM model for efficient coding",
        "reasoning": true,
        "tool_call": true,
        "reasoning_options": [
          {"type": "effort", "values": ["low", "high", "max"]},
          {"type": "verbosity", "values": ["brief"]}
        ],
        "modalities": {"input": ["text", "image"], "output": ["text"]},
        "limit": {"context": 200000, "output": 32000}
      },
      "qwen3-coder": {
        "name": "Qwen3 Coder",
        "reasoning": false,
        "tool_call": true,
        "modalities": {"input": ["text"], "output": ["text"]},
        "limit": {"context": 262144, "output": 65536}
      }
    }
  },
  "zenmux": {
    "models": {"unrelated": {"name": "Unrelated", "limit": {"context": 1}}}
  }
}`

func TestParseModelsDevCatalog(t *testing.T) {
	catalog, errParse := parseModelsDevCatalog([]byte(sampleModelsDevCatalog))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	entry, okEntry := catalog["glm-5.3-flash-free"]
	if !okEntry {
		t.Fatalf("zen provider entry missing")
	}
	if entry.Limit.Context != 200000 || entry.Limit.Output != 32000 {
		t.Fatalf("limit = %+v", entry.Limit)
	}
	if len(catalog) != 2 {
		t.Fatalf("catalog should only keep the zen provider, got %d entries", len(catalog))
	}
}

func TestParseModelsDevCatalogMissingProvider(t *testing.T) {
	catalog, errParse := parseModelsDevCatalog([]byte(`{"zenmux": {"models": {}}}`))
	if errParse != nil {
		t.Fatalf("parse should tolerate missing provider: %v", errParse)
	}
	if len(catalog) != 0 {
		t.Fatalf("expected empty catalog, got %d", len(catalog))
	}
}

func TestApplyModelsDevCatalog(t *testing.T) {
	catalog, errParse := parseModelsDevCatalog([]byte(sampleModelsDevCatalog))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	models := []Model{
		// 空字段全部由目录补齐。
		{ID: "glm-5.3-flash-free", Name: "glm-5.3-flash-free"},
		// 上游已给的元数据优先，目录不覆盖。
		{ID: "qwen3-coder", Name: "Qwen3 Coder", ContextWindow: 111, SupportsTools: false},
		// 目录里没有的模型保持原样。
		{ID: "unknown-free", Name: "unknown-free"},
	}
	applyModelsDevCatalog(models, catalog)

	glm := models[0]
	if glm.ContextWindow != 200000 || glm.MaxOutputTokens != 32000 {
		t.Fatalf("glm limits = %d/%d", glm.ContextWindow, glm.MaxOutputTokens)
	}
	if !glm.IsReasoning || !glm.SupportsTools || !glm.SupportsImages {
		t.Fatalf("glm capabilities wrong: reasoning=%v tools=%v images=%v", glm.IsReasoning, glm.SupportsTools, glm.SupportsImages)
	}
	if got, want := glm.Name, "GLM-5.3-Flash Free"; got != want {
		t.Fatalf("glm name = %q, want %q", got, want)
	}
	if glm.Description == "" {
		t.Fatalf("glm description should be filled from catalog")
	}
	wantEfforts := []string{"low", "high", "max"}
	if len(glm.Efforts) != len(wantEfforts) {
		t.Fatalf("glm efforts = %v, want %v", glm.Efforts, wantEfforts)
	}
	for i, want := range wantEfforts {
		if glm.Efforts[i] != want {
			t.Fatalf("glm efforts = %v, want %v", glm.Efforts, wantEfforts)
		}
	}

	qwen := models[1]
	if qwen.ContextWindow != 111 {
		t.Fatalf("qwen context = %d, upstream value should win", qwen.ContextWindow)
	}
	if qwen.IsReasoning {
		t.Fatalf("non-reasoning model must stay non-reasoning")
	}

	unknown := models[2]
	if unknown.ContextWindow != 0 || unknown.MaxOutputTokens != 0 || len(unknown.Efforts) != 0 {
		t.Fatalf("unknown model should stay untouched: %+v", unknown)
	}
}

func TestEffortsFromModelsDevIgnoresNonEffortOptions(t *testing.T) {
	catalog, errParse := parseModelsDevCatalog([]byte(sampleModelsDevCatalog))
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	efforts := effortsFromModelsDev(catalog["glm-5.3-flash-free"])
	if len(efforts) != 3 || efforts[0] != "low" {
		t.Fatalf("efforts = %v, verbosity option must be ignored", efforts)
	}
}

// TestApplyModelsDevCatalogMarksFreeOnlyOnZeroPrice 锁定零倍率的判定来源。
//
// models.dev 的 cost 是美元单价，不是倍率；只有输入与输出单价都为 0 才是
// 零倍率信号。没有 cost 块的条目不能当成零倍率——那只是目录缺数据。
func TestApplyModelsDevCatalogMarksFreeOnlyOnZeroPrice(t *testing.T) {
	models := []Model{{ID: "free-model"}, {ID: "paid-model"}, {ID: "unknown-model"}}
	catalog := modelsDevCatalog{
		"free-model":    {Cost: &modelsDevCost{}},
		"paid-model":    {Cost: &modelsDevCost{Input: 1.5, Output: 2}},
		"unknown-model": {},
	}
	applyModelsDevCatalog(models, catalog)

	if models[0].Credits != "0.00X" {
		t.Fatalf("零单价应记 0.00X，实际 %q", models[0].Credits)
	}
	if models[1].Credits != "" {
		t.Fatalf("有价格的模型不能伪装成倍率，实际 %q", models[1].Credits)
	}
	if models[2].Credits != "" {
		t.Fatalf("缺 cost 块不能当免费，实际 %q", models[2].Credits)
	}
}

// TestParseModelsKeepsNonFreeNamedModels 锁定「清单不按名字过滤」。
//
// 官方目录里 big-pickle 是零价模型（还是登出状态的默认模型），但不带
// "-free" 后缀。按名字筛会把官方默认模型从清单里抹掉。
func TestParseModelsKeepsNonFreeNamedModels(t *testing.T) {
	body := []byte(`{"object":"list","data":[
		{"id":"big-pickle","object":"model"},
		{"id":"glm-5.3","object":"model"},
		{"id":"deepseek-v4.1-flash-free","object":"model"}
	]}`)
	models, errParse := parseModels(body)
	if errParse != nil {
		t.Fatalf("parse: %v", errParse)
	}
	got := make(map[string]bool, len(models))
	for _, model := range models {
		got[model.ID] = true
	}
	for _, want := range []string{"big-pickle", "glm-5.3", "deepseek-v4.1-flash-free"} {
		if !got[want] {
			t.Fatalf("模型 %q 被过滤掉了；清单应保留上游返回的全部模型", want)
		}
	}
}

// TestFreeOnlyKeepsZeroPriceModels 锁定清单只展示免费层。
//
// 上游 /v1/models 同时返回几十个付费模型和调试用的 test 模型；本供应商
// 只接入免费额度，付费模型在插件里必然 402。判定用目录单价而不是名字，
// 所以 big-pickle（零价无 free 后缀）要留下，目录里没有的 test 要剔除。
func TestFreeOnlyKeepsZeroPriceModels(t *testing.T) {
	catalog := modelsDevCatalog{
		"big-pickle":               {Cost: &modelsDevCost{}},
		"deepseek-v4.1-flash-free": {Cost: &modelsDevCost{}},
		"glm-5.3":                  {Cost: &modelsDevCost{Input: 1.5, Output: 6}},
	}
	models := []Model{
		{ID: "big-pickle"},
		{ID: "deepseek-v4.1-flash-free"},
		{ID: "glm-5.3"},
		{ID: "test"}, // 上游调试模型，目录里没有
	}
	got := freeOnly(models, catalog)
	kept := make(map[string]bool, len(got))
	for _, model := range got {
		kept[model.ID] = true
	}
	if !kept["big-pickle"] || !kept["deepseek-v4.1-flash-free"] {
		t.Fatalf("零价模型被剔除：%v", kept)
	}
	if kept["glm-5.3"] {
		t.Fatal("付费模型不应出现在免费供应商的清单里")
	}
	if kept["test"] {
		t.Fatal("目录里没有的模型（无价格数据）不应展示")
	}

	// 目录不可用时不过滤：一次目录抖动不该让所有免费模型消失。
	if all := freeOnly(models, nil); len(all) != len(models) {
		t.Fatalf("目录不可用时不应过滤，得到 %d/%d", len(all), len(models))
	}
}

// TestAdapterModelCredits 锁定倍率表与清单同源。
func TestAdapterModelCredits(t *testing.T) {
	v := &adapter{}
	v.modelsToPluginAPI([]Model{
		{ID: "deepseek-v4-flash-free", Credits: "0.00X"},
		{ID: "plain"},
	})
	credits := v.ModelCredits()
	if got := credits["deepseek-v4-flash-free"]; got != "0.00X" {
		t.Fatalf("倍率 = %q, want 0.00X", got)
	}
	if _, exists := credits["plain"]; exists {
		t.Fatal("没有倍率的模型不应进表")
	}
}
