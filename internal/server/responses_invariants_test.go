package server

// responses_invariants_test.go — responsesStreamTranslate 的 SSE 事件顺序契约。
//
// 守的是 OpenAI Responses 客户端能否解析的契约（来源：omp 审计 F-API-2，契约
// 已验证成立，但此前无常驻测试）。一旦被回归破坏，客户端表现是「解析失败」
// 「流到一半断」「凭空多出一个 output item」——这类问题在集成层很难定位。
//
// 要守的不变量：
//   - sequence_number 从 0 严格连续递增
//   - response.created 是第一个事件，response.in_progress 紧随其后
//   - 每个 item 的 output_item.added 早于其 delta，delta 早于其 *.done
//   - 所有 output_item.done 早于 response.completed
//   - item_id 跨帧稳定（同一 item 的所有事件 item_id 一致）
//   - finish_reason 缺失时必须发 response.failed，绝不伪造 response.completed
//     （responses.go:743-756）
//   - 正常收尾的最后是 data: [DONE]
//
// 起点：omp 交付的设计（fix/tests-resident @ aa6cadd 回频道消息）。落点时改了
// 两处，都不是无条件成立，原样落会假红：
//  1. 原设计用 lastIndexOf 找 *.done 并断言「delta 早于 done」无条件成立——
//     但 fail  ！路径提前 return，根本不会产出任何 *.done 事件（responses.go:745-759）。
//     本文件改为按 item 分组后再断言，缺 done 的 item 直接跳过。
//  2. 原设计断言「output_item.done 晚于 response.completed」用 map 全量比对，
//     fail 路径下 map 为空等于什么都没守；本文件只对 completed 用例断言。
import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
)

// respEvt 一条 Responses 事件：event: 行的 type + data: 解出的 JSON。
type respEvt struct {
	Type   string
	Seq    float64
	ItemID string
	Index  float64
	Status string // response 信封的 status（failed/completed）
	Item   map[string]any
	Extra  map[string]any
}

// parseResponsesSSE 把 translate 写出的 SSE 文本解成事件序列。
func parseResponsesSSE(t *testing.T, raw string) []respEvt {
	t.Helper()
	var out []respEvt
	var curType string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, "event:"):
			curType = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var doc map[string]any
			if err := json.Unmarshal([]byte(payload), &doc); err != nil {
				t.Fatalf("事件 data 不是合法 JSON: %v (%s)", err, payload)
			}
			ev := respEvt{Type: curType, Extra: doc}
			if v, ok := doc["sequence_number"].(float64); ok {
				ev.Seq = v
			}
			if v, ok := doc["item_id"].(string); ok {
				ev.ItemID = v
			}
			if v, ok := doc["output_index"].(float64); ok {
				ev.Index = v
			}
			if item, ok := doc["item"].(map[string]any); ok {
				ev.Item = item
				if id, _ := item["id"].(string); id != "" {
					ev.ItemID = id
				}
			}
			if resp, ok := doc["response"].(map[string]any); ok {
				if s, ok := resp["status"].(string); ok {
					ev.Status = s
				}
			}
			out = append(out, ev)
		}
	}
	if len(out) == 0 {
		t.Fatalf("一个事件都没解析到，原始输出: %q", raw)
	}
	return out
}

// firstIdx 某类型事件首次出现的下标（-1 = 没出现）。
func firstIdx(evs []respEvt, typ string) int {
	for i := range evs {
		if evs[i].Type == typ {
			return i
		}
	}
	return -1
}

// itemGroup 同一 item（按 item_id 归组）在事件流中的位置快照。
type itemGroup struct {
	typ     string
	addedAt int
	deltaAt int
	doneAt  int
}

// collectItems 按 item 类型归组，补齐 reasoning / function_call 的 added 下标。
// 重复出现 added 之类的异常通过返回值上报，不在这里 t.Errorf。
func collectItems(evs []respEvt) (groups map[string]itemGroup, problems []string) {
	reasoning := itemGroup{typ: "reasoning", addedAt: -1, deltaAt: -1, doneAt: -1}
	message := itemGroup{typ: "message", addedAt: -1, deltaAt: -1, doneAt: -1}
	calls := itemGroup{typ: "function_call", addedAt: -1, deltaAt: -1, doneAt: -1}
	message.deltaAt = firstIdx(evs, "response.output_text.delta")
	message.doneAt = firstIdx(evs, "response.output_text.done")
	reasoning.deltaAt = firstIdx(evs, "response.reasoning_summary_text.delta")
	reasoning.doneAt = firstIdx(evs, "response.reasoning_summary_text.done")
	calls.deltaAt = firstIdx(evs, "response.function_call_arguments.delta")
	calls.doneAt = firstIdx(evs, "response.function_call_arguments.done")
	for i := range evs {
		if evs[i].Type != "response.output_item.added" || evs[i].Item == nil {
			continue
		}
		typ, _ := evs[i].Item["type"].(string)
		switch typ {
		case "reasoning":
			if reasoning.addedAt >= 0 {
				problems = append(problems, fmt.Sprintf("reasoning 出现两次 output_item.added（%d 与 %d）", reasoning.addedAt, i))
			}
			reasoning.addedAt = i
		case "message":
			if message.addedAt >= 0 {
				problems = append(problems, fmt.Sprintf("message 出现两次 output_item.added（%d 与 %d）", message.addedAt, i))
			}
			message.addedAt = i
		case "function_call":
			if calls.addedAt < 0 {
				calls.addedAt = i
			}
		}
	}
	return map[string]itemGroup{"reasoning": reasoning, "message": message, "function_call": calls}, problems
}

