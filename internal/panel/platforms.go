package panel

// platforms.go 平台注册表 —— **平台身份与能力的单一事实源**。
//
// 为什么要有这张表：平台清单原先散在 8 处（面板的账号目录、API Key 授权白名单、
// 外部账号增删白名单、登录入池的 provider 分派、以及前端的 4 张名字表）。
// 平台数上到两位数之后，「加一个平台要改 8 个地方、漏一处就是界面上少一块或
// 多一个点了报错的按钮」成了必然。现在后端这一张表说了算，前端全部从
// `GET /panel/api/platforms` 渲染。
//
// 表里只放**跨界面共用的事实**：id / 展示名 / 分组 / 模型前缀 / 入池方式 /
// 有无签到。各平台登录表单的字段细节仍在前端（那是纯 UI 关切），但**平台是否
// 存在、叫什么、能不能签到、走哪个前缀**只在这里定义一次。

import (
	"net/http"
	"sort"
)

// 入池方式：决定前端出哪一套交互。
const (
	LoginNone     = ""         // 无（本机凭据 / 配置页填写）
	LoginOAuth    = "oauth"    // 浏览器 OAuth 设备授权
	LoginDetect   = "detect"   // 本机客户端登录态检测
	LoginSMS      = "sms"      // 手机短信验证码
	LoginQR       = "qr"       // 扫码（面板出二维码）
	LoginDevice   = "device"   // 设备授权链接（浏览器打开）
	LoginCode     = "code"     // 设备码（用户在浏览器输入码）
	LoginCallback = "callback" // 本机回调（浏览器授权后自动跳回网关，无需人工操作）
	LoginManual   = "manual"   // 逐字段手工填写凭据
	LoginConfig   = "config"   // 配置页填写
)

// 分组按**能力**分（不是按「怎么配置」——那是 Login 字段的事）：
const (
	// GroupGateway 有模型前缀，可直接当 OpenAI 端点调用。
	GroupGateway = "gateway"
	// GroupPoints 无对话 API，只做每日签到/积分领取。
	GroupPoints = "points"
)

// Platform 一个平台的元数据。
type Platform struct {
	ID   string `json:"id"`   // provider id（也是落盘契约，不要改）
	Name string `json:"name"` // 展示名
	// Group 分组（gateway / points / local）。
	Group string `json:"group"`
	// Prefix 模型名前缀（"" = 该平台不是对话上游）。
	Prefix string `json:"prefix,omitempty"`
	// Login 入池方式（见上面常量；"" = 不通过「添加账号」入池）。
	Login string `json:"login,omitempty"`
	// Checkin 是否有每日签到（决定外部平台页签不签到的按钮）。
	Checkin bool `json:"checkin"`
	// Note 一句说明（界面上作为提示）。
	Note string `json:"note,omitempty"`
}

