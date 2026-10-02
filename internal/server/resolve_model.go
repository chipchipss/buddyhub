package server

import "strings"

// zaiModelPrefix Z.AI（GLM Coding Plan API Key 通道）路由前缀。
const zaiModelPrefix = "zai:"

// isZaiModel 判据。此前分发链里是内联的 strings.HasPrefix，抽出来是为了让
// 每条桥接都有同一个形状的判据，好被 gatewayRoutes 表统一核对。
func isZaiModel(bare string) bool { return strings.HasPrefix(bare, zaiModelPrefix) }

// resolveModel 解析模型名协议（PLAN D6）：
//
//	分布式前缀： "[realm:]model"
//
// 取第一个 ":"，前段恰为 "cn"/"global" 才剥离；否则视为裸名，realm=cn、bare=原串。
// 大小写敏感（前缀必须是精确的小写枚举）。bare 即出站/选号/账本使用的裸模型名。
//
// 导出为 ResolveModel（cmd/server/main.go 粘性闭包需要），包内简写 resolveModel。
func resolveModel(model string) (realm, bare string) {
	idx := strings.IndexByte(model, ':')
	if idx < 0 {
		return "cn", model
	}
	prefix := model[:idx]
	if prefix != "cn" && prefix != "global" {
		return "cn", model
	}
	return prefix, model[idx+1:]
}

// ResolveModel 是 resolveModel 的导出面（跨包调用）。
func ResolveModel(model string) (realm, bare string) { return resolveModel(model) }

// PlatformOf 由裸模型名判定其所属平台（多 Key 平台授权用）。
// 与 handler 路由分支一一对应：qoder:/codex:/free:/loomy:/zai: 前缀通道，
// 其余（裸名）归腾讯池（realm 细分 cn/global 由调用方按需二次判断）。
func PlatformOf(bare string) string {
	for _, p := range []struct{ prefix, name string }{
		{qoderModelPrefix, "qoder"},
		{copilotModelPrefix, "copilot"},
		{clineModelPrefix, "cline"},
		{autoclawModelPrefix, "autoclaw"},
		{qclawModelPrefix, "qclaw"},
		{traeModelPrefix, "trae"},
		{accioModelPrefix, "accio"},
		{traeworkModelPrefix, "traework"},
		{codexModelPrefix, "codex"},
		{freeModelPrefix, "free"},
		{loomyModelPrefix, "loomy"},
		{zaiModelPrefix, "zai"},
		{raccoonModelPrefix, "raccoon"},
		{codeartsModelPrefix, "codearts"},
		{imaModelPrefix, "ima"},
		{marvisModelPrefix, "marvis"},
	} {
		if strings.HasPrefix(bare, p.prefix) {
			return p.name
		}
	}
	return "workbuddy"
}
