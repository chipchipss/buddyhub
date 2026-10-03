//go:build !windows

// Package upstream: loomy_dpapi_other.go — 非 Windows 平台的 DPAPI 桩。
//
// Windows DPAPI（CryptProtectData）绑定「本机 + 当前用户」，没有跨平台等价物。
// 这些桩存在的唯一目的是让 loomy_store.go 的 ProtectPassword /
// UnprotectPassword 里的 runtime.GOOS 分支在非 Windows 上也能编译——
// 真正的行为由那边的分支保证：非 Windows 一律走 "plain:" 前缀明文落盘
// （文件由 extstore 以 0600 写出），根本不会调到下面这两个函数。
package upstream

import "errors"

// dpapiProtect 非 Windows 不可用（调用方不该走到这里）。
func dpapiProtect(src []byte) ([]byte, error) {
	return nil, errors.New("DPAPI 仅 Windows 可用")
}

// dpapiUnprotect 非 Windows 不可用：dpapi: 密文只能在原机原用户解密。
func dpapiUnprotect(src []byte) ([]byte, error) {
	return nil, errors.New("dpapi 密文只能在 Windows 原机原用户解密")
}
