package server

// modelfamily.go 跨平台的**同族模型索引** —— 跨平台额度降级的依据。
//
// ── 为什么需要它 ────────────────────────────────────────────────────
// 模型名带前缀就是路由协议（`cn:glm-5.2` 只走腾讯），而**同一个基础模型
// 在各平台叫法都不同**：
//
//	 glm-5.3  ← cn:glm-5.3 | zai:GLM-5.3 | raccoon:sn-glm-5-3
//	          | autoclaw:zaicoding_glm-5.3 | cline:cline-pass/glm-5.3
//	 glm-5.3-flash ← 上面几家 + loomy:GLM-5.3-Flash
//
// 「腾讯没额度了」时，网关必须知道该拿哪个名字去打下一家——这就是本文件的
// 唯一职责。实测（对真实 /v1/models 归一化）得 10 个跨平台族，另有 56 个
// 孤立族（如 qoder 的 `lite`/`ultimate`/`qfmodel` 是**档位名**，归一化后
// 互不相同、也不会匹配别家 —— 这是**正确的**：档位不可跨平台替代）。
//
// ── 构建时机：**只在首选平台失败之后** ───────────────────────────────
// 各平台目录是 10 分钟缓存的网络调用（冷缓存单个 15–20s），`codex` 每次扫
// 文件系统。放在请求热路径上会把一次成功请求拖慢到分钟级，所以索引懒构建
// + TTL——失败已经发生了，多等一秒可接受。
// （`loomyCatalog` 曾经没有缓存、每次调用都打网络，已补；见 loomy_bridge.go。）
//
// ⚠️ 不要图省事改用 `modelList()`：它除了目录还会调
// `ContextWindowListingV4` / `EffortListing`（**打 models.dev 的网络请求**），
// 冷启动可达分钟级，且带一堆与路由无关的展示字段。
import (
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/chipchipss/buddyhub/internal/extprovider/keypool"
)

// digitDotRe 找 `数字-数字`。Go 的 RE2 **不支持 lookaround**，所以判断放到
// ReplaceAllStringFunc 里做（见 dotDigits）。
var digitDotRe = regexp.MustCompile(`(\d+)-(\d+)`)

// dotDigits 只把**版本号形态**的 `5-3` 折成 `5.3`。
//
// 两边都 ≤2 位才转：`sn-glm-5-3`→`glm-5.3`、`deepseek-v4-1-flash`→
// `deepseek-v4.1-flash`（正好对上 cn 的同名）。2024-01 这类日期、123-456
// 这类 id 不转——把它们折成小数只会制造出两边都不认的族键。
func dotDigits(m string) string {
	parts := digitDotRe.FindStringSubmatch(m)
	if parts == nil || len(parts[1]) > 2 || len(parts[2]) > 2 {
		return m
	}
	return parts[1] + "." + parts[2]
}

// dateSuffixRe 结尾 4 位数字视作日期：`deepseek-v4-flash-0731` → `…-flash`。
// 只在**结尾**剥（那里不会误伤 `glm-5.3` 里的数字段）。
var dateSuffixRe = regexp.MustCompile(`-[0-9]{4}$`)

// zaiCatalog Z.AI 的模型（列表与描述同源）。
//
// 从 modelList 的内联块里抽出来：候选索引与 /v1/models 必须**同源**，
// 否则会出现「模型列表里有、降级候选里没有」这种查不出来的问题。
//
// 前 6 条是 Coding Plan（订阅）档；后 2 条是 Z.AI 开放平台 API Key 免费档
// （官方小写名，走 passthrough），实际可用性仍受该 Key 额度约束——由按模型
// 冷却如实上报，不代表一定能出。
var zaiCatalog = []struct{ id, desc string }{
	{"GLM-5.3-Flash", "GLM 5.3 Flash（智谱，最快）"},
	{"GLM-5.3", "GLM 5.3（智谱旗舰）"},
	{"GLM-5.2", "GLM 5.2"},
	{"GLM-5-Turbo", "GLM 5 Turbo"},
	{"GLM-5.1", "GLM 5.1"},
	{"GLM-4.7", "GLM 4.7"},
	{"glm-4-flash", "GLM-4 Flash（开放平台免费档，受 Key 额度）"},
	{"glm-4-air", "GLM-4 Air（开放平台档，受 Key 额度）"},
}

// zaiReady 判定 Z.AI 通道是否配置了可用凭据（与 modelList 的门控同口径）。
func (h *Handler) zaiReady() bool {
	if len(h.cfg.ZaiKeys) > 0 || len(h.cfg.BigModelKeys) > 0 {
		return true
	}
	return h.cfg.Zai != nil && h.cfg.Zai.Count() > 0
}

