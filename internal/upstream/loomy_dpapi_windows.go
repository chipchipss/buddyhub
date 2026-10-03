//go:build windows

// Package upstream: loomy_dpapi_windows.go — Loomy 密码的 Windows DPAPI 加解密。
//
// 与 loomy_store.go 里的 runtime 分支配对：**只有** Windows 编译这份，
// 其余平台由 loomy_dpapi_other.go 提供「不解密」的桩（见那边注释）。
// 单独成文件而不是塞 build tag 到 loomy_store.go 里，是因为 loomy_store.go
// 本身是跨平台共用的（凭据结构、session 判据、密码前缀都在那儿）。
package upstream

import (
	"errors"
	"syscall"
	"unsafe"
)

// blob DPAPI 的 DATA_BLOB 结构。
type blob struct {
	cbData uint32
	pbData *byte
}

var (
	crypt32                = syscall.NewLazyDLL("crypt32.dll")
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procCryptProtectData   = crypt32.NewProc("CryptProtectData")
	procCryptUnprotectData = crypt32.NewProc("CryptUnprotectData")
	procLocalFree          = kernel32.NewProc("LocalFree")
)

// dpapiProtect 用 DPAPI（CurrentUser 作用域）加密。
func dpapiProtect(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, errors.New("empty")
	}
	in := blob{cbData: uint32(len(src)), pbData: &src[0]}
	var out blob
	// CRYPTPROTECT_UI_FORBIDDEN = 0x1
	r1, _, err := procCryptProtectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		0x1, uintptr(unsafe.Pointer(&out)))
	if r1 == 0 {
		return nil, err
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return unsafe.Slice(out.pbData, out.cbData), nil
}

// dpapiUnprotect 解 DPAPI 密文（换用户/换机会失败）。
//
// ⚠️ CryptUnprotectData 是 **7 参**（末两个是 dwFlags 与 pDataOut）。
// 原实现漏传 dwFlags 只给了 6 个——Go 的 Call 是变参、不校验元数，于是
// pDataOut 拿到垃圾值，解密恒回 "The data is invalid"（即 Windows 上的
// 「存密码无人值守续期」从未真正跑通）。这里按真实签名补齐。
func dpapiUnprotect(src []byte) ([]byte, error) {
	if len(src) == 0 {
		return nil, errors.New("empty")
	}
	in := blob{cbData: uint32(len(src)), pbData: &src[0]}
	var out blob
	r1, _, err := procCryptUnprotectData.Call(
		uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0,
		0, // dwFlags
		uintptr(unsafe.Pointer(&out)))
	if r1 == 0 {
		return nil, err
	}
	defer localFree(uintptr(unsafe.Pointer(out.pbData)))
	return unsafe.Slice(out.pbData, out.cbData), nil
}

func localFree(p uintptr) { procLocalFree.Call(p) }
