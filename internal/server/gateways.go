package server

// gateways.go 对话桥接的路由表。
//
// ── 这份表存在的唯一理由是**交叉校验** ────────────────────────────
// 「注册表声明了前缀、桥接却没写」这种遗漏，此前**任何测试都抓不到**：
//
//   - 面板那条 TestEveryGatewayPrefixIsRoutable 拿注册表自己验证自己
//     （platformOfInPanel 遍历的就是同一个 platforms 切片）—— 循环断言，恒真；
//   - 运行时则静默掉进 chatCompletions 裸名分支，由腾讯池回「模型不存在」，
//     看起来像上游问题而不是缺了桥接。
//
// 有了这张表：面板侧对表、表内自洽，两头任一不同步都会红。
//
// 新增桥接的三步（缺一步就有对应的红灯）：
//  1. 写 `isXxxModel` 判据 + `chatCompletions` 里的分发分支
//  2. 这里加一行（表内自洽测试会核对前缀 ↔ 判据，也核对 PlatformOf）
//  3. 面板注册表把该平台改成 GroupGateway + Prefix（面板侧测试会核对注册表 ↔ 本表）
var gatewayRoutes = []struct {
	Prefix string
	Match  func(string) bool
}{
	{qoderModelPrefix, isQoderModel},
	{copilotModelPrefix, isCopilotModel},
	{clineModelPrefix, isClineModel},
	{autoclawModelPrefix, isAutoClawModel},
	{qclawModelPrefix, isQClawModel},
	{traeModelPrefix, isTraeModel},
	{accioModelPrefix, isAccioModel},
	{traeworkModelPrefix, isTraeWorkModel},
	{codexModelPrefix, isCodexModel},
	{freeModelPrefix, isFreeModel},
	{loomyModelPrefix, isLoomyModel},
	{zaiModelPrefix, isZaiModel},
	{raccoonModelPrefix, isRaccoonModel},
	{codeartsModelPrefix, isCodeArtsModel},
	{imaModelPrefix, isIMAModel},
	{marvisModelPrefix, isMarvisModel},
}

// GatewayPrefixes 导出全部**有对话桥接**的模型前缀。
//
// 面板包的平台注册表拿它做交叉校验（panel 不 import server 的生产代码，
// 只在 _test.go 里 import，不构成环）。
func GatewayPrefixes() []string {
	out := make([]string, 0, len(gatewayRoutes))
	for _, r := range gatewayRoutes {
		out = append(out, r.Prefix)
	}
	return out
}
