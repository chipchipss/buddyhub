package server

import "testing"

// normalizeModel 的表全部来自**真实 /v1/models**（2026-10-01 实测）——
// 这些规则一旦改坏，跨平台降级会把请求打到语义不同的模型上，
// 而那种错误在日志里只表现为"回答风格不对"，很难定位。
func TestNormalizeModelFamilies(t *testing.T) {
	// 同族：不同平台对同一个基础模型的叫法
	same := [][2]string{
		{"GLM-5.3", "glm-5.3"},            // zai 大写
		{"sn-glm-5-3", "glm-5.3"},         // raccoon 商汤前缀 + 数字段
		{"zaicoding_glm-5.3", "glm-5.3"},  // autoclaw
		{"cline-pass/glm-5.3", "glm-5.3"}, // cline 计费通道命名空间
		{"cline-free/deepseek-v4.1-flash", "deepseek-v4.1-flash"},
		{"sn-deepseek-v4-1-flash", "deepseek-v4.1-flash"}, // 数字段折小数点后正好对上
		{"deepseek-v4-flash-0731", "deepseek-v4-flash"},   // 结尾日期后缀
		{"MiniMax-M3", "minimax-m3"},                      // 大小写
		{"cline-pass/mimo-v2.5", "mimo-v2.5"},
		{"GLM-5.3-Flash", "glm-5.3-flash"},
		{"zai_glm-5.3-flash", "glm-5.3-flash"},
	}
	for _, c := range same {
		if got := normalizeModel(c[0]); got != c[1] {
			t.Errorf("normalizeModel(%q) = %q，期望 %q", c[0], got, c[1])
		}
	}

	// 不同族：看着像、实则不是同一个东西（互换会出错）
	for _, s := range []string{
		"lite", "ultimate", "qfmodel", // qoder 的**档位名**，不是模型
		"raccoon-8c4485",              // 商汤私有型号
		"anthropic/claude-sonnet-5.5", // 只有 cline 有
	} {
		if normalizeModel(s) == normalizeModel("glm-5.3") {
			t.Errorf("%q 不该与 glm-5.3 同族", s)
		}
	}
}

// `auto` 是两套完全不同的自动路由，必须排除在跨平台替代之外——
// 把 `cn:auto` 换成 `zai_auto` 是"换了个路由器"，不是"换个平台用同一模型"。
func TestNormalizeModelExcludesAuto(t *testing.T) {
	for _, s := range []string{"auto", "AUTO", "Auto"} {
		if got := normalizeModel(s); len(got) == 0 || got[0] != 0 {
			t.Errorf("normalizeModel(%q) = %q，应返回带 \\x00 前缀的哨兵值（表示不参与族）", s, got)
		}
	}
	// 日期/长 id 不该被折成小数点（否则会造出两边都不认的族键）
	if got := normalizeModel("run-2024-01"); got != "run-2024-01" {
		t.Errorf("日期段不该折成小数: %q", got)
	}
}

// buildFamilyIndex：带前缀 id → 族键。键的剥离必须与 familyAlts 侧一致，
// 否则"索引里有、查询时找不到"——这种 bug 表现为降级永远不发生。
func TestBuildFamilyIndexGroupsByNormalizedKey(t *testing.T) {
	idx := buildFamilyIndex([]string{
		"cn:glm-5.3",
		"zai:GLM-5.3",
		"raccoon:sn-glm-5-3",
		"autoclaw:zaicoding_glm-5.3",
		"cline:cline-pass/glm-5.3",
		"cn:glm-5.3-flash",
		"zai:GLM-5.3-Flash",
		"qoder:lite",
		"cn:auto",
		"autoclaw:zai_auto",
	})
	got := idx["glm-5.3"]
	if len(got) != 5 {
		t.Errorf("glm-5.3 应有 5 个成员，得到 %d: %v", len(got), got)
	}
	for _, want := range []string{"cn:glm-5.3", "zai:GLM-5.3", "raccoon:sn-glm-5-3"} {
		if !contains(got, want) {
			t.Errorf("族里缺 %s: %v", want, got)
		}
	}
	if len(idx["glm-5.3-flash"]) != 2 {
		t.Errorf("glm-5.3-flash 应有 2 个成员: %v", idx["glm-5.3-flash"])
	}
	// `auto` 被排除，不该出现在任何族里
	for key, members := range idx {
		for _, m := range members {
			if bareModelName(m) == "auto" || bareModelName(m) == "zai_auto" {
				t.Errorf("`%s` 不该进族 %q（两套不同的自动路由）", bareModelName(m), key)
			}
		}
	}
	// 归一化键与查询侧用的是同一个函数 —— 这里断言可查到
	if v := idx[normalizeModel(bareModelName("raccoon:sn-glm-5-3"))]; len(v) == 0 {
		t.Errorf("用 familyAlts 同款的键查不到成员（两边剥离不一致）")
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
