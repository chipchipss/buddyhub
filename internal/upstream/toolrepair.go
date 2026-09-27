// Package upstream: toolrepair.go 工具调用序列自愈。
//
// 移植自 workbuddy-gateway toolrepair.go（协议修复层，与原实现逐字等价，
// 仅去掉 gateway 的 debug 日志钩子，改为直接 log.Printf）。
//
// 规则：
//   - 先按 tool call ID 做对称裁剪：无结果的调用、无调用的结果、重复项全部删除；
//   - 仅当同一批次至少有两个调用时，才把夹在调用和结果之间的普通消息移到结果之后；
//   - Responses API 转换产生的连续/交错单调用 assistant 消息会合并为一个并行批次；
//   - 结果按原始出现顺序保留，不按调用顺序重排。
//
// 修复目标：避免国际站返回 11148 tool_call_sequence_broken。
package upstream

import (
	"log"
	"strings"
)

// ToolSequenceRepairReport 修复统计（不含任何消息正文）。
type ToolSequenceRepairReport struct {
	OriginalMessages   int
	FinalMessages      int
	ParallelBatches    int
	MovedMessages      int
	MergedCallMessages int
	DroppedCalls       int
	DroppedOutputs     int
	TopologyBefore     string
	TopologyAfter      string
}

func (r ToolSequenceRepairReport) Changed() bool {
	return r.OriginalMessages != r.FinalMessages || r.MovedMessages > 0 || r.MergedCallMessages > 0 ||
		r.DroppedCalls > 0 || r.DroppedOutputs > 0
}

// RepairToolMessageSequence 修复发往上游的现代 tool_calls/tool 序列。
func RepairToolMessageSequence(obj map[string]any) ToolSequenceRepairReport {
	messages, ok := obj["messages"].([]any)
	report := ToolSequenceRepairReport{OriginalMessages: len(messages), FinalMessages: len(messages)}
	if !ok || len(messages) == 0 {
		return report
	}
	topologyBefore := toolMessageTopology(messages)

	// 每个 ID 只保留第一条调用，以及位于该调用之后的第一条结果。
	callMessageIndex := make(map[string]int)
	callEntryIndex := make(map[string]int)
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok || roleOfMessage(message) != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for entryIndex, callAny := range calls {
			id := toolCallID(callAny)
			if id == "" {
				continue
			}
			if _, exists := callMessageIndex[id]; !exists {
				callMessageIndex[id] = messageIndex
				callEntryIndex[id] = entryIndex
			}
		}
	}

	outputMessageIndex := make(map[string]int)
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok || roleOfMessage(message) != "tool" {
			continue
		}
		id := toolOutputID(message)
		callIndex, exists := callMessageIndex[id]
		if !exists || messageIndex <= callIndex {
			continue
		}
		if _, exists := outputMessageIndex[id]; !exists {
			outputMessageIndex[id] = messageIndex
		}
	}

	pairedIDs := make(map[string]bool, len(outputMessageIndex))
	for id := range outputMessageIndex {
		pairedIDs[id] = true
	}

	filtered := make([]any, 0, len(messages))
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok {
			filtered = append(filtered, messageAny)
			continue
		}

		switch roleOfMessage(message) {
		case "assistant":
			calls, hasCalls := message["tool_calls"].([]any)
			if !hasCalls || len(calls) == 0 {
				filtered = append(filtered, messageAny)
				continue
			}
			kept := make([]any, 0, len(calls))
			for entryIndex, callAny := range calls {
				id := toolCallID(callAny)
				if id != "" && pairedIDs[id] && callMessageIndex[id] == messageIndex && callEntryIndex[id] == entryIndex {
					kept = append(kept, callAny)
				} else {
					report.DroppedCalls++
				}
			}
			if len(kept) > 0 {
				message["tool_calls"] = kept
				filtered = append(filtered, messageAny)
				continue
			}
			delete(message, "tool_calls")
			if assistantMessageHasPayload(message) {
				filtered = append(filtered, messageAny)
			}

		case "tool":
			id := toolOutputID(message)
			if id != "" && pairedIDs[id] && outputMessageIndex[id] == messageIndex {
				filtered = append(filtered, messageAny)
			} else {
				report.DroppedOutputs++
			}

		default:
			filtered = append(filtered, messageAny)
		}
	}

	repaired := repairParallelToolBatches(filtered, &report)
	obj["messages"] = repaired
	report.FinalMessages = len(repaired)
	if report.Changed() {
		report.TopologyBefore = topologyBefore
		report.TopologyAfter = toolMessageTopology(repaired)
	}
	return report
}

