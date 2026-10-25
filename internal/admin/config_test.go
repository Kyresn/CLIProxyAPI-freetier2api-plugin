package admin

import "testing"

func TestDecodeBlockListsAndUnknownKeys(t *testing.T) {
	raw := []byte(`
enabled: true
priority: 1
enabled_realms:
  - cn
  - global
extra_models: [glm-5.2, qwen-max]
disabled_models:
  - cn:glm-5.2
  - qwen-3.5-plus
auto_checkin: false
vendor_specific_key: whatever
`)
	cfg, err := DecodePluginConfig(raw, DefaultPluginConfig())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(cfg.EnabledRealms) != 2 || cfg.EnabledRealms[0] != "cn" || cfg.EnabledRealms[1] != "global" {
		t.Fatalf("realms = %#v", cfg.EnabledRealms)
	}
	if len(cfg.ExtraModels) != 2 || cfg.ExtraModels[0] != "glm-5.2" {
		t.Fatalf("extra models = %#v", cfg.ExtraModels)
	}
	if len(cfg.DisabledModels) != 2 {
		t.Fatalf("disabled models = %#v", cfg.DisabledModels)
	}
	if cfg.AutoCheckin {
		t.Fatal("auto_checkin should be false")
	}
	if cfg.Raw["vendor_specific_key"] != "whatever" {
		t.Fatalf("raw map did not keep unknown key: %#v", cfg.Raw)
	}
}

func TestDecodeValidation(t *testing.T) {
	for _, raw := range []string{
		"enabled_realms: mars\n",
		"log_level: verbose\n",
		"auto_checkin_at: 25:00\n",
		"prompt_mode: overwrite\n",
		"model_name_tag: inline\n",
	} {
		if _, err := DecodePluginConfig([]byte(raw), DefaultPluginConfig()); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

// TestDeprecatedPromptKeys 验证旧键名仍被解析成全局提示词配置。
func TestDeprecatedPromptKeys(t *testing.T) {
	cfg, err := DecodePluginConfig([]byte("workbuddy_prompt_mode: append\nworkbuddy_prompt_file: /tmp/p.md\n"), DefaultPluginConfig())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.PromptMode != PromptModeAppend {
		t.Fatalf("prompt mode = %q", cfg.PromptMode)
	}
	if cfg.Raw[PromptFileKey] != "/tmp/p.md" {
		t.Fatalf("prompt file = %q", cfg.Raw[PromptFileKey])
	}
}

func TestParseClock(t *testing.T) {
	hour, minute, err := ParseClock("09:05")
	if err != nil || hour != 9 || minute != 5 {
		t.Fatalf("ParseClock = %d,%d,%v", hour, minute, err)
	}
}

func TestDefaultStateDirHonoursEnv(t *testing.T) {
	t.Setenv(StateDirEnvOverride, "/tmp/custom-state")
	if got := DefaultStateDir(); got != "/tmp/custom-state" {
		t.Fatalf("state dir = %q", got)
	}
}

// TestNormalizeModelNameTag 锁住渠道标识模式的取值集合。
func TestNormalizeModelNameTag(t *testing.T) {
	valid := map[string]string{
		"":          ModelNameTagOff,
		"off":       ModelNameTagOff,
		" PREFIX ":  ModelNameTagPrefix,
		"suffix":    ModelNameTagSuffix,
		" SUFFIX  ": ModelNameTagSuffix,
	}
	for raw, want := range valid {
		got, err := NormalizeModelNameTag(raw)
		if err != nil || got != want {
			t.Errorf("NormalizeModelNameTag(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := NormalizeModelNameTag("inline"); err == nil {
		t.Fatal("unsupported tag must be an error")
	}
}

// TestDecodeModelNameTag 验证 YAML 键解析与生效优先级：页面覆盖 > YAML > off。
func TestDecodeModelNameTag(t *testing.T) {
	cfg, err := DecodePluginConfig([]byte("model_name_tag: Prefix\n"), DefaultPluginConfig())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if cfg.ModelNameTag != ModelNameTagPrefix {
		t.Fatalf("model name tag = %q", cfg.ModelNameTag)
	}
	if got := EffectiveModelNameTag(cfg, "suffix"); got != ModelNameTagSuffix {
		t.Fatalf("page override = %q", got)
	}
	if got := EffectiveModelNameTag(cfg, " "); got != ModelNameTagPrefix {
		t.Fatalf("yaml fallback = %q", got)
	}
	if got := EffectiveModelNameTag(PluginConfig{}, ""); got != ModelNameTagOff {
		t.Fatalf("default = %q", got)
	}
}
