package upstream

import (
	"runtime"
	"strings"
	"testing"
)

// 密码加解密必须跨平台往返通——这条是 build tag 拆分（DPAPI 独立成
// loomy_dpapi_windows.go）的护栏：拆错了会出现「Windows 编不过」或
// 「非 Windows 编不过」，而这类问题在只跑单一平台的 CI 上看不见。
//
// ⚠️ 非 Windows 只校验往返 + 前缀（明文路径必然通）。Windows 走 DPAPI，
// 其往返依赖本机 DPAPI 可用性（精简系统/服务账户下可能不可用），故失败时
// 只告警不判失败——**测试不该因运行环境而红**。DPAPI 本身的可用性由
// ProtectPassword 的降级分支兜住（失败即落 plain: 明文）。
func TestProtectUnprotectRoundTrip(t *testing.T) {
	const pw = "p@ssw0rd-中文-🔐"
	stored, err := ProtectPassword(pw)
	if err != nil {
		t.Fatalf("ProtectPassword: %v", err)
	}
	if stored == pw {
		t.Fatal("落盘值不能是明文原样")
	}
	got, err := UnprotectPassword(stored)
	if err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("本机 DPAPI 不可用（精简系统/服务账户等）：%v", err)
		}
		t.Fatalf("UnprotectPassword: %v", err)
	}
	if got != pw {
		t.Errorf("往返不一致: got %q want %q", got, pw)
	}
}

// 前缀必须与平台相符：Windows 走 dpapi:，其它平台走 plain:。
// （非 Windows 用明文是有意取舍——DPAPI 没有跨平台等价物，文件由 extstore
// 以 0600 写出；前缀让读侧能判断来源，避免把明文当密文解。）
func TestPasswordPrefixMatchesPlatform(t *testing.T) {
	stored, err := ProtectPassword("secret")
	if err != nil {
		t.Fatalf("ProtectPassword: %v", err)
	}
	if runtime.GOOS == "windows" {
		if !strings.HasPrefix(stored, pwPrefixDPAPI) {
			t.Errorf("Windows 应落 dpapi: 前缀，实际 %q", stored[:min(12, len(stored))])
		}
		return
	}
	if !strings.HasPrefix(stored, pwPrefixPlain) {
		t.Errorf("非 Windows 应落 plain: 前缀，实际 %q", stored[:min(12, len(stored))])
	}
}

// 空密码不加密也不报错（调用方据此判断「没存密码」）。
func TestProtectEmptyPassword(t *testing.T) {
	got, err := UnprotectPassword("")
	if err != nil || got != "" {
		t.Errorf("空值应直返空串且无错: got %q err %v", got, err)
	}
}

// 无前缀的旧数据按明文兼容（升级不能把老用户的密码变成读不出来）。
func TestUnprotectLegacyNoPrefix(t *testing.T) {
	got, err := UnprotectPassword("legacy-plain")
	if err != nil || got != "legacy-plain" {
		t.Errorf("无前缀应按明文兼容: got %q err %v", got, err)
	}
}

// dpapi: 密文在非 Windows 上必须明确报错，而不是静默返回乱码——
// 静默失败会让「无人值守续期」拿着错误密码反复重登。
// 报错还要给出下一步：跨机迁移后运维面对一条死消息，只能靠猜。
func TestDPAPICipherOnNonWindowsFailsLoudly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 上本就应能解密")
	}
	stored := pwPrefixDPAPI + "QUJD"
	_, err := UnprotectPassword(stored)
	if err == nil {
		t.Fatal("非 Windows 解 dpapi: 密文应报错")
	}
	if !strings.Contains(err.Error(), "重新登录") {
		t.Errorf("错误里没有下一步动作，运维无从处理: %v", err)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
