package cursor

// 模型目录与 effort 变体选择的测试。
// 全部用**构造的 picker JSON**喂给 modelsOf（纯函数，不走网络），
// 钉死的是 fork 语义在数据上的表现：上下文尺寸分列、effort 档位匹配、
// 变体选择、Max Mode 判定、别名/legacySlug 解析、兜底分支。
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ── 测试数据构造 ──────────────────────────────────────────────────────

// variant 构造一个变体。
func variant(slug string, nonMax, maxMode bool, params ...string) variantRaw {
	v := variantRaw{LegacySlug: slug, IsDefaultNonMaxConfig: nonMax, IsMaxMode: maxMode}
	for i := 0; i+1 < len(params); i += 2 {
		v.ParameterValues = append(v.ParameterValues, paramValuePair{ID: params[i], Value: params[i+1]})
	}
	return v
}

// enumDef 一个 enum 参数定义。
func enumDef(id string, values ...string) paramDef {
	var vs []paramValue
	for _, v := range values {
		vs = append(vs, paramValue{Value: v})
	}
	return paramDef{ID: id, Params: paramType{EnumParameter: &enumParam{Values: vs}}}
}

// boolDef 一个 bool 参数定义。
func boolDef(id string) paramDef {
	return paramDef{ID: id, Params: paramType{BooleanParameter: &enumParam{
		Values: []paramValue{{Value: "true"}, {Value: "false"}},
	}}}
}

// samplePicker 一个带两个上下文尺寸 + effort 档位的 Claude 模型，
// 外加一个无尺寸的简单模型。
func samplePicker() []pickerModelRaw {
	return []pickerModelRaw{
		{
			Name: "claude-opus-5", ClientDisplayName: "Claude Opus 5",
			ContextTokenLimit: 200_000,
			ParameterDefinitions: []paramDef{
				enumDef("context", "200k", "300k"),
				enumDef("effort", "low", "medium", "high", "extra-high"),
				boolDef("fast"),
			},
			Variants: []variantRaw{
				variant("claude-opus-5-5-medium", true, false, "context", "200k", "effort", "medium", "fast", "false"),
				variant("claude-opus-5-5-high", false, false, "context", "200k", "effort", "high", "fast", "false"),
				variant("claude-opus-5-5-xhigh", false, false, "context", "200k", "effort", "extra-high", "fast", "false"),
				variant("claude-opus-5-5-medium-300k", true, false, "context", "300k", "effort", "medium", "fast", "false"),
				variant("claude-opus-5-5-high-300k", false, false, "context", "300k", "effort", "high", "fast", "false"),
				variant("claude-opus-5-5-xhigh-300k", false, false, "context", "300k", "effort", "extra-high", "fast", "false"),
				variant("claude-opus-5-5-fast", false, false, "context", "200k", "effort", "medium", "fast", "true"),
			},
		},
		{
			Name: "glm-5", ClientDisplayName: "GLM 5",
			Variants: []variantRaw{
				variant("glm-5-v1", true, false),
			},
		},
	}
}

// ── 目录构造 ──────────────────────────────────────────────────────────

// 上下文尺寸分列：带 context 参数的模型每个尺寸一条，名字带 @300k。
func TestModelsOfContextSplit(t *testing.T) {
	ms := modelsOf(samplePicker(), nil)
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	if !contains(ids, "claude-opus-5@200k") || !contains(ids, "claude-opus-5@300k") {
		t.Fatalf("上下文尺寸未分列: %v", ids)
	}
	if !contains(ids, "glm-5") {
		t.Fatalf("无尺寸模型应保留裸名: %v", ids)
	}
	for _, m := range ms {
		if m.ID == "claude-opus-5@300k" {
			if m.Context != 300_000 {
				t.Fatalf("300k 上下文 = %d, want 300000", m.Context)
			}
			if m.Name != "Claude Opus 5 @ 300k" {
				t.Fatalf("展示名 = %q", m.Name)
			}
			// 样本里 fast 变体只挂在 200k（context=200k,fast=true），
			// 300k 尺寸没有 fast 变体——fastAt 按尺寸判（fork L665-669）。
			if m.Fast {
				t.Fatal("300k 尺寸不该有 fast 变体（样本里 fast 只挂在 200k）")
			}
		}
		if m.ID == "claude-opus-5@200k" {
			if !m.Fast {
				t.Fatal("200k 尺寸应有 fast 变体")
			}
		}
		if m.ID == "glm-5" {
			// 目录没给 contextTokenLimit：兜底 DefaultContext。
			if m.Context != DefaultContext {
				t.Fatalf("glm-5 上下文 = %d, want 兜底 %d", m.Context, DefaultContext)
			}
		}
	}
}

