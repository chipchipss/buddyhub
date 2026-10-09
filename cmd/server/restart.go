// restart.go 进程自我重启：配置页「立即重启」那条路。
//
// 为什么不能盲目 spawn：systemd / docker 的重启策略本来就会在进程退出后把它带
// 回来，此时自己再拉一个，就会有两个进程抢同一个监听端口（表现是「重启后网关
// 起不来 / 端口被占用」）。所以判据只用**正面证据**——只有明确知道有人在监督时
// 才只退出不自举；拿不准时按裸跑处理（自己拉起再接棒），最坏情况是多起一个进程
// 由监督器报端口冲突，也不会把服务留在原地不动。
package main

import (
	"log"
	"os"
	"os/exec"
	"time"
)

// selfRestart 换一个新进程接棒，然后本进程退出。返回的 error 只到「没换成」为止。
func selfRestart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if supervised() {
		log.Printf("restart: 检测到监督器，本进程退出后由它拉起（不自行 spawn，避免两个进程抢端口）")
		go func() { time.Sleep(120 * time.Millisecond); os.Exit(0) }()
		return nil
	}
	wd, wdErr := os.Getwd()
	cmd := exec.Command(exe, os.Args[1:]...)
	cmd.Dir = wd
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// 不 detach：本进程退出后子进程会被 init 收养并继续跑，两个平台上都是如此；
	// 而 detach 需要按 OS 分支设 SysProcAttr，多出来的复杂度换不到任何保证。
	if err := cmd.Start(); err != nil {
		return err
	}
	log.Printf("restart: 已拉起新进程 pid=%d（%s %v），本进程退出", cmd.Process.Pid, exe, os.Args[1:])
	if wdErr != nil {
		log.Printf("restart: 取工作目录失败，新进程用其默认目录：%v", wdErr)
	}
	go func() { time.Sleep(120 * time.Millisecond); os.Exit(0) }()
	return nil
}

// supervised 只看明确的监督器痕迹：systemd 的 sd_notify 套接字、容器运行时注入的
// container 变量，以及本仓库 systemd unit / compose 里显式声明的 BUDYHUB_SUPERVISED。
// 判据一律是「有没有正面证据」，不分平台：Windows 上没人设这些变量，自然走自举；
// 按 OS 硬短路会让这条判据在某个平台上变成不可测的死逻辑。
func supervised() bool {
	for _, k := range []string{"NOTIFY_SOCKET", "container", "BUDYHUB_SUPERVISED"} {
		if v := os.Getenv(k); v != "" && v != "0" && v != "false" {
			return true
		}
	}
	return false
}
