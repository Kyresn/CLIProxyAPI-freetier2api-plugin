package workbuddy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
	"freetier2api-plugin/internal/core"
	"freetier2api-plugin/internal/logger"
)

// 本文件实现成长任务的列表 / 报名 / 领奖接口。
//
// 三条路径的形态差异是这里最容易踩的坑：
//   - 列表与报名在对话域（chatBase），带业务信封；
//   - **领奖在 Web 域**（webBase），task_code 在**路径**里、无 body、带 x-client-platform: web。
//     对话域的 /tasks/reward/claim 路径**不存在**，恒返回 400 "task not completed"，
//     这是早期领奖全部失败的真实原因。
//   - 小程序口径的任务码要走带 X-Client-Platform: miniprogram 的同名接口。

// Task 是一条成长任务的状态。
type Task struct {
	// Code 是任务标识。
	Code string
	// Title 是任务名。
	Title string
	// Current / Target 是进度。
	Current int64
	Target  int64
	// Claimed 表示奖励已领取。
	Claimed bool
	// Locked 表示任务处于锁定链上（如每日零点解锁的连续任务）。
	Locked bool
	// AcceptStatus 是报名状态：not_accepted / accepted / claimed。
	AcceptStatus string
}

// Claimable 报告任务是否已达标且未领奖。
//
// 本地推算而非依赖上游字段：上游的 claimable 标记并不总是可信
// （部分形态不下发该字段）。
func (t Task) Claimable() bool {
	if t.Claimed || t.Locked {
		return false
	}
	return t.Target > 0 && t.Current >= t.Target
}

// Done 报告任务是否已完成（无论是否领奖）。
func (t Task) Done() bool { return t.Target > 0 && t.Current >= t.Target }

// growthTaskEntry 是任务条目的原始形态。
//
// progress 字段有两种形态：对象 {current,target} 与平铺的 current/target，
// 两种都要兼容（上游不同接口形态不一致）。
type growthTaskEntry struct {
	TaskCode     string          `json:"task_code"`
	TaskCodeAlt  string          `json:"taskCode"`
	Title        string          `json:"title"`
	TitleAlt     string          `json:"task_name"`
	Claimed      bool            `json:"claimed"`
	Locked       bool            `json:"locked"`
	AcceptStatus string          `json:"accept_status"`
	Progress     json.RawMessage `json:"progress"`
	Current      int64           `json:"current"`
	Target       int64           `json:"target"`
}

// toTask 把原始条目归一化。
func (e growthTaskEntry) toTask() Task {
	task := Task{
		Code:         core.FirstNonEmpty(e.TaskCode, e.TaskCodeAlt),
		Title:        core.FirstNonEmpty(e.Title, e.TitleAlt),
		Claimed:      e.Claimed,
		Locked:       e.Locked,
		AcceptStatus: strings.TrimSpace(e.AcceptStatus),
		Current:      e.Current,
		Target:       e.Target,
	}
	// progress 为对象形态时以它为准。
	if len(e.Progress) > 0 {
		var nested struct {
			Current int64 `json:"current"`
			Target  int64 `json:"target"`
		}
		if errUnmarshal := json.Unmarshal(e.Progress, &nested); errUnmarshal == nil {
			if nested.Current != 0 || nested.Target != 0 {
				task.Current = nested.Current
				task.Target = nested.Target
			}
		}
	}
	return task
}

// ListTasks 拉取成长任务列表（默认口径）。
func (c *Client) ListTasks(ctx context.Context, cred *Credential) ([]Task, error) {
	return c.listTasks(ctx, cred, false)
}

// ListTasksMP 拉取小程序口径的任务列表。
//
// 小程序列表是默认列表的**超集**（含 mp 专属任务），合并时必须按任务码去重。
func (c *Client) ListTasksMP(ctx context.Context, cred *Credential) ([]Task, error) {
	return c.listTasks(ctx, cred, true)
}

func (c *Client) listTasks(ctx context.Context, cred *Credential, mp bool) ([]Task, error) {
	if cred.Realm().IsGlobal() {
		// global 账号没有成长任务体系。
		return nil, nil
	}
	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodGet, endpoints.ChatBase+growthTasksPath, nil)
	if errReq != nil {
		return nil, fmt.Errorf("build tasks request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	if mp {
		applyMPHeaders(req)
	}

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return nil, errCall
	}
	return parseGrowthTasks(data)
}

// parseGrowthTasks 解析任务列表响应。
//
// data 可能是数组，也可能是带 tasks 键的对象。
func parseGrowthTasks(data json.RawMessage) ([]Task, error) {
	var entries []growthTaskEntry
	if errUnmarshal := json.Unmarshal(data, &entries); errUnmarshal != nil {
		var wrapper struct {
			Tasks []growthTaskEntry `json:"tasks"`
		}
		if errWrapper := json.Unmarshal(data, &wrapper); errWrapper != nil {
			return nil, fmt.Errorf("decode tasks: %w", errUnmarshal)
		}
		entries = wrapper.Tasks
	}
	out := make([]Task, 0, len(entries))
	for _, entry := range entries {
		task := entry.toTask()
		if task.Code == "" {
			continue
		}
		out = append(out, task)
	}
	return out, nil
}

// AcceptTasks 批量报名任务。
//
// 报名本身**不产生进度**（进度靠行为事件点亮），它只是让状态机从
// not_accepted 走到 accepted。但少了它，部分任务的事件不会被计分。
func (c *Client) AcceptTasks(ctx context.Context, cred *Credential, codes []string) error {
	return c.acceptTasks(ctx, cred, codes, false)
}

func (c *Client) acceptTasks(ctx context.Context, cred *Credential, codes []string, mp bool) error {
	if len(codes) == 0 {
		return nil
	}
	body := map[string]any{"task_codes": codes}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return fmt.Errorf("encode accept request: %w", errEncode)
	}
	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+growthTasksAcceptPath, strings.NewReader(string(encoded)))
	if errReq != nil {
		return fmt.Errorf("build accept request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	if mp {
		applyMPHeaders(req)
	}
	_, errCall := c.doJSON(req)
	return errCall
}

// ClaimReward 领取任务奖励（Web 域）。
//
// 已领取时返回 (0, 0, nil)：上游用 already_claimed 标记表达幂等，
// 把它当错误会让自动任务每次重复执行都报错。
func (c *Client) ClaimReward(ctx context.Context, cred *Credential, code string) (credit, energy int64, err error) {
	if strings.TrimSpace(code) == "" {
		return 0, 0, fmt.Errorf("task code is required")
	}
	endpoints := GetEndpoints(RegionCN)
	claimURL := endpoints.WebBase + growthTasksClaimBase + "/" + url.PathEscape(strings.TrimSpace(code)) + "/claim"
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
	if errReq != nil {
		return 0, 0, fmt.Errorf("build claim request: %w", errReq)
	}
	// 领奖走 web 指纹：Referer 指向成长中心页。
	applyWebHeaders(req, cred, endpoints.WebBase+"/profile/growth-center")

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return 0, 0, errCall
	}
	return parseClaimResult(data)
}

