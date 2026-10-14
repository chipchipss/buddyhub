// Cursor 模型目录与 effort/fast 变体选择（fork index.mjs L522-827、
// L2167-2180 移植，MIT）。
//
// 背景：Cursor 的模型选择器（AiService/AvailableModels，useModelParameters
// 打开后）把每个模型按**上下文尺寸**分列（"Claude Opus 5.5 @ 300k"），
// effort（推理档）与 fast 不是独立模型，而是**模型自己的参数**。请求最终
// 发出的是"变体自己的 id"（legacySlug，cursor-agent models 里那个
// claude-opus-5-5-high）+ 参数 + max_mode。
//
// 因此一个正确的模型名必须过两层：
//  1. 目录里有这个名字（pick）：按上下文尺寸 + 请求的 effort + fast 匹配变体；
//  2. 目录里没有但它是 Cursor 自己的 id（named）：legacySlug 即该变体；
//     家族 id 是"离默认最近的那个变体"；picker 名字/legacySlugs 是默认变体。
//
// 两层都不中的目录外名字：原样送 + maxMode=false + 无参数（兜底分支）。
//
// 清单两个来源（fork usable L765-783）：
//   - AiService/AvailableModels（useModelParameters）：picker 目录（30s 超时）
//   - AgentService/GetUsableModels（8s 超时）：账号实际可用的变体 id 集合
//     （这个账号没拿到的变体要从目录里摘掉，如"Sonnet 4.6 只有 medium"）
//     两个都拿不到时保留上一次的清单（fork：拿不到就留着旧的，不炸）。
package cursor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 端到端路径（fork L770-771）。
const (
	pathAvailableModels = "aiserver.v1.AiService/AvailableModels"
	pathUsableModels    = "agent.v1.AgentService/GetUsableModels"
	// pickerTimeout / usableTimeout（fork 同值）。
	pickerTimeout = 30 * time.Second
	usableTimeout = 8 * time.Second
)

// ModelsKeep 清单缓存时长（fork MODELS_KEEP L48：10 分钟）。
const ModelsKeep = 10 * time.Minute

// DefaultContext 目录没给上下文上限时的兜底（fork DEFAULT_CONTEXT L47）。
const DefaultContext = 200_000

// levelRank effort 档位序（fork LEVEL_RANK L533；fitEffort 按距离取近）。
var levelRank = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// effortParams 请求里可能出现的 effort 参数名（fork EFFORT_PARAMS L534）。
var effortParams = []string{"effort", "reasoning_effort", "reasoning"}

// idEfforts Cursor 自己 id 里的 effort 词 → 档位（fork ID_EFFORTS L538）。
// extra-high 必须在 high 之前：它是 high 的前缀，先匹配会切错。
var idEfforts = []struct{ word, level string }{
	{"extra-high", "xhigh"},
	{"xhigh", "xhigh"},
	{"minimal", "minimal"},
	{"none", "none"},
	{"low", "low"},
	{"medium", "medium"},
	{"high", "high"},
	{"max", "max"},
}

// pickerSkip CLI 选择器也不列的模型（fork PICKER_SKIP L568）。
var pickerSkip = map[string]bool{
	"claude-4.5-haiku": true, "claude-4.5-haiku-thinking": true,
	"gemini-2.5-pro": true, "gemini-2.5-flash": true,
	"claude-haiku-4-5": true,
}

// ── 上游 JSON 的本地形态 ───────────────────────────────────────────────
// 只解析用到的字段，其余丢弃（上游加字段不影响我们——pb.go 同款前向兼容
// 思路，JSON 侧由 struct 缺省零值天然达成）。

// paramType 一个参数定义的类型描述（enum 或 bool）。
type paramType struct {
	EnumParameter    *enumParam `json:"enumParameter"`
	BooleanParameter *enumParam `json:"booleanParameter"`
}

type enumParam struct {
	Values []paramValue `json:"values"`
}

type paramValue struct {
	Value string `json:"value"`
}

type paramDef struct {
	ID     string    `json:"id"`
	Params paramType `json:"parameterType"`
}

type paramValuePair struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

