package minimaxcode

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// Model 是 MiniMax Code 支持的模型元数据。
type Model struct {
	ID            string
	Name          string
	Description   string
	ContextWindow int64
	MaxOutput     int64
	SupportsTools bool
	IsReasoning   bool
	UpstreamModel string
	Variant       string
}

// bundledModels 返回 MiniMax Code 的内置模型清单（探测失败时的兜底）。
func bundledModels() []Model {
	return []Model{
		{
			ID:            "minimax-agent",
			Name:          "MiniMax Agent",
			Description:   "通用 Agent，自动规划并调用工具",
			ContextWindow: 128000,
			MaxOutput:     8192,
			SupportsTools: true,
			UpstreamModel: "",
		},
		{
			ID:            "minimax-m3",
			Name:          "MiniMax M3",
			Description:   "对话模式，响应更快",
			ContextWindow: 1000000,
			MaxOutput:     16384,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M3",
		},
		{
			ID:            "minimax-m3-thinking",
			Name:          "MiniMax M3 Thinking",
			Description:   "深度思考模式，推理内容走 reasoning_content",
			ContextWindow: 1000000,
			MaxOutput:     16384,
			SupportsTools: true,
			IsReasoning:   true,
			UpstreamModel: "MiniMax-M3",
			Variant:       "thinking",
		},
		{
			ID:            "minimax-m3.1-flash",
			Name:          "MiniMax M3.1 Flash",
			Description:   "新一代 Flash 模型，512K 上下文，响应更快",
			ContextWindow: 512000,
			MaxOutput:     16384,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M3.1-Flash",
		},
		{
			ID:            "minimax-m3.1-flash-thinking",
			Name:          "MiniMax M3.1 Flash Thinking",
			Description:   "M3.1 Flash 的深度思考模式",
			ContextWindow: 512000,
			MaxOutput:     16384,
			SupportsTools: true,
			IsReasoning:   true,
			UpstreamModel: "MiniMax-M3.1-Flash",
			Variant:       "thinking",
		},
		{
			ID:            "minimax-m2.7",
			Name:          "MiniMax M2.7",
			Description:   "上一代对话模型",
			ContextWindow: 128000,
			MaxOutput:     8192,
			SupportsTools: true,
			UpstreamModel: "MiniMax-M2.7",
		},
	}
}

// FindModel 根据客户端请求的模型 ID 查找模型配置。
func findModel(modelID string) (Model, bool) {
	clean := strings.ToLower(strings.TrimSpace(modelID))
	// 如果带有前缀，剥离前缀
	if idx := strings.Index(clean, ":"); idx >= 0 {
		clean = clean[idx+1:]
	}
	for _, m := range bundledModels() {
		if strings.EqualFold(m.ID, clean) {
			return m, true
		}
	}
	// 默认回落到 minimax-agent
	return bundledModels()[0], false
}

// FetchModels 返回供宿主注册的模型清单。
func FetchModels(ctx context.Context, region Region) ([]Model, error) {
	_ = ctx
	_ = region
	return bundledModels(), nil
}

// 本文件实现「按账号工作区解析上游模型 ID」。
//
// 为什么需要它：上游会改名/下线模型（实测 2026-10-25：订阅工作区里的
// M3.1-Flash 已从 MiniMax-M3.1-Flash-Preview 更名为 MiniMax-M3.1-Flash），
// 而内置目录是编译期固化的——继续发旧 ID 会被 Agent 会话以
// 1406010011 "explicit model is not registered" 拒绝。
// 权威来源是 account 域的 /config（签名请求），它按账号列出当前可用的
// model_id；解析失败时回落内置目录（旧行为），不拖垮对话。

const (
	workspaceModelsTTL        = 10 * time.Minute
	workspaceModelsFailWindow = time.Minute
)

// workspaceModel 是工作区配置里的一个可用模型。
type workspaceModel struct {
	ModelID      string
	DisplayName  string
	ContextLimit int64
	Variants     []string
}

type workspaceCacheEntry struct {
	models    []workspaceModel
	fetchedAt time.Time
	lastFail  time.Time
}

// workspaceCache 是账号工作区目录缓存。
//
// 必须挂在 adapter 实例上而不是包级变量：同进程的多个 App 实例各自独立，
// 缓存生命周期跟着实例走（与 trae/qoder/workbuddy 的目录缓存一致）。零值可用。
type workspaceCache struct {
	mu      sync.Mutex
	entries map[string]workspaceCacheEntry
}

