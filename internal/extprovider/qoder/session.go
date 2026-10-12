// Package qoder: session.go — 阶段 3：会话隔离与长会话压缩。
//
// 移植自 9router-qoder-plus（HEAD 290c68c）：
//   - 会话隔离：open-sse/utils/sessionManager.js（L137-224 resolveSessionIdentity）
//     ——Qoder 服务端按 session_id 保有状态，账号不是对话边界：同一账号的两个
//     客户端可合法并行，不能共用账号级会话。fork 对 scope="qoder" 的裁决是
//     **无客户端会话键时生成一次性 sessionId（ephemeral），绝不复用账号级状态**
//     （L211-217 注释原文：完整消息历史每次请求都发，隔离比共享安全）。
//   - 长会话压缩：open-sse/translator/concerns/qoderCompaction.js（40 行全文）
//   - src/shared/constants/codexCompat.js——固定 1M 上下文窗口 / 900K 自动
//     压缩线（ratio 0.9），压缩提示词原文移植，压缩内容用前缀+base64url 编码
//     往返（parse/build 对称）。
//
// buddyhub 侧差异：会话键解析不自己实现，复用 internal/session.ExtractKey
// （已覆盖 conversation_id/prompt_cache_key 等客户端字段，见该包契约）；
// 本文件只负责"从会话键派生稳定的上游 session_id"与压缩原语。
package qoder

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// ── 会话隔离（fork sessionManager.js 语义）──

// 长上下文窗口与自动压缩线（fork src/shared/constants/codexCompat.js L2-3：
// QODER_CONTEXT_WINDOW=1_000_000 / QODER_AUTO_COMPACT_TOKEN_LIMIT=900_000，
// ratio = 0.9，固定不变——Qoder 全模型同一条线，不按 per-model 漂移）。
const (
	// ContextWindow Qoder 固定上下文窗口（1M tokens）。
	ContextWindow = 1_000_000
	// AutoCompactTokenLimit 自动压缩触发线（900K tokens = 窗口的 90%）。
	AutoCompactTokenLimit = 900_000
)

// sessionTTL / sessionMaxEntries 会话存储 TTL 与容量上限
// （fork MEMORY_CONFIG.sessionTtlMs 语义 + 1000 条安全帽）。
const (
	sessionTTL        = 30 * time.Minute
	sessionMaxEntries = 1000
)

// sessionEntry 一条会话记录（fork runtimeSessionStore 的 value 形状）。
type sessionEntry struct {
	sessionID string
	lastUsed  time.Time
}

// SessionStore 会话键 → 上游 session_id 的稳定映射（fork runtimeSessionStore）。
// 同一会话键在 TTL 内始终得到同一 session_id（prompt cache 连续性）；
// 不同会话键得到不同 session_id（账号级状态不串话）。
// 并发安全（网关多请求并行）；TTL 惰性清理（读路径顺手过期）。
type SessionStore struct {
	mu      sync.Mutex
	entries map[string]sessionEntry
}

// NewSessionStore 建空存储。
func NewSessionStore() *SessionStore {
	return &SessionStore{entries: make(map[string]sessionEntry)}
}

