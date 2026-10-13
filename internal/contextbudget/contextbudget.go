// Package contextbudget 提供 provider 级上下文预算裁剪（移植自
// vhondo100/zcode-leech 的 pool/context_window.py，MIT）。
//
// 为什么需要：小窗口 provider（Qoder 的 900K 压缩线覆盖不到的模型、腾讯池
// 各上游等）收到 200k+ token 的会话历史时会噎住或静默截断——"lower model /
// lost context" 的根因。网关在出站前按预算裁掉最旧的、与当前问题无关的
// 上下文，且**只删不改**。
//
// Kovak 规则（原样移植，不妥协）：
//   - 预算内 → 逐字透传，零损失零回归（body 一个字节不动）；
//   - 超预算 → 构造最佳**逐字**子集：
//     1. 必留：system 消息、最后一条 user 消息（当前问题）、近期尾部
//     （连续性，占预算 55%，至少 8 条）；
//     2. 精确检索：旧消息里命中当前问题关键词（文件名/标识符/报错串）的
//     按得分逐字拉回——从不改写、从不摘要；
//     3. tool 完整性：assistant 的 tool_calls 与其 tool 响应永不拆散；
//     4. 绝不编造、绝不摘要、绝不重排（输出按原索引排序）。
//
// 无状态设计（与 leech 一致）：客户端每次重发全量历史，不需要数据库。
//
// token 估算是启发式（英文 ≈4 字符/token，代码 ≈3.2），刻意保守——
// 宁可少填不可超填真实窗口。
package contextbudget

import (
	"encoding/json"
	"regexp"
	"strings"
)

// 估算参数（fork context_window.py L41-46）。
const (
	textCharsPerToken  = 4.0
	codeCharsPerToken  = 3.2
	perMessageOverhead = 4 // 每条消息的 role 等固定开销
	codeyThreshold     = 0.08
	codeyScanChars     = 4000
)

// 窗口参数（fork L170-175）。
const (
	// RecencyFrac 预算中留给近期尾部的比例（连续性）。
	RecencyFrac = 0.55
	// RecencyMinMsgs 尾部至少保留的消息条数（tool 交互短但关键）。
	RecencyMinMsgs = 8
	// retrievalRemainFloor 检索回填的剩余预算下限（token）。
	retrievalRemainFloor = 32
)

// Meta 裁剪结果统计（fork window_messages 的 meta）。
type Meta struct {
	Windowed       bool `json:"windowed"`        // 是否发生了裁剪（或增删）
	OriginalTokens int  `json:"original_tokens"` // 原始估算 token
	WindowedTokens int  `json:"windowed_tokens"` // 裁剪后估算 token
	Kept           int  `json:"kept"`            // 保留条数
	Dropped        int  `json:"dropped"`         // 丢弃条数
	Retrieved      int  `json:"retrieved"`       // 检索回填条数
	Budget         int  `json:"budget"`          // 生效预算
}

// stopwords 关键词提取的停用词（fork L120-132）。
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and this that with from have has had was were will would should
		could what when where which there their about into your yours you are for not but they them then
		than just also been being some more most other only very over under again once here please can
		cant cannot make made using used use need want tell show give get let like know think thing
		things stuff going gonna okay ok yes no hi hello hey thanks thank well now still really right back`) {
		stopwords[w] = true
	}
}

// keywordRe 标识符形态的关键词（fork L136：字母开头 + 3 位以上字母数字._-/）。
var keywordRe = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_.\-/]{3,}`)

// ── token 估算（fork L45-85）────────────────────────────────────────────

// isCodey 内容是否为代码形态（前 4000 字符里结构符号密度 > 8%）。
func isCodey(text string) bool {
	if text == "" {
		return false
	}
	scan := text
	if len(scan) > codeyScanChars {
		scan = scan[:codeyScanChars]
	}
	score := 0
	for _, ch := range scan {
		switch ch {
		case '\t', '\n', '{', '}', '[', ']', '(', ')', ';', '=', '<', '>', '|', '&':
			score++
		}
	}
	return float64(score)/float64(len(scan)) > codeyThreshold
}