// familyTTL 索引有效期。10 分钟与各平台目录自己的缓存同档——
// 目录本身也是 10 分钟一刷，更短没意义，更长会让新入池的模型迟迟进不了索引。
const familyTTL = 10 * time.Minute

var (
	familyMu     sync.Mutex
	familyIdxAt  time.Time
	familyIdx    map[string][]string
	familyBuilds int // 构建次数（测试用：断言热路径不构建）
)

// modelNamespaces 归一化时要剥掉的**平台命名空间前缀**。
//
// 有前缀的是上游自己的命名空间（`cline-pass/` 是计费通道选择器），剥掉才能
// 让 `cline-pass/glm-5.3` 与 `cn:glm-5.3` 归到同一族。顺序即优先级：长的在前
// （`cline-free/` 必须先于 `cline-`）。
var modelNamespaces = []string{
	"cline-free/", "cline-pass/", "cline-cloud/", "stealth/",
	"anthropic/", "openai/", "moonshotai/", "spacexai/",
	"sn-", "zai_", "zaicoding_",
}

// normalizeModel 把上游模型名折成族键。
//
// 规则保守，每一条都对**真实目录**验证过：
//   - 剥平台命名空间（上表）→ 小写
//   - `数字-数字` → `数字.数字`：`sn-glm-5-3`→`glm-5.3`、
//     `sn-deepseek-v4-1-flash`→`deepseek-v4.1-flash`（正好对上 cn 的同名）
//   - 去结尾 4 位日期：`deepseek-v4-flash-0731`→`deepseek-v4-flash`
//
// **`auto` 必须排除**：`cn:auto` 是腾讯的自动路由、`zai_auto` 是 AutoClaw 的，
// 归一化后都是 `auto` 会被判成同族——但它们是两套完全不同的路由器，
// 互换会让请求打到语义不同的地方。
func normalizeModel(s string) string {
	lower := strings.ToLower(strings.TrimSpace(s))
	for _, ns := range modelNamespaces {
		if strings.HasPrefix(lower, ns) {
			lower = strings.TrimPrefix(lower, ns)
			break
		}
	}
	lower = digitDotRe.ReplaceAllStringFunc(lower, dotDigits)
	lower = dateSuffixRe.ReplaceAllString(lower, "")
	if lower == "auto" || lower == "" {
		return "\x00" + s // 归不到任何族（`\x00` 前缀保证不与真实族撞）
	}
	return lower
}

// platformModelIDs 一次取齐所有平台的**带前缀**模型 id（只读目录，不查上下文长度）。
//
// 与 modelList 的差别是刻意的：这里只要 id，不碰 ContextWindowListingV4 /
// EffortListing（那两个会打 models.dev）。
func (h *Handler) platformModelIDs() []string {
	var out []string
	add := func(prefix, id string) {
		if id != "" {
			out = append(out, prefix+id)
		}
	}
	// 腾讯 CN 池（10 分钟缓存）。global realm 属同一平台，PlatformOf 同为
	// workbuddy，会在候选阶段被排除，故不必收录。
	for _, m := range h.fetchDynamicModels() {
		add("cn:", m.ID)
	}
	for _, m := range qoderCatalog() {
		add(qoderModelPrefix, m.id)
	}
	if h.zaiReady() {
		for _, m := range zaiCatalog {
			add(zaiModelPrefix, m.id)
		}
	}
	if keyConfigured() {
		for _, m := range keypoolCatalog() {
			add(freeModelPrefix, m.ID)
		}
	}
	if len(keypool.ListCodexAccounts()) > 0 {
		add(codexModelPrefix, "gpt-6-sol")
		add(codexModelPrefix, "gpt-6-astra")
	}
	for _, m := range h.copilotCatalog() {
		add(copilotModelPrefix, m.ID)
	}
	for _, m := range h.clineCatalog() {
		add(clineModelPrefix, m.ID)
	}
	for _, m := range h.autoclawCatalog() {
		add(autoclawModelPrefix, m.ID)
	}
	for _, m := range h.qclawCatalog() {
		add(qclawModelPrefix, m.ID)
	}
	for _, m := range h.traeCatalog() {
		add(traeModelPrefix, m.ID)
	}
	for _, m := range h.accioCatalog() {
		add(accioModelPrefix, m.ID)
	}
	for _, m := range h.traeworkCatalog() {
		add(traeworkModelPrefix, m.ID)
	}
	for _, m := range h.raccoonCatalog() {
		add(raccoonModelPrefix, m.ID)
	}
	for _, m := range h.codeartsCatalog() {
		add(codeartsModelPrefix, m.ID)
	}
	// loomyCatalog 曾经**没有缓存**（注释写"10 分钟"实现却每次打网络），
	// 已在 loomy_bridge.go 补上——本索引会遍历全部平台的目录，没缓存就等于
	// 每 10 分钟白打一发外网。其余平台此前就是 10 分钟缓存。
	for _, m := range h.loomyCatalog() {
		id, _ := m["id"].(string)
		add(loomyModelPrefix, id)
	}
	return out
}

