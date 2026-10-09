// restart_test.go 盯的是 supervised() 这一个判据：判错方向的代价不对称——
// 该自举时没自举 = 服务停在原地不动（用户点了「立即重启」却没了网关）；
// 不该自举时自举 = 监督器又拉起一个进程抢同一个监听端口。
// 所以只允许「有正面证据」才走退出交给监督器这条路。
package main

import "testing"

func TestSupervisedNeedsPositiveEvidence(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"裸跑（没有任何监督器痕迹）", nil, false},
		{"systemd sd_notify", map[string]string{"NOTIFY_SOCKET": "/run/systemd/notify"}, true},
		{"本仓库 unit/compose 显式声明", map[string]string{"BUDYHUB_SUPERVISED": "1"}, true},
		{"声明成 0 不算监督器", map[string]string{"BUDYHUB_SUPERVISED": "0"}, false},
		{"容器运行时注入", map[string]string{"container": "podman"}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for k, v := range c.env {
				t.Setenv(k, v)
			}
			if got := supervised(); got != c.want {
				t.Fatalf("supervised() = %v，期望 %v", got, c.want)
			}
		})
	}
}