// variantRaw 一个变体：isDefault/MaxMode 标记 + 参数取值 + legacySlug。
type variantRaw struct {
	LegacySlug            string           `json:"legacySlug"`
	IsDefaultNonMaxConfig bool             `json:"isDefaultNonMaxConfig"`
	IsDefaultMaxConfig    bool             `json:"isDefaultMaxConfig"`
	IsMaxMode             bool             `json:"isMaxMode"`
	ParameterValues       []paramValuePair `json:"parameterValues"`
}

// pickerModelRaw 选择器里的一个模型。
type pickerModelRaw struct {
	Name                        string       `json:"name"`
	ClientDisplayName           string       `json:"clientDisplayName"`
	LegacySlugs                 []string     `json:"legacySlugs"`
	IDAliases                   []string     `json:"idAliases"`
	IsHidden                    bool         `json:"isHidden"`
	IsChatOnly                  bool         `json:"isChatOnly"`
	OnlySupportsCmdK            bool         `json:"onlySupportsCmdK"`
	SupportsAgent               *bool        `json:"supportsAgent"`
	SupportsNonMaxMode          *bool        `json:"supportsNonMaxMode"`
	ContextTokenLimit           int64        `json:"contextTokenLimit"`
	ContextTokenLimitForMaxMode int64        `json:"contextTokenLimitForMaxMode"`
	ParameterDefinitions        []paramDef   `json:"parameterDefinitions"`
	Variants                    []variantRaw `json:"variants"`
}

// modelsPayload AiService/AvailableModels 的响应。
type modelsPayload struct {
	Models []pickerModelRaw `json:"models"`
}

// usablePayload AgentService/GetUsableModels 的响应。
type usablePayload struct {
	Models []struct {
		ModelID        string `json:"modelId"`
		DisplayModelID string `json:"displayModelId"`
	} `json:"models"`
}

// ── 目录条目（fork modelsOf 的产物）───────────────────────────────────

// Model 目录里的一项（fork L644 的 entry：一个模型的一个上下文尺寸）。
type Model struct {
	// ID 发给上游的模型名（裸名，或 name@size 带上下文参数的那种）。
	ID string `json:"id"`
	// Name 展示名（"Claude Opus 5.5 @ 300k"）。
	Name string `json:"name"`
	// Run 选择器里的原始模型名（opencode 侧的 id 用它做键）。
	Run string `json:"run"`
	// Context 上下文窗口 token（目录没给时兜底 DefaultContext）。
	Context int64 `json:"context"`
	// Size 上下文参数原值（"" 表示该模型没有上下文参数）。
	Size string `json:"size"`
	// Efforts 可选的推理档（档位 → Cursor 参数值），nil = 该模型不支持档位。
	Efforts map[string]string `json:"efforts,omitempty"`
	// EffortParam 档位参数名（effort/reasoning_effort/reasoning/thinking）。
	EffortParam string `json:"effort_param,omitempty"`
	// Fast 是否提供 fast 变体。
	Fast bool `json:"fast,omitempty"`

	// 内部：变体表与默认变体（pick 用）。
	variants []variantRaw
	def      *variantRaw
	partial  bool // 账号只被服务了一部分变体（取最近可用）
	// legacy/aliases：该模型在 Cursor 侧的历史 id 与别名（named 第三分支）。
	legacy  []string
	aliases []string
}

// Variant 一次请求解析出的目标（fork pick 的返回）。
// 参数类型沿用 runbuild.go 的 ModelParam（同一语义：模型参数 id/value）。
type Variant struct {
	ID      string // 发给上游的 id（legacySlug 优先）
	Params  []ModelParam
	MaxMode bool
}

// ── 小工具（fork 逐条对应）────────────────────────────────────────────

// cutSuffix 去掉指定后缀（空后缀或不够长返回 ""，fork cut L540）。
func cutSuffix(s, suffix string) (string, bool) {
	if len(s) > len(suffix) && strings.HasSuffix(s, suffix) {
		return s[:len(s)-len(suffix)], true
	}
	return "", false
}

// cleanSpaces 压缩空白（fork clean L570：去零宽空格）。
func cleanSpaces(s string) string {
	s = strings.ReplaceAll(s, "\u200b", "")
	return strings.Join(strings.Fields(s), " ")
}