// platforms 注册表。**新增平台只改这一处**（外加前端那张表单字段表）。
var platforms = []Platform{
	// ── 对话通道 ──
	{ID: "workbuddy", Name: "腾讯 WorkBuddy", Group: GroupGateway, Prefix: "cn:", Login: LoginOAuth,
		Checkin: true, Note: "成长任务全自动"},
	{ID: "loomy", Name: "Loomy（讯飞）", Group: GroupGateway, Prefix: "loomy:", Login: LoginDetect,
		Checkin: true, Note: "客户端检测 / 手机号 / 短信"},
	{ID: "zai", Name: "Z.AI / 智谱", Group: GroupGateway, Prefix: "zai:", Login: LoginOAuth,
		Checkin: true, Note: "Coding Plan JWT + API Key 双通道"},
	{ID: "copilot", Name: "GitHub Copilot", Group: GroupGateway, Prefix: "copilot:", Login: LoginCode,
		Note: "设备码授权"},
	{ID: "cline", Name: "Cline", Group: GroupGateway, Prefix: "cline:", Login: LoginCode,
		Note: "含免费池，无需订阅"},
	{ID: "autoclaw", Name: "AutoClaw（智谱）", Group: GroupGateway, Prefix: "autoclaw:", Login: LoginSMS,
		Note: "手机号登录（国内版）"},
	{ID: "qoder", Name: "Qoder（阿里）", Group: GroupGateway, Prefix: "qoder:", Login: LoginDevice,
		Checkin: true, Note: "设备授权登录"},
	{ID: "qclaw", Name: "QClaw（腾讯）", Group: GroupGateway, Prefix: "qclaw:", Login: LoginQR,
		Note: "微信扫码登录（上游已宣布停运）"},
	{ID: "trae", Name: "Trae（字节）", Group: GroupGateway, Prefix: "trae:", Login: LoginCallback,
		Note: "浏览器授权（本机回调）"},
	{ID: "accio", Name: "Accio（阿里）", Group: GroupGateway, Prefix: "accio:", Login: LoginCallback,
		Note: "浏览器授权（本机回调）"},
	{ID: "traework", Name: "TraeWork（字节）", Group: GroupGateway, Prefix: "traework:", Login: LoginManual,
		Checkin: true, Note: "粘贴客户端凭据（含每日签到）"},
	{ID: "raccoon", Name: "小浣熊（商汤）", Group: GroupGateway, Prefix: "raccoon:", Login: LoginQR,
		Checkin: true, Note: "微信扫码登录（OpenAI 兼容直连）"},

	{ID: "codearts", Name: "CodeArts（华为云）", Group: GroupGateway, Prefix: "codearts:", Login: LoginManual,
		Checkin: true, Note: "永久 AK/SK（建议）"},

	// ── 积分 / 签到平台（无对话 API）──
	{ID: "lobsterai", Name: "LobsterAI（有道）", Group: GroupPoints, Login: LoginManual,
		Checkin: true, Note: "从客户端凭据文件复制"},

	// 本机凭据 / 配置页填写的通道：同样是可调用的对话通道，只是不从
	// 「添加账号」入池（Login 字段说明了这一点）。
	{ID: "codex", Name: "Codex（ChatGPT 订阅）", Group: GroupGateway, Prefix: "codex:", Login: LoginNone,
		Note: "本机 ~/.codex 凭据"},
	{ID: "free", Name: "免费 Key 池", Group: GroupGateway, Prefix: "free:", Login: LoginConfig,
		Note: "groq / 智谱 / llm7 / openrouter"},
}

// platformByID 按 id 取平台（不存在返回零值 + false）。
func platformByID(id string) (Platform, bool) {
	for _, p := range platforms {
		if p.ID == id {
			return p, true
		}
	}
	return Platform{}, false
}

// platformName 展示名（未知 id 原样返回，界面上不会出现空白）。
func platformName(id string) string {
	if p, ok := platformByID(id); ok {
		return p.Name
	}
	return id
}

// platformPrefix 模型前缀（未知 id 返回空串 = 非对话上游）。
func platformPrefix(id string) string {
	if p, ok := platformByID(id); ok {
		return p.Prefix
	}
	return ""
}

// platformHasCheckin 是否有每日签到。
func platformHasCheckin(id string) bool {
	if p, ok := platformByID(id); ok {
		return p.Checkin
	}
	return false
}

// platformsInGroup 取某分组的平台（保持注册表顺序）。
func platformsInGroup(group string) []Platform {
	var out []Platform
	for _, p := range platforms {
		if p.Group == group {
			out = append(out, p)
		}
	}
	return out
}

// platformIDs 全部平台 id（排序后，供 API Key 授权白名单校验）。
func platformIDs() []string {
	out := make([]string, 0, len(platforms))
	for _, p := range platforms {
		out = append(out, p.ID)
	}
	sort.Strings(out)
	return out
}

// getPlatforms GET /panel/api/platforms —— 平台注册表（前端据此渲染全部清单）。
//
// 前端不再自带任何平台名字表：一处定义、处处一致，「加平台漏改某张表」
// 从根上消掉。
func (p *Panel) getPlatforms(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"platforms": platforms,
	})
}

// platformUsesExtstore 该平台的账号是否落在 extstore（data/ext-accounts.json）。
//
// 腾讯池走 auths/、Z.AI 走自己的账号池、Codex 读本机、免费池读配置——
// 这四个不属于 extstore，其余都是。手工添加与登录入池都据此校验。
func platformUsesExtstore(id string) bool {
	if _, ok := platformByID(id); !ok {
		return false
	}
	switch id {
	case "workbuddy", "zai", "codex", "free":
		return false
	}
	return true
}
