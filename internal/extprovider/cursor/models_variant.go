// Part of the Cursor model catalog (ported from magpie index.mjs L522-827, MIT).
//
// Variant resolution: requested model name + effort + fast -> the variant id,
// parameters and max_mode sent upstream
// (fork fitEffort/nearest/branch/pick/named/request, L671-827).
package cursor

import (
	"sort"
)

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