// models 返回该账号工作区当前可用的模型清单（带 TTL 与失败冷却）。
//
// 失败时沿用上一次成功的结果；从未成功过则返回 nil，调用方回落内置目录。
func (c *workspaceCache) models(ctx context.Context, cred *Credential, baseURLOverride string) []workspaceModel {
	if cred == nil {
		return nil
	}
	key := workspaceAccountKey(cred, baseURLOverride)
	if c != nil {
		c.mu.Lock()
		entry, okEntry := c.entries[key]
		c.mu.Unlock()
		if okEntry {
			if time.Since(entry.fetchedAt) < workspaceModelsTTL {
				return entry.models
			}
			// 失败冷却期内不重复打上游：额度/模型列表逐账号刷新时，
			// 上游抖动不该让每条消息都多付一次超时。
			if !entry.lastFail.IsZero() && time.Since(entry.lastFail) < workspaceModelsFailWindow {
				return entry.models
			}
		}
	}

	models, errFetch := fetchWorkspaceModels(ctx, cred, baseURLOverride)
	if c == nil {
		if errFetch != nil {
			return nil
		}
		return models
	}

	now := time.Now()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = map[string]workspaceCacheEntry{}
	}
	entry := c.entries[key]
	if errFetch != nil || len(models) == 0 {
		logger.Debug("minimax workspace models: fetch failed for %s: %v", key, errFetch)
		entry.lastFail = now
		c.entries[key] = entry
		return entry.models
	}
	for cachedKey, cached := range c.entries {
		if cachedKey == key {
			continue
		}
		if now.Sub(cached.fetchedAt) >= workspaceModelsTTL &&
			(cached.lastFail.IsZero() || now.Sub(cached.lastFail) >= workspaceModelsFailWindow) {
			delete(c.entries, cachedKey)
		}
	}
	c.entries[key] = workspaceCacheEntry{models: models, fetchedAt: now}
	return models
}

// workspaceAccountKey 用稳定账号身份做缓存键（不把令牌原文留在 map 键里），
// 并带上 baseURL：切换镜像后不会命中旧站点的目录。
func workspaceAccountKey(cred *Credential, baseURLOverride string) string {
	account := core.FirstNonEmpty(cred.UserID, cred.Email)
	if account == "" {
		sum := sha256.Sum256([]byte(cred.AccessToken))
		account = hex.EncodeToString(sum[:8])
	}
	return account + "|" + AgentHostFor(cred.Region, baseURLOverride)
}

// fetchWorkspaceModels 拉取并解析 /config 的 models 列表（签名 account 域请求）。
func fetchWorkspaceModels(ctx context.Context, cred *Credential, baseURLOverride string) ([]workspaceModel, error) {
	base := AgentHostFor(cred.Region, baseURLOverride)
	target := buildAgentTarget(cred, base, EpConfig, http.MethodGet, "", false)
	resp, errDo := doRequest(ctx, cred, target, base, 20*time.Second)
	if errDo != nil {
		return nil, errDo
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, core.NewPluginError("minimax_config_unavailable",
			"config endpoint returned "+resp.Status, resp.StatusCode)
	}
	var body struct {
		Models []struct {
			ModelID           string   `json:"model_id"`
			DisplayName       string   `json:"display_name"`
			ContextLimit      int64    `json:"context_limit"`
			SupportedVariants []string `json:"supported_variants"`
		} `json:"models"`
	}
	if errDecode := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&body); errDecode != nil {
		return nil, errDecode
	}
	models := make([]workspaceModel, 0, len(body.Models))
	for _, item := range body.Models {
		id := strings.TrimSpace(item.ModelID)
		if id == "" {
			continue
		}
		models = append(models, workspaceModel{
			ModelID:      id,
			DisplayName:  item.DisplayName,
			ContextLimit: item.ContextLimit,
			Variants:     item.SupportedVariants,
		})
	}
	return models, nil
}