// tokensOf "300k"→300000 / "1m"→1000000（fork tokensOf L573）。
func tokensOf(v string) int64 {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0
	}
	n, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	unit := int64(1_000)
	if strings.EqualFold(m[2], "m") {
		unit = 1_000_000
	}
	return int64(n * float64(unit))
}

var sizeRe = regexp.MustCompile(`(?i)^(\d+(?:\.\d+)?)([km])$`)

// valuesOf 变体的参数取值（fork valuesOf L589）。
func valuesOf(v *variantRaw) map[string]string {
	if v == nil {
		return nil
	}
	out := make(map[string]string, len(v.ParameterValues))
	for _, p := range v.ParameterValues {
		out[p.ID] = p.Value
	}
	return out
}

// paramDefs 模型的参数定义：id → 可选值（fork paramDefs L579-587）。
func paramDefs(m *pickerModelRaw) map[string][]string {
	out := map[string][]string{}
	for _, d := range m.ParameterDefinitions {
		if d.ID == "" {
			continue
		}
		var vs []string
		if d.Params.EnumParameter != nil {
			for _, v := range d.Params.EnumParameter.Values {
				vs = append(vs, v.Value)
			}
		} else if d.Params.BooleanParameter != nil {
			for _, v := range d.Params.BooleanParameter.Values {
				vs = append(vs, v.Value)
			}
		}
		if len(vs) > 0 {
			out[d.ID] = vs
		}
	}
	return out
}

// defaultVariant CLI 给"按名字点的模型"选的默认变体（fork L593-596）。
func defaultVariant(vs []variantRaw) *variantRaw {
	for i := range vs {
		if vs[i].IsDefaultNonMaxConfig {
			return &vs[i]
		}
	}
	for i := range vs {
		if vs[i].IsDefaultMaxConfig {
			return &vs[i]
		}
	}
	if len(vs) > 0 {
		return &vs[0]
	}
	return nil
}

// needsMax 该变体是否只能跑 Max Mode（fork needsMax L600）。
func needsMax(m *pickerModelRaw, v *variantRaw) bool {
	if m != nil && m.SupportsNonMaxMode != nil && !*m.SupportsNonMaxMode {
		return true
	}
	return v != nil && v.IsMaxMode
}

// effortsOf 模型支持的档位（fork effortsOf L604-615）。
// thinking 单独成开关时：none=false、high=true。
func effortsOf(defs map[string][]string) (pid string, levels map[string]string) {
	for _, p := range effortParams {
		if len(defs[p]) > 0 {
			pid = p
			break
		}
	}
	if pid == "" {
		if hasTrueFalse(defs["thinking"]) {
			return "thinking", map[string]string{"none": "false", "high": "true"}
		}
		return "", nil
	}
	levels = map[string]string{}
	for _, v := range defs[pid] {
		l := v
		if l == "extra-high" {
			l = "xhigh"
		}
		if contains(levelRank, l) {
			levels[l] = v
		}
	}
	return pid, levels
}

func hasTrueFalse(vs []string) bool {
	hasT, hasF := false, false
	for _, v := range vs {
		if v == "true" {
			hasT = true
		}
		if v == "false" {
			hasF = true
		}
	}
	return hasT && hasF
}

// served 账号实际被服务的变体（fork served L622-627）：
// 有可用清单时，legacySlug 不在里面的变体是账号没拿到的；
// 一个都没命中（picker 独有，如 GLM-5.3）则整表保留。
func served(m *pickerModelRaw, ids map[string]bool) []variantRaw {
	vs := m.Variants
	if ids == nil {
		return vs
	}
	var got []variantRaw
	for _, v := range vs {
		if v.LegacySlug == "" || ids[v.LegacySlug] {
			got = append(got, v)
		}
	}
	anyLegacy := false
	for _, v := range got {
		if v.LegacySlug != "" {
			anyLegacy = true
			break
		}
	}
	if anyLegacy {
		return got
	}
	return vs
}

// fastAt 该上下文尺寸下是否有账号可用的 fast 变体（fork fastAt L665-669）。
func fastAt(vs []variantRaw, size string) bool {
	for i := range vs {
		have := valuesOf(&vs[i])
		if have["fast"] != "true" {
			continue
		}
		if size == "" || have["context"] == "" || have["context"] == size {
			return true
		}
	}
	return false
}