// msgText 取消息的全部文本（content 的字符串或 parts 的 text，加 tool_calls
// 的 name/arguments）——检索打分与关键词提取都用它。
func msgText(m map[string]any) string {
	var parts []string
	switch c := m["content"].(type) {
	case string:
		parts = append(parts, c)
	case []any:
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					parts = append(parts, t)
				}
			}
		}
	}
	if tcs, ok := m["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tcm["function"].(map[string]any); ok {
				if n, ok := fn["name"].(string); ok {
					parts = append(parts, n)
				}
				if a, ok := fn["arguments"].(string); ok {
					parts = append(parts, a)
				}
			}
		}
	}
	return strings.Join(parts, "\n")
}

// msgChars 消息的字符量（fork _msg_chars：content + tool_calls）。
func msgChars(m map[string]any) int {
	n := 0
	switch c := m["content"].(type) {
	case string:
		n += len(c)
	case []any:
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					n += len(t)
				}
			}
		}
	}
	if tcs, ok := m["tool_calls"].([]any); ok {
		for _, tc := range tcs {
			tcm, ok := tc.(map[string]any)
			if !ok {
				continue
			}
			if fn, ok := tcm["function"].(map[string]any); ok {
				if name, ok := fn["name"].(string); ok {
					n += len(name)
				}
				if args, ok := fn["arguments"].(string); ok {
					n += len(args)
				}
			}
		}
	}
	return n
}

// msgTokens 单条消息的估算 token（fork _msg_tokens）。
func msgTokens(m map[string]any) int {
	chars := msgChars(m)
	rate := textCharsPerToken
	if isCodey(msgContentString(m)) {
		rate = codeCharsPerToken
	}
	return int(float64(chars)/rate) + perMessageOverhead
}

// msgContentString 取 content 的字符串形态（codey 判定用；parts 形态拼 text）。
func msgContentString(m map[string]any) string {
	switch c := m["content"].(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, p := range c {
			if pm, ok := p.(map[string]any); ok {
				if t, ok := pm["text"].(string); ok {
					b.WriteString(t)
				}
			}
		}
		return b.String()
	}
	return ""
}

// EstimateTokens 消息列表的估算总 token（fork estimate_tokens）。
// 导出供测试与调用方预估（裁剪前后对比）。
func EstimateTokens(messages []any) int {
	total := 0
	for _, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		total += msgTokens(m)
	}
	return total
}

// ── 关键词（fork L135-166）─────────────────────────────────────────────

