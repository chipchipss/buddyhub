// Part of the Cursor model catalog (ported from magpie index.mjs L522-827, MIT).
//
// Catalog construction: raw picker models -> catalog entries
// (fork paramDefs/effortsOf/served/fastAt/splitID/modelsOf, L568-669).
// Package doc, types and constants live in models.go.
package cursor

import (
	"regexp"
	"strconv"
	"strings"
)

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
