package core

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"freetier2api-plugin/cpasdk/pluginapi"
)

// 本文件提供各供应商共用的解析与转换助手。
//
// 这些函数原本在多个供应商包里各有一份（签名还漂移过），统一收敛到这里：
// 同一种语义只允许有一份实现，供应商包只负责"哪些键、什么特征"的差异。

// FirstNonEmpty 返回第一个去空白后非空的字符串（返回去空白后的值）。
func FirstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

// GetString 依次尝试多个键，返回第一个非空字符串（去空白）。
func GetString(m map[string]any, keys ...string) string {
	if m == nil {
		return ""
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		if text, okText := value.(string); okText {
			if trimmed := strings.TrimSpace(text); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// GetMap 依次尝试多个键，返回第一个 map 值（缺失或类型不符返回 nil）。
func GetMap(m map[string]any, keys ...string) map[string]any {
	if m == nil {
		return nil
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		if sub, okSub := value.(map[string]any); okSub {
			return sub
		}
	}
	return nil
}

// GetInt64 依次尝试多个键，返回第一个可转换的整数。
//
// 不做任何单位归一化：需要把毫秒转成秒的供应商用 GetUnixSeconds。
func GetInt64(m map[string]any, keys ...string) int64 {
	if m == nil {
		return 0
	}
	for _, key := range keys {
		value, okValue := m[key]
		if !okValue || value == nil {
			continue
		}
		switch number := value.(type) {
		case float64:
			return int64(number)
		case int64:
			return number
		case int:
			return int64(number)
		case json.Number:
			if parsed, errParse := number.Int64(); errParse == nil {
				return parsed
			}
		}
	}
	return 0
}

// millisecondThreshold 是「这个时间戳看起来是毫秒」的判定阈值。
//
// 1e12 秒约等于公元 33658 年，正常秒级时间戳不可能超过它；反之毫秒级
// 时间戳（13 位）必然超过。上游各家混用两种单位，统一在这里归一化。
const millisecondThreshold = 1e12

// GetUnixSeconds 与 GetInt64 相同，但把明显是毫秒的时间戳归一化成秒。
func GetUnixSeconds(m map[string]any, keys ...string) int64 {
	value := GetInt64(m, keys...)
	if value > millisecondThreshold {
		return value / 1000
	}
	return value
}

// FileNameMatchesConvention 判断文件名是否符合某个供应商的命名约定。
//
// 约定：<name>.json / <name>-<id>.json / <name>_<id>.json / <name>.<id>.json，
// 或名字里含 -<name> / _<name> 段（如 my-cline.json）。
func FileNameMatchesConvention(fileName string, names ...string) bool {
	base := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(fileName), ".json"))
	if base == "" {
		return false
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		name = strings.ToLower(name)
		if base == name {
			return true
		}
		for _, separator := range []string{"-", "_", "."} {
			if strings.HasPrefix(base, name+separator) ||
				strings.Contains(base, separator+name) {
				return true
			}
		}
	}
	return false
}

// VendorFieldVerdict 按凭证里的 vendor 字段判定归属。
//
// 返回 (decided, claimed)：
//   - decided=false 表示 vendor 字段没有给出结论（缺失，或只是家族别名，
//     例如 minimaxcode 的 "minimax"、trae 的 "trae"），调用方继续看其它证据；
//   - decided=true 且 claimed=true 表示明确属于本供应商；
//   - decided=true 且 claimed=false 表示明确属于别家，**必须拒绝**。
//
// 这一段是"归属必须偏严"的第一道闸：显式声明了别家就绝不认领。
func VendorFieldVerdict(raw map[string]any, vendorID string, aliases ...string) (decided, claimed bool) {
	declared := strings.ToLower(GetString(raw, "vendor"))
	if declared == "" {
		return false, false
	}
	if declared == strings.ToLower(strings.TrimSpace(vendorID)) {
		return true, true
	}
	for _, alias := range aliases {
		if declared == strings.ToLower(strings.TrimSpace(alias)) {
			return false, false
		}
	}
	return true, false
}

// NativeExtractor 从归一化凭证里取回供应商原生对象。
type NativeExtractor[T any] func(*Credential) (*T, bool)

// ConvertModels 把供应商原生模型清单批量转成宿主契约类型。
//
// vendorID 传空串：本插件的模型以裸名注册（见 SplitModelID 的说明）。
// convert 返回 false 表示跳过该条目（通常是缺 ID）。
func ConvertModels[T any](models []T, convert func(T) (ModelDescriptor, bool)) []pluginapi.ModelInfo {
	out := make([]pluginapi.ModelInfo, 0, len(models))
	for _, model := range models {
		descriptor, okModel := convert(model)
		if !okModel {
			continue
		}
		out = append(out, ModelInfoToPluginAPI("", descriptor))
	}
	return out
}

// DefaultMaxOutputTokens 是上游没给输出上限时的兜底值。
//
// 取 16384：这是各家上游对主流模型的常见默认值，也是 qoder2api 与
// workbuddy2api 各自的兜底清单里一致采用的值。
const DefaultMaxOutputTokens int64 = 16384

// ContentTypeJSON 是插件返回给宿主的 JSON 内容类型。
const ContentTypeJSON = "application/json; charset=utf-8"

// DefaultContextWindowTokens 是模型元数据缺失时的兜底上下文窗口。
//
// 与 workbuddy 兜底表一致（1,000,000）：取小了会让客户端误以为超出上下文，
// 取大了只会让上游按自身限制拒绝——后者可恢复。
const DefaultContextWindowTokens int64 = 1000000

// DefaultContextWindow 返回兜底上下文窗口（忽略模型名）。
func DefaultContextWindow(_ string) int64 { return DefaultContextWindowTokens }

// 本文件提供与供应商无关的请求解析助手。

// ExtractClientIP 从请求头里取客户端 IP（X-Forwarded-For 优先）。
func ExtractClientIP(header http.Header) string {
	if header == nil {
		return ""
	}
	if forwarded := strings.TrimSpace(header.Get("X-Forwarded-For")); forwarded != "" {
		if idx := strings.IndexByte(forwarded, ','); idx >= 0 {
			forwarded = forwarded[:idx]
		}
		return strings.TrimSpace(forwarded)
	}
	return strings.TrimSpace(header.Get("X-Real-IP"))
}

// 本文件提供供应商共用的随机串与 PKCE 助手。
//
// 这些逻辑原本在 codearts/qoder/zcode/opencodezen/minimaxcode/workbuddy
// 各有一份（签名还不一致）；统一到这里，随机源不可用时统一走确定性兜底。

var randomFallbackCounter int64

// RandomHex 生成 n 字节随机十六进制串（2n 个字符）。
//
// 随机源不可用时退回基于时间与计数的确定性串：这些 id 只用于会话/请求标识的
// 唯一性，退化的熵比让整条链路失败更可接受。
func RandomHex(n int) string {
	if n <= 0 {
		n = 16
	}
	buf := make([]byte, n)
	if _, errRand := rand.Read(buf); errRand != nil {
		return fmt.Sprintf("fallback-%d-%d", time.Now().UnixNano(), atomic.AddInt64(&randomFallbackCounter, 1))
	}
	return hex.EncodeToString(buf)
}

// jwtPayload 解码 JWT 的第二段 payload（不验签）；解不出返回 nil。
func jwtPayload(token string) []byte {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) < 2 {
		return nil
	}
	payload, errDecode := base64.RawURLEncoding.DecodeString(parts[1])
	if errDecode == nil {
		return payload
	}
	seg := parts[1]
	if rem := len(seg) % 4; rem > 0 {
		seg += strings.Repeat("=", 4-rem)
	}
	if payload, errDecode = base64.URLEncoding.DecodeString(seg); errDecode == nil {
		return payload
	}
	return nil
}