// keywords 从文本提取检索关键词（标识符形态，去停用词）。
func keywords(text string) map[string]struct{} {
	out := map[string]struct{}{}
	for _, t := range keywordRe.FindAllString(strings.ToLower(text), -1) {
		t = strings.Trim(t, "._-/")
		if len(t) >= 4 && !stopwords[t] && !isAllDigits(t) {
			out[t] = struct{}{}
		}
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// score 消息与关键词集合的命中次数（fork _score：出现次数求和）。
func score(m map[string]any, kws map[string]struct{}) int {
	if len(kws) == 0 {
		return 0
	}
	low := strings.ToLower(msgText(m))
	n := 0
	for k := range kws {
		n += strings.Count(low, k)
	}
	return n
}

// ── 窗口（fork window_messages L182-358）───────────────────────────────

// Window 把消息列表裁剪进预算。
//
// 口径（fork L182-358 的忠实移植）：
//   - budget<=0 → 关闭，原样返回（buddyhub 默认关，零回归）；
//   - 空列表 → 原样返回；
//   - 预算内 → 逐字透传（返回同一切片，windowed=false）；
//   - 超预算 → 按"必留 + 检索回填 + tool 完整性 + 末尾微修"构造逐字子集，
//     输出按原索引排序（绝不重排）。
//
// messages 的元素应为 map[string]any（OpenAI 消息）；非 map 元素原样保留
// 在输出里（不参与估算，也不被裁剪——畸形输入不赌命）。
func Window(messages []any, budget int) ([]any, Meta) {
	if budget <= 0 || len(messages) == 0 {
		return messages, Meta{Budget: budget}
	}

	total := EstimateTokens(messages)
	if total <= budget {
		// 预算内：逐字透传（fork L217-231 的 unpinned 分支）。
		return messages, Meta{
			OriginalTokens: total, WindowedTokens: total,
			Kept: len(messages), Budget: budget,
		}
	}

	// 1. 最后一条 user 消息（当前问题，必留）。
	lastUser := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if roleOf(messages[i]) == "user" {
			lastUser = i
			break
		}
	}
	if lastUser == -1 {
		lastUser = len(messages) - 1
	}

	// 2. system 消息（永不丢）。
	var sysIdx []int
	for i, raw := range messages {
		if roleOf(raw) == "system" {
			sysIdx = append(sysIdx, i)
		}
	}

	// 3. 近期尾部：从末尾回走，填满 recency 份额。
	//    fork L249-262：已有 >=8 条且再加就超 recency 预算、且尾部已过 70%
	//    线时停；否则继续加。
	recencyBudget := int(float64(budget) * RecencyFrac)
	var tail []int
	tailTokens := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if isSys(i, sysIdx) {
			continue
		}
		t := msgTokens(mustMap(messages[i]))
		if len(tail) > 0 && tailTokens+t > recencyBudget && len(tail) >= RecencyMinMsgs {
			if tailTokens > int(float64(recencyBudget)*0.7) {
				break
			}
		}
		tail = append(tail, i)
		tailTokens += t
	}
	// 反转成正序。
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}

	// 4. 基础集合 = system ∪ tail ∪ {lastUser}。
	include := map[int]bool{}
	for _, i := range sysIdx {
		include[i] = true
	}
	for _, i := range tail {
		include[i] = true
	}
	include[lastUser] = true

	// 5. 精确检索：用当前问题的关键词，从溢出集里逐字拉回。
	kws := keywords(msgText(mustMap(messages[lastUser])))
	var overflow []int
	for i := range messages {
		if !include[i] && i != lastUser {
			overflow = append(overflow, i)
		}
	}
	used := 0
	for i := range include {
		used += msgTokens(mustMap(messages[i]))
	}
	var retrieved []int
	if used < budget && len(overflow) > 0 && len(kws) > 0 {
		// 得分降序、同分按新近降序（fork key=lambda x: (-x[0], -x[1])）。
		type scored struct {
			s, i int
		}
		list := make([]scored, 0, len(overflow))
		for _, i := range overflow {
			list = append(list, scored{score(mustMap(messages[i]), kws), i})
		}
		// 稳定排序：得分降序 + 索引降序。
		for a := 1; a < len(list); a++ {
			for b := a; b > 0; b-- {
				x, y := list[b-1], list[b]
				if x.s < y.s || (x.s == y.s && x.i < y.i) {
					list[b-1], list[b] = list[b], list[b-1]
				} else {
					break
				}
			}
		}
		for _, sc := range list {
			if sc.s <= 0 {
				break
			}
			t := msgTokens(mustMap(messages[sc.i]))
			if used+t > budget {
				continue
			}
			include[sc.i] = true
			retrieved = append(retrieved, sc.i)
			used += t
			if budget-used < retrievalRemainFloor {
				break
			}
		}
	}

	// 6. tool 完整性：assistant(tool_calls) 与其 tool 响应永不拆散，
	//    tool 响应必须带它的发起者。迭代到不动点（fork L297-323）。
	for changed := true; changed; {
		changed = false
		idxs := sortedKeys(include)
		for _, i := range idxs {
			m := mustMap(messages[i])
			if roleOf(m) == "assistant" && hasToolCalls(m) {
				for j := i + 1; j < len(messages) && roleOf(messages[j]) == "tool"; j++ {
					if !include[j] {
						include[j] = true
						changed = true
					}
				}
			} else if roleOf(m) == "tool" {
				for j := i - 1; j >= 0; j-- {
					r := roleOf(messages[j])
					if r == "assistant" && hasToolCalls(mustMap(messages[j])) {
						if !include[j] {
							include[j] = true
							changed = true
						}
						break
					}
					if r == "user" {
						break
					}
				}
			}
		}
	}

	// 6b. tool 完整性新增的也要计进预算（fork L325-327）。
	used = 0
	for i := range include {
		used += msgTokens(mustMap(messages[i]))
	}

	// 7. 仍超预算（巨型 tool blob）：先砍最旧的非必要（绝不碰
	//    tail/system/lastUser；检索回填的优先保），砍无可砍才动尾部最旧的
	//    （fork L329-343）。
	for used > budget {
		var candidates []int
		for _, i := range sortedKeys(include) {
			if !isSys(i, sysIdx) && i != lastUser && !inList(retrieved, i) && !inList(tail, i) {
				candidates = append(candidates, i)
			}
		}
		if len(candidates) == 0 {
			for _, i := range sortedKeys(include) {
				if !isSys(i, sysIdx) && i != lastUser {
					candidates = append(candidates, i)
				}
			}
			if len(candidates) == 0 {
				break
			}
		}
		i := candidates[0]
		delete(include, i)
		used -= msgTokens(mustMap(messages[i]))
	}

	// 输出：按原索引排序（绝不重排）。
	out := make([]any, 0, len(include))
	for i, raw := range messages {
		if include[i] {
			out = append(out, raw)
		}
	}
	kept := len(out)
	return out, Meta{
		Windowed:       true,
		OriginalTokens: total,
		WindowedTokens: EstimateTokens(out),
		Kept:           kept,
		Dropped:        len(messages) - kept,
		Retrieved:      len(retrieved),
		Budget:         budget,
	}
}