// SessionIDFor 返回会话键对应的上游 session_id：
//   - 键为空 → 一次性 UUID（fork scope="qoder" 的 ephemeral 分支：无客户端
//     会话键时隔离优先，绝不复用账号/连接级状态）；
//   - 命中且未过期 → 既有 id（TTL 内刷新）；
//     -未命中/已过期 → 生成新 id 并记录（超容量逐最旧）。
func (s *SessionStore) SessionIDFor(key string) string {
	if key == "" {
		return newRequestID() // ephemeral：每个独立首回合一个身份
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.entries[key]; ok && time.Since(e.lastUsed) < sessionTTL {
		e.lastUsed = time.Now()
		s.entries[key] = e
		return e.sessionID
	}
	if len(s.entries) >= sessionMaxEntries {
		var oldestKey string
		var oldest time.Time
		for k, e := range s.entries {
			if oldestKey == "" || e.lastUsed.Before(oldest) {
				oldestKey, oldest = k, e.lastUsed
			}
		}
		delete(s.entries, oldestKey)
	}
	id := newRequestID()
	s.entries[key] = sessionEntry{sessionID: id, lastUsed: time.Now()}
	return id
}

// Len 当前条目数（测试/观测用）。
func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// ── 长会话压缩（fork qoderCompaction.js 语义）──

// compactionPrefix 压缩内容前缀（fork COMPACTION_PREFIX 原文，v1）。
const compactionPrefix = "9router:qoder-compact:v1:"

// CompactionPrompt 压缩提示词（fork QODER_COMPACTION_PROMPT 三句原文）：
// 总结对话、保留目标/决策/约束/进度/文件符号/命令结果/未决错误/下一步，
// 不调工具、只回总结、无废话。
const CompactionPrompt = "Create a compact continuation summary of the conversation above. " +
	"Preserve the user's goals, requirements, decisions, constraints, current progress, relevant files and symbols, command results, unresolved errors, and exact next steps. " +
	"Do not call tools. Return only the summary, with no preamble or commentary."

// CompactionPayload 压缩内容载荷（fork buildQoderCompactionContent 的 payload）。
type CompactionPayload struct {
	Version   int    `json:"v"`
	Model     string `json:"model,omitempty"`
	CreatedAt int64  `json:"created_at"`
	Summary   string `json:"summary"`
}

// BuildCompactionContent 构造压缩内容串：prefix + base64url(JSON(payload))
// （fork buildQoderCompactionContent L15-23 原文语义）。
func BuildCompactionContent(p CompactionPayload) string {
	if p.Version == 0 {
		p.Version = 1
	}
	if p.CreatedAt == 0 {
		p.CreatedAt = time.Now().UnixMilli()
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return compactionPrefix + base64.RawURLEncoding.EncodeToString(raw)
}

// ParseCompactionContent 解析压缩内容串；非本格式/损坏/v≠1/空 summary 返回 nil
// （fork parseQoderCompactionContent L25-38 原文语义）。
func ParseCompactionContent(value string) *CompactionPayload {
	if !strings.HasPrefix(value, compactionPrefix) {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, compactionPrefix))
	if err != nil {
		return nil
	}
	var p CompactionPayload
	if json.Unmarshal(raw, &p) != nil {
		return nil
	}
	if p.Version != 1 || strings.TrimSpace(p.Summary) == "" {
		return nil
	}
	return &p
}

// ShouldCompact 判断估算 token 数是否达到自动压缩线（fork computeAutoCompactLimit
// 的 ratio 语义：固定 90% 线，min 120K / max 1M 的钳制在固定窗口下等价于 900K）。
func ShouldCompact(estimatedTokens int) bool {
	return estimatedTokens >= AutoCompactTokenLimit
}

// IsCompactionRequest 判断请求是否是压缩触发（fork isQoderCompactionRequest）：
// body._compact === true，或 input 数组含 compaction_trigger 项。
func IsCompactionRequest(incoming []byte) bool {
	var probe struct {
		Compact *bool `json:"_compact"`
		Input   []struct {
			Type string `json:"type"`
		} `json:"input"`
		Messages []struct {
			Type string `json:"type"`
		} `json:"messages"`
	}
	if json.Unmarshal(incoming, &probe) != nil {
		return false
	}
	if probe.Compact != nil && *probe.Compact {
		return true
	}
	for _, it := range probe.Input {
		if it.Type == "compaction_trigger" {
			return true
		}
	}
	for _, m := range probe.Messages {
		if m.Type == "compaction_trigger" {
			return true
		}
	}
	return false
}

// EstimateTokens 粗估请求 token 数（fork 的估算器是字符级启发式，qoder.js
// L1735 注释：实测 ~1M 估算 vs 524K 实际，宁可高估不可低估）。
// 口径：非 ASCII 按 1 token/字（中文场景），ASCII 按 ~4 字符/token 加权。
// 只用于压缩线判断，不用于计费。
func EstimateTokens(messages []Message) int {
	total := 0
	for _, m := range messages {
		total += estimateTextTokens(contentText(m.Content))
	}
	return total
}

// Message 压缩估算用的最小消息形状（OpenAI messages 语义）。
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

// contentText 把 string 或 [{type:text,text:...}] 数组内容收敛成纯文本。
func contentText(c any) string {
	switch v := c.(type) {
	case string:
		return v
	case []any:
		var b strings.Builder
		for _, item := range v {
			if obj, ok := item.(map[string]any); ok {
				if t, ok := obj["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// estimateTextTokens 字符级 token 估算：CJK 每字 1 token，其余 ~4 字符 1 token。
func estimateTextTokens(s string) int {
	if s == "" {
		return 0
	}
	cjk := 0
	other := 0
	for _, r := range s {
		if r >= 0x2E80 && r <= 0x9FFF || r >= 0xF900 && r <= 0xFAFF ||
			r >= 0xFF00 && r <= 0xFFEF || r >= 0x3000 && r <= 0x303F {
			cjk++
		} else {
			other++
		}
	}
	return cjk + (other+3)/4
}

// StableSessionHash 会话键 → 16 位稳定哈希（fork stableHash L1090-1098 语义：
// sha256 前 16 hex 字符）。桥接层用它把客户端会话键派生成上游 session_id，
// 同键同值、跨进程稳定（区别于进程内随机的 SessionIDFor）。
func StableSessionHash(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	return hex.EncodeToString(sum[:8])
}