// buildFamilyIndex 由带前缀的模型 id 建族索引（纯函数，便于直接测）。
func buildFamilyIndex(fulls []string) map[string][]string {
	idx := map[string][]string{}
	for _, full := range fulls {
		key := normalizeModel(bareModelName(full))
		if strings.HasPrefix(key, "\x00") {
			continue // 归不到族（如 auto）——不参与跨平台替代
		}
		idx[key] = append(idx[key], full)
	}
	return idx
}

// familyIndex 族 → 该族的全部带前缀模型 id（懒构建 + TTL）。
func (h *Handler) familyIndex() map[string][]string {
	familyMu.Lock()
	defer familyMu.Unlock()
	if time.Since(familyIdxAt) < familyTTL && familyIdx != nil {
		return familyIdx
	}
	familyBuilds++
	idx := buildFamilyIndex(h.platformModelIDs())
	familyIdx, familyIdxAt = idx, time.Now()
	return idx
}

// familyAlts 返回与 bareModel **同族**的其它平台模型名（带前缀），按剩余额度降序。
//
// 排除两类：
//   - **同平台**：`PlatformOf` 相同的候选一律排除。腾讯（裸名 / `cn:`）打空时
//     本来就在自己的轮转里换过号了，再来一遍只是重复；同理 raccoon 内部也有
//     自己的账号轮转。
//   - 原名本身。
//
// 这是**失败之后才调用**的函数（懒构建索引的原因见文件头）。
func (h *Handler) familyAlts(bareModel string) []string {
	fam := normalizeModel(modelBare(bareModel))
	if strings.HasPrefix(fam, "\x00") {
		return nil // 本模型不参与跨平台替代
	}
	self := PlatformOf(bareModel)
	var alts []string
	for _, full := range h.familyIndex()[fam] {
		if full == bareModel || PlatformOf(full) == self {
			continue
		}
		alts = append(alts, full)
	}
	// 按剩余额度降序；同分保持稳定（避免每次请求顺序抖动）
	sort.SliceStable(alts, func(i, j int) bool {
		return h.platformQuota(PlatformOf(alts[i])) > h.platformQuota(PlatformOf(alts[j]))
	})
	return alts
}

// modelBare 取参与归一化的**上游模型名**（剥掉 realm 与平台前缀）。
//
// 必须与 familyIndex 的剥离规则**完全一致**——两边各写一份的结果是键对不上，
// 永远匹配不到族（`raccoon:sn-glm-5-3` 归一化成 `raccoon:sn-glm-5-3`，
// 而索引侧是 `glm-5.3`，看似有候选实则一条也返回不了）。
// 所以两边都走 `bareModelName`。
func modelBare(m string) string { return bareModelName(m) }

// bareModelName 先剥 realm（`cn:`/`global:`），再剥平台前缀，留下上游自己的名。
func bareModelName(m string) string {
	_, bare := resolveModel(m)
	if i := strings.IndexByte(bare, ':'); i >= 0 {
		bare = bare[i+1:]
	}
	return bare
}

// platformQuota 排序键：该平台还有多少额度。
//
//   - 腾讯池读内存（`Pool.List()`，5 分钟由调度器刷新）——零网络
//   - 外部平台读 5 分钟余额刷新落下的缓存（`ExtManager.CachedBalance`）
//   - **没测得过 → 0**：让"测得过的"排在前面。这是有意的保守——
//     未知不该被当成"额度很多"而挤掉一个已知有余额的平台
//   - 已知耗尽的平台排最后（其额度为 0，与未知同档；真正的隔离靠
//     `extstore` 的冷却与"耗尽后不再首选"）
func (h *Handler) platformQuota(platform string) float64 {
	if platform == "workbuddy" {
		best := 0.0
		if h.cfg.Pool == nil {
			return 0
		}
		for _, st := range h.cfg.Pool.List() {
			if st.Disabled {
				continue
			}
			if c := float64(st.Credits); c > best {
				best = c
			}
		}
		return best
	}
	ext := extProviderOf(platform)
	if bal, ok := h.extBalance(ext); ok {
		return bal
	}
	return 0
}

// extProviderOf 平台名 → extstore 的 provider id。
//
// 只有一个别名对：注册表里的网关是 `loomy`（带 `loomy:` 前缀），
// 而落 extstore 的账号 provider 是 `loomy-cli`（密码/短信登录那套）。
// 不映射的话余额缓存永远查不到，loomy 会被恒排在最后。
func extProviderOf(platform string) string {
	if platform == "loomy" {
		return "loomy-cli"
	}
	return platform
}