// describeGroup 生成「added=… delta=… done=…」式的定位信息。
func describeGroup(name string, g itemGroup) string {
	return fmt.Sprintf("%s: added=%d delta=%d done=%d", name, g.addedAt, g.deltaAt, g.doneAt)
}

// checkGlobalInvariants 所有用例都要成立的不变量（seq 单调 / 首两事件 / item_id 稳定）。
func checkGlobalInvariants(t *testing.T, evs []respEvt) {
	t.Helper()

	// 1) sequence_number 从 0 严格连续递增。
	for i := range evs {
		if evs[i].Seq != float64(i) {
			t.Errorf("事件 %d(%s) sequence_number=%v，期望 %d（必须从 0 连续递增）", i, evs[i].Type, evs[i].Seq, i)
		}
	}

	// 2) response.created 必须是第一个事件，response.in_progress 紧随其后。
	if evs[0].Type != "response.created" {
		t.Errorf("首个事件 = %s，期望 response.created", evs[0].Type)
	}
	if len(evs) < 2 || evs[1].Type != "response.in_progress" {
		t.Errorf("第二个事件 = %v，期望 response.in_progress", evs[1:min(2, len(evs))])
	}

	// 3) item_id 跨帧稳定：同一 item_id 下不得混入不同 item 类型的事件。
	owner := map[string]string{}
	for i := range evs {
		if evs[i].Type != "response.output_item.added" && evs[i].Type != "response.output_item.done" {
			continue
		}
		if evs[i].Item == nil {
			t.Errorf("事件 %d(%s) 缺 item 对象", i, evs[i].Type)
			continue
		}
		typ, _ := evs[i].Item["type"].(string)
		id, _ := evs[i].Item["id"].(string)
		if typ == "" {
			t.Errorf("事件 %d(%s) 的 item 缺 type", i, evs[i].Type)
			continue
		}
		if id == "" {
			t.Errorf("事件 %d(%s) 的 item 缺 id", i, evs[i].Type)
			continue
		}
		if prev, dup := owner[id]; dup && prev != typ {
			t.Errorf("item_id %s 同时被标记为 %s 与 %s（item_id 跨帧不稳定）", id, prev, typ)
		}
		owner[id] = typ
	}
}

// checkItemOrdering 每个 item 的 added → delta → done 顺序。
// 只对本次流里真实出现的 item 生效：fail 路径提前 return，没有 *.done，不能误红。
func checkItemOrdering(t *testing.T, groups map[string]itemGroup) {
	t.Helper()
	for _, name := range sortedKeys(groups) {
		g := groups[name]
		if g.deltaAt < 0 {
			continue // 该 item 本次没出现
		}
		if g.addedAt < 0 || g.addedAt > g.deltaAt {
			t.Errorf("%s 的 delta 出现在 added 之前（%s）", name, describeGroup(name, g))
		}
		if g.doneAt >= 0 && g.doneAt < g.deltaAt {
			t.Errorf("%s 的 delta 出现在 done 之后（%s）", name, describeGroup(name, g))
		}
		if g.addedAt >= 0 && g.doneAt >= 0 && g.addedAt > g.doneAt {
			t.Errorf("%s 的 added 出现在 done 之后（%s）", name, describeGroup(name, g))
		}
	}
}