// MaskToken 返回脱敏后的 token（展示用）：保留前 4 后 4 位，中间用 * 填充。
// 不超过 8 位时全部遮挡；空值返回空串。
func MaskToken(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 8 {
		return strings.Repeat("*", len(trimmed))
	}
	return trimmed[:4] + strings.Repeat("*", len(trimmed)-8) + trimmed[len(trimmed)-4:]
}

// JWTExpiry 解码 JWT payload 的 exp（秒），解不出返回 0。不验签，只读 exp。
func JWTExpiry(token string) int64 {
	payload := jwtPayload(token)
	if len(payload) == 0 {
		return 0
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil || claims.Exp <= 0 {
		return 0
	}
	return claims.Exp
}

// JWTUserID 解码 JWT payload 里的用户 id（依次尝试常见键），解不出返回空。
func JWTUserID(token string) string {
	payload := jwtPayload(token)
	if len(payload) == 0 {
		return ""
	}
	var claims map[string]any
	if errUnmarshal := json.Unmarshal(payload, &claims); errUnmarshal != nil {
		return ""
	}
	return GetString(claims, "user_id", "userId", "uid", "userID")
}

// TokenFingerprint 返回 token 的稳定短指纹（SHA-256 前 12 位十六进制）。
//
// 用于「上游不给 uid」时给账号一个稳定且互不相同的文件名后缀（见 FileNameFor）：
// 同一份凭证每次算出同一个值，不同凭证实际上不可能相同，且不可逆、不落盘。
// 空值返回空串。
func TokenFingerprint(token string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(trimmed))
	return hex.EncodeToString(sum[:6])
}

// NewPKCEPair 生成 PKCE 的 verifier 与 S256 challenge。
func NewPKCEPair() (verifier, challenge string, err error) {
	buffer := make([]byte, 32)
	if _, errRead := rand.Read(buffer); errRead != nil {
		return "", "", errRead
	}
	verifier = base64.RawURLEncoding.EncodeToString(buffer)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return verifier, challenge, nil
}

// FirstNonZero 返回第一个非零整数。
func FirstNonZero(values ...int64) int64 {
	for _, value := range values {
		if value != 0 {
			return value
		}
	}
	return 0
}

// FloatFromAny 宽容解析数字：接受 JSON 数字、json.Number 与数字字符串。
//
// 上游同一个字段在不同接口 / 不同版本上可能下发 12 / "12" / null，严格解码会
// 让整条响应解析失败，把一次成功签到报成 decode 错误。无法解析返回 0。
func FloatFromAny(value any) float64 {
	switch number := value.(type) {
	case float64:
		return number
	case float32:
		return float64(number)
	case int:
		return float64(number)
	case int64:
		return float64(number)
	case json.Number:
		if parsed, errParse := number.Float64(); errParse == nil {
			return parsed
		}
	case string:
		if parsed, errParse := strconv.ParseFloat(strings.TrimSpace(number), 64); errParse == nil {
			return parsed
		}
	}
	return 0
}

// IntFromAny 把 JSON 数值转成 int（缺失或非法返回 0，小数截断）。
func IntFromAny(value any) int {
	return int(FloatFromAny(value))
}

// StringField 读取 map 中的字符串字段（去空白）。
func StringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return strings.TrimSpace(value)
}

// MaxInt 返回较大的整数。
func MaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ContainsAny 报告文本是否包含任一标记。
func ContainsAny(text string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// SleepContext 是可取消的睡眠；返回 false 表示上下文已取消。
func SleepContext(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// RawStringField 读取 map 中的字符串字段（不裁剪空白）。
func RawStringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	value, _ := m[key].(string)
	return value
}
