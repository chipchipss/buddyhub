package zai

import (
	"crypto/rand"
	"encoding/hex"
	"math/big"
)

// Profile 每账号的桌面设备档案（X-Platform / X-Os-* / X-Client-* / X-Device-Mid 的取值来源）。
//
// 为什么一号一台：上游按设备维度做关联。多账号共用同一设备形态（或暴露
// 宿主机 Linux 内核特征）会让一批账号被识别为同一来源，牵一发动全身。
// 档案在入池时生成一次并持久化，跨请求稳定；换发即换一整台 SKU（含新 MID）。
type Profile struct {
	Platform  string `json:"platform"`   // darwin / win32
	Arch      string `json:"arch"`       // arm64 / x64
	OSVersion string `json:"os_version"` // X-Os-Version（os.release() 语义）
	Screen    string `json:"screen"`     // 分辨率（仅展示与自洽校验用）
	Language  string `json:"language"`
	Timezone  string `json:"timezone"`
	DeviceMid string `json:"device_mid"`
}

// sku 一台真实桌面形态：(平台, 架构, 系统版本, 分辨率, 权重)。
// 分辨率与平台绑定——Mac 的逻辑分辨率不会出现在 Windows 上。
type sku struct {
	platform, arch, osVersion, screen string
	weight                            int
}

// skuPool 常见桌面端形态（权重按装机量粗估）。取值均为真实机型事实：
// macOS 15 的内核版本是 darwin 24.x、Windows 11 23H2/24H2 是 10.0.22631/26100、
// MacBook Air 13" 逻辑分辨率 1512x982 等。
var skuPool = []sku{
	// Apple silicon 笔记本（主力）
	{"darwin", "arm64", "24.6.0", "1512x982", 10},
	{"darwin", "arm64", "24.5.0", "1512x982", 10},
	{"darwin", "arm64", "24.6.0", "1728x1117", 8},
	{"darwin", "arm64", "25.5.0", "1512x982", 8},
	{"darwin", "arm64", "24.5.0", "1728x1117", 8},
	{"darwin", "arm64", "25.5.0", "1728x1117", 6},
	{"darwin", "arm64", "23.6.0", "1512x982", 5},
	{"darwin", "arm64", "24.5.0", "2560x1440", 4},
	{"darwin", "arm64", "24.6.0", "2560x1600", 3},
	// Intel Mac 存量（Ventura/Sonoma；新系统不再配 x64）
	{"darwin", "x64", "23.6.0", "1920x1080", 2},
	{"darwin", "x64", "22.6.0", "1440x900", 2},
	// Windows 11 主流 + 少量 Win10
	{"win32", "x64", "10.0.22631", "1920x1080", 8},
	{"win32", "x64", "10.0.26100", "1920x1080", 7},
	{"win32", "x64", "10.0.22631", "2560x1440", 5},
	{"win32", "x64", "10.0.26200", "1920x1080", 4},
	{"win32", "x64", "10.0.26100", "2560x1440", 3},
	{"win32", "x64", "10.0.22621", "1920x1080", 3},
	{"win32", "x64", "10.0.22631", "3840x2160", 2},
	{"win32", "x64", "10.0.19045", "1920x1080", 2},
	{"win32", "x64", "10.0.26100", "2560x1600", 1},
}

// locales 语言 × 时区对（取真实地区组合，不混搭）。
var locales = []struct{ lang, tz string }{
	{"zh-CN", "Asia/Shanghai"},
	{"zh-CN", "Asia/Shanghai"},
	{"zh-CN", "Asia/Shanghai"},
	{"en-US", "America/Los_Angeles"},
	{"en-US", "America/New_York"},
	{"en-US", "Europe/London"},
}

// newProfile 抽样一台桌面 SKU + 地区对 + 全新 device_mid。
func newProfile() *Profile {
	total := 0
	for _, s := range skuPool {
		total += s.weight
	}
	pick := randInt(total)
	var chosen sku
	for _, s := range skuPool {
		if pick < s.weight {
			chosen = s
			break
		}
		pick -= s.weight
	}
	loc := locales[randInt(len(locales))]
	return &Profile{
		Platform:  chosen.platform,
		Arch:      chosen.arch,
		OSVersion: chosen.osVersion,
		Screen:    chosen.screen,
		Language:  loc.lang,
		Timezone:  loc.tz,
		DeviceMid: newUUID(),
	}
}

// EnsureProfile 补齐缺失档案（老数据/手工导入）。
func (a *Account) EnsureProfile() {
	if a.Fingerprint == nil || a.Fingerprint.DeviceMid == "" {
		a.Fingerprint = newProfile()
	}
}

// PlatformFull X-Platform 头取值：`<platform>-<arch>`。
func (p *Profile) PlatformFull() string {
	if p == nil {
		return "darwin-arm64"
	}
	return p.Platform + "-" + p.Arch
}

// OSCategory X-Os-Category 头取值。
func (p *Profile) OSCategory() string {
	if p == nil {
		return "macos"
	}
	switch p.Platform {
	case "darwin", "macos":
		return "macos"
	case "win32", "windows":
		return "windows"
	default:
		return "linux"
	}
}

func randInt(n int) int {
	if n <= 1 {
		return 0
	}
	v, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(v.Int64())
}

func newUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString(b[:])
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:32]
}

// UUID 导出（追踪头用）。
func UUID() string { return newUUID() }