// toolMessageTopology 有界消息拓扑签名：仅角色与工具调用 ID（最多 200 条），不含正文。
func toolMessageTopology(messages []any) string {
	const maxEntries = 200
	parts := make([]string, 0, maxEntries)
	for i, messageAny := range messages {
		if i >= maxEntries {
			parts = append(parts, "...")
			break
		}
		message, ok := messageAny.(map[string]any)
		if !ok {
			parts = append(parts, "?")
			continue
		}
		role := roleOfMessage(message)
		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				ids := make([]string, 0, len(calls))
				for _, callAny := range calls {
					ids = append(ids, toolCallID(callAny))
				}
				parts = append(parts, "assistant["+strings.Join(ids, ",")+"]")
				continue
			}
		}
		if role == "tool" {
			parts = append(parts, "tool["+toolOutputID(message)+"]")
			continue
		}
		parts = append(parts, role)
	}
	return strings.Join(parts, " > ")
}

// repairParallelToolBatches 合并连续/交错的单调用 assistant 消息为并行批次。
func repairParallelToolBatches(messages []any, report *ToolSequenceRepairReport) []any {
	out := make([]any, 0, len(messages))
	for i := 0; i < len(messages); {
		first, firstCalls, ok := assistantCalls(messages[i])
		if !ok {
			out = append(out, messages[i])
			i++
			continue
		}

		batchCalls := append([]any(nil), firstCalls...)
		deferred := make([]any, 0)
		mergedMessages := 0
		j := i + 1
		for j < len(messages) {
			if next, calls, hasCalls := assistantCalls(messages[j]); hasCalls {
				if !assistantCallOnly(next) {
					break
				}
				batchCalls = append(batchCalls, calls...)
				mergedMessages++
				j++
				continue
			}
			if message, ok := messages[j].(map[string]any); ok && roleOfMessage(message) == "tool" {
				break
			}
			deferred = append(deferred, messages[j])
			j++
		}

		if len(batchCalls) < 2 {
			out = append(out, messages[i])
			i++
			continue
		}

		ids := make(map[string]bool, len(batchCalls))
		for _, callAny := range batchCalls {
			if id := toolCallID(callAny); id != "" {
				ids[id] = true
			}
		}
		outputs := make([]any, 0, len(ids))
		found := make(map[string]bool, len(ids))
		k := j
		for k < len(messages) && len(found) < len(ids) {
			if _, _, nextBatch := assistantCalls(messages[k]); nextBatch {
				break
			}
			if message, ok := messages[k].(map[string]any); ok && roleOfMessage(message) == "tool" {
				id := toolOutputID(message)
				if ids[id] && !found[id] {
					outputs = append(outputs, messages[k])
					found[id] = true
					k++
					continue
				}
			}
			deferred = append(deferred, messages[k])
			k++
		}

		if len(found) != len(ids) {
			out = append(out, messages[i])
			i++
			continue
		}

		first["tool_calls"] = batchCalls
		out = append(out, first)
		out = append(out, outputs...)
		out = append(out, deferred...)
		report.ParallelBatches++
		report.MovedMessages += len(deferred)
		report.MergedCallMessages += mergedMessages
		i = k
	}
	return out
}

func assistantCalls(messageAny any) (map[string]any, []any, bool) {
	message, ok := messageAny.(map[string]any)
	if !ok || roleOfMessage(message) != "assistant" {
		return nil, nil, false
	}
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return nil, nil, false
	}
	return message, calls, true
}

func assistantCallOnly(message map[string]any) bool {
	if assistantMessageHasPayload(message) {
		return false
	}
	for key := range message {
		switch key {
		case "role", "content", "tool_calls":
		default:
			return false
		}
	}
	return true
}

func assistantMessageHasPayload(message map[string]any) bool {
	content, exists := message["content"]
	if !exists || content == nil {
		return false
	}
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	default:
		return true
	}
}

func toolCallID(callAny any) string {
	call, ok := callAny.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := call["id"].(string)
	return strings.TrimSpace(id)
}

func toolOutputID(message map[string]any) string {
	id, _ := message["tool_call_id"].(string)
	return strings.TrimSpace(id)
}

func roleOfMessage(message map[string]any) string {
	role, _ := message["role"].(string)
	return role
}

// LogToolSequenceRepair 修复事件日志（修复发生才打；正文永不入日志）。
func LogToolSequenceRepair(model string, report ToolSequenceRepairReport) {
	if report.Changed() {
		log.Printf("[ToolSequenceRepair] 模型=%s 已修复 原消息=%d 修复后=%d 并行批次=%d 移出插入=%d 合并调用=%d 删无果调用=%d 删孤儿结果=%d 拓扑 %q → %q",
			model, report.OriginalMessages, report.FinalMessages, report.ParallelBatches,
			report.MovedMessages, report.MergedCallMessages, report.DroppedCalls, report.DroppedOutputs,
			report.TopologyBefore, report.TopologyAfter)
	}
}
