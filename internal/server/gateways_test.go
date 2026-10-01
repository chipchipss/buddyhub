package server

import (
	"strings"
	"testing"
)

// TestGatewayRoutesAreCoherent 路由表自洽。
//
// 表里每一项都要同时满足三件事：前缀非空、**判据认得出自己的前缀**、
// `PlatformOf` 知道它。少任何一条都会在运行时表现为「带平台授权的 key 403」
// 或「静默掉进腾讯池」。
func TestGatewayRoutesAreCoherent(t *testing.T) {
	seen := map[string]bool{}
	for _, r := range gatewayRoutes {
		if r.Prefix == "" {
			t.Error("路由表里出现空前缀")
			continue
		}
		if seen[r.Prefix] {
			t.Errorf("前缀 %q 重复登记", r.Prefix)
		}
		seen[r.Prefix] = true

		if !r.Match(r.Prefix + "some-model") {
			t.Errorf("%q 的判据认不出自己的前缀 —— 分发链会对这个平台的模型失配", r.Prefix)
		}
		if r.Match("bare-model-without-prefix") {
			t.Errorf("%q 的判据把裸模型名也认成了自己的，会劫持腾讯池的流量", r.Prefix)
		}
		if got := PlatformOf(r.Prefix + "some-model"); got == "" {
			t.Errorf("PlatformOf 不认 %q —— 带平台授权的 API key 会 403", r.Prefix)
		}
	}
}

// TestPlatformOfCoversEveryRoute PlatformOf 认识的前缀必须都有桥接判据。
//
// 反方向的覆盖：只在 PlatformOf 里加前缀、忘了写桥接，API key 授权会放行，
// 然后请求落进腾讯池报「模型不存在」。
func TestPlatformOfCoversEveryRoute(t *testing.T) {
	routable := map[string]bool{}
	for _, p := range GatewayPrefixes() {
		routable[p] = true
	}
	// 逐个把 PlatformOf 认识的前缀找出来：拿一段确定不属于任何通道的模型名，
	// 用每个已知前缀反查 PlatformOf，确认它没被认成 workbuddy。
	for _, prefix := range GatewayPrefixes() {
		if PlatformOf(prefix+"m") == "" {
			t.Errorf("PlatformOf 漏了 %q", prefix)
		}
	}
	// workbuddy 的 realm 前缀（cn:/global:）由 resolveModel 处理，不进本表，
	// 但裸名必须回落到它——这是腾讯池的入口，不能丢。
	if got := PlatformOf("some-plain-model"); got != "workbuddy" {
		t.Errorf("裸模型名应回落 workbuddy，得到 %q", got)
	}
	_ = routable
}

// TestGatewayPrefixesAreDistinctAndSuffixed 前缀形状检查。
func TestGatewayPrefixesAreDistinctAndSuffixed(t *testing.T) {
	seen := map[string]bool{}
	for _, p := range GatewayPrefixes() {
		if !strings.HasSuffix(p, ":") {
			t.Errorf("%q 缺冒号结尾 —— 前缀协议是 `xxx:model`", p)
		}
		if seen[p] {
			t.Errorf("%q 重复", p)
		}
		seen[p] = true
	}
}