// splitID 把 Cursor 自己的 id 拆成家族 + effort（fork splitID L547-565）。
func splitID(id string) (family, effort string) {
	s, fast, thinking := id, false, false
	if b, ok := cutSuffix(s, "-fast"); ok {
		s, fast = b, true
	}
	if b, ok := cutSuffix(s, "-thinking"); ok {
		s, thinking = b, true
	}
	for _, ie := range idEfforts {
		if b, ok := cutSuffix(s, "-"+ie.word); ok {
			s, effort = b, ie.level
			break
		}
	}
	if !thinking {
		if b, ok := cutSuffix(s, "-thinking"); ok {
			s, thinking = b, true
		}
	}
	// 还原后缀：fork 把 thinking/fast 拼回去（family 是带后缀的形态）。
	if thinking {
		s += "-thinking"
	}
	if fast {
		s += "-fast"
	}
	return s, effort
}

// ── 目录构造（fork modelsOf L632-660）─────────────────────────────────

// modelsOf 把 picker 原始模型翻成目录条目：每个上下文尺寸一条。
func modelsOf(picker []pickerModelRaw, ids map[string]bool) []Model {
	var out []Model
	used := map[string]bool{}
	for i := range picker {
		m := &picker[i]
		if m.Name == "" || m.IsHidden || m.IsChatOnly || m.OnlySupportsCmdK ||
			(m.SupportsAgent != nil && !*m.SupportsAgent) || pickerSkip[m.Name] {
			continue
		}
		defs := paramDefs(m)
		variants := served(m, ids)
		partial := len(variants) < len(m.Variants)
		has := func(pid, value string) bool {
			if !partial {
				return true
			}
			anyPid := false
			for _, v := range variants {
				if _, ok := valuesOf(&v)[pid]; ok {
					anyPid = true
					break
				}
			}
			if !anyPid {
				return true
			}
			for _, v := range variants {
				if valuesOf(&v)[pid] == value {
					return true
				}
			}
			return false
		}
		def := defaultVariant(variants)
		pid, all := effortsOf(defs)
		levels := map[string]string{}
		for k, v := range all {
			if has(pid, v) {
				levels[k] = v
			}
		}
		title := cleanSpaces(firstNonEmpty(m.ClientDisplayName, m.Name))

		var contexts []string
		for _, c := range defs["context"] {
			if has("context", c) {
				contexts = append(contexts, c)
			}
		}
		if len(contexts) > 0 {
			for _, c := range contexts {
				id := m.Name + "@" + c
				if used[id] {
					continue
				}
				used[id] = true
				out = append(out, Model{
					ID: id, Name: title + " @ " + c, Run: m.Name,
					Context: firstNonZero(tokensOf(c), DefaultContext),
					Size:    c, Efforts: levels, EffortParam: pid,
					Fast:     fastAt(variants, c),
					variants: variants, def: def, partial: partial,
				})
			}
		} else if !used[m.Name] {
			used[m.Name] = true
			limit := m.ContextTokenLimit
			if def != nil && needsMax(m, def) {
				limit = firstNonZero(m.ContextTokenLimitForMaxMode, m.ContextTokenLimit)
			}
			id := m.Name
			if m.Name == "default" {
				id = "auto"
			}
			out = append(out, Model{
				ID: id, Name: title, Run: m.Name,
				Context: firstNonZero(limit, DefaultContext),
				Size:    "", Efforts: levels, EffortParam: pid,
				Fast:     fastAt(variants, ""),
				variants: variants, def: def, partial: partial,
			})
		}
	}
	return out
}

// ── effort 匹配（fork fitEffort / nearest / branch）───────────────────