// "default" 模型对外叫 "auto"（fork L656）。
func TestModelsOfDefaultBecomesAuto(t *testing.T) {
	ms := modelsOf([]pickerModelRaw{{
		Name:     "default",
		Variants: []variantRaw{variant("default-v1", true, false)},
	}}, nil)
	if len(ms) != 1 || ms[0].ID != "auto" {
		t.Fatalf("default 应映射为 auto: %+v", ms)
	}
}

// 跳过规则：isHidden / isChatOnly / supportsAgent=false / PICKER_SKIP
// 的模型不进目录（fork L636、L568）。
func TestModelsOfSkipsUnusable(t *testing.T) {
	ms := modelsOf([]pickerModelRaw{
		{Name: "hidden-one", IsHidden: true, Variants: []variantRaw{variant("h", true, false)}},
		{Name: "chat-only", IsChatOnly: true, Variants: []variantRaw{variant("c", true, false)}},
		{Name: "no-agent", SupportsAgent: ptrBool(false), Variants: []variantRaw{variant("n", true, false)}},
		{Name: "claude-4.5-haiku", Variants: []variantRaw{variant("h2", true, false)}},
		{Name: "ok-model", Variants: []variantRaw{variant("o", true, false)}},
	}, nil)
	if len(ms) != 1 || ms[0].ID != "ok-model" {
		t.Fatalf("应只剩 ok-model: %+v", ms)
	}
}

// usable 清单裁剪：账号没有的变体被摘掉（fork served L622-627）。
// 这里造一个"只有 medium 可用"的账号：high/xhigh 变体应消失。
func TestModelsOfServedFiltering(t *testing.T) {
	ids := map[string]bool{
		"claude-opus-5-5-medium":      true,
		"claude-opus-5-5-medium-300k": true,
		"glm-5-v1":                    true,
	}
	ms := modelsOf(samplePicker(), ids)
	for _, m := range ms {
		if m.ID == "claude-opus-5@200k" {
			// 只剩 medium 变体：high/xhigh 不该还能选。
			if _, ok := m.Efforts["high"]; ok {
				t.Fatalf("账号没有 high 变体却仍列出 high 档: %v", m.Efforts)
			}
			if _, ok := m.Efforts["medium"]; !ok {
				t.Fatal("medium 变体可用，medium 档应保留")
			}
		}
	}
}

// ── effort 匹配 ───────────────────────────────────────────────────────

// fitEffort 取最近档：ultra→max；extra-high 视作 xhigh；none 只在点名时给。
func TestFitEffort(t *testing.T) {
	levels := []string{"low", "medium", "high", "xhigh"}
	if got := fitEffort("ultra", levels); got != "xhigh" {
		t.Fatalf("ultra → %q, want xhigh", got)
	}
	if got := fitEffort("minimal", levels); got != "low" {
		t.Fatalf("minimal → %q, want low", got)
	}
	if got := fitEffort("high", levels); got != "high" {
		t.Fatalf("high → %q, want high", got)
	}
	// none 不是最低档：模型自己没有 none 时按距离取最近（low），
	// 只有模型自己有 none 时才给 none（fork L743-757 的语义：
	// "none 只在点名时给"指的是它不参与 none 的距离竞争）。
	if got := fitEffort("none", levels); got != "low" {
		t.Fatalf("none（模型无 none 档）→ %q, want low", got)
	}
	if got := fitEffort("none", []string{"none", "high"}); got != "none" {
		t.Fatalf("none（模型有 none 档）→ %q, want none", got)
	}
	// 目录里没有该档且名字不在 LEVEL_RANK：空串（不瞎猜）。
	if got := fitEffort("bogus", levels); got != "" {
		t.Fatalf("bogus → %q, want 空", got)
	}
}

// tokensOf：300k / 1m / 非法值。
func TestTokensOf(t *testing.T) {
	if got := tokensOf("300k"); got != 300_000 {
		t.Fatalf("300k = %d", got)
	}
	if got := tokensOf("1m"); got != 1_000_000 {
		t.Fatalf("1m = %d", got)
	}
	if got := tokensOf("1.5m"); got != 1_500_000 {
		t.Fatalf("1.5m = %d", got)
	}
	if got := tokensOf("nonsense"); got != 0 {
		t.Fatalf("非法值 = %d, want 0", got)
	}
}