// ClaimRewardMP 用小程序口径领奖。
//
// 部分任务/租户在对话域返回 400，此时降级到 Web 域重试。
func (c *Client) ClaimRewardMP(ctx context.Context, cred *Credential, code string) (credit, energy int64, err error) {
	if strings.TrimSpace(code) == "" {
		return 0, 0, fmt.Errorf("task code is required")
	}
	endpoints := GetEndpoints(cred.Realm())
	claimURL := endpoints.ChatBase + growthTasksClaimBase + "/" + url.PathEscape(strings.TrimSpace(code)) + "/claim"
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, claimURL, nil)
	if errReq != nil {
		return 0, 0, fmt.Errorf("build mp claim request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	req.Header.Set("X-Client-Platform", "miniprogram")

	data, errCall := c.doJSON(req)
	if errCall != nil {
		// 对话域失败时降级到 Web 域（上游两个口径的可用性不一致）。
		return c.ClaimReward(ctx, cred, code)
	}
	return parseClaimResult(data)
}

// parseClaimResult 解析领奖响应。
//
// 领奖是有副作用的请求。响应无法解析时不能把它当作「已领取」处理，否则
// HTML 错误页、截断 JSON 等异常会被静默报告成成功且奖励为 0。用指针字段
// 一次解码即可同时得到值与「字段是否存在」，无需先解成 map 再解一次 struct。
//
// 必须至少出现一个领奖契约字段（already_claimed / credit / energy）：
// 空对象、null、完全陌生的 JSON 对象都不是成功响应。
func parseClaimResult(data json.RawMessage) (credit, energy int64, err error) {
	var payload struct {
		AlreadyClaimed *bool  `json:"already_claimed"`
		Credit         *int64 `json:"credit"`
		Energy         *int64 `json:"energy"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return 0, 0, fmt.Errorf("decode claim result: %w", errUnmarshal)
	}
	if payload.AlreadyClaimed == nil && payload.Credit == nil && payload.Energy == nil {
		return 0, 0, fmt.Errorf("decode claim result: missing reward fields")
	}
	if payload.AlreadyClaimed != nil && *payload.AlreadyClaimed {
		return 0, 0, nil
	}
	if payload.Credit != nil {
		credit = *payload.Credit
	}
	if payload.Energy != nil {
		energy = *payload.Energy
	}
	return credit, energy, nil
}

// IsMPTaskCode 报告任务码是否属于小程序口径。
//
// 小程序专属任务不会出现在默认列表里，必须走 mp 口径查询与领奖。
func IsMPTaskCode(code string) bool {
	return mpTaskCodes[strings.TrimSpace(code)]
}

// mpTaskCodes 是小程序口径专属任务码。
//
// 说明：Tasks_4..7 处于「每日零点解锁一环」的锁定链上，
// 其判据为预置推测（解锁后需实测校正）；locked 状态的任务永不入队
// （locked 时报名不落账）。
var mpTaskCodes = map[string]bool{
	"school_season":      true,
	"Sequential_Tasks_1": true,
	"Sequential_Tasks_2": true,
	"Sequential_Tasks_3": true,
	"Sequential_Tasks_4": true,
	"Sequential_Tasks_5": true,
	"Sequential_Tasks_6": true,
	"Sequential_Tasks_7": true,
}

// 本文件实现需要真实对话的辅助方法。
//
// 部分任务（专家使用、技能尝鲜、夜猫子、桌面开学季）的判据要求
// **真实对话产生的服务端 requestId** —— 自造 UUID 不会被计分。
// 因此这些方法会发一次真实对话，并从 SSE 流里抓出上游返回的 id。

// serverRequestIDRe 校验上游返回的 id 形态（32 位 hex，可带 cmb- 前缀）。
var serverRequestIDRe = regexp.MustCompile(`^(cmb-)?[0-9a-f]{32}$`)

// desktopChatSystemPrompt 是辅助对话使用的 system 提示词。
const desktopChatSystemPrompt = "You are a helpful assistant. 当前处于中文环境，使用简体中文回答。"

// desktopChatUserPrompt 是辅助对话的用户消息（极短，省额度）。
const desktopChatUserPrompt = "1+1等于几？直接回答。"

// DesktopChat 发一次真实对话但不关心 requestId（夜猫子等场景）。
func (c *Client) DesktopChat(ctx context.Context, cred *Credential, expertID, modelID, modelName, conversationID string) error {
	_, errChat := c.desktopChatInternal(ctx, cred, expertID, modelID, modelName, conversationID, false)
	return errChat
}

// DesktopChatWithRequestID 发一次真实对话并返回上游的服务端 requestId。
//
// 用于需要「真实对话回执」的任务（专家使用、技能尝鲜、桌面开学季）。
func (c *Client) DesktopChatWithRequestID(ctx context.Context, cred *Credential, expertID, modelID, conversationID string) (string, error) {
	return c.desktopChatInternal(ctx, cred, expertID, modelID, modelID, conversationID, true)
}

// desktopChatInternal 是辅助对话的实现。
//
// needRequestID 为 true 时，若流里抓不到合法的服务端 id 会返回错误——
// 因为自造 id 不会被计分，静默继续只会让任务白跑。
func (c *Client) desktopChatInternal(ctx context.Context, cred *Credential, expertID, modelID, modelName, conversationID string, needRequestID bool) (string, error) {
	if strings.TrimSpace(modelID) == "" {
		modelID = "fast-model"
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = modelID
	}
	body := map[string]any{
		"model": modelID,
		"messages": []any{
			map[string]any{"role": "system", "content": desktopChatSystemPrompt},
			map[string]any{"role": "user", "content": desktopChatUserPrompt},
		},
		"agent":       "cli",
		"temperature": 1,
		"stream":      true,
		"stream_options": map[string]any{
			"include_usage": true,
		},
	}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return "", fmt.Errorf("encode desktop chat: %w", errEncode)
	}

	endpoints := GetEndpoints(cred.Realm())
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+chatCompletionsPath, bytes.NewReader(encoded))
	if errReq != nil {
		return "", fmt.Errorf("build desktop chat request: %w", errReq)
	}
	c.applyDesktopChatHeaders(req, cred, expertID, conversationID, modelID)

	resp, errDo := c.chatClient(chatHeaderTimeout).Do(req)
	if errDo != nil {
		return "", transientError(errDo)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode >= 400 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
		return "", enrichClassifyError(resp.StatusCode, string(raw), resp.Header)
	}

	requestID, errRead := scanServerRequestID(resp.Body)
	if errRead != nil && needRequestID {
		return "", errRead
	}
	return requestID, nil
}

// applyDesktopChatHeaders 写入桌面辅助对话的请求头。
//
// 与常规对话的差别：带 X-Agent-Intent/Type 与桌面 UA，
// 这样上游把它归到「桌面客户端」的行为统计里（部分任务按此判定）。
func (c *Client) applyDesktopChatHeaders(req *http.Request, cred *Credential, expertID, conversationID, modelID string) {
	applyCommonHeaders(req, c, cred)
	applyAuthHeaders(req, cred)

	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", desktopUA())
	req.Header.Set("X-Domain", strings.TrimSuffix(GetEndpoints(cred.Realm()).ChatBase, "/"))
	req.Header.Set("X-Product", "SaaS")
	req.Header.Set("X-Agent-Intent", "craft")
	req.Header.Set("X-Agent-Type", "main")
	req.Header.Set("X-IDE-Name", "WorkBuddy")
	req.Header.Set("X-IDE-Type", "WorkBuddy")
	req.Header.Set("X-IDE-Version", DesktopClientVersion)
	if strings.TrimSpace(expertID) != "" {
		req.Header.Set("X-Expert-Id", strings.TrimSpace(expertID))
	}
	if strings.TrimSpace(conversationID) != "" {
		req.Header.Set("X-Conversation-ID", strings.TrimSpace(conversationID))
	}
	req.Header.Set("X-Request-ID", c.rt.deriveID(cred.UIDValue(), "req")+newHexID()[:6])
}

// scanServerRequestID 从 SSE 流里抓出上游的服务端 requestId。
//
// 判据：第一个形如 "id":"<32hex>"（可带 cmb- 前缀）的值。
// 抓不到返回错误——调用方需要它来构造被计分的事件链。
func scanServerRequestID(reader io.Reader) (string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 8*1024), 1<<20)
	deadline := time.Now().Add(30 * time.Second)

	for scanner.Scan() {
		if time.Now().After(deadline) {
			break
		}
		line := scanner.Text()
		index := strings.Index(line, `"id":"`)
		if index < 0 {
			continue
		}
		rest := line[index+len(`"id":"`):]
		end := strings.IndexByte(rest, '"')
		if end < 0 {
			continue
		}
		candidate := rest[:end]
		if serverRequestIDRe.MatchString(candidate) {
			// 上游可能返回 cmb- 前缀形态，事件链里用去掉前缀的纯 hex。
			return strings.TrimPrefix(candidate, "cmb-"), nil
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return "", fmt.Errorf("read desktop chat stream: %w", errScan)
	}
	return "", fmt.Errorf("no server request id found in chat stream (self-generated ids are not scored)")
}

// DesktopAppearanceSet 设置主题外观。
//
// 注意：**单发 set 不计分**，必须叠加 appearance_skin_apply 事件（见任务动作表）。
func (c *Client) DesktopAppearanceSet(ctx context.Context, cred *Credential, resourceKey string) error {
	endpoints := GetEndpoints(cred.Realm())
	body := map[string]any{"kind": "theme", "resource_key": strings.TrimSpace(resourceKey)}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return fmt.Errorf("encode appearance request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+appearanceSetPath, bytes.NewReader(encoded))
	if errReq != nil {
		return fmt.Errorf("build appearance request: %w", errReq)
	}
	c.applyDesktopHeaders(req, cred)
	_, errCall := c.doJSON(req)
	return errCall
}

// MarketExpert 是专家市场里的一位专家。
type MarketExpert struct {
	ID         string
	Name       string
	Profession string
	Type       string
	Version    string
	Category   string
}

// MarketExpertList 拉取专家市场列表。
//
// expertType 为 "agent" 取单专家、"team" 取专家团。
// **expert_id 必须是市场里的真实值**：自造 id 不会计入任务进度。
func (c *Client) MarketExpertList(ctx context.Context, cred *Credential, expertType string) ([]MarketExpert, error) {
	endpoints := GetEndpoints(cred.Realm())
	body := map[string]any{
		"page": 1, "page_size": 20, "sort_by": "reco_rank", "sort_order": "desc",
	}
	if trimmed := strings.TrimSpace(expertType); trimmed != "" {
		body["expert_type"] = trimmed
	}
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, fmt.Errorf("encode expert list request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost,
		endpoints.ChatBase+expertMarketListPath, bytes.NewReader(encoded))
	if errReq != nil {
		return nil, fmt.Errorf("build expert list request: %w", errReq)
	}
	c.applyDesktopHeaders(req, cred)

	data, errCall := c.doJSON(req)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Experts []struct {
			ExpertID      string   `json:"expert_id"`
			ExpertType    string   `json:"expert_type"`
			DisplayNameZH string   `json:"display_name_zh"`
			ProfessionZH  string   `json:"profession_zh"`
			Version       string   `json:"version"`
			Categories    []string `json:"categories"`
		} `json:"experts"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode expert list: %w", errUnmarshal)
	}
	out := make([]MarketExpert, 0, len(payload.Experts))
	for _, item := range payload.Experts {
		id := strings.TrimSpace(item.ExpertID)
		if id == "" {
			// 空 id 服务端不入账，跳过。
			continue
		}
		expert := MarketExpert{
			ID:         id,
			Name:       strings.TrimSpace(item.DisplayNameZH),
			Profession: strings.TrimSpace(item.ProfessionZH),
			Type:       strings.TrimSpace(item.ExpertType),
			Version:    strings.TrimSpace(item.Version),
		}
		if len(item.Categories) > 0 {
			expert.Category = strings.TrimSpace(item.Categories[0])
		}
		out = append(out, expert)
	}
	return out, nil
}

// 本文件实现成长体系的活动接口：连登、旅行、夜猫子、开学季、礼包与补偿。
//
// 这些接口全部挂在对话域或计费域，形态各异（信封嵌套、幂等码、业务错误表达状态），
// 因此逐个封装成方法，让上层逻辑只管业务语义。

// Streak 是连登状态。
type Streak struct {
	// Days 是连续登录天数。
	Days int64
	// MakeupCards 是补签卡余额。
	MakeupCards int64
	// Tiers 是各兑换档位。
	Tiers []StreakTier
}

// StreakTier 是一个兑换档位。
type StreakTier struct {
	// Tier 是档位标识（7d / 14d / 28d）。
	Tier string
	// Status 是状态：locked / claimed / 空（可兑换）。
	Status string
}

// GrowthStreak 查询连登状态。
func (c *Client) GrowthStreak(ctx context.Context, cred *Credential) (*Streak, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthStreakPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Streak struct {
			Days int64 `json:"days"`
		} `json:"streak"`
		MakeupCards struct {
			Balance int64 `json:"balance"`
		} `json:"makeup_cards"`
		RedemptionStatus struct {
			// 档位状态按 tier_7d_status / tier_14d_status / tier_28d_status 三个键给出。
			Tier7d  string `json:"tier_7d_status"`
			Tier14d string `json:"tier_14d_status"`
			Tier28d string `json:"tier_28d_status"`
		} `json:"redemption_status"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode streak: %w", errUnmarshal)
	}
	streak := &Streak{Days: payload.Streak.Days, MakeupCards: payload.MakeupCards.Balance}
	for _, item := range []struct {
		tier   string
		status string
	}{
		{"7d", payload.RedemptionStatus.Tier7d},
		{"14d", payload.RedemptionStatus.Tier14d},
		{"28d", payload.RedemptionStatus.Tier28d},
	} {
		if strings.TrimSpace(item.status) == "" {
			continue
		}
		streak.Tiers = append(streak.Tiers, StreakTier{Tier: item.tier, Status: item.status})
	}
	return streak, nil
}

// GrowthRedeemTier 兑换连登档位。
//
// 未解锁时上游返回 403「连续登录天数不足」，属预期结果，调用方静默跳过。
func (c *Client) GrowthRedeemTier(ctx context.Context, cred *Credential, tier string) error {
	clientToken := newHexID()
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthRedeemPath, map[string]any{
		"tier": strings.TrimSpace(tier), "client_token": clientToken,
	})
	return errCall
}

// LotteryChances 查询可用抽奖次数。
func (c *Client) LotteryChances(ctx context.Context, cred *Credential) (int, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthLotterySummary, nil)
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Chances int `json:"chances"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return 0, fmt.Errorf("decode lottery summary: %w", errUnmarshal)
	}
	return payload.Chances, nil
}

// LotteryDraw 执行一次抽奖。
func (c *Client) LotteryDraw(ctx context.Context, cred *Credential) (string, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthLotteryDraw, map[string]any{
		"client_token": newHexID(),
	})
	if errCall != nil {
		return "", errCall
	}
	return string(data), nil
}

// HeatmapYesterdayMissed 报告昨日是否漏签。
func (c *Client) HeatmapYesterdayMissed(ctx context.Context, cred *Credential) (bool, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthHeatmapPath, nil)
	if errCall != nil {
		return false, errCall
	}
	yesterday := time.Now().AddDate(0, 0, -1).In(cstZone).Format("2006-01-02")
	// 响应形态：cells 数组或日期为键的 map，两种都试。
	var wrapper struct {
		Cells []struct {
			Date  string `json:"date"`
			Score int64  `json:"score"`
		} `json:"cells"`
	}
	if errUnmarshal := json.Unmarshal(data, &wrapper); errUnmarshal == nil {
		for _, cell := range wrapper.Cells {
			if cell.Date == yesterday {
				return cell.Score == 0, nil
			}
		}
	}
	var byDate map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(data, &byDate); errUnmarshal == nil {
		if raw, okRaw := byDate[yesterday]; okRaw {
			var cell struct {
				Score int64 `json:"score"`
			}
			if errCell := json.Unmarshal(raw, &cell); errCell == nil {
				return cell.Score == 0, nil
			}
		}
	}
	return false, nil
}

// UseMakeupCard 使用补签卡补签指定日期。
func (c *Client) UseMakeupCard(ctx context.Context, cred *Credential, date string) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, growthMakeupCardsPath, map[string]any{
		"target_date": strings.TrimSpace(date),
	})
	return errCall
}

// cstZone 是上游自然日使用的时区（固定 UTC+8，不依赖系统 tzdata）。
var cstZone = time.FixedZone("CST", 8*3600)

// BuddyInfo 是猫档案。
type BuddyInfo struct {
	// HasBuddy 表示是否已领养。
	HasBuddy bool
}

// BuddyInfo 查询猫档案。
func (c *Client) BuddyInfo(ctx context.Context, cred *Credential) (*BuddyInfo, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, buddyInfoPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		Buddy json.RawMessage `json:"buddy"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode buddy info: %w", errUnmarshal)
	}
	trimmed := strings.TrimSpace(string(payload.Buddy))
	return &BuddyInfo{HasBuddy: trimmed != "" && trimmed != "null"}, nil
}

// BuddyAgreement 同意领养协议（幂等）。
func (c *Client) BuddyAgreement(ctx context.Context, cred *Credential) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyAgreementPath, map[string]any{"agree": true})
	return errCall
}

// BuddyFirst 领取第一只猫。
//
// 失败通常因为「当日活跃上报不足」（first_buddy 任务未完成），
// 这不是账号问题，调用方应视为「今天跳过」而不是错误。
func (c *Client) BuddyFirst(ctx context.Context, cred *Credential) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyFirstPath, map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.Credit, nil
}

// TravelStatus 是旅行状态。
type TravelStatus struct {
	// State 是状态：idle（可派出）/ traveling（在途）/ arrived（可领奖）。
	State string
	// RecordID 是本次旅行的记录标识（领奖必填，为 0 表示无可领）。
	RecordID int64
	// DailyLimitReached 表示今日已派出过。
	DailyLimitReached bool
}

// TravelStatus 查询旅行状态。
func (c *Client) TravelStatus(ctx context.Context, cred *Credential) (*TravelStatus, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, buddyTravelStatusPath, nil)
	if errCall != nil {
		return nil, errCall
	}
	var payload struct {
		State             string `json:"state"`
		Status            string `json:"status"`
		RecordID          int64  `json:"record_id"`
		DailyLimitReached bool   `json:"daily_limit_reached"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode travel status: %w", errUnmarshal)
	}
	return &TravelStatus{
		State:             core.FirstNonEmpty(payload.State, payload.Status),
		RecordID:          payload.RecordID,
		DailyLimitReached: payload.DailyLimitReached,
	}, nil
}

// TravelDepart 派猫出门旅行。
func (c *Client) TravelDepart(ctx context.Context, cred *Credential, locationID int) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyTravelDepartPath, map[string]any{
		"location_id": locationID,
	})
	return errCall
}

// TravelClaim 领取旅行奖励。
func (c *Client) TravelClaim(ctx context.Context, cred *Credential, recordID int64) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, buddyTravelClaimPath, map[string]any{
		"record_id": recordID,
	})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		RewardCredit int64 `json:"reward_credit"`
		Credit       int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return core.FirstNonZero(payload.RewardCredit, payload.Credit), nil
}

// BlackcatNeed 返回夜猫子任务的缺口次数。
func (c *Client) BlackcatNeed(ctx context.Context, cred *Credential) (int64, error) {
	tasks, errTasks := c.ListTasks(ctx, cred)
	if errTasks != nil {
		return 0, errTasks
	}
	for _, task := range tasks {
		if task.Code != "black_cat" {
			continue
		}
		if task.Claimed || task.Done() {
			return 0, nil
		}
		need := task.Target - task.Current
		if need < 0 {
			need = 0
		}
		return need, nil
	}
	return 0, nil
}

// 礼包与补偿的路径访问器（供上层遍历调用）。
func ClaimGiftPath() string         { return claimGiftPath }
func ClaimCompensationPath() string { return claimCompPath }

// ClaimBillingReward 领取计费域的一次性奖励（礼包 / 补偿）。
//
// 已领过时上游返回业务错误，调用方按「无新增」处理。
func (c *Client) ClaimBillingReward(ctx context.Context, cred *Credential, path string) (int64, error) {
	endpoints := GetEndpoints(cred.Realm())
	data, errCall := c.billingJSON(cred, endpoints.BillingBase+strings.TrimSpace(path), http.MethodPost, map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Credit int64 `json:"credit"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.Credit, nil
}