// fitEffort 取离请求档位最近的一档（fork fitEffort L743-757）。
// ultra→max；extra-high 即 xhigh（fork 在 L611/L736 都这么归一，请求侧
// 也照做——Cursor 的参数值叫 extra-high，客户端侧档位叫 xhigh）；
// **none 不是最低档**：模型没有 none 时按距离取近（低档），只有模型
// 自己有 none 或点名 none 且正好命中才给 none（fork 的语义，"none 只在
// 点名时给"指的是 LEVEL_RANK 里它排最前但不参与距离竞争）。
func fitEffort(want string, levels []string) string {
	if want == "ultra" {
		want = "max"
	}
	if want == "extra-high" {
		want = "xhigh"
	}
	if len(levels) == 0 || contains(levels, want) {
		return want
	}
	at := indexOfStr(levelRank, want)
	if at < 0 {
		return ""
	}
	best, dist := "", len(levelRank)
	for _, l := range levels {
		i := indexOfStr(levelRank, l)
		if i < 0 || l == "none" {
			continue
		}
		d := abs(i - at)
		if d < dist || (d == dist && i > at) {
			best, dist = l, d
		}
	}
	return best
}

// nearest 参数最接近的变体（fork nearest L673-682）：
// 命中参数数，上下文尺寸对上加权 100。
func nearest(vs []variantRaw, want map[string]string) *variantRaw {
	var best *variantRaw
	most := -1
	for i := range vs {
		have := valuesOf(&vs[i])
		n := 0
		for k, v := range want {
			if have[k] == v {
				n++
			}
		}
		if have["context"] == want["context"] && want["context"] != "" {
			n += 100
		}
		if n > most {
			best, most = &vs[i], n
		}
	}
	return best
}

// pick 一次请求解析成目标变体（fork pick L690-719）。
func pick(e *Model, effort string, fast bool, seed *variantRaw) Variant {
	def := e.def
	want := map[string]string{}
	for k, v := range valuesOf(seedOr(def, seed)) {
		want[k] = v
	}
	if e.Size != "" {
		want["context"] = e.Size
	}
	if effort == "off" {
		effort = "none"
	}
	if e.EffortParam != "" && e.EffortParam != "thinking" && effort == "none" {
		if _, ok := e.Efforts["none"]; !ok {
			if _, hasThinking := want["thinking"]; hasThinking {
				want["thinking"] = "false"
			}
		}
	}
	if e.EffortParam != "" {
		at := ""
		if effort != "" {
			at = fitEffort(effort, keysOf(e.Efforts))
		}
		if at != "" {
			if v, ok := e.Efforts[at]; ok {
				want[e.EffortParam] = v
			}
		}
		// 变体自己决定要不要思考（除非请求点名了某个 id）。
		if seed == nil && e.EffortParam != "thinking" {
			if _, hasThinking := want["thinking"]; hasThinking && at != "" {
				want["thinking"] = "true"
			}
		}
	}
	if e.Fast {
		if fast {
			want["fast"] = "true"
		} else {
			want["fast"] = "false"
		}
	}

	v := findSame(e.variants, want)
	if v == nil && seed != nil {
		v = branch(e, want)
	}
	if v == nil && e.partial {
		v = nearest(e.variants, want)
	}

	var params []ModelParam
	if v != nil {
		merged := map[string]string{}
		for k, val := range want {
			merged[k] = val
		}
		for k, val := range valuesOf(v) {
			merged[k] = val
		}
		for k, val := range merged {
			params = append(params, ModelParam{ID: k, Value: val})
		}
	} else {
		for k, val := range want {
			params = append(params, ModelParam{ID: k, Value: val})
		}
	}
	sort.Slice(params, func(i, j int) bool { return params[i].ID < params[j].ID })

	// 变体定的 Max Mode；没匹配上时按尺寸是否超出非 Max 上限判（fork L713）。
	maxMode := false
	if v != nil {
		maxMode = needsMax(nil, v)
	} else if def != nil {
		maxMode = needsMax(nil, def)
	}
	// 变体自己的 id 优先；上游要的是 legacySlug（fork L714-717 注释）。
	id := e.Run
	if v != nil && v.LegacySlug != "" {
		id = v.LegacySlug
	} else if def != nil && def.LegacySlug != "" {
		id = def.LegacySlug
	}
	return Variant{ID: id, Params: params, MaxMode: maxMode}
}

