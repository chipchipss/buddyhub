// Package upstream: reasoningrepair.go DeepSeek 多轮思维链历史修复。
//
// 移植自 workbuddy-gateway reasoningrepair.go（逐字等价，日志钩子简化）。
//
// 修复规则：
//   - 仅对 deepseek* 模型生效；
//   - 客户端显式开启 thinking 或历史已有推理痕迹时启用；
//   - 已有 reasoning_content 字符串不覆盖；reasoning 字段的推理复制到
//     reasoning_content；缺失补空串；上游部分租户要求 len(reasoning)>0，
//     空时给单个空格占位（不捏造思维链）。
package upstream

import "strings"

// ReasoningRepairReport 只统计字段形状与数量，不保存或记录思维链正文。
type ReasoningRepairReport struct {
	Applied                bool
	ThinkingEnabled        bool
	HasTrace               bool
	AssistantMessages      int
	PreservedContent       int
	CopiedFromReasoning    int
	EmptyContentAdded      int
	InvalidContentReplaced int
	ReasoningMirrored      int
	ReasoningPlaceholders  int
}

// RepairReasoningHistory 在发往上游的 Chat 消息形态上做一次共享修复。
// 必须在 tool_call 序列修复后、json.Marshal 前调用。
func RepairReasoningHistory(obj map[string]any) ReasoningRepairReport {
	var report ReasoningRepairReport
	model, _ := obj["model"].(string)
	if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "deepseek") {
		return report
	}
	messages, ok := obj["messages"].([]any)
	if !ok || len(messages) == 0 {
		return report
	}

	report.ThinkingEnabled = thinkingExplicitlyEnabled(obj)
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		if _, exists := msg["reasoning_content"]; exists {
			report.HasTrace = true
			break
		}
		if reasoning, ok := msg["reasoning"].(string); ok && reasoning != "" {
			report.HasTrace = true
			break
		}
	}
	// 显式关闭且无既有推理痕迹时不碰历史。关闭后续思考不意味着要抹掉此前
	// 已经产生的推理：有痕迹仍须回放，避免多轮上下文不一致。
	if !report.ThinkingEnabled && !report.HasTrace {
		return report
	}
	report.Applied = true
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok || msg["role"] != "assistant" {
			continue
		}
		report.AssistantMessages++
		rc, isString := msg["reasoning_content"].(string)
		if isString {
			report.PreservedContent++
		} else {
			if _, exists := msg["reasoning_content"]; exists {
				report.InvalidContentReplaced++
			}
			if original, ok := msg["reasoning"].(string); ok {
				rc = original
				report.CopiedFromReasoning++
			} else {
				rc = ""
				report.EmptyContentAdded++
			}
			msg["reasoning_content"] = rc
		}
		if existing, ok := msg["reasoning"].(string); ok && existing != "" {
			continue
		}
		if rc != "" {
			msg["reasoning"] = rc
			report.ReasoningMirrored++
		} else {
			// 对齐上游部分租户对 len(reasoning)>0 的校验；单个空格不是推理内容。
			msg["reasoning"] = " "
			report.ReasoningPlaceholders++
		}
	}
	return report
}

// thinkingExplicitlyEnabled 仅识别客户端已明确开启的思考信号；不替用户开启思考、
// 不更改原有 effort 或 thinking.type。显式 disabled 的优先级高于其他字段。
func thinkingExplicitlyEnabled(obj map[string]any) bool {
	if thinking, ok := obj["thinking"].(map[string]any); ok {
		switch strings.ToLower(strings.TrimSpace(stringField(thinking, "type"))) {
		case "disabled":
			return false
		case "enabled":
			return true
		}
	}
	for _, key := range []string{"reasoning_effort", "reasoningEffort", "reasoning_summary"} {
		if enabledEffort(stringField(obj, key)) {
			return true
		}
	}
	if reasoning, ok := obj["reasoning"].(map[string]any); ok {
		return enabledEffort(stringField(reasoning, "effort"))
	}
	return false
}

func stringField(obj map[string]any, key string) string {
	value, _ := obj[key].(string)
	return value
}

func enabledEffort(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "off", "disabled":
		return false
	default:
		return true
	}
}