func sortedKeys(m map[string]itemGroup) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestResponsesStreamEventOrderInvariants 完整流：reasoning + message +
// 两个分片 function_call，断言全链路事件顺序。
func TestResponsesStreamEventOrderInvariants(t *testing.T) {
	src := strings.Join([]string{
		`data: {"choices":[{"delta":{"role":"assistant"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"先想"}}]}`,
		`data: {"choices":[{"delta":{"reasoning_content":"一下"}}]}`,
		`data: {"choices":[{"delta":{"content":"答"}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_b","function":{"name":"g","arguments":"{"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`,
		`data: [DONE]`,
		``,
	}, "\n")

	rec := httptest.NewRecorder()
	usage, finish, sawDone, scanErr := responsesStreamTranslate(rec, strings.NewReader(src), "m1")
	if scanErr != nil {
		t.Fatalf("translate 报错: %v", scanErr)
	}
	if !sawDone {
		t.Error("没看到 [DONE]")
	}
	if finish != "tool_calls" {
		t.Errorf("finishReason = %q, want tool_calls", finish)
	}
	if usage == nil || usage["total_tokens"] != float64(3) {
		t.Errorf("usage = %v, want total_tokens=3", usage)
	}

	evs := parseResponsesSSE(t, rec.Body.String())
	checkGlobalInvariants(t, evs)
	groups, problems := collectItems(evs)
	for _, p := range problems {
		t.Error(p)
	}
	checkItemOrdering(t, groups)

	// 所有 output_item.done 必须早于 response.completed。
	completed := firstIdx(evs, "response.completed")
	if completed < 0 {
		t.Fatalf("有 finish_reason 却没收尾 response.completed，末事件 = %s", evs[len(evs)-1].Type)
	}
	for i := completed + 1; i < len(evs); i++ {
		if evs[i].Type == "response.output_item.done" {
			t.Errorf("output_item.done(%d) 晚于 response.completed(%d)", i, completed)
		}
	}

	// 收尾必须是 completed 且信封 status=completed，output 三类 item 齐全。
	last := evs[len(evs)-1]
	if last.Type != "response.completed" {
		t.Fatalf("最后一个事件 = %s, want response.completed", last.Type)
	}
	if last.Status != "completed" {
		t.Errorf("completed 信封 status = %q, want completed", last.Status)
	}
	resp, _ := last.Extra["response"].(map[string]any)
	output, _ := resp["output"].([]any)
	kinds := map[string]int{}
	for _, it := range output {
		item, _ := it.(map[string]any)
		typ, _ := item["type"].(string)
		kinds[typ]++
	}
	for _, want := range []string{"reasoning", "message", "function_call"} {
		if kinds[want] == 0 {
			t.Errorf("output 缺 %s item: %v", want, output)
		}
	}
	if kinds["function_call"] != 2 {
		t.Errorf("function_call item 数 = %d, want 2（两个工具调用）", kinds["function_call"])
	}

	// 流尾必须是 SSE 的 data: [DONE]（OpenAI 客户端以此收尾）。
	if !strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
		t.Errorf("输出未以 data: [DONE] 收尾: %q", rec.Body.String())
	}
}

// TestResponsesStreamNoFinishReasonFails 干净 EOF 但无 finish_reason：
// 必须发 response.failed，绝不伪造 response.completed（responses.go:743-756）。
func TestResponsesStreamNoFinishReasonFails(t *testing.T) {
	src := `data: {"choices":[{"delta":{"content":"半句"}}]}` + "\n"

	rec := httptest.NewRecorder()
	_, finish, _, scanErr := responsesStreamTranslate(rec, strings.NewReader(src), "m1")
	if scanErr != nil {
		t.Fatalf("干净 EOF 不该报 scanErr: %v", scanErr)
	}
	if finish != "" {
		t.Errorf("finishReason = %q, want 空", finish)
	}

	evs := parseResponsesSSE(t, rec.Body.String())
	checkGlobalInvariants(t, evs)
	groups, problems := collectItems(evs)
	for _, p := range problems {
		t.Error(p)
	}
	checkItemOrdering(t, groups)

	if i := firstIdx(evs, "response.completed"); i >= 0 {
		t.Fatalf("无 finish_reason 时伪造了 response.completed（事件 %d）", i)
	}
	fi := -1
	for i := len(evs) - 1; i >= 0; i-- {
		if evs[i].Type == "response.failed" {
			fi = i
			break
		}
	}
	if fi < 0 {
		t.Fatalf("无 finish_reason 时应发 response.failed，实际末事件 = %s", evs[len(evs)-1].Type)
	}
	if evs[fi].Status != "failed" {
		t.Errorf("failed 信封 status = %q, want failed", evs[fi].Status)
	}
	errObj, _ := evs[fi].Extra["response"].(map[string]any)["error"].(map[string]any)
	if errObj["code"] != "stream_closed_without_finish" {
		t.Errorf("error.code = %v, want stream_closed_without_finish", errObj["code"])
	}
	// failed 是最后一个事件，且流尾不得出现 SSE 的 data: [DONE]。
	if fi != len(evs)-1 {
		t.Errorf("response.failed(%d) 之后还有事件 %s", fi, evs[fi+1].Type)
	}
	if strings.HasSuffix(rec.Body.String(), "data: [DONE]\n\n") {
		t.Error("失败路径不应以 data: [DONE] 收尾（客户端会当成完整回复）")
	}
}