// splitID：家族 + effort 拆解（fork L547-565 的 thinking/fast 还原）。
func TestSplitID(t *testing.T) {
	family, effort := splitID("claude-opus-5-5-high")
	if family != "claude-opus-5-5" || effort != "high" {
		t.Fatalf("high: %q/%q", family, effort)
	}
	family, effort = splitID("claude-opus-5-5-extra-high")
	if effort != "xhigh" {
		t.Fatalf("extra-high 应为 xhigh, got %q", effort)
	}
	family, effort = splitID("claude-opus-5-5")
	if family != "claude-opus-5-5" || effort != "" {
		t.Fatalf("无 effort: %q/%q", family, effort)
	}
	// fast 后缀不影响 effort 解析。
	family, _ = splitID("claude-opus-5-5-fast")
	if family != "claude-opus-5-5-fast" {
		t.Fatalf("fast 家族 = %q（fork 把 fast 拼回家族名）", family)
	}
}

// ── 变体选择（request/pick）───────────────────────────────────────────

// 目录内名字 + effort：发出对应变体的 legacySlug + 参数。
func TestRequestPickByEffort(t *testing.T) {
	cat := &Catalog{models: modelsOf(samplePicker(), nil), pickerRaw: samplePicker()}
	cat.attachAliases()

	v := cat.Request("claude-opus-5@300k", "high", false)
	if v.ID != "claude-opus-5-5-high-300k" {
		t.Fatalf("high@300k 变体 = %q, want claude-opus-5-5-high-300k", v.ID)
	}
	// 参数里应带 effort=high 与 context=300k。
	if !hasParam(v.Params, "context", "300k") {
		t.Fatalf("参数缺 context: %+v", v.Params)
	}
	if !hasParam(v.Params, "effort", "high") {
		t.Fatalf("参数缺 effort=high: %+v", v.Params)
	}
}

// extra-high 视作 xhigh（fork L694、L611）。
func TestRequestExtraHighMapsToXhigh(t *testing.T) {
	cat := &Catalog{models: modelsOf(samplePicker(), nil), pickerRaw: samplePicker()}
	v := cat.Request("claude-opus-5@200k", "extra-high", false)
	if v.ID != "claude-opus-5-5-xhigh" {
		t.Fatalf("extra-high 变体 = %q, want claude-opus-5-5-xhigh", v.ID)
	}
}

// 目录外名字：原样送、无参数、非 Max Mode（fork L826 兜底）。
func TestRequestUnknownModelFallsThrough(t *testing.T) {
	cat := &Catalog{models: modelsOf(samplePicker(), nil)}
	v := cat.Request("some-unknown-model", "", false)
	if v.ID != "some-unknown-model" {
		t.Fatalf("目录外名字应原样送, got %q", v.ID)
	}
	if len(v.Params) != 0 || v.MaxMode {
		t.Fatalf("兜底不该带参数或 MaxMode: %+v", v)
	}
}

// "auto" 换成 "default"（fork L826）。
func TestRequestAutoMapsToDefault(t *testing.T) {
	cat := &Catalog{models: modelsOf(samplePicker(), nil)}
	v := cat.Request("auto", "", false)
	if v.ID != "default" {
		t.Fatalf("auto → %q, want default", v.ID)
	}
}

// 目录里有 "default" 模型时，auto 走目录条目（id 已是 auto）。
func TestRequestAutoWithCatalogEntry(t *testing.T) {
	picker := []pickerModelRaw{{
		Name:     "default",
		Variants: []variantRaw{variant("default-slug", true, false)},
	}}
	cat := &Catalog{models: modelsOf(picker, nil), pickerRaw: picker}
	cat.attachAliases()
	v := cat.Request("auto", "", false)
	if v.ID != "default-slug" {
		t.Fatalf("auto 命中目录条目应走 pick（legacySlug 优先）, got %q", v.ID)
	}
}

// legacySlug 点名：目录列表里没有这个 id，但它是某变体自己的 id
// （fork named 第一分支）。
func TestRequestLegacySlugNamed(t *testing.T) {
	cat := &Catalog{models: modelsOf(samplePicker(), nil), pickerRaw: samplePicker()}
	v := cat.Request("claude-opus-5-5-high-300k", "", false)
	if v.ID != "claude-opus-5-5-high-300k" {
		t.Fatalf("legacySlug 点名应解析为该变体, got %q", v.ID)
	}
}

// picker 名字（无尺寸模型）走默认变体（fork named 第三分支）。
func TestRequestPickerNameDefaultVariant(t *testing.T) {
	cat := &Catalog{models: modelsOf(samplePicker(), nil), pickerRaw: samplePicker()}
	v := cat.Request("glm-5", "", false)
	if v.ID != "glm-5-v1" {
		t.Fatalf("picker 名字应走默认变体, got %q", v.ID)
	}
}