// findSame 参数完全相容的变体（fork pick 的 same 判定 L706-709）：
// 双方都有的键必须一致，单边缺失视为相容。
func findSame(vs []variantRaw, want map[string]string) *variantRaw {
	for i := range vs {
		have := valuesOf(&vs[i])
		ok := true
		for k := range mergeKeys(have, want) {
			hv, hasH := have[k]
			wv, hasW := want[k]
			if hasH && hasW && hv != wv {
				ok = false
				break
			}
		}
		if ok {
			return &vs[i]
		}
	}
	return nil
}

// branch 点名的 id 在 effort 上没有对应变体时，取该 effort 最近的
// （fork branch L728-739）：避免"claude-opus-5-thinking-high 配
// thinking=false"这种自相矛盾的组合。
func branch(e *Model, want map[string]string) *variantRaw {
	if e.EffortParam == "" || e.EffortParam == "thinking" || e.def == nil {
		return nil
	}
	def := valuesOf(e.def)
	// want 与默认只差 effort/context 时不分支（点名的 effort 就是全部差异）。
	for k := range want {
		if k == e.EffortParam || k == "context" {
			continue
		}
		if dv, ok := def[k]; ok && dv != want[k] {
			return nil
		}
	}
	var vs []variantRaw
	for _, v := range e.variants {
		have := valuesOf(&v)
		if _, ok := have[e.EffortParam]; !ok {
			continue
		}
		ok := true
		for k := range want {
			if k == e.EffortParam {
				continue
			}
			if hv, hasH := have[k]; hasH && hv != want[k] {
				ok = false
				break
			}
		}
		if ok {
			vs = append(vs, v)
		}
	}
	levels := make([]string, 0, len(vs))
	for _, v := range vs {
		levels = append(levels, valuesOf(&v)[e.EffortParam])
	}
	at := fitEffort(levelOf(want[e.EffortParam]), levels)
	if at == "" {
		return nil
	}
	for i := range vs {
		if levelOf(valuesOf(&vs[i])[e.EffortParam]) == at {
			return &vs[i]
		}
	}
	return nil
}

// levelOf 参数值 → 档位（extra-high 即 xhigh）。
func levelOf(v string) string {
	if v == "extra-high" {
		return "xhigh"
	}
	return v
}

// Request 一次请求的模型解析（fork request L821-827）。
//
// named 命中时（点的是 Cursor 自己的 id，如 claude-opus-5-5-high）：
// 请求没点名 effort 就用 id 自带的档位，fast 用 `请求 || id 自带`
// （fork L825 的 `effort || old.effort` / `fast || old.fast`——注意是 ||，
// 所以点名的 fast 变体一定开 fast，只有目录条目能让它关）。
// 目录里没有这个名字且不是 Cursor 自己的 id：返回兜底目标（名字原样送、
// auto→default、无参数、非 Max Mode）——永不返回 nil，调用方无需判空。
func (c *Catalog) Request(model, effort string, fast bool) Variant {
	if entry := c.byID(model); entry != nil {
		return pick(entry, effort, fast, nil)
	}
	if named := c.named(model); named != nil {
		if effort == "" {
			effort = named.effort
		}
		if named.fast {
			fast = true
		}
		return pick(named.entry, effort, fast, named.seed)
	}
	// 兜底：目录外名字原样送（fork L826）。
	run := model
	if run == "auto" {
		run = "default"
	}
	return Variant{ID: run, Params: nil, MaxMode: false}
}

// namedEntry fork named 的一条命中。
type namedEntry struct {
	entry *Model
	seed  *variantRaw
	fast  bool
	// effort 家族 id 自带的档位（splitID 从 legacySlug 拆出来的；
	// 变体 id 精确命中时为空——档位已在变体自己身上了）。
	effort string
}