// PruneBody 对 OpenAI 请求体做预算裁剪（只动 messages，其余字段逐字不动）。
//
//   - budget<=0 → 原样返回（关闭）；
//   - 解析失败 / 无 messages 数组 → 原样返回（fail-open：出站改写绝不能
//     阻塞请求转发，与 prompt.Rewrite 同一纪律）；
//   - 未触发裁剪（meta.Windowed=false）→ 返回**原始字节**（不重新序列化，
//     保证预算内 body 逐字一致）。
func PruneBody(body []byte, budget int) ([]byte, Meta) {
	if budget <= 0 || len(body) == 0 {
		return body, Meta{Budget: budget}
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, Meta{Budget: budget}
	}
	msgs, ok := obj["messages"].([]any)
	if !ok {
		return body, Meta{Budget: budget}
	}
	windowed, meta := Window(msgs, budget)
	if !meta.Windowed {
		return body, meta
	}
	obj["messages"] = windowed
	out, err := json.Marshal(obj)
	if err != nil {
		return body, meta
	}
	return out, meta
}

// ── 小工具 ────────────────────────────────────────────────────────────

func roleOf(raw any) string {
	m, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	r, _ := m["role"].(string)
	return r
}

func mustMap(raw any) map[string]any {
	m, _ := raw.(map[string]any)
	return m
}

func hasToolCalls(m map[string]any) bool {
	tcs, ok := m["tool_calls"].([]any)
	return ok && len(tcs) > 0
}

func isSys(i int, sysIdx []int) bool { return inList(sysIdx, i) }

func inList(xs []int, x int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

func sortedKeys(m map[int]bool) []int {
	out := make([]int, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for a := 1; a < len(out); a++ {
		for b := a; b > 0 && out[b-1] > out[b]; b-- {
			out[b-1], out[b] = out[b], out[b-1]
		}
	}
	return out
}