// fast：请求 service_tier=fast 时才开，且只在该模型有 fast 变体时。
func TestRequestFastGating(t *testing.T) {
	cat := &Catalog{models: modelsOf(samplePicker(), nil), pickerRaw: samplePicker()}
	on := cat.Request("claude-opus-5@200k", "medium", true)
	if !hasParam(on.Params, "fast", "true") {
		t.Fatalf("fast=true 应开: %+v", on.Params)
	}
	off := cat.Request("claude-opus-5@200k", "medium", false)
	if !hasParam(off.Params, "fast", "false") {
		t.Fatalf("fast=false 应显式关（有 fast 变体的模型）: %+v", off.Params)
	}
	// glm-5 没有 fast 参数定义：请求 fast 也不该凭空造参数。
	noFast := cat.Request("glm-5", "", true)
	if hasParam(noFast.Params, "fast", "true") {
		t.Fatalf("无 fast 变体的模型不该带 fast 参数: %+v", noFast.Params)
	}
}

// Max Mode：supportsNonMaxMode=false 的模型必须 Max Mode（fork needsMax）。
func TestRequestMaxModeFromModel(t *testing.T) {
	picker := []pickerModelRaw{{
		Name:               "max-only-model",
		SupportsNonMaxMode: ptrBool(false),
		ContextTokenLimit:  100_000,
		Variants: []variantRaw{
			variant("max-only-v1", false, true),
		},
	}}
	cat := &Catalog{models: modelsOf(picker, nil), pickerRaw: picker}
	v := cat.Request("max-only-model", "", false)
	if !v.MaxMode {
		t.Fatalf("只支持 Max Mode 的模型应带 MaxMode: %+v", v)
	}
}

// ── 网络层（本地假上游，不走真实 Cursor）──────────────────────────────

// FetchCatalog 走假上游：两个 unary 端点都答，目录按 JSON 形状构造。
func TestFetchCatalogFromFakeUpstream(t *testing.T) {
	srv := newFakeCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "AvailableModels"):
			picker := samplePicker()
			raw, _ := json.Marshal(modelsPayload{Models: picker})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(raw)
		case strings.HasSuffix(r.URL.Path, "GetUsableModels"):
			_, _ = w.Write([]byte(`{"models":[{"modelId":"claude-opus-5-5-medium"}]}`))
		default:
			w.WriteHeader(404)
		}
	})
	defer srv.Close()

	c := New()
	c.APIBase = srv.URL
	cat, err := FetchCatalog(context.Background(), c, "tok-test")
	if err != nil {
		t.Fatalf("FetchCatalog: %v", err)
	}
	ms := cat.Models()
	if len(ms) == 0 {
		t.Fatal("目录为空")
	}
	var ids []string
	for _, m := range ms {
		ids = append(ids, m.ID)
	}
	if !contains(ids, "claude-opus-5@200k") {
		t.Fatalf("目录缺 claude-opus-5@200k: %v", ids)
	}
	// 缓存时间已记录。
	if cat.FetchedAt().IsZero() {
		t.Fatal("FetchedAt 未记录")
	}
}

// 上游列不出模型：报错（fork：no models 即失败，不静默给空目录）。
func TestFetchCatalogEmptyIsError(t *testing.T) {
	srv := newFakeCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"models":[]}`))
	})
	defer srv.Close()
	c := New()
	c.APIBase = srv.URL
	if _, err := FetchCatalog(context.Background(), c, "tok-test"); err == nil {
		t.Fatal("空目录应报错（静默给空目录会让调用方以为没有任何模型可用）")
	}
}

// usable 端点失败不算整体失败：保留整个 picker（fork L775-777 的
// catch(() => null) 语义）。
func TestFetchCatalogUsableFailureKeepsPicker(t *testing.T) {
	srv := newFakeCursorServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "AvailableModels") {
			picker := samplePicker()
			raw, _ := json.Marshal(modelsPayload{Models: picker})
			_, _ = w.Write(raw)
			return
		}
		w.WriteHeader(500) // usable 挂了
	})
	defer srv.Close()
	c := New()
	c.APIBase = srv.URL
	cat, err := FetchCatalog(context.Background(), c, "tok-test")
	if err != nil {
		t.Fatalf("usable 失败不该让整体失败: %v", err)
	}
	// 保留整个 picker：high 变体仍在。
	for _, m := range cat.Models() {
		if m.ID == "claude-opus-5@200k" {
			if _, ok := m.Efforts["high"]; !ok {
				t.Fatal("usable 失败时应保留全部档位")
			}
		}
	}
}

// ── 小工具 ────────────────────────────────────────────────────────────

func hasParam(ps []ModelParam, id, value string) bool {
	for _, p := range ps {
		if p.ID == id && p.Value == value {
			return true
		}
	}
	return false
}

func ptrBool(b bool) *bool { return &b }

// newFakeCursorServer 一个只回 JSON 的假上游（APIBase 直接指它，不走
// 生产域名重写——避免再犯"测试打真实上游"那类错）。
func newFakeCursorServer(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(h)
}
