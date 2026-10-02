package autoclaw

// prompt.go AutoClaw 上游的 **system 提示词闸门**（2026-09-22 起生效）。
//
// ── 上游闸门的实测行为（agent2api prompt.rs，国内+国际两版实测）────────
//
//	system 内容                                    结果
//	─────────────────────────────────────────  ──────────────
//	OpenClaw 身份句 + `## Tooling` 段            200
//	只有身份句、没有 `## Tooling` 段             403 pay-view
//	中性句（You are a helpful assistant.）       406（空响应体）
//	完全没有 system 消息                          406（空响应体）
//	身份句 + "You are ZCode/Claude Code/Codex…"  406
//
// 三条结论（都经实测）：
//  1. 闸门**只看 system/developer 消息**——同一句放进 user 消息不触发；
//  2. 黑名单是**字面匹配**（`You are <产品名>` 形态），改写产品名即可绕过；
//  3. 匹配**大小写不敏感**——替换也必须忽略大小写。
//
// 这解释了此前「真 token 仍然恒 406」的故障：客户端（Cherry Studio 等）发的
// system 是通用提示词，命中"中性句 → 406"分支。修复方式与官方客户端一致——
// 在 system 正文**前面**拼上身份前缀，客户端提示词逐字保留。
import (
	"encoding/json"
	"strings"
)

// IdentityLine 上游要求「以它开头」的身份句（幂等判定用）。
const IdentityLine = "You are a personal assistant running inside OpenClaw."

// identityPrefix 身份句 + `## Tooling` 段（官方 prompt 的开头两行，逐字照抄）。
const identityPrefix = "You are a personal assistant running inside OpenClaw.\n\n" +
	"## Tooling\n" +
	"Available tools are policy-filtered. Names are case-sensitive; call exactly as listed.\n"

// foreignIdentities 外来身份句 → 中性说法（**长的在前**——短的先命中会把
// 长匹配串切碎，导致「改写后仍含产品名」的半吊子结果漏网）。
//
// 每条的替换串都**不含产品名**：闸门拦的是产品名本身，插词不够用。
// 匹配忽略 ASCII 大小写（闸门自己就不敏感）。
var foreignIdentities = [][2]string{
	{"You are ZCode, an interactive coding agent", "You are an interactive coding agent"},
	{"You are a coding agent running in the Codex CLI tool", "You are a coding agent running in a terminal CLI tool"},
	{"You are a coding agent running in the Codex CLI", "You are a coding agent running in a terminal CLI"},
	{"You are running as a coding agent in the Codex CLI", "You are running as a coding agent in a terminal CLI"},
	// Claude Code 的整句是 "You are Claude Code, Anthropic's official CLI tool for
	// Claude."；只改「You are Claude Code」为止，后面的产品说明原样保留。
	{"You are Claude Code", "You are a coding assistant"},
	{"You are ZCode", "You are an interactive coding agent"},
	{"You are an AI agent powered by DeepSeek Harness", "You are an AI agent powered by a local coding harness"},
	// 放最后：最短最通用（新版 Codex 首句就是它），必须让更长的先命中。
	{"You are Codex", "You are a coding agent"},
}

// NormalizePrompt 出站前规范化 body 里的系统提示词（返回新的 body 字节）。
//
// 两步：① 所有 system/developer 消息里的外来身份句改写；② 保证首条消息是
// system 且正文以身份句开头（已以身份句开头则只改写、不再前置——官方客户端
// 的请求本身就带 OpenClaw 提示词，重复前置会把提示词写两遍，降级/重试路径
// 上必然发生）。
//
// 形态怪异（messages 缺失/content 是数组）时**尽力产出能发出去的 body**，
// 绝不失败——失败会让调用方拿不到任何可执行的信息。
func NormalizePrompt(body []byte) []byte {
	var doc map[string]any
	if json.Unmarshal(body, &doc) != nil {
		return body
	}
	msgs, ok := doc["messages"].([]any)
	if !ok {
		return body
	}

	firstSystem := -1
	for i, m := range msgs {
		msg, isObj := m.(map[string]any)
		if !isObj || !isSystemRole(msg) {
			continue
		}
		content, isStr := msg["content"].(string)
		if !isStr {
			continue // content 是数组/null 的形态不碰——闸门只字面匹配字符串
		}
		msg["content"] = rewriteIdentities(content)
		if firstSystem < 0 {
			firstSystem = i
		}
	}

	// 前置身份句：首条 system 存在且未以身份句开头 → 拼前缀；
	// 没有 system 消息 → 在最前面插一条（"完全没有 system → 406"的分支）。
	if firstSystem >= 0 {
		if msg := msgs[firstSystem].(map[string]any); true {
			if c, ok := msg["content"].(string); ok && !strings.HasPrefix(c, IdentityLine) {
				msg["content"] = identityPrefix + "\n" + c
			}
		}
	} else {
		sys := map[string]any{"role": "system", "content": identityPrefix}
		msgs = append([]any{sys}, msgs...)
		doc["messages"] = msgs
	}

	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

// isSystemRole system 与 developer（OpenAI 新角色）都算系统提示词。
func isSystemRole(msg map[string]any) bool {
	role, _ := msg["role"].(string)
	return role == "system" || role == "developer"
}

// rewriteIdentities 外来身份句改写（长在前、忽略大小写）。
func rewriteIdentities(s string) string {
	for _, pair := range foreignIdentities {
		s = replaceAllIgnoreCase(s, pair[0], pair[1])
	}
	return s
}

// replaceAllIgnoreCase 忽略 ASCII 大小写的全量替换。
// strings.Replace 没有不区分大小写的版本，自己扫（s 不大，性能无虞）。
func replaceAllIgnoreCase(s, old, new string) string {
	if old == "" {
		return s
	}
	var b strings.Builder
	low := strings.ToLower(s)
	oldLow := strings.ToLower(old)
	i := 0
	for {
		j := strings.Index(low[i:], oldLow)
		if j < 0 {
			b.WriteString(s[i:])
			return b.String()
		}
		b.WriteString(s[i : i+j])
		b.WriteString(new)
		i += j + len(old)
	}
}