// mergeWorkspaceModels 以内置目录为骨架，补上工作区里新增的模型。
//
// 内置目录里的同名模型不替换元数据：工作区只声明了 ID/展示名/上下文，
// 直接覆盖会丢掉 MaxOutput、工具能力与 -thinking 变体。工作区显式声明
// thinking 的新模型额外注册一个 <id>-thinking 入口，与内置清单命名一致。
func mergeWorkspaceModels(static []pluginapi.ModelInfo, workspace []workspaceModel) []pluginapi.ModelInfo {
	if len(workspace) == 0 {
		return static
	}
	out := make([]pluginapi.ModelInfo, 0, len(static)+len(workspace))
	out = append(out, static...)
	present := make(map[string]bool, len(out))
	for _, model := range out {
		present[strings.ToLower(strings.TrimSpace(model.ID))] = true
	}
	for _, item := range workspace {
		id := strings.ToLower(strings.TrimSpace(item.ModelID))
		if id == "" || present[id] {
			continue
		}
		present[id] = true
		name := core.FirstNonEmpty(strings.TrimSpace(item.DisplayName), item.ModelID)
		out = append(out, workspaceModelInfo(id, name, "", item.ContextLimit))
		if !workspaceDeclaresVariant(item, "thinking") {
			continue
		}
		thinkingID := id + "-thinking"
		if present[thinkingID] {
			continue
		}
		present[thinkingID] = true
		out = append(out, workspaceModelInfo(thinkingID, name+" Thinking",
			"深度思考模式，推理内容走 reasoning_content", item.ContextLimit))
	}
	return out
}

// workspaceModelInfo 把工作区模型补成完整的对外条目。
//
// /config 只给 ID/展示名/上下文，输出上限与工具能力缺失；沿用内置目录的
// 兜底值，避免注册后 MaxCompletionTokens=0、SupportsTools=false 与同包
// 其它模型描述不一致。
func workspaceModelInfo(id, displayName, description string, contextLimit int64) pluginapi.ModelInfo {
	if contextLimit <= 0 {
		contextLimit = core.DefaultContextWindowTokens
	}
	return core.ModelInfoToPluginAPI("", core.ModelDescriptor{
		ID:              id,
		Name:            displayName,
		Description:     description,
		ContextLength:   contextLimit,
		MaxOutputTokens: core.DefaultMaxOutputTokens,
		SupportsTools:   true,
	})
}

// resolveWorkspaceModel 把客户端请求的模型解析成工作区当前注册的 model_id。
//
// 匹配顺序：请求 ID（去 -thinking 后缀再试）→ 内置目录的 UpstreamModel。
// 命中工作区模型时同时校对 variant（工作区声明支持才带上）。
// 未命中返回 ok=false，调用方沿用内置目录的 UpstreamModel（旧行为）。
func resolveWorkspaceModel(ctx context.Context, cache *workspaceCache, cred *Credential, baseURLOverride, modelID string, bundled Model) (string, string, bool) {
	models := cache.models(ctx, cred, baseURLOverride)
	if len(models) == 0 {
		return "", "", false
	}
	variant := bundled.Variant
	base := normalizeModelProbe(modelID)
	if strings.HasSuffix(base, "thinking") {
		// thinking 变体在请求侧以后缀表达；工作区侧以 supported_variants 表达。
		if variant == "" {
			variant = "thinking"
		}
		base = strings.TrimSuffix(strings.TrimSuffix(base, "thinking"), "-")
	}
	for _, item := range models {
		if !strings.EqualFold(item.ModelID, base) && !strings.EqualFold(item.ModelID, bundled.UpstreamModel) {
			continue
		}
		resolved := item.ModelID
		if variant != "" && !workspaceSupportsVariant(item, variant) {
			// 工作区明确不支持该变体：按无变体处理（宁可直接出话也不发非法参数）。
			variant = ""
		}
		return resolved, variant, true
	}
	return "", "", false
}

// workspaceDeclaresVariant 报告工作区是否**显式**声明了某变体。
func workspaceDeclaresVariant(item workspaceModel, variant string) bool {
	for _, declared := range item.Variants {
		if strings.EqualFold(strings.TrimSpace(declared), variant) {
			return true
		}
	}
	return false
}

// workspaceSupportsVariant 报告工作区模型是否支持某变体。
//
// 未声明 supported_variants 视为支持：内置 -thinking 一直携带 variant，
// 工作区没给出否定证据时不擅自关掉（沿用旧行为）。
func workspaceSupportsVariant(item workspaceModel, variant string) bool {
	return len(item.Variants) == 0 || workspaceDeclaresVariant(item, variant)
}

// normalizeModelProbe 归一化请求 ID 用于工作区匹配：剥供应商前缀、小写。
func normalizeModelProbe(modelID string) string {
	clean := strings.ToLower(strings.TrimSpace(modelID))
	if idx := strings.Index(clean, ":"); idx >= 0 {
		clean = clean[idx+1:]
	}
	return clean
}