// named 目录里没有、但是 Cursor 自己的 id（fork named L794-815）。
func (c *Catalog) named(id string) *namedEntry {
	models := c.distinctModels()
	at := func(e *Model, v *variantRaw) *Model {
		ctx := ""
		if v != nil {
			ctx = valuesOf(v)["context"]
		}
		for i := range c.models {
			if c.models[i].Run == e.Run && c.models[i].Size == ctx {
				return &c.models[i]
			}
		}
		return e
	}
	// 第一分支：id 是某个变体自己的 legacySlug。**先过滤**出那个变体再
	// 在它里面取最近默认的——直接在全表里 nearest 会落到默认变体上，
	// 点名就白点了（fork 的 as(e, vs, ...) 里 vs 是已过滤的，L797-803）。
	for _, e := range models {
		var vs []variantRaw
		for _, v := range e.variants {
			if v.LegacySlug == id {
				vs = append(vs, v)
			}
		}
		if len(vs) > 0 {
			seed := nearest(vs, valuesOf(e.def))
			return &namedEntry{entry: at(e, seed), seed: seed, fast: valuesOf(seed)["fast"] == "true"}
		}
	}
	family, effort := splitID(id)
	for _, e := range models {
		var vs []variantRaw
		for _, v := range e.variants {
			if v.LegacySlug != "" {
				if f, _ := splitID(v.LegacySlug); f == family {
					vs = append(vs, v)
				}
			}
		}
		if len(vs) > 0 {
			seed := nearest(vs, valuesOf(e.def))
			return &namedEntry{entry: at(e, seed), seed: seed,
				fast: valuesOf(seed)["fast"] == "true", effort: effort}
		}
	}
	for _, e := range models {
		names := []string{e.pickerName()}
		names = append(names, e.legacySlugs()...)
		names = append(names, e.idAliases()...)
		if contains(names, id) {
			return &namedEntry{entry: at(e, e.def)}
		}
	}
	return nil
}

func (c *Catalog) byID(id string) *Model {
	for i := range c.models {
		if c.models[i].ID == id {
			return &c.models[i]
		}
	}
	return nil
}

// distinctModels 每个模型只留第一条（fork named L795）。
func (c *Catalog) distinctModels() []*Model {
	seen := map[string]bool{}
	var out []*Model
	for i := range c.models {
		if seen[c.models[i].Run] {
			continue
		}
		seen[c.models[i].Run] = true
		out = append(out, &c.models[i])
	}
	return out
}

func (m *Model) pickerName() string    { return m.Run }
func (m *Model) legacySlugs() []string { return m.legacy }
func (m *Model) idAliases() []string   { return m.aliases }

// ── 清单获取 ──────────────────────────────────────────────────────────

// Catalog 一个 token 的模型目录（fork usable + lists Map）。
type Catalog struct {
	token   string
	models  []Model
	fetched time.Time
	// pickerRaw 原始 JSON（named 的 legacySlugs/idAliases 要用）。
	pickerRaw []pickerModelRaw
}

// CatalogCache 目录缓存（fork usable 的 lists Map：按 token 摘要存，
// ModelsKeep 分钟内不重复拉）。零值可用。
type CatalogCache struct {
	mu    sync.Mutex
	byTok map[string]*Catalog
}

// Get 取（或拉）一个 token 的目录（fork usable L765-783）。
// 未命中才拉上游；拉失败时**保留上一次结果**（fork：拿不到就留着旧的，
// 不清空——清空会让调用方误判"这个账号没有任何模型可用"）。
func (cc *CatalogCache) Get(ctx context.Context, client *Client, token string) (*Catalog, error) {
	cc.mu.Lock()
	if cc.byTok == nil {
		cc.byTok = map[string]*Catalog{}
	}
	key := catalogKey(token)
	if had := cc.byTok[key]; had != nil && time.Since(had.fetched) < ModelsKeep {
		cc.mu.Unlock()
		return had, nil
	}
	cc.mu.Unlock()

	cat, err := FetchCatalog(ctx, client, token)
	if err != nil {
		cc.mu.Lock()
		had := cc.byTok[key]
		cc.mu.Unlock()
		if had != nil {
			return had, nil // 保留旧目录（fork 同款）
		}
		return nil, err
	}
	cc.mu.Lock()
	cc.byTok[key] = cat
	cc.mu.Unlock()
	return cat, nil
}

// Invalidate 作废一个 token 的缓存（重新登录后调用，让下次重拉）。
func (cc *CatalogCache) Invalidate(token string) {
	if strings.TrimSpace(token) == "" {
		return
	}
	cc.mu.Lock()
	defer cc.mu.Unlock()
	delete(cc.byTok, catalogKey(token))
}

