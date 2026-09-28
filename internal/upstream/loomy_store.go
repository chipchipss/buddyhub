// Package upstream: loomy_store.go — Loomy 密码安全存储 + 无人值守续期。
//
// 密码落盘策略（用户明确选择的无人值守模式）：
//   - Windows：DPAPI（CryptProtectData，CurrentUser 作用域）加密——绑定当前
//     Windows 用户，换用户/拷贝文件到别的机器都解不开。存 base64 前缀 "dpapi:"。
//   - 非 Windows：无等价系统设施，退回明文落盘（文件由 extstore 以 0600 写出），
//     存前缀 "plain:"。文档明确标注该差异。
//
// 续期语义（对齐 loomy2api）：session 14 天、无刷新 token；剩余 <3 天时用
// 存量密码静默重登，新 session 双写（外部池 + data/loomy-session.json）。
package upstream

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

// loomyCredWithPassword loomy-cli 凭据（含可选存密密码）。
// 与 saveLoomyLogin 落库结构兼容：旧凭据无 password 字段照常解析。
type LoomyCred struct {
	Session  string `json:"session"`
	UserID   string `json:"userid"`
	Phone    string `json:"phone"`
	Password string `json:"password,omitempty"` // "dpapi:" 或 "plain:" 前缀
	// LoginAt session 签发时间（Unix 秒）——续期判据。旧凭据缺省 0=未知，
	// 未知时按“可能过期”处理（首轮维护即尝试重登，有密码才动作）。
	LoginAt int64 `json:"login_at,omitempty"`
}

// SessionDaysLeft 按登录时间估算剩余天数；未知返回 -1。
func (c *LoomyCred) SessionDaysLeft() float64 {
	if c.LoginAt <= 0 {
		return -1
	}
	elapsed := timeNow() - c.LoginAt
	remain := LoomySessionExpireSec - elapsed
	if remain <= 0 {
		return 0
	}
	return float64(remain) / 86400.0
}

// NeedsRenew 剩余 <3 天（或时间未知但有密码——首轮校验）。
func (c *LoomyCred) NeedsRenew() bool {
	if c.Password == "" {
		return false
	}
	d := c.SessionDaysLeft()
	if d < 0 {
		return true // 时间未知：有密码就试着续（重登成功即刷新时间戳）
	}
	return d < 3
}

// timeNow 独立变量便于测试注入。
var timeNow = func() int64 { return time.Now().Unix() }

// ---- 密码加解密 ----

const (
	pwPrefixDPAPI = "dpapi:"
	pwPrefixPlain = "plain:"
)

// ProtectPassword 加密密码（Windows DPAPI / 其他平台明文+前缀）。
func ProtectPassword(plain string) (string, error) {
	if runtime.GOOS == "windows" {
		blob, err := dpapiProtect([]byte(plain))
		if err != nil {
			// DPAPI 失败（如精简系统）：降级明文，不阻塞无人值守
			return pwPrefixPlain + plain, nil
		}
		return pwPrefixDPAPI + base64.StdEncoding.EncodeToString(blob), nil
	}
	return pwPrefixPlain + plain, nil
}

// UnprotectPassword 还原密码；无密码返回空串。
func UnprotectPassword(stored string) (string, error) {
	if stored == "" {
		return "", nil
	}
	if len(stored) > len(pwPrefixDPAPI) && stored[:len(pwPrefixDPAPI)] == pwPrefixDPAPI {
		if runtime.GOOS != "windows" {
			return "", errors.New("dpapi 密文只能在 Windows 原机原用户解密")
		}
		blob, err := base64.StdEncoding.DecodeString(stored[len(pwPrefixDPAPI):])
		if err != nil {
			return "", err
		}
		out, err := dpapiUnprotect(blob)
		if err != nil {
			return "", fmt.Errorf("DPAPI 解密失败（换用户/换机?）: %w", err)
		}
		return string(out), nil
	}
	if len(stored) > len(pwPrefixPlain) && stored[:len(pwPrefixPlain)] == pwPrefixPlain {
		return stored[len(pwPrefixPlain):], nil
	}
	// 无前缀：旧数据兼容，视为明文
	return stored, nil
}

// ---- DPAPI (crypt32.dll) ----

type blob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
)

func dpapiProtect(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, errors.New("empty")
	}
	in := blob{cbData: uint32(len(src)), pbData: &src[0]}
	var out blob
	// CRYPTPROTECT_UI_FORBIDDEN
	r1, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		0x1, uintptr(unsafe.Pointer(&out)))
	if r1 == 0 {
		return nil, err
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return unsafe.Slice(out.pbData, out.cbData), nil
}

func dpapiUnprotect(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, errors.New("empty")
	}
	in := blob{cbData: uint32(len(src)), pbData: &src[0]}
	var out blob
	r1, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&out)))
	if r1 == 0 {
		return nil, err
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return unsafe.Slice(out.pbData, out.cbData), nil
}

func localFree(p uintptr) { procLocalFree.Call(p) }

// ParseLoomyCred 从外部池凭据 JSON 解析（兼容旧结构无 password/login_at）。
func ParseLoomyCred(raw json.RawMessage) (*LoomyCred, error) {
	var c LoomyCred
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, err
	}
	return &c, nil
}
