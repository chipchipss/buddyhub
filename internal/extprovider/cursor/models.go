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
	"sort"
	"strings"
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