// catalogKey token 的 sha256 摘要作缓存键（明文 token 不进 map 键，
// 与 fork 同样处理：它也是 hash 后才存）。
func catalogKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// FetchCatalog 拉取并构造目录（fork usable L765-783）。
// 上游给不出目录时返回上一次的（有缓存）；什么都没有才报错。
func FetchCatalog(ctx context.Context, client *Client, token string) (*Catalog, error) {
	// picker 与 usable 并发拉（fork Promise.all）。
	type pickerRes struct {
		models []pickerModelRaw
		err    error
	}
	type usableRes struct {
		ids map[string]bool
		err error
	}
	pc := make(chan pickerRes, 1)
	uc := make(chan usableRes, 1)

	go func() {
		raw, err := unaryJSON(ctx, client, pathAvailableModels, token,
			map[string]any{"useModelParameters": true, "doNotUseMarkdown": true}, pickerTimeout)
		var out pickerRes
		if err == nil {
			var p modelsPayload
			if json.Unmarshal(raw, &p) == nil {
				out.models = p.Models
			}
		} else {
			out.err = err
		}
		pc <- out
	}()
	go func() {
		raw, err := unaryJSON(ctx, client, pathUsableModels, token, nil, usableTimeout)
		var out usableRes
		if err == nil {
			var p usablePayload
			if json.Unmarshal(raw, &p) == nil {
				ids := map[string]bool{}
				for _, m := range p.Models {
					if m.ModelID != "" {
						ids[m.ModelID] = true
					}
					if m.DisplayModelID != "" {
						ids[m.DisplayModelID] = true
					}
				}
				if len(ids) > 0 {
					out.ids = ids
				}
			}
		} else {
			out.err = err
		}
		uc <- out
	}()

	p := <-pc
	u := <-uc
	// usable 拿不到不算失败（fork：usable 拿不到就保留整个 picker）。
	if p.err != nil {
		return nil, fmt.Errorf("cursor: 模型目录拉取失败: %w", p.err)
	}
	models := modelsOf(p.models, u.ids)
	if len(models) == 0 {
		return nil, errors.New("cursor: 上游列不出任何模型")
	}
	cat := &Catalog{token: token, models: models, pickerRaw: p.models, fetched: time.Now()}
	cat.attachAliases()
	return cat, nil
}

// attachAliases 把每个模型的别名挂到条目上（named 第三分支）。
func (c *Catalog) attachAliases() {
	byName := map[string]int{}
	for i, m := range c.pickerRaw {
		byName[m.Name] = i
	}
	for i := range c.models {
		idx, ok := byName[c.models[i].Run]
		if !ok {
			continue
		}
		c.models[i].legacy = c.pickerRaw[idx].LegacySlugs
		c.models[i].aliases = c.pickerRaw[idx].IDAliases
	}
}

// Models 目录条目（调用方只读）。
func (c *Catalog) Models() []Model {
	if c == nil {
		return nil
	}
	return c.models
}

// FetchedAt 拉取时间（缓存判断用）。
func (c *Catalog) FetchedAt() time.Time {
	if c == nil {
		return time.Time{}
	}
	return c.fetched
}

// unaryJSON 一次 Connect unary 调用，返回响应体（fork unary L187-202 同款）。
func unaryJSON(ctx context.Context, c *Client, path, token string, body any, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var payload []byte
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		payload = b
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiBase()+"/"+path, strings.NewReader(string(payload)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connect-Protocol-Version", "1")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("x-cursor-client-type", "cli")
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
	}
	return raw, nil
}

// ── 通用小工具 ────────────────────────────────────────────────────────

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}

func indexOfStr(xs []string, v string) int {
	for i, x := range xs {
		if x == v {
			return i
		}
	}
	return -1
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

func firstNonZero(a, b int64) int64 {
	if a != 0 {
		return a
	}
	return b
}

func seedOr(a, b *variantRaw) *variantRaw {
	if b != nil {
		return b
	}
	return a
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func mergeKeys(a, b map[string]string) map[string]struct{} {
	out := map[string]struct{}{}
	for k := range a {
		out[k] = struct{}{}
	}
	for k := range b {
		out[k] = struct{}{}
	}
	return out
}
