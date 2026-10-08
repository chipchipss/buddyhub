package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readUnit 把 deploy/buddyhub.service 读成 section -> key -> values。
// systemd 允许同一 key 多次出现（ReadWritePaths 就是），所以值是切片。
func readUnit(t *testing.T) map[string]map[string][]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "buddyhub.service"))
	if err != nil {
		t.Fatalf("读不到 deploy/buddyhub.service：%v", err)
	}
	unit := map[string]map[string][]string{}
	section := ""
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if unit[section] == nil {
				unit[section] = map[string][]string{}
			}
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok || section == "" {
			continue
		}
		unit[section][strings.TrimSpace(k)] = append(unit[section][strings.TrimSpace(k)], strings.TrimSpace(v))
	}
	return unit
}

// unitValue 返回 section 下第一个值，缺失即 t.Fatal——这些断言的意义就是
// 「有人把某行删了/改错了」，所以不能容忍静默跳过。
func unitValue(t *testing.T, unit map[string]map[string][]string, section, key string) string {
	t.Helper()
	vals, ok := unit[section][key]
	if !ok || len(vals) == 0 {
		t.Fatalf("[%s] 缺 %s=（unit 里这一行不能删，见文件头注释）", section, key)
	}
	return vals[0]
}

// TestDeployUnitKeepsLoadBearingDirectives 守住 Linux 裸机路径的三条「删了不报错、
// 只会静默跑错」的赋值，外加 unit 与二进制/配置目录的自洽性。
//
// 为什么要测：GitHub runner 没有 systemd，`systemd-analyze verify` 在容器化的
// runner 上不可靠，CI 编不出这条路径的证据。而 unit 文件里删掉一行不会让任何人
// 报警——只会得到「服务 active 但凭据往 / 写、排程差 8 小时、配置写不回」。
func TestDeployUnitKeepsLoadBearingDirectives(t *testing.T) {
	unit := readUnit(t)

	wd := unitValue(t, unit, "Service", "WorkingDirectory")
	if !strings.HasPrefix(wd, "/var/lib/") {
		t.Errorf("WorkingDirectory=%q：配置里 auth_dir/state_file 是相对路径，这条决定凭据落在哪", wd)
	}

	tz := ""
	for _, e := range unit["Service"]["Environment"] {
		if strings.HasPrefix(e, "TZ=") {
			tz = e
		}
	}
	if tz == "" {
		t.Fatal("[Service] 缺 Environment=TZ=：UTC 宿主上排程的小时槽会比北京时间晚 8 小时")
	}
	if tz != "TZ=Asia/Shanghai" {
		t.Errorf("Environment=%q 不是 Asia/Shanghai（排程的日界按固定 UTC+8 算）", tz)
	}

	cfgAbs := ""
	exec := unitValue(t, unit, "Service", "ExecStart")
	for _, f := range strings.Fields(exec) {
		if strings.HasPrefix(f, "-config") {
			cfgAbs = strings.TrimPrefix(f, "-config")
		} else if cfgAbs == "" && strings.HasPrefix(f, "/etc/") {
			cfgAbs = f
		}
	}
	if cfgAbs == "" {
		t.Fatalf("ExecStart=%q 没带 -config 绝对路径（相对路径会随 CWD 漂移）", exec)
	}
	if !strings.HasSuffix(exec, "/buddyhub -config "+cfgAbs) && !strings.Contains(exec, "buddyhub") {
		t.Errorf("ExecStart=%q 启动的不是 buddyhub 二进制", exec)
	}

	// ProtectSystem=strict 把 / 挂成只读：配置与数据必须显式可写，否则
	// 占位密钥轮换/面板保存都会静默失败。
	// 用字符串切片而不是 filepath.Dir：unit 里的路径是 Linux 的，在 Windows 开发机上
	// filepath 会把 /etc/buddyhub 变成 \etc\buddyhub，比对必然对不上（第一版就踩了）。
	readwrite := strings.Fields(unitValue(t, unit, "Service", "ReadWritePaths"))
	cfgDir := cfgAbs[:strings.LastIndex(cfgAbs, "/")]
	for _, want := range []string{wd, cfgDir} {
		found := false
		for _, p := range readwrite {
			if p == want {
				found = true
			}
		}
		if !found {
			t.Errorf("ReadWritePaths=%v 不含 %q：ProtectSystem=strict 下这里写不进去", readwrite, want)
		}
	}
	if strings.Contains(exec, "-config /") && cfgDir == "/" {
		t.Errorf("配置在根目录（%s），ReadWritePaths 授权会波及整个 /", cfgAbs)
	}

	if strings.EqualFold(unitValue(t, unit, "Service", "User"), "root") {
		t.Error("[Service] User=root：凭据目录 0600 挡不住同机 root 进程写的东西，且容器/裸机都不需要")
	}
	if unitValue(t, unit, "Service", "KillSignal") != "SIGTERM" {
		t.Error("KillSignal 必须是 SIGTERM，否则不走 main.go 的 srv.Shutdown 优雅停机")
	}
	if unitValue(t, unit, "Service", "UMask") != "0077" {
		t.Error("UMask=0077：落盘的是 accessToken/refreshToken，新建目录默认不该 group/other 可读")
	}
	if unitValue(t, unit, "Install", "WantedBy") != "multi-user.target" {
		t.Error("[Install] WantedBy=multi-user.target 不能省，否则 enable 不开机自启")
	}
}