// SchoolStatus 是开学季活动状态。
type SchoolStatus struct {
	// InPeriod 表示当前是否在活动期内。
	InPeriod bool
	// Tasks 是活动任务。
	Tasks []SchoolTask
}

// SchoolTask 是一个开学季任务。
type SchoolTask struct {
	// Code 是任务标识。
	Code string
	// Status 是状态：pending / in_progress / claimed / completed。
	Status string
	// Progress / TargetCount 是进度。
	Progress    int64
	TargetCount int64
}

// SchoolTasks 查询开学季任务。
func (c *Client) SchoolTasks(ctx context.Context, cred *Credential) (*SchoolStatus, error) {
	endpoints := GetEndpoints(cred.Realm())
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, schoolBasePath+"/tasks", nil)
	if errCall != nil {
		return nil, errCall
	}
	_ = endpoints
	var payload struct {
		InPeriod bool `json:"in_period"`
		Tasks    []struct {
			Code        string `json:"task_code"`
			Status      string `json:"status"`
			Progress    int64  `json:"progress"`
			TargetCount int64  `json:"target_count"`
		} `json:"tasks"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode school tasks: %w", errUnmarshal)
	}
	status := &SchoolStatus{InPeriod: payload.InPeriod}
	for _, item := range payload.Tasks {
		status.Tasks = append(status.Tasks, SchoolTask{
			Code: item.Code, Status: item.Status,
			Progress: item.Progress, TargetCount: item.TargetCount,
		})
	}
	return status, nil
}

// SchoolTaskViewed 激活一个开学季任务（pending → in_progress）。
//
// 这是**计数前置**：未激活的任务，后续行为事件不会被计分。
func (c *Client) SchoolTaskViewed(ctx context.Context, cred *Credential, code string) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost,
		schoolBasePath+"/tasks/"+url.PathEscape(strings.TrimSpace(code))+"/viewed", map[string]any{})
	return errCall
}

// SchoolTaskClaim 领取开学季任务奖励，返回获得的抽奖次数。
func (c *Client) SchoolTaskClaim(ctx context.Context, cred *Credential, code string) (int64, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost,
		schoolBasePath+"/tasks/"+url.PathEscape(strings.TrimSpace(code))+"/claim", map[string]any{})
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		ChanceGranted int64 `json:"chance_granted"`
	}
	_ = json.Unmarshal(data, &payload)
	return payload.ChanceGranted, nil
}

// SchoolShareComplete 完成分享任务。
//
// 纯前端上报：服务端不校验真实分享回执，直接调用即点亮。
func (c *Client) SchoolShareComplete(ctx context.Context, cred *Credential) error {
	_, errCall := c.growthJSON(ctx, cred, http.MethodPost, schoolBasePath+"/tasks/share-complete",
		map[string]any{"channel": "wechat"})
	return errCall
}

// SchoolChances 查询开学季抽奖次数余额。
func (c *Client) SchoolChances(ctx context.Context, cred *Credential) (int, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, schoolBasePath+"/config", nil)
	if errCall != nil {
		return 0, errCall
	}
	var payload struct {
		Chance struct {
			Balance int `json:"balance"`
		} `json:"chance"`
	}
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return 0, fmt.Errorf("decode school config: %w", errUnmarshal)
	}
	return payload.Chance.Balance, nil
}

// SchoolDraw 执行一次开学季抽奖。
func (c *Client) SchoolDraw(ctx context.Context, cred *Credential) (string, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodPost, schoolBasePath+"/wheel/draw",
		map[string]any{"draw_uuid": newHexID()})
	if errCall != nil {
		return "", errCall
	}
	return string(data), nil
}

// SchoolExpertUseEvents 构造开学季专家使用事件链（4 事件）。
func SchoolExpertUseEvents(expertID, expertName, conversationID string) []map[string]any {
	requestID := "wb2api-exp-req-" + randomHexForExport(6)
	return []map[string]any{
		{
			"eventCode": "expert_summon_click", "id": expertID, "name": expertName,
			"type": "16-BackToSchool", "position": 0,
		},
		{
			"eventCode": "expert_summoned", "id": expertID, "name": expertName,
			"type": "16-BackToSchool",
		},
		{
			"eventCode": "expert_actual_use", "id": expertID, "name": expertName,
			"type": "16-BackToSchool", "characterCount": 14, "expertType": "builtin",
			"conversationId": conversationID, "requestId": requestID,
		},
		{
			"eventCode": "chat_request_send", "inputLength": 14, "isPlan": false,
			"maxSteps": 500, "expertId": expertID, "expertName": expertName,
			"conversationId": conversationID, "requestId": requestID,
			"parentConversationId": conversationID,
			"codebuddy.session_id": conversationID,
		},
	}
}

// growthJSON 发起一次成长域请求并解开信封。
func (c *Client) growthJSON(ctx context.Context, cred *Credential, method, path string, body any) (json.RawMessage, error) {
	endpoints := GetEndpoints(cred.Realm())
	var reader *strings.Reader
	if body != nil {
		encoded, errEncode := json.Marshal(body)
		if errEncode != nil {
			return nil, fmt.Errorf("encode %s request: %w", path, errEncode)
		}
		reader = strings.NewReader(string(encoded))
	} else {
		reader = strings.NewReader("")
	}
	req, errReq := http.NewRequestWithContext(ctx, method, endpoints.ChatBase+path, reader)
	if errReq != nil {
		return nil, fmt.Errorf("build %s request: %w", path, errReq)
	}
	c.applyBillingHeaders(req, cred)
	// 开学季链路走计费域。
	if strings.HasPrefix(path, schoolBasePath) {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, method, bodyOrEmpty(body))
		return data, errCall
	}
	return c.doJSON(req)
}

// bodyOrEmpty 归一化请求体（nil 时给空对象，上游要求 body 是对象）。
func bodyOrEmpty(body any) any {
	if body == nil {
		return map[string]any{}
	}
	return body
}

// ReportDesktopEvents 用桌面指纹族上报事件（对话域 /v2/report）。
func (c *Client) ReportDesktopEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(cred.Realm())
	return c.postReport(ctx, cred, endpoints.ChatBase+reportPath, events, func(req *http.Request) {
		c.applyDesktopHeaders(req, cred)
	})
}

// ReportBillingEvents 用 CLI/billing 指纹族上报事件（计费域 /v2/report）。
func (c *Client) ReportBillingEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(cred.Realm())
	return c.postReport(ctx, cred, endpoints.BillingBase+reportPath, events, func(req *http.Request) {
		c.applyBillingHeaders(req, cred)
	})
}

// ReportWebEvents 用 web 指纹族上报事件（Web 域 /v2/report）。
func (c *Client) ReportWebEvents(ctx context.Context, cred *Credential, pageURL string, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(RegionCN)
	return c.postReport(ctx, cred, endpoints.WebBase+reportPath, events, func(req *http.Request) {
		applyWebHeaders(req, cred, pageURL)
	})
}

// ReportMPEvents 用小程序指纹族上报事件。
//
// 注意 base 用 CN 计费域硬编码而非按 realm 选：global 账号也没有小程序任务，
// 行为一致，跟随原项目实现。
func (c *Client) ReportMPEvents(ctx context.Context, cred *Credential, events []map[string]any) error {
	if len(events) == 0 {
		return nil
	}
	endpoints := GetEndpoints(RegionCN)
	return c.postReport(ctx, cred, endpoints.BillingBase+reportPath, events, func(req *http.Request) {
		c.applyBillingHeaders(req, cred)
		applyMPHeaders(req)
	})
}

// postReport 是所有指纹族共用的上报出口。
func (c *Client) postReport(ctx context.Context, cred *Credential, url string, events []map[string]any, decorate func(*http.Request)) error {
	// body 必须是数组：上游按数组解析，单对象会被静默丢弃。
	encoded, errEncode := json.Marshal(events)
	if errEncode != nil {
		return fmt.Errorf("encode report events: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return fmt.Errorf("build report request: %w", errReq)
	}
	decorate(req)

	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return fmt.Errorf("report request: %w", errDo)
	}
	defer closeBody(resp.Body)

	body := make([]byte, 0, 1024)
	buffer := make([]byte, 1024)
	for {
		read, errRead := resp.Body.Read(buffer)
		if read > 0 {
			body = append(body, buffer[:read]...)
			if len(body) > apiBodyLimit {
				break
			}
		}
		if errRead != nil {
			break
		}
	}
	if resp.StatusCode >= 400 {
		return enrichClassifyError(resp.StatusCode, string(body), resp.Header)
	}
	// 上报接口对「已上报过」也返回成功，因此不检查业务码。
	return nil
}

// ReportChatActivity 上报一条对话活跃事件（CLI/billing 指纹）。
//
// 这一条上报同时点亮 growth 连登与解锁 first_buddy 任务，
// 因此是领养与连登体系的前置条件。
func (c *Client) ReportChatActivity(ctx context.Context, cred *Credential, conversationID, requestID, modelID, modelName string) error {
	if strings.TrimSpace(requestID) == "" {
		requestID = conversationID
	}
	if strings.TrimSpace(modelID) == "" {
		modelID = defaultReportModelID
	}
	if strings.TrimSpace(modelName) == "" {
		modelName = defaultReportModelName
	}
	now := nowMs()
	event := map[string]any{
		"eventCode": "chat_request_send", "timestamp": now, "reportDelay": 0,
		"mode": "craft", "conversationId": conversationID, "requestId": requestID,
		"inputLength":      12,
		"requestModelId":   modelID,
		"requestModelName": modelName,
		"isPlan":           false, "isAutoExecuteTerminal": false, "isAutoModify": false,
		"codebaseEnable": false,
		"maxToken":       0, "maxSteps": 0, "temperature": 0, "maxRetries": 0,
		"mentionContexts": []any{}, "knowledgeId": "", "knowledgeName": "",
		"codebaseId": "", "mentionContextCount": 0, "command": "",
		"expertId": "", "recommendId": "", "skillId": "", "skillCount": 0,
		"totalCount": 0, "fileUri": "", "presentAt": now,
		"traceId": requestID, "rootRequestId": requestID,
		"parentConversationId": conversationID,
		"agentName":            "default", "agentType": "conversation",
		// userId 缺失会让服务端静默丢弃这条事件。
		"userId": cred.UIDValue(),
	}
	return c.ReportBillingEvents(ctx, cred, []map[string]any{event})
}

// 上报默认模型（上游对活跃上报不强校验模型，用通用值即可）。
const (
	defaultReportModelID   = "deepseek-v4-flash"
	defaultReportModelName = "DeepSeek V4 Flash"
)

// ReportWebElementClick 上报一次 web 元素点击（Library_read 等任务）。
func (c *Client) ReportWebElementClick(ctx context.Context, cred *Credential, pageURL, elementID, elementName string) error {
	now := nowMs()
	event := map[string]any{
		"eventCode": "web_element_click", "timestamp": now, "reportDelay": 0,
		"pageURL": pageURL, "elementId": elementID, "elementName": elementName,
		"os": "Win32", "arch": "", "osVersion": "10.0", "userAgent": chromeUA,
		"machineId":    c.rt.deriveID(cred.UIDValue(), "webmachine"),
		"userId":       cred.UIDValue(),
		"userNickname": cred.NicknameValue(),
		"enterpriseId": cred.EnterpriseIDValue(),
	}
	return c.ReportWebEvents(ctx, cred, pageURL, []map[string]any{event})
}

// nowMs 返回当前毫秒时间戳。
func nowMs() int64 { return time.Now().UnixMilli() }

// randomHexForExport 生成随机 hex（供包内构造事件 id 使用）。
func randomHexForExport(n int) string {
	buffer := make([]byte, n)
	if _, errRand := rand.Read(buffer); errRand != nil {
		sum := sha256.Sum256([]byte(fmt.Sprintf("fallback-%d", time.Now().UnixNano())))
		return hex.EncodeToString(sum[:n])
	}
	return hex.EncodeToString(buffer)
}

// 本文件是 WorkBuddy 的任务中心：动作表、扫描、执行、开学季与定时任务族。
//
// 这些规则完全由 CodeBuddy 上游的活动体系决定（任务码、事件链、计分窗口），
// 因此留在供应商包内；根层只做账号遍历、并发队列与 HTTP 响应。

// taskAction 是一个可自动完成的成长任务。
type taskAction struct {
	Code     string
	Desc     string
	UsesChat bool
	Run      func(ctx context.Context, client *Client, credential *Credential) (string, error)
}

// autoActions 是「一键完成」覆盖的任务表（顺序即依赖序）。
//
// 唯一不做的 Expert_Philanthropy 需要真实捐款（服务端校验捐赠回执）。
var autoActions = []taskAction{
	{Code: "chat_5", Desc: "上报 5 条对话活跃事件", Run: runChatFive},
	{Code: "first_buddy", Desc: "解锁并领养第一只 Buddy", Run: runFirstBuddy},
	{Code: "Model_chat_GLM5.2", Desc: "glm-5.2 真实对话一次并对齐上报", Run: runModelChat, UsesChat: true},
	{Code: "RichMeow_Chat", Desc: "桌面指纹对话事件链", Run: runRichMeow},
	{Code: "Buddy_App", Desc: "进入 Buddy 应用事件链", Run: runBuddyApp},
	{Code: "Buddy_App_QQ", Desc: "进入企鹅教师助手事件链", Run: runBuddyApp},
	{Code: "automation_1", Desc: "上报定时任务创建事件", Run: runAutomation},
	{Code: "Library_read", Desc: "上报资料库阅读事件", Run: runLibraryRead},
	{Code: "template_5", Desc: "上报模板使用事件组 ×5", Run: runTemplateUse},
	{Code: "playbook_prompt", Desc: "上报灵感案例发送事件组", Run: runPlaybookPrompt},
	{Code: "create_canvas", Desc: "上报设计画布创建事件组", Run: runDesignCanvas},
	{Code: "expert_5", Desc: "真实专家召唤+使用链 ×5", Run: runExpertFive, UsesChat: true},
	{Code: "Expert_team_use_3", Desc: "真实专家团召唤+使用链 ×3", Run: runExpertTeam, UsesChat: true},
	{Code: "Hp_Appearance", Desc: "设置主题 + 皮肤生效事件", Run: runAppearance},
	{Code: "skill_1", Desc: "真实对话 + 技能加载事件", Run: runSkillFresh, UsesChat: true},
	{Code: "Expert_lighthouse", Desc: "轻量云专家召唤+使用链", Run: runExpertLighthouse, UsesChat: true},
	{Code: "black_cat", Desc: "夜猫子：夜间窗口内 glm-5.2 对话", Run: runBlackCat, UsesChat: true},
}

func autoActionFor(code string) *taskAction {
	trimmed := strings.TrimSpace(code)
	for index := range autoActions {
		if autoActions[index].Code == trimmed {
			return &autoActions[index]
		}
	}
	return nil
}

func autoActionIndex(code string) int {
	trimmed := strings.TrimSpace(code)
	for index := range autoActions {
		if autoActions[index].Code == trimmed {
			return index
		}
	}
	return len(autoActions)
}

const (
	TaskKindTravel   = "travel"
	TaskKindActivity = "activity"
	TaskKindBlackcat = "blackcat"
	TaskKindSchool   = "school"
	TaskKindStreak   = "streak"
	// TaskKindDailyBonus 是国际版专属任务族：每日活跃奖励（国际版签到）。
	TaskKindDailyBonus = "dailybonus"
)

// TaskSpecsFor 返回本区域注册的任务族与默认排程。
//
// 国际版只有一个任务族：每日活跃奖励。它的机制与 CN 签到不同（活跃信标
// 点亮热力图，见 tasks.go），因此不并入 checkin 族的 9/21 点，
// 而是按参考实现取 4 个排程点兜底。
func TaskSpecsFor(region Region) []core.TaskSpec {
	if region.IsGlobal() {
		return []core.TaskSpec{
			{
				Kind:         TaskKindDailyBonus,
				Label:        "每日活跃奖励",
				Description:  "国际版每日签到：活跃信标点亮成长热力图，Bonus Pack 次日凌晨发放",
				DefaultHours: globalDailyBonusHours,
				Manual:       true,
				Route:        "/dailybonus",
			},
		}
	}
	return []core.TaskSpec{
		{Kind: TaskKindTravel, Label: "猫猫旅行", Description: "每日猫猫旅行任务", DefaultHours: []int{9, 21}, Manual: true, Route: "/travel"},
		{Kind: TaskKindActivity, Label: "活跃上报", Description: "每日活跃与连登上报", DefaultHours: []int{10}, Manual: true, Route: "/activity"},
		{Kind: core.TaskKindKeepalive, Label: "Token 保活", Description: "刷新账号 token", DefaultHours: []int{22}, Manual: true},
		{Kind: TaskKindBlackcat, Label: "夜猫子", Description: "夜间任务补足", DefaultHours: []int{23}, Manual: true, Route: "/blackcat"},
		{Kind: TaskKindSchool, Label: "开学季", Description: "开学季活动闭环", AfterCheckin: true, Manual: true, Route: "/school/run"},
		{Kind: TaskKindStreak, Label: "连登奖励", Description: "连登兑换与抽奖闭环", AfterCheckin: true},
	}
}

// TaskSpecs 实现 core.TaskCatalogProvider。
func (v *adapter) TaskSpecs() []core.TaskSpec { return TaskSpecsFor(v.region) }

// ---- 任务扫描 ----

func (v *adapter) InspectTasks(ctx context.Context, cred *core.Credential) (core.TaskInspection, error) {
	if v.region.IsGlobal() {
		return core.TaskInspection{}, nil
	}
	native, err := v.Native(cred)
	if err != nil {
		return core.TaskInspection{}, err
	}
	client := v.client(ctx)
	growth, errGrowth := client.ListTasks(ctx, native)
	if errGrowth != nil {
		return core.TaskInspection{}, errGrowth
	}
	if mpTasks, errMP := client.ListTasksMP(ctx, native); errMP == nil {
		growth = mergeTasks(growth, mpTasks)
	}
	inspection := core.TaskInspection{}
	for _, task := range growth {
		if item, okItem := pendingGrowthItem(task); okItem {
			inspection.Items = append(inspection.Items, item)
		}
	}
	sortItemsByAction(inspection.Items)
	if school, errSchool := client.SchoolTasks(ctx, native); errSchool == nil && school != nil {
		inspection.InPeriod = school.InPeriod
		for _, task := range school.Tasks {
			if item, okItem := pendingSchoolItem(task); okItem {
				inspection.Items = append(inspection.Items, item)
			}
		}
	}
	return inspection, nil
}

func sortItemsByAction(items []core.TaskItem) {
	for i := 1; i < len(items); i++ {
		for j := i; j > 0 && autoActionIndex(items[j].Code) < autoActionIndex(items[j-1].Code); j-- {
			items[j], items[j-1] = items[j-1], items[j]
		}
	}
}

func pendingGrowthItem(task Task) (core.TaskItem, bool) {
	if task.Claimed || task.Locked {
		return core.TaskItem{}, false
	}
	action := autoActionFor(task.Code)
	if action == nil {
		return core.TaskItem{}, false
	}
	return core.TaskItem{
		Kind:     "growth",
		Code:     task.Code,
		Title:    core.FirstNonEmpty(task.Title, action.Desc, task.Code),
		Current:  task.Current,
		Target:   task.Target,
		UsesChat: action.UsesChat,
	}, true
}

func pendingSchoolItem(task SchoolTask) (core.TaskItem, bool) {
	switch task.Code {
	case "task_student_verify":
		return core.TaskItem{}, false
	case "desktop_chat_1_time":
		if task.Status == "claimed" {
			return core.TaskItem{}, false
		}
	default:
		if task.Status == "claimed" || task.Status == "completed" {
			return core.TaskItem{}, false
		}
	}
	return core.TaskItem{
		Kind:     "school",
		Code:     task.Code,
		RunCode:  "school_daily",
		Title:    schoolTaskTitle(task.Code),
		Current:  task.Progress,
		Target:   task.TargetCount,
		UsesChat: task.Code == "desktop_chat_1_time",
	}, true
}

func schoolTaskTitle(code string) string {
	switch code {
	case "share_invite":
		return "分享活动"
	case "desktop_chat_1_time":
		return "桌面端体验"
	case "chat_3_times":
		return "和 AI 对话 3 次"
	case "expert_use":
		return "召唤开学季专家"
	}
	return code
}

func mergeTasks(primary, extra []Task) []Task {
	seen := make(map[string]bool, len(primary))
	out := make([]Task, 0, len(primary)+len(extra))
	for _, task := range primary {
		seen[task.Code] = true
		out = append(out, task)
	}
	for _, task := range extra {
		if seen[task.Code] {
			continue
		}
		out = append(out, task)
	}
	return out
}

// ---- 任务执行 ----

func (v *adapter) RunTask(ctx context.Context, cred *core.Credential, code string) (core.TaskRunResult, error) {
	if v.region.IsGlobal() {
		return core.TaskRunResult{Status: "skipped", Message: "国际版账号没有成长任务"}, nil
	}
	native, err := v.Native(cred)
	if err != nil {
		return core.TaskRunResult{}, err
	}
	client := v.client(ctx)
	if strings.TrimSpace(code) == "school_daily" {
		message, errRun := RunSchool(ctx, client, native)
		if errRun != nil {
			return core.TaskRunResult{}, errRun
		}
		if message == "" {
			message = "开学季无需动作（活动期外或已完成）"
		}
		return core.TaskRunResult{Status: "done", Message: message}, nil
	}
	message, credit, energy, errRun := runGrowthTask(ctx, client, native, code)
	if errRun != nil {
		return core.TaskRunResult{}, errRun
	}
	return core.TaskRunResult{Status: "done", Message: message, Credit: credit, Energy: energy}, nil
}

// PrepareTasks 批量报名未接受的任务（队列与一键完成的前置）。
func (v *adapter) PrepareTasks(ctx context.Context, cred *core.Credential) int {
	if v.region.IsGlobal() {
		return 0
	}
	native, err := v.Native(cred)
	if err != nil {
		return 0
	}
	return acceptPendingTasks(ctx, v.client(ctx), native)
}

func (v *adapter) RunAllTasks(ctx context.Context, cred *core.Credential) []core.AutoTaskResult {
	if v.region.IsGlobal() {
		return nil
	}
	native, err := v.Native(cred)
	if err != nil {
		return []core.AutoTaskResult{{Code: "(账号)", Status: "error", Message: err.Error()}}
	}
	client := v.client(ctx)
	results := make([]core.AutoTaskResult, 0, len(autoActions)+1)
	if accepted := acceptPendingTasks(ctx, client, native); accepted > 0 {
		results = append(results, core.AutoTaskResult{
			Code: "(批量报名)", Status: "done",
			Message: fmt.Sprintf("报名 %d 个任务", accepted),
		})
		core.SleepContext(ctx, 1050*time.Millisecond)
	}
	for _, action := range autoActions {
		task, errTask := findGrowthTask(ctx, client, native, action.Code)
		if errTask != nil {
			results = append(results, core.AutoTaskResult{Code: action.Code, Status: "error", Message: errTask.Error()})
			continue
		}
		if task == nil {
			results = append(results, core.AutoTaskResult{Code: action.Code, Status: "skipped", Message: "该账号无此任务"})
			continue
		}
		if task.Claimed {
			results = append(results, core.AutoTaskResult{Code: action.Code, Status: "skipped", Message: "已领取过奖励"})
			continue
		}
		if action.Code == "black_cat" && !InNightWindow(time.Now()) {
			results = append(results, core.AutoTaskResult{
				Code: action.Code, Status: "skipped",
				Message: "当前不在 23:00-08:00 计分窗口，排程会在夜间自动补足",
			})
			continue
		}
		message, credit, energy, errRun := runGrowthTask(ctx, client, native, action.Code)
		result := core.AutoTaskResult{Code: action.Code, Credit: credit, Energy: energy}
		if errRun != nil {
			result.Status = "error"
			result.Message = errRun.Error()
		} else {
			result.Status = "done"
			result.Message = message
		}
		if latest, errLatest := findGrowthTask(ctx, client, native, action.Code); errLatest == nil && latest != nil {
			result.Progress = fmt.Sprintf("%d/%d", latest.Current, latest.Target)
		}
		results = append(results, result)
		core.SleepContext(ctx, 1050*time.Millisecond)
	}
	return results
}

func runGrowthTask(ctx context.Context, client *Client, credential *Credential, code string) (string, int64, int64, error) {
	action := autoActionFor(code)
	if action == nil {
		return "", 0, 0, fmt.Errorf("该任务没有对应的自动动作（可能需要客户端内交互）")
	}
	task, errTask := findGrowthTask(ctx, client, credential, code)
	if errTask == nil && task != nil {
		if task.Claimed {
			return "任务已完成且已领奖", 0, 0, nil
		}
		if task.Done() {
			credit, energy, errClaim := claimGrowthReward(ctx, client, credential, code)
			if errClaim != nil {
				return "", 0, 0, fmt.Errorf("任务已达标，但领奖失败：%w", errClaim)
			}
			msg := "任务已达标（跳过重复执行）"
			if credit > 0 || energy > 0 {
				msg = fmt.Sprintf("任务已达标，已领奖 +%d 积分 +%d 能量", credit, energy)
			}
			return msg, credit, energy, nil
		}
	}
	message, errRun := action.Run(ctx, client, credential)
	if errRun != nil {
		return "", 0, 0, errRun
	}
	if waitGrowthClaimable(ctx, client, credential, code) {
		credit, energy, errClaim := claimGrowthReward(ctx, client, credential, code)
		if errClaim != nil {
			return "", 0, 0, fmt.Errorf("%s；但领奖失败：%w", message, errClaim)
		}
		if credit > 0 || energy > 0 {
			message += fmt.Sprintf("（领奖 +%d 积分 +%d 能量）", credit, energy)
		}
		return message, credit, energy, nil
	}
	return message, 0, 0, nil
}

func waitGrowthClaimable(ctx context.Context, client *Client, credential *Credential, code string) bool {
	mp := IsMPTaskCode(code)
	attempts := 4
	if mp {
		attempts = 2
	}
	for attempt := 1; attempt < attempts; attempt++ {
		if !core.SleepContext(ctx, 3*time.Second) {
			return false
		}
		task, errTask := findGrowthTask(ctx, client, credential, code)
		if errTask != nil {
			continue
		}
		if task == nil || task.Claimed {
			return false
		}
		if task.Claimable() || task.Done() {
			return true
		}
	}
	return false
}

func findGrowthTask(ctx context.Context, client *Client, credential *Credential, code string) (*Task, error) {
	var list []Task
	var errList error
	if IsMPTaskCode(code) {
		list, errList = client.ListTasksMP(ctx, credential)
	} else {
		list, errList = client.ListTasks(ctx, credential)
	}
	if errList != nil {
		return nil, errList
	}
	for index := range list {
		if list[index].Code == code {
			return &list[index], nil
		}
	}
	return nil, nil
}

// claimGrowthReward 领奖并把错误交给调用方。
//
// 早期这里吞掉错误返回 (0, 0)：领奖响应解析失败（HTML 错误页、上游改结构）
// 会被 runGrowthTask 当成「任务已达标但没奖励」，用户看到的是一句成功文案。
// 领奖有副作用，失败必须让页面看见。
func claimGrowthReward(ctx context.Context, client *Client, credential *Credential, code string) (int64, int64, error) {
	if IsMPTaskCode(code) {
		credit, energy, errClaim := client.ClaimRewardMP(ctx, credential, code)
		if errClaim != nil {
			logger.Debug("claim mp reward for %s failed: %v", code, errClaim)
			return 0, 0, errClaim
		}
		return credit, energy, nil
	}
	credit, energy, errClaim := client.ClaimReward(ctx, credential, code)
	if errClaim != nil {
		logger.Debug("claim reward for %s failed: %v", code, errClaim)
		return 0, 0, errClaim
	}
	return credit, energy, nil
}

func acceptPendingTasks(ctx context.Context, client *Client, credential *Credential) int {
	tasksList, errTasks := client.ListTasks(ctx, credential)
	if errTasks != nil {
		return 0
	}
	var codes []string
	for _, task := range tasksList {
		if task.Claimed || task.Locked {
			continue
		}
		status := strings.ToLower(task.AcceptStatus)
		if status == "accepted" || status == "completed" || status == "claimed" {
			continue
		}
		if autoActionFor(task.Code) == nil && !IsMPTaskCode(task.Code) {
			continue
		}
		codes = append(codes, task.Code)
	}
	if len(codes) == 0 {
		return 0
	}
	accepted := 0
	for start := 0; start < len(codes); start += 20 {
		end := start + 20
		if end > len(codes) {
			end = len(codes)
		}
		if errAccept := client.AcceptTasks(ctx, credential, codes[start:end]); errAccept == nil {
			accepted += end - start
		}
		core.SleepContext(ctx, 1050*time.Millisecond)
	}
	return accepted
}

// ---- 开学季状态 ----

func (v *adapter) TaskStatus(ctx context.Context, cred *core.Credential) core.TaskStatusResult {
	if v.region.IsGlobal() {
		return core.TaskStatusResult{Error: "global realm（无开学季活动）"}
	}
	native, err := v.Native(cred)
	if err != nil {
		return core.TaskStatusResult{Error: err.Error()}
	}
	client := v.client(ctx)
	result := core.TaskStatusResult{}
	status, errStatus := client.SchoolTasks(ctx, native)
	if errStatus != nil {
		result.Error = errStatus.Error()
	} else if status != nil {
		result.InPeriod = status.InPeriod
		for _, task := range status.Tasks {
			result.Tasks = append(result.Tasks, core.TaskStatusView{
				Code: task.Code, Title: schoolTaskTitle(task.Code),
				Status: task.Status, Progress: task.Progress, Target: task.TargetCount,
			})
		}
	}
	if chances, errChances := client.SchoolChances(ctx, native); errChances == nil {
		result.Chances = chances
	}
	return result
}

// ---- 定时任务族 ----

func (v *adapter) RunScheduled(ctx context.Context, cred *core.Credential, kind string) (string, error) {
	if v.region.IsGlobal() {
		if kind != TaskKindDailyBonus {
			return "", nil
		}
		native, errNative := v.Native(cred)
		if errNative != nil {
			return "", errNative
		}
		result, errBonus := v.client(ctx).GlobalDailyBonus(ctx, native)
		if errBonus != nil {
			return "", errBonus
		}
		if result.Already {
			return fmt.Sprintf("今日已点亮（已连续点亮 %d 天）", result.Streak), nil
		}
		return fmt.Sprintf("活跃信标已上报（已连续点亮 %d 天）", result.Streak), nil
	}
	native, err := v.Native(cred)
	if err != nil {
		return "", err
	}
	client := v.client(ctx)
	switch kind {
	case TaskKindActivity:
		conversationID := fmt.Sprintf("wb2api-%d", time.Now().UnixMilli())
		if errReport := client.ReportChatActivity(ctx, native, conversationID, "", "", ""); errReport != nil {
			return "", errReport
		}
		return checkActivityStreak(ctx, client, native), nil
	case TaskKindTravel:
		return RunTravel(ctx, client, native), nil
	case TaskKindBlackcat:
		if !InNightWindow(time.Now()) {
			return "", nil
		}
		return RunBlackcat(ctx, client, native)
	case TaskKindSchool:
		return RunSchool(ctx, client, native)
	case TaskKindStreak:
		return RunStreakBonus(ctx, client, native)
	}
	return "", fmt.Errorf("unknown scheduled task kind %q", kind)
}

func checkActivityStreak(ctx context.Context, client *Client, credential *Credential) string {
	streak, errStreak := client.GrowthStreak(ctx, credential)
	if errStreak != nil {
		return ""
	}
	if streak.Days == 0 {
		logger.Error("activity: report succeeded but streak.days=0 (event may have been silently dropped)")
		return ""
	}
	return fmt.Sprintf("连登天数 %d", streak.Days)
}

// ---- 动作实现 ----

func runChatFive(ctx context.Context, client *Client, credential *Credential) (string, error) {
	for index := 0; index < 5; index++ {
		conversationID := fmt.Sprintf("wb2api-chat5-%d-%d", time.Now().UnixMilli(), index)
		if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "", ""); errReport != nil {
			return "", errReport
		}
		if !core.SleepContext(ctx, 1050*time.Millisecond) {
			return "", ctx.Err()
		}
	}
	return "上报 5 条活跃事件", nil
}

func runFirstBuddy(ctx context.Context, client *Client, credential *Credential) (string, error) {
	return RunTravel(ctx, client, credential), nil
}

func runModelChat(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-glm-%d", time.Now().UnixMilli())
	if errChat := client.DesktopChat(ctx, credential, "", "glm-5.2", "GLM-5.2", conversationID); errChat != nil {
		return "", errChat
	}
	if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "glm-5.2", "GLM-5.2"); errReport != nil {
		return "", errReport
	}
	return "glm-5.2 对话 + 上报", nil
}

func runRichMeow(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-meow-%d", time.Now().UnixMilli())
	requestID := NewHexID()
	events := client.withDesktopFingerprint(credential,
		DesktopChatSequence(conversationID, requestID, NewHexID(), "fast-model", "fast-model"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "桌面对话事件链", nil
}

func runBuddyApp(ctx context.Context, client *Client, credential *Credential) (string, error) {
	events := client.withDesktopFingerprint(credential,
		DesktopBuddyAppSequence(DefaultBuddyID, DefaultBuddyName))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "Buddy 应用事件链", nil
}

func runAutomation(ctx context.Context, client *Client, credential *Credential) (string, error) {
	events := client.withDesktopFingerprint(credential, DesktopAutomationCreateEvent("wb2api 自动化"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "定时任务创建事件", nil
}

func runLibraryRead(ctx context.Context, client *Client, credential *Credential) (string, error) {
	const pageURL = "https://www.workbuddy.cn/space/d/o0KWYeynteVv06UnAZqIFm"
	if errReport := client.ReportWebElementClick(ctx, credential, pageURL,
		"library_doc_intro_click", "WorkBuddy资料库介绍"); errReport != nil {
		return "", errReport
	}
	return "资料库阅读事件", nil
}

func runTemplateUse(ctx context.Context, client *Client, credential *Credential) (string, error) {
	templates := []struct{ id, name string }{
		{"1", "深度研究"}, {"2", "周报生成"}, {"3", "竞品分析"},
		{"4", "活动策划"}, {"5", "代码评审"},
	}
	for _, template := range templates {
		conversationID := fmt.Sprintf("wb2api-tpl-%d-%s", time.Now().UnixMilli(), template.id)
		requestID := NewHexID()
		events := client.withDesktopFingerprint(credential,
			DesktopTemplateUseSequence(conversationID, requestID, template.id, template.name))
		if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
			return "", errReport
		}
		if !core.SleepContext(ctx, 1050*time.Millisecond) {
			return "", ctx.Err()
		}
	}
	return "模板使用事件 ×5", nil
}

func runPlaybookPrompt(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-pb-%d", time.Now().UnixMilli())
	requestID := fmt.Sprintf("wb2api-pb-req-%d", time.Now().UnixMilli())
	events := client.withDesktopFingerprint(credential,
		DesktopPlaybookPromptSequence(conversationID, requestID,
			"pm-gtm-launch-plan", "新产品上市 GTM 发布计划一页纸"))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "灵感案例事件组", nil
}

func runDesignCanvas(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-canvas-%d", time.Now().UnixMilli())
	requestID := NewHexID()
	events := client.withDesktopFingerprint(credential,
		DesktopDesignCanvasSequence(conversationID, requestID))
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "设计画布事件组", nil
}

func runExpertFive(ctx context.Context, client *Client, credential *Credential) (string, error) {
	return runExpertChain(ctx, client, credential, "agent", 5)
}

func runExpertTeam(ctx context.Context, client *Client, credential *Credential) (string, error) {
	return runExpertChain(ctx, client, credential, "team", 3)
}

func runExpertChain(ctx context.Context, client *Client, credential *Credential, expertType string, count int) (string, error) {
	experts, errList := client.MarketExpertList(ctx, credential, expertType)
	if errList != nil {
		return "", errList
	}
	if len(experts) == 0 {
		return "", fmt.Errorf("专家市场没有可用的 %s 类型专家", expertType)
	}
	completed := 0
	for index := 0; index < count; index++ {
		expert := experts[index%len(experts)]
		conversationID := fmt.Sprintf("wb2api-expert-%d-%d", time.Now().UnixMilli(), index)
		if errReport := client.ReportDesktopEvents(ctx, credential,
			client.withDesktopFingerprint(credential, DesktopExpertSummonSequence(expert))); errReport != nil {
			return "", errReport
		}
		if !core.SleepContext(ctx, 6*time.Second) {
			return "", ctx.Err()
		}
		requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, expert.ID, "fast-model", conversationID)
		if errChat != nil {
			logger.Debug("expert chat failed for %s: %v", expert.ID, errChat)
			continue
		}
		if errReport := client.ReportDesktopEvents(ctx, credential,
			client.withDesktopFingerprint(credential, DesktopExpertActualUseEvent(expert, conversationID, requestID))); errReport != nil {
			return "", errReport
		}
		completed++
		if !core.SleepContext(ctx, 6*time.Second) {
			return "", ctx.Err()
		}
	}
	if completed == 0 {
		return "", fmt.Errorf("专家使用链一次都没成功")
	}
	return fmt.Sprintf("专家使用链 %d 次", completed), nil
}

func runAppearance(ctx context.Context, client *Client, credential *Credential) (string, error) {
	if errSet := client.DesktopAppearanceSet(ctx, credential, "theme-tkmw7j"); errSet != nil {
		return "", errSet
	}
	events := client.withDesktopFingerprint(credential, []map[string]any{{
		"eventCode":   "appearance_skin_apply",
		"resourceKey": "theme-tkmw7j", "kind": "theme", "mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "主题设置 + 皮肤事件", nil
}

func runSkillFresh(ctx context.Context, client *Client, credential *Credential) (string, error) {
	conversationID := fmt.Sprintf("wb2api-skill-%d", time.Now().UnixMilli())
	requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, "", "fast-model", conversationID)
	if errChat != nil {
		return "", errChat
	}
	events := client.withDesktopFingerprint(credential, []map[string]any{{
		"eventCode": "skill_info", "skillId": "skill_2097350077599879168",
		"conversationId": conversationID, "requestId": requestID,
		"parentConversationId": conversationID, "mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "技能加载事件", nil
}

func runExpertLighthouse(ctx context.Context, client *Client, credential *Credential) (string, error) {
	const expertID = "ex_2cvvUZQhDyeJ"
	const expertName = "轻量云专家"
	conversationID := fmt.Sprintf("wb2api-lighthouse-%d", time.Now().UnixMilli())

	created := DesktopChatSequence(conversationID, NewHexID(), NewHexID(), "fast-model", "fast-model")
	if len(created) > 0 {
		created[0]["has_expert"] = true
		created[0]["expert_id"] = expertID
		created[0]["expert_name"] = expertName
		created[0]["expert_industry_id"] = ""
	}
	if errReport := client.ReportDesktopEvents(ctx, credential, client.withDesktopFingerprint(credential, created)); errReport != nil {
		return "", errReport
	}
	if !core.SleepContext(ctx, 6*time.Second) {
		return "", ctx.Err()
	}
	requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, expertID, "fast-model", conversationID)
	if errChat != nil {
		return "", errChat
	}
	events := client.withDesktopFingerprint(credential, []map[string]any{{
		"eventCode": "expert_actual_use", "id": expertID, "name": expertName,
		"expertTitle": expertName, "type": "", "expertType": "builtin",
		"source": "builtin", "version": "1.0.0",
		"cost": 0, "characterCount": 14,
		"conversationId": conversationID, "requestId": requestID,
		"messageId":      "msg-" + tailOf(requestID, 8),
		"requestModelId": "fast-model", "requestModelName": "fast-model",
		"mode": "LOCAL",
	}})
	if errReport := client.ReportDesktopEvents(ctx, credential, events); errReport != nil {
		return "", errReport
	}
	return "轻量云专家链", nil
}

func runBlackCat(ctx context.Context, client *Client, credential *Credential) (string, error) {
	return RunBlackcat(ctx, client, credential)
}

func (c *Client) withDesktopFingerprint(credential *Credential, events []map[string]any) []map[string]any {
	return ApplyDesktopFingerprint(credential, events, c.rt)
}

func tailOf(text string, n int) string {
	if len(text) <= n {
		return text
	}
	return text[len(text)-n:]
}

// 本文件实现国际版（global）账号的「每日活跃奖励」，即国际版的签到。
//
// 背景（结论来自 codebuddy-intl-cpa 项目的实测，2026-09-28 → 09-29，
// 7 个账号交叉验证）：国际版没有国内版「加油站」式的每日签到——
// 签到活动状态接口对国际版永远 active:false，daily-checkin 端点也不会入账。
// 国际版的每日收入路径是**活跃信标**：官方客户端每次对话都会向
// POST /v2/report 发一条 chat_request_send 事件。当天成长热力图
// （GET /activity/growth/heatmap）score > 0 即视为「点亮」，次日凌晨
// （中国时区）自动发放 30 积分的 Bonus Pack（平台奖励积分，30 天有效）；
// 连续点亮还能解锁连登档位（14d +50、28d +150）。
//
// 设计要点（与参考实现保持一致）：
//   - 幂等：以热力图最后一格作为 API 自己的「今天」，点亮则不再上报，
//     因此不依赖本地时区做自然日判定；
//   - 信标不重试：事件按天幂等，但盲目重发会把当天计数翻倍（score 2 → 4）；
//     单次失败交给下一个排程点兜底；
//   - 每天多个排程点：已点亮的点只花一次热力图读，几乎零成本。

// globalDailyBonusHours 是国际版每日活跃奖励的默认排程点（本地时间）。
// 4 个点足够兜住「宿主刚启动网络未就绪」这类瞬时失败；运行幂等，
// 多跑只多一次热力图读。
var globalDailyBonusHours = []int{8, 12, 16, 20}

// growthHeatmapCell 是成长热力图上的一格。
type growthHeatmapCell struct {
	Date  string `json:"date"`
	Score int64  `json:"score"`
}

// GrowthHeatmapToday 读取成长热力图，返回「今天」的一格与全部已点亮日期
// （热力图顺序，最早在前）。
//
// 响应有两种形态：cells 数组，或日期为键的 map（与 HeatmapYesterdayMissed
// 的解析保持一致）。空热力图按错误处理，避免把「读不到」误判成「未点亮」。
func (c *Client) GrowthHeatmapToday(ctx context.Context, cred *Credential) (growthHeatmapCell, []string, error) {
	data, errCall := c.growthJSON(ctx, cred, http.MethodGet, growthHeatmapPath, nil)
	if errCall != nil {
		return growthHeatmapCell{}, nil, errCall
	}
	return parseGrowthHeatmap(data)
}

// parseGrowthHeatmap 解析热力图响应（纯函数，便于对两种响应形态做单测）。
func parseGrowthHeatmap(data json.RawMessage) (growthHeatmapCell, []string, error) {
	var wrapper struct {
		Cells []growthHeatmapCell `json:"cells"`
	}
	if errUnmarshal := json.Unmarshal(data, &wrapper); errUnmarshal == nil && len(wrapper.Cells) > 0 {
		lit := make([]string, 0, len(wrapper.Cells))
		for _, cell := range wrapper.Cells {
			if cell.Score > 0 {
				lit = append(lit, cell.Date)
			}
		}
		return wrapper.Cells[len(wrapper.Cells)-1], lit, nil
	}
	var byDate map[string]struct {
		Score int64 `json:"score"`
	}
	if errUnmarshal := json.Unmarshal(data, &byDate); errUnmarshal != nil || len(byDate) == 0 {
		return growthHeatmapCell{}, nil, fmt.Errorf("heatmap empty or unrecognised")
	}
	dates := make([]string, 0, len(byDate))
	for date := range byDate {
		dates = append(dates, date)
	}
	sort.Strings(dates)
	lit := make([]string, 0, len(dates))
	for _, date := range dates {
		if byDate[date].Score > 0 {
			lit = append(lit, date)
		}
	}
	last := dates[len(dates)-1]
	return growthHeatmapCell{Date: last, Score: byDate[last].Score}, lit, nil
}

// DailyBonusBeaconEvent 构造一条每日活跃信标（chat_request_send）。
//
// 字段形态镜像官方客户端的对话上报：conversationId/requestId 用一次性
// 随机值，让每天看起来都是新会话而非重放；userId 必须带，缺失时上游
// 返回 200 但静默丢弃事件。单测锁字段名——写错一个就是静默不计分。
func DailyBonusBeaconEvent(credential *Credential) map[string]any {
	now := nowMs()
	conversationID := "wb-daily-" + randomHexForExport(8)
	return map[string]any{
		"eventCode":            "chat_request_send",
		"timestamp":            now,
		"mode":                 "craft",
		"conversationId":       conversationID,
		"requestId":            conversationID,
		"inputLength":          12,
		"requestModelId":       "deepseek-v4-flash",
		"requestModelName":     "DeepSeek V4 Flash",
		"mentionContexts":      []any{},
		"knowledgeId":          []any{},
		"knowledgeName":        []any{},
		"presentAt":            now,
		"rootRequestId":        conversationID,
		"parentConversationId": conversationID,
		"agentName":            "default",
		"agentType":            "conversation",
		"userId":               credential.UIDValue(),
	}
}

// SendDailyBonusBeacon 上报一条每日活跃信标（计费域 /v2/report）。
//
// 刻意走单次上报出口（postReport 无重试）：理由见文件头设计要点。
func (c *Client) SendDailyBonusBeacon(ctx context.Context, cred *Credential) error {
	if cred == nil {
		return fmt.Errorf("credential is required")
	}
	return c.ReportBillingEvents(ctx, cred, []map[string]any{DailyBonusBeaconEvent(cred)})
}

// GlobalDailyBonus 执行一次国际版每日活跃奖励（幂等）。
//
// 返回的 CheckinResult 语义：
//   - Already=true：今天已点亮（本轮或早前轮次点亮的都算），无需再上报；
//   - Already=false：本轮新上报了信标。积分在次日凌晨发放，因此 Credit/Energy
//     此刻拿不到，恒为 0；Streak 携带热力图上的点亮天数供展示。
//
// 仅接受 global 域凭证：CN 的成长域在 copilot.tencent.com，收入路径是
// DailyCheckin，两者互不通用。
func (c *Client) GlobalDailyBonus(ctx context.Context, cred *Credential) (*CheckinResult, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !region.IsGlobal() {
		return nil, fmt.Errorf("daily bonus is only available for global-realm accounts")
	}
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	todayKey := time.Now().In(cstZone).Format("2006-01-02")
	_, lit, errToday := c.GrowthHeatmapToday(ctx, cred)
	if errToday != nil {
		return nil, fmt.Errorf("heatmap: %w", errToday)
	}
	// 必须按日期匹配「今天」，不能把最后一格当成今天：热力图顺序、
	// 是否含今天都不是协议保证的。
	if heatmapLitOn(lit, todayKey) {
		return &CheckinResult{Already: true, Streak: int64(len(lit))}, nil
	}
	if errSend := c.SendDailyBonusBeacon(ctx, cred); errSend != nil {
		return nil, fmt.Errorf("report: %w", errSend)
	}
	// 热力图聚合滞后数秒，立即回读可能仍未点亮。当天状态以本条上报为准，
	// 下个排程点会看到点亮；这里不因「尚未聚合」把签到判成失败。
	if _, litAfter, errRe := c.GrowthHeatmapToday(ctx, cred); errRe == nil {
		lit = litAfter
	}
	return &CheckinResult{
		Streak:  int64(len(lit)),
		Message: fmt.Sprintf("活跃信标已上报（已连续点亮 %d 天），奖励积分次日凌晨发放", len(lit)),
	}, nil
}

// heatmapLitOn 报告热力图是否包含指定日期的点亮记录。
func heatmapLitOn(lit []string, date string) bool {
	for _, item := range lit {
		if item == date {
			return true
		}
	}
	return false
}

// 本文件实现连登兑换与抽奖、猫猫旅行、夜猫子、开学季四套闭环。
//
// 共同特征：都挂在「签到之后」或独立整点执行，且全部按天幂等。

// 连登状态类型（Streak / StreakTier）定义在 activity.go —— 它们是上游接口的
// 返回结构，属于协议层；本文件只消费它们。合并前两处各有一份同名同形的定义，
// 是跨包重复，已去重。

// RunStreakBonus 执行单账号的连登闭环。
//
// 步骤（顺序有依赖）：
//  1. 补签昨日（有漏签且有补签卡时）；
//  2. 领取新手礼包与活动补偿（每号一次，无则静默跳过）；
//  3. 兑换所有已解锁档位；
//  4. 抽完所有抽奖次数。
func RunStreakBonus(ctx context.Context, client *Client, credential *Credential) (string, error) {
	parts := make([]string, 0, 4)

	if message := makeupYesterday(ctx, client, credential); message != "" {
		parts = append(parts, message)
	}

	// 礼包与补偿：上游用业务错误表达「已领过」，因此错误被忽略。
	for _, claim := range []struct {
		name string
		path string
	}{
		{"新手礼包", ClaimGiftPath()},
		{"活动补偿", ClaimCompensationPath()},
	} {
		if credit, errClaim := client.ClaimBillingReward(ctx, credential, claim.path); errClaim == nil && credit > 0 {
			parts = append(parts, fmt.Sprintf("%s +%d", claim.name, credit))
		}
	}

	streak, errStreak := client.GrowthStreak(ctx, credential)
	if errStreak != nil {
		return strings.Join(parts, "；"), errStreak
	}

	// 兑换已解锁档位。未解锁时上游返回 403，属预期，静默跳过。
	redeemed := 0
	for _, tier := range streak.Tiers {
		if strings.EqualFold(tier.Status, "locked") || strings.EqualFold(tier.Status, "claimed") {
			continue
		}
		if errRedeem := client.GrowthRedeemTier(ctx, credential, tier.Tier); errRedeem != nil {
			continue
		}
		redeemed++
		if !core.SleepContext(ctx, reportGap) {
			return strings.Join(parts, "；"), ctx.Err()
		}
	}
	if redeemed > 0 {
		parts = append(parts, fmt.Sprintf("兑换 %d 个档位", redeemed))
	}

	// 抽奖次数只能从兑换获得，因此必须在兑换之后。
	chances, errChances := client.LotteryChances(ctx, credential)
	if errChances == nil && chances > 0 {
		drawn := 0
		for index := 0; index < chances; index++ {
			if _, errDraw := client.LotteryDraw(ctx, credential); errDraw != nil {
				break
			}
			drawn++
			if !core.SleepContext(ctx, 2*time.Second) {
				return strings.Join(parts, "；"), ctx.Err()
			}
		}
		if drawn > 0 {
			parts = append(parts, fmt.Sprintf("抽奖 %d 次", drawn))
		}
	}
	return strings.Join(parts, "；"), nil
}

// makeupYesterday 补签昨日（有漏签且有补签卡时）。
func makeupYesterday(ctx context.Context, client *Client, credential *Credential) string {
	missed, errMissed := client.HeatmapYesterdayMissed(ctx, credential)
	if errMissed != nil || !missed {
		return ""
	}
	streak, errStreak := client.GrowthStreak(ctx, credential)
	if errStreak != nil || streak.MakeupCards <= 0 {
		return ""
	}
	yesterday := time.Now().AddDate(0, 0, -1).Format("2006-01-02")
	if errUse := client.UseMakeupCard(ctx, credential, yesterday); errUse != nil {
		return ""
	}
	return "补签昨日"
}

// RunTravel 执行单账号猫猫旅行的一个动作。
//
// 状态机：无猫 → 领养；有猫 → 按 status 分派（idle 派出 / arrived 领奖 / traveling 跳过）。
func RunTravel(ctx context.Context, client *Client, credential *Credential) string {
	info, errInfo := client.BuddyInfo(ctx, credential)
	if errInfo != nil {
		return ""
	}
	if !info.HasBuddy {
		return adoptBuddy(ctx, client, credential)
	}

	status, errStatus := client.TravelStatus(ctx, credential)
	if errStatus != nil {
		return ""
	}
	switch status.State {
	case "arrived":
		if status.RecordID <= 0 {
			return ""
		}
		credit, errClaim := client.TravelClaim(ctx, credential, status.RecordID)
		if errClaim != nil {
			return ""
		}
		return fmt.Sprintf("旅行领奖 +%d", credit)
	case "idle":
		if status.DailyLimitReached {
			return ""
		}
		if errDepart := client.TravelDepart(ctx, credential, travelLocationID); errDepart != nil {
			return ""
		}
		return "派出旅行"
	default:
		// traveling 或未知状态：本轮不做动作。
		return ""
	}
}

// travelLocationID 是旅行目的地（古镇客栈）。
//
// 四个地点的收益与时长完全相同，因此没有「最优解」，取固定值即可。
const travelLocationID = 4

// adoptBuddy 领养第一只猫。
//
// **必须先上报一条活跃事件**：上游的 first_buddy 任务以「当日有活跃上报」
// 为前置，缺了它领养恒失败于 "first_buddy task not completed yet"。
func adoptBuddy(ctx context.Context, client *Client, credential *Credential) string {
	conversationID := fmt.Sprintf("wb2api-adopt-%d", nowUnixMs())
	if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "", ""); errReport != nil {
		return ""
	}
	if !core.SleepContext(ctx, reportGap) {
		return ""
	}
	// 同意协议是幂等的，重复调用无害。
	if errAgreement := client.BuddyAgreement(ctx, credential); errAgreement != nil {
		return ""
	}
	credit, errFirst := client.BuddyFirst(ctx, credential)
	if errFirst != nil {
		// 门槛未达（当日活跃上报不足）：明天再试，今天不再重试轰炸。
		return ""
	}
	return fmt.Sprintf("领养成功 +%d", credit)
}

// RunBlackcat 补足夜猫子任务。
//
// 窗口外不做任何事（调用方应先检查），窗口内按差额补对话次数。
func RunBlackcat(ctx context.Context, client *Client, credential *Credential) (string, error) {
	if !InNightWindow(time.Now()) {
		return "", nil
	}
	need, errNeed := client.BlackcatNeed(ctx, credential)
	if errNeed != nil {
		return "", errNeed
	}
	if need <= 0 {
		return "", nil
	}
	completed := int64(0)
	for index := int64(0); index < need; index++ {
		conversationID := fmt.Sprintf("wb2api-night-%d-%d", nowUnixMs(), index)
		// 真实对话：上游要求夜猫子判据是「窗口内的真实 glm-5.2 对话」。
		if errChat := client.DesktopChat(ctx, credential, "", "glm-5.2", "GLM-5.2", conversationID); errChat != nil {
			break
		}
		if errReport := client.ReportChatActivity(ctx, credential, conversationID, "", "glm-5.2", "GLM-5.2"); errReport != nil {
			break
		}
		completed++
		if !core.SleepContext(ctx, nightChatGap) {
			return "", ctx.Err()
		}
	}
	if completed == 0 {
		return "", nil
	}
	return fmt.Sprintf("夜间对话 %d 次", completed), nil
}

// InNightWindow 报告当前是否在夜猫子计分窗口内（23:00 - 08:00）。
func InNightWindow(now time.Time) bool {
	hour := now.Hour()
	return hour >= 23 || hour < 8
}

// RunSchool 执行开学季闭环。
//
// 活动期外静默返回（不报错）：活动结束后排程继续跑也不会做无用功，
// 无需下线代码。
func RunSchool(ctx context.Context, client *Client, credential *Credential) (string, error) {
	status, errStatus := client.SchoolTasks(ctx, credential)
	if errStatus != nil {
		return "", errStatus
	}
	if !status.InPeriod {
		return "", nil
	}

	completed := 0
	for _, task := range status.Tasks {
		switch task.Code {
		case "task_student_verify":
			// 需微信真实学生认证，无法自动化。
			continue
		}
		if strings.EqualFold(task.Status, "claimed") {
			continue
		}
		// viewed 是计数前置：未激活的任务，后续行为事件不计分。
		if errView := client.SchoolTaskViewed(ctx, credential, task.Code); errView != nil {
			continue
		}
		if errRun := runSchoolTask(ctx, client, credential, task.Code); errRun != nil {
			continue
		}
		// 等异步计分落定再领奖。
		if !waitSchoolDone(ctx, client, credential, task.Code) {
			continue
		}
		if _, errClaim := client.SchoolTaskClaim(ctx, credential, task.Code); errClaim != nil {
			continue
		}
		completed++
	}

	// 抽完抽奖次数。
	drawn := 0
	if chances, errChances := client.SchoolChances(ctx, credential); errChances == nil {
		for index := 0; index < chances; index++ {
			if _, errDraw := client.SchoolDraw(ctx, credential); errDraw != nil {
				break
			}
			drawn++
			if !core.SleepContext(ctx, 2*time.Second) {
				break
			}
		}
	}
	if completed == 0 && drawn == 0 {
		return "", nil
	}
	return fmt.Sprintf("完成 %d 个任务、抽奖 %d 次", completed, drawn), nil
}

// runSchoolTask 执行单个开学季任务的行为。
func runSchoolTask(ctx context.Context, client *Client, credential *Credential, code string) error {
	switch code {
	case "share_invite":
		// 纯前端上报，服务端不校验真实分享回执。
		return client.SchoolShareComplete(ctx, credential)
	case "desktop_chat_1_time":
		// 需要一次真实桌面对话 + 桌面事件链。
		conversationID := fmt.Sprintf("wb2api-school-%d", nowUnixMs())
		requestID, errChat := client.DesktopChatWithRequestID(ctx, credential, "", "fast-model", conversationID)
		if errChat != nil {
			return errChat
		}
		return client.ReportDesktopEvents(ctx, credential, withFingerprint(credential,
			DesktopChatSequence(conversationID, requestID, randomHex(16), "fast-model", "fast-model"), client.rt))
	case "chat_3_times":
		// 3 条小程序对话埋点（无需真实会话）。
		for index := 0; index < 3; index++ {
			conversationID := fmt.Sprintf("wb2api-chat-%d-%d", nowUnixMs(), index)
			if errReport := client.ReportMPEvents(ctx, credential, SchoolChatTimesEvents(conversationID, "")); errReport != nil {
				return errReport
			}
			if !core.SleepContext(ctx, mpActionGap) {
				return ctx.Err()
			}
		}
		return nil
	case "expert_use":
		// 小程序专家链（用固定的返校季专家）。
		conversationID := fmt.Sprintf("wb2api-exp-%d", nowUnixMs())
		return client.ReportMPEvents(ctx, credential,
			SchoolExpertUseEvents(schoolExpertID, schoolExpertName, conversationID))
	}
	return fmt.Errorf("unsupported school task %s", code)
}

// 开学季专家（活动内置的固定专家）。
const (
	schoolExpertID   = "ex_jB0dyFIQJEWa"
	schoolExpertName = "论文写作导师"
)

// waitSchoolDone 轮询等待开学季任务的异步计分落定。
//
// 上报 200 ≠ 计分：上游计分是异步的（实测 2.5 秒内落定），
// 立即回读会误判「未达标」从而跳过领奖。
func waitSchoolDone(ctx context.Context, client *Client, credential *Credential, code string) bool {
	for attempt := 0; attempt < schoolPollLoops; attempt++ {
		if !core.SleepContext(ctx, schoolPollGap) {
			return false
		}
		status, errStatus := client.SchoolTasks(ctx, credential)
		if errStatus != nil {
			continue
		}
		for _, task := range status.Tasks {
			if task.Code == code && task.TargetCount > 0 && task.Progress >= task.TargetCount {
				return true
			}
		}
	}
	return false
}

// withFingerprint 给事件链注入桌面指纹（供包内直接调用 ReportDesktopEvents 的场景）。
func withFingerprint(credential *Credential, events []map[string]any, rt *runtime) []map[string]any {
	base := desktopFingerprint(credential, rt)
	out := make([]map[string]any, 0, len(events))
	for _, event := range events {
		out = append(out, mergeEvent(base, event))
	}
	return out
}

// CheckinResult 是一次签到的结果。
type CheckinResult struct {
	// Already 表示今天已经签过（幂等成功，不是错误）。
	Already bool
	// Credit / Energy 是本次签到所得。
	Credit int64
	Energy int64
	// Streak 是连续签到天数。
	Streak int64
	// NotRecorded 表示本次没有真正领到（活动未开启 / 领取处理中 / 无资格）。
	NotRecorded bool
	// Message 是上游给出的可读说明。
	Message string
}

// checkinActivityStatus 是签到状态的权威来源。
type checkinActivityStatus struct {
	Active         bool  `json:"active"`
	TodayCheckedIn bool  `json:"today_checked_in"`
	StreakDays     int64 `json:"streak_days"`
}

// CheckinActivityStatus 查询签到活动状态（POST checkin-activity-status）。
//
// 这是「活动是否开启 / 今天是否已领」的权威判据；不要用 checkin-status，
// 后者返回占位数据（active:false、checkin_dates:null）。
func (c *Client) CheckinActivityStatus(cred *Credential) (*checkinActivityStatus, error) {
	endpoints := GetEndpoints(cred.Realm())
	data, errCall := c.billingJSON(cred, endpoints.BillingBase+checkinActivityStatusPath, http.MethodPost, map[string]any{})
	if errCall != nil {
		return nil, errCall
	}
	var status checkinActivityStatus
	if errUnmarshal := json.Unmarshal(data, &status); errUnmarshal != nil {
		return nil, fmt.Errorf("decode checkin activity status: %w", errUnmarshal)
	}
	return &status, nil
}

// DailyCheckin 执行每日签到。
//
// 先查权威状态再领取：活动未开启 / 今日已领由状态接口判定，而不是把上游的
// 业务拒绝当成失败。领取回执按业务码与文案分类——code=0 也可能表示「签到失败」，
// 只有真正领到才记成功。
func (c *Client) DailyCheckin(cred *Credential) (*CheckinResult, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	endpoints := GetEndpoints(region)
	paths := []string{dailyCheckinPathV2}
	if region.IsGlobal() {
		paths = []string{dailyCheckinPath, dailyCheckinPathV2}
	}

	// 状态预查失败不阻断领取：把瞬时故障当成「不能签到」会让账号整天漏签。
	if status, errStatus := c.CheckinActivityStatus(cred); errStatus == nil && status != nil {
		if !status.Active {
			return &CheckinResult{NotRecorded: true, Message: "签到活动未开启"}, nil
		}
		if status.TodayCheckedIn {
			return &CheckinResult{Already: true, Streak: status.StreakDays}, nil
		}
	}

	var lastErr error
	for _, path := range paths {
		status, code, msg, data, raw, errCall := c.claimDailyCheckin(cred, endpoints.BillingBase+path)
		if errCall != nil {
			lastErr = errCall
			if upstreamErr, okErr := errCall.(*core.Failure); okErr && upstreamErr.Kind == core.FailureKindNotFound {
				continue
			}
			return nil, errCall
		}
		switch {
		case code == 10001 || code == 1001:
			// 10001 不是「签到成功」专属码，但在这个端点上表示已领。
			if core.ContainsAny(msg, []string{"失败"}) {
				return nil, &core.Failure{Status: status, Code: fmt.Sprintf("%d", code), Msg: msg, Kind: core.FailureKindClient}
			}
			return &CheckinResult{Already: true, Message: msg}, nil
		case code == 1002 || code == 1003:
			// 1002 = 无资格，1003 = 活动结束：不是失败，也不是已签到。
			return &CheckinResult{NotRecorded: true, Message: core.FirstNonEmpty(msg, "当前无领取资格")}, nil
		case code != 0:
			return nil, Classify(status, string(raw))
		}
		// code == 0：仍要看文案——上游会用 code=0 + 「签到失败」表达没签成。
		if core.ContainsAny(msg, []string{"失败"}) {
			return nil, &core.Failure{Status: status, Code: "0", Msg: msg, Kind: core.FailureKindClient}
		}
		if core.ContainsAny(msg, []string{"处理中", "请勿重复操作", "系统繁忙", "稍后再试"}) {
			return &CheckinResult{NotRecorded: true, Message: msg}, nil
		}
		var payload struct {
			Credit    int64  `json:"credit"`
			Energy    int64  `json:"energy"`
			Streak    int64  `json:"streak"`
			StreakAlt int64  `json:"streak_days"`
			Message   string `json:"message"`
		}
		// 领取已经成功：回执结构变了只丢明细（积分 / 连签），不能把一次
		// 真实到账的签到报成失败，否则用户会重复点、历史里也记成 error。
		if len(data) > 0 {
			if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
				logger.Debug("workbuddy: 签到回执解析失败，按成功处理: %v", errUnmarshal)
			}
		}
		return &CheckinResult{
			Credit:  payload.Credit,
			Energy:  payload.Energy,
			Streak:  core.FirstNonZero(payload.Streak, payload.StreakAlt),
			Message: core.FirstNonEmpty(payload.Message, msg),
		}, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return nil, fmt.Errorf("checkin endpoint is unavailable")
}

// claimDailyCheckin 发一次领取请求并返回原始信封字段。
//
// 不用 billingJSON：它在业务码非 0 时只返回分类错误，丢掉了 msg 与 data，
// 而签到结果恰好要靠文案区分「已签到 / 处理中 / 失败」。
func (c *Client) claimDailyCheckin(cred *Credential, url string) (status int, code int64, msg string, data json.RawMessage, raw []byte, err error) {
	body, errMarshal := json.Marshal(map[string]any{})
	if errMarshal != nil {
		return 0, 0, "", nil, nil, errMarshal
	}
	req, errReq := http.NewRequestWithContext(c.opts.Context, http.MethodPost, url, strings.NewReader(string(body)))
	if errReq != nil {
		return 0, 0, "", nil, nil, fmt.Errorf("build checkin request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)

	resp, errDo := c.shortClient().Do(req)
	if errDo != nil {
		return 0, 0, "", nil, nil, fmt.Errorf("checkin request: %w", errDo)
	}
	defer closeBody(resp.Body)
	raw, errRead := io.ReadAll(io.LimitReader(resp.Body, apiBodyLimit))
	if errRead != nil {
		return resp.StatusCode, 0, "", nil, nil, fmt.Errorf("read checkin response: %w", errRead)
	}
	var envelope struct {
		Code int64           `json:"code"`
		Msg  string          `json:"msg"`
		Data json.RawMessage `json:"data"`
	}
	if errUnmarshal := json.Unmarshal(raw, &envelope); errUnmarshal != nil {
		if resp.StatusCode >= 400 {
			return resp.StatusCode, 0, "", nil, raw, enrichClassifyError(resp.StatusCode, string(raw), resp.Header)
		}
		return resp.StatusCode, 0, "", nil, raw, fmt.Errorf("decode checkin response: %w", errUnmarshal)
	}
	if resp.StatusCode >= 400 && envelope.Code == 0 {
		return resp.StatusCode, 0, envelope.Msg, envelope.Data, raw, enrichClassifyError(resp.StatusCode, string(raw), resp.Header)
	}
	return resp.StatusCode, envelope.Code, envelope.Msg, envelope.Data, raw, nil
}

// Balance 是账号的资源余额。
//
// 数量用 float64：个人资源包的额度是整数，但企业用量接口的 credit 是小数
// （实测 2.59 / 3.87 / 4.11）。用整数会把小数截断，消耗 0.9 积分时面板
// 上完全看不到变化。
type Balance struct {
	// Remain 是剩余积分。
	Remain float64
	// Total 是本期总额度。
	Total float64
	// Enterprise 表示这份余额来自企业用量接口（不是个人资源包）。
	Enterprise bool
	// Packages 是套餐明细。
	Packages []BalancePackage
}

// BalancePackage 是单项资源包的余额。
type BalancePackage struct {
	// Name 是资源包名称。
	Name string
	// Remain 是剩余量。
	Remain float64
	// Total 是总量。
	Total float64
	// ExpireAt 是过期时刻（Unix 秒，0 表示不过期）。
	ExpireAt int64
	// ResetAt 是周期回满时刻（Unix 秒，0 表示无周期）。
	//
	// 与 ExpireAt 刻意分开：企业额度到点是**回满**，不是作废。
	// 混用会让面板把「下月回满」显示成「即将过期」。
	ResetAt int64
	// Period 是周期文案（如 "2026-10-01 ~ 2026-10-31"），空表示不展示。
	Period string
}

// billingResourceAccount 是余额响应里的单个套餐。
//
// 字段是**上游原样的 PascalCase**（上游该接口不使用 camelCase）。
type billingResourceAccount struct {
	PackageName         string `json:"PackageName"`
	CycleEndTime        string `json:"CycleEndTime"` // "2006-01-02 15:04:05"，空 = 无到期
	CapacitySize        int64  `json:"CapacitySize"`
	CapacityRemain      int64  `json:"CapacityRemain"`
	CapacityUsed        int64  `json:"CapacityUsed"`
	CycleCapacitySize   int64  `json:"CycleCapacitySize"`
	CycleCapacityRemain int64  `json:"CycleCapacityRemain"`
	CycleCapacityUsed   int64  `json:"CycleCapacityUsed"`
}

// billingResourceResponse 是余额接口的响应结构。
//
// 双层信封：Response.Data.Accounts。请求体也必须带分页与产品码，
// 否则上游返回空列表（这一点早期实现漏了，导致余额恒为 0）。
type billingResourceResponse struct {
	Response struct {
		Data struct {
			Accounts []billingResourceAccount `json:"Accounts"`
		} `json:"Data"`
	} `json:"Response"`
}

// packageRemainUsed 聚合单个套餐的 remain/used/size。
//
// 取数规则（照抄上游的单一事实来源，勿改）：
//   - Cycle 期套餐优先（CycleCapacitySize > 0）：用 Cycle 三字段，
//     remain 先钳到 [0, size]；used 取 CycleUsed 与 size-remain 的较大者，
//     且当 used 更大时反推 remain = size - used；
//   - 否则回退 Capacity 三字段，used 为 0 但 size > remain 时按差值补 used。
func packageRemainUsed(account billingResourceAccount) (remain, used, size int64) {
	if account.CycleCapacitySize > 0 {
		remain = account.CycleCapacityRemain
		size = account.CycleCapacitySize
		if remain < 0 {
			remain = 0
		}
		if remain > size {
			remain = size
		}
		used = size - remain
		if account.CycleCapacityUsed > used {
			used = account.CycleCapacityUsed
			if size >= used {
				remain = size - used
			}
		}
		return remain, used, size
	}
	remain = account.CapacityRemain
	used = account.CapacityUsed
	size = account.CapacitySize
	if used == 0 && size > remain {
		used = size - remain
	}
	return remain, used, size
}

// FetchBalance 查询账号余额。
//
// 个人资源接口只覆盖个人套餐；企业账号的额度见 resolveBalance。
func (c *Client) FetchBalance(cred *Credential) (*Balance, error) {
	if cred == nil {
		return nil, fmt.Errorf("credential is required")
	}
	region := cred.Realm()
	if !c.enabledRealm(region) {
		return nil, fmt.Errorf("realm %s is disabled by plugin configuration", region)
	}
	// 请求体是上游要求的固定形状：缺 ProductCode 或分页会拿到空列表。
	now := time.Now()
	body := map[string]any{
		"PageNumber":               1,
		"PageSize":                 100,
		"ProductCode":              "p_tcaca",
		"Status":                   []int{0, 3},
		"PackageEndTimeRangeBegin": now.Format(packageEndLayout),
		"PackageEndTimeRangeEnd":   now.Add(365 * 101 * 24 * time.Hour).Format(packageEndLayout),
	}

	data, errCall := c.billingMeterJSON(cred, billingMeterPaths(region, billingMeterPath, billingMeterPathV2), body)
	if errCall != nil {
		return nil, errCall
	}
	var payload billingResourceResponse
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode balance: %w", errUnmarshal)
	}
	return c.resolveBalance(cred, region, parseBalance(payload))
}

// billingMeterPaths 返回某计费接口的候选路径。
//
// global 先试无 /v2 的路径，404 时回落带 /v2 的路径；CN 只有带 /v2 的。
// 这个差异来自上游两个部署的历史，不是笔误。
func billingMeterPaths(region Region, plain, v2 string) []string {
	if region.IsGlobal() {
		return []string{plain, v2}
	}
	return []string{v2}
}

// billingMeterJSON 依次尝试候选路径，返回第一个成功响应解开的 data。
//
// 只在 404 时换路径：其他错误换路径也救不回来。
func (c *Client) billingMeterJSON(cred *Credential, paths []string, body any) (json.RawMessage, error) {
	endpoints := GetEndpoints(cred.Realm())
	lastErr := fmt.Errorf("billing endpoint is unavailable")
	for _, path := range paths {
		data, errCall := c.billingJSON(cred, endpoints.BillingBase+path, http.MethodPost, body)
		if errCall == nil {
			return data, nil
		}
		lastErr = errCall
		if upstreamErr, okErr := errCall.(*core.Failure); !okErr || upstreamErr.Kind != core.FailureKindNotFound {
			return nil, errCall
		}
	}
	return nil, lastErr
}

// parseBalance 归一化余额响应。
//
// remain/total 是各套餐的**求和**（用户关心的是账号总量，不是单个套餐）。
func parseBalance(payload billingResourceResponse) *Balance {
	balance := &Balance{}
	for _, account := range payload.Response.Data.Accounts {
		remain, _, size := packageRemainUsed(account)
		balance.Remain += float64(remain)
		balance.Total += float64(size)
		balance.Packages = append(balance.Packages, BalancePackage{
			Name:     strings.TrimSpace(account.PackageName),
			Remain:   float64(remain),
			Total:    float64(size),
			ExpireAt: parsePackageExpiry(account.CycleEndTime),
		})
	}
	return balance
}

// resolveBalance 在企业账号拿到空个人额度时改用企业用量接口。
//
// 企业账号请求个人接口得到的是 HTTP 200 + 空 Accounts（不是错误），
// 直接展示就是 0 / 0，操作者会误以为账号已耗尽。企业额度只有企业端点
// 能给出，因此这里按「个人无额度 + 有企业租户 id」判定后回退。
// 非企业账号保持原行为：空 Accounts 就是真的没有额度。
func (c *Client) resolveBalance(cred *Credential, region Region, personal *Balance) (*Balance, error) {
	if personal.Total > 0 {
		return personal, nil
	}
	if strings.TrimSpace(cred.EnterpriseIDValue()) == "" {
		return personal, nil
	}
	return c.fetchEnterpriseBalance(cred, region)
}

// enterpriseUsageResponse 是企业用量接口的响应。
//
// credit 是**本周期已消耗**（小数积分），limitNum 是本周期额度，剩余 = 两者之差。
// cycleEndTime / cycleResetTime 只是周期回满时刻，不是资源包到期。
type enterpriseUsageResponse struct {
	Credit         *float64 `json:"credit"`
	LimitNum       *float64 `json:"limitNum"`
	CycleStartTime string   `json:"cycleStartTime"`
	CycleEndTime   string   `json:"cycleEndTime"`
	CycleResetTime string   `json:"cycleResetTime"`
}

// fetchEnterpriseBalance 用企业用量接口合成一份 Balance。
//
// 请求体给空对象：该端点按 token 里的企业租户定位，不需要分页与产品码；
// 个人资源包那套必填字段在这里不适用。
func (c *Client) fetchEnterpriseBalance(cred *Credential, region Region) (*Balance, error) {
	data, errCall := c.billingMeterJSON(cred,
		billingMeterPaths(region, enterpriseUsagePath, enterpriseUsagePathV2), map[string]any{})
	if errCall != nil {
		return nil, errCall
	}
	return parseEnterpriseBalance(data)
}

// parseEnterpriseBalance 归一化企业用量响应。
//
// limitNum 缺失视为契约不符：把它当 0 会重复「0 / 0」这个已被报告的假象。
func parseEnterpriseBalance(data json.RawMessage) (*Balance, error) {
	var payload enterpriseUsageResponse
	if errUnmarshal := json.Unmarshal(data, &payload); errUnmarshal != nil {
		return nil, fmt.Errorf("decode enterprise balance: %w", errUnmarshal)
	}
	if payload.LimitNum == nil {
		return nil, fmt.Errorf("decode enterprise balance: missing limitNum")
	}
	total := *payload.LimitNum
	used := 0.0
	if payload.Credit != nil {
		used = *payload.Credit
	}
	remain := total - used
	if remain < 0 {
		remain = 0
	}
	if remain > total {
		remain = total
	}
	balance := &Balance{Remain: remain, Total: total, Enterprise: true}
	period := ""
	start, end := strings.TrimSpace(payload.CycleStartTime), strings.TrimSpace(payload.CycleEndTime)
	if start != "" && end != "" {
		period = start + " ~ " + end
	}
	balance.Packages = append(balance.Packages, BalancePackage{
		Name:    "企业额度",
		Remain:  remain,
		Total:   total,
		ResetAt: parsePackageExpiry(core.FirstNonEmpty(payload.CycleResetTime, payload.CycleEndTime)),
		Period:  period,
	})
	return balance, nil
}

// parsePackageExpiry 解析套餐到期时刻（UTC+8 墙钟）；解析失败返回 0。
func parsePackageExpiry(raw string) int64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0
	}
	parsed, errParse := time.ParseInLocation(packageEndLayout, trimmed, cstZone)
	if errParse != nil {
		return 0
	}
	return parsed.Unix()
}

// billingJSON 发起一次计费域请求并解开信封。
func (c *Client) billingJSON(cred *Credential, url, method string, body any) (json.RawMessage, error) {
	encoded, errEncode := json.Marshal(body)
	if errEncode != nil {
		return nil, fmt.Errorf("encode request: %w", errEncode)
	}
	req, errReq := http.NewRequestWithContext(c.opts.Context, method, url, strings.NewReader(string(encoded)))
	if errReq != nil {
		return nil, fmt.Errorf("build request: %w", errReq)
	}
	c.applyBillingHeaders(req, cred)
	return c.doJSON(req)
}

// FetchQuota 汇总账号的额度信息（供宿主的管理端展示）。
func (c *Client) FetchQuota(cred *Credential) (*Balance, error) {
	return c.FetchBalance(cred)
}

// BuildQuotaResponse 把账号额度转成宿主的归一化额度结构。
//
// 结构层次：Summary 放关键数字（宿主的额度卡片直接读它），
// Groups/Buckets 放明细（展开后看）。
func BuildQuotaResponse(credential *Credential, balance *Balance) pluginapi.QuotaFetchResponse {
	region := credential.Realm()
	plan := string(region)
	planName := RealmDisplayName(region)
	if balance.Enterprise {
		// 企业额度与个人套餐是两条互不相通的额度线，不能混成同一个套餐名。
		plan = "enterprise"
		planName = "企业额度"
	}
	response := pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{
			Plan:     plan,
			TierName: planName,
		},
		Summary: []pluginapi.QuotaMetric{
			{Key: "credit_remain", Label: "剩余积分", Value: balance.Remain, Format: "number"},
			{Key: "credit_total", Label: "总额度", Value: balance.Total, Format: "number"},
		},
	}
	if balance.Total > 0 {
		used := balance.Total - balance.Remain
		if used < 0 {
			used = 0
		}
		response.Summary = append(response.Summary,
			pluginapi.QuotaMetric{Key: "credit_used", Label: "已用积分", Value: used, Format: "number"})
	}
	if len(balance.Packages) > 0 {
		buckets := make([]pluginapi.QuotaBucket, 0, len(balance.Packages))
		for _, pkg := range balance.Packages {
			fraction := 0.0
			if pkg.Total > 0 {
				fraction = pkg.Remain / pkg.Total
			}
			description := formatCreditAmount(pkg.Remain) + " / " + formatCreditAmount(pkg.Total)
			if pkg.Period != "" {
				description += " · 周期 " + pkg.Period
			}
			bucket := pluginapi.QuotaBucket{
				Window:            pkg.Name,
				RemainingFraction: fraction,
				Description:       description,
			}
			// 到期（作废）与回满（周期重置）分开取值：企业额度到点回满，
			// 不能被展示成「即将过期」。
			if pkg.ExpireAt > 0 {
				bucket.ResetTime = FormatUnixTime(pkg.ExpireAt)
			} else if pkg.ResetAt > 0 {
				bucket.ResetTime = FormatUnixTime(pkg.ResetAt)
			}
			buckets = append(buckets, bucket)
		}
		response.Groups = []pluginapi.QuotaGroup{{
			DisplayName: "资源包明细",
			Buckets:     buckets,
		}}
	}
	return response
}

// formatCreditAmount 把积分数量格式化为可读文本。
//
// 企业额度是小数（实测 4.11 / 2.59），个人额度是整数：两位小数既够
// 表达前者，也不会把后者写成 "2000.00"，同时隐去 float 运算的长尾。
func formatCreditAmount(value float64) string {
	return strconv.FormatFloat(math.Round(value*100)/100, 'f', -1, 64)
}

// FormatUnixTime 把 Unix 秒格式化为 RFC3339（无效值返回空串）。
func FormatUnixTime(seconds int64) string {
	if seconds <= 0 {
		return ""
	}
	return time.Unix(seconds, 0).UTC().Format(time.RFC3339)
}
