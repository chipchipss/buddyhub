// Package atomicfile 提供「写完再替换」的单文件写入。
//
// 为什么单独成包：本仓库的落盘点分两类。一类（pool / extstore / zai / model
// catalog / auth）的 saveLocked 整程持自身的锁，写盘天生串行，不会互踩；另一类
// （scheduler 的日状态、usage 的记录器）刻意把快照放在锁内、写盘放在锁外——理由
// 是写盘耗时不该挡住判定读。这个取舍本身要对，但它把临时文件暴露在并发之下：
// 固定名 path+".tmp" 时两个 writer 会互相把对方的中间态 rename 走，实测后果不是
// 「多一次失败日志」而是**目标文件整个消失**（rename 报「找不到文件」），下次启动
// 读不到当日状态 = 全池重签一遍。
//
// 这里给的就是那两条不变量：临时名唯一（同目录内不撞车）+ 失败必清理（不留残骸）。
// 调用方若还需要「同一时刻只有一个 writer」，自己持写锁；本包不代替那层。
package atomicfile

import (
	"os"
	"path/filepath"
	"sync"
)

// 按路径串行。唯一临时名解决了「两个 writer 互踩中间态」，但目标文件本身在
// Windows 上仍不能并发覆盖——rename 撞上已存在目标会报 Access is denied。
// 与其要求每个调用方自备写锁（忘记就是又一次 usage.json 消失），不如在这里
// 把同一文件的写排成一条队。表里每个被写过的路径留一把锁，本仓库是个位数。
var (
	locksMu sync.Mutex
	locks   = map[string]*sync.Mutex{}
)

func lockFor(path string) *sync.Mutex {
	key, err := filepath.Abs(path)
	if err != nil {
		key = path
	}
	locksMu.Lock()
	defer locksMu.Unlock()
	l, ok := locks[key]
	if !ok {
		l = &sync.Mutex{}
		locks[key] = l
	}
	return l
}

// Write 把 raw 原子写入 path（目录不存在则创建，权限 0600：落盘的都是凭据/账目）。
// 对同一 path 的并发调用会串行执行，最后一次调用决定文件内容。
func Write(path string, raw []byte) error {
	l := lockFor(path)
	l.Lock()
	defer l.Unlock()

	dir := filepath.Dir(path)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	// Windows：rename 不覆盖已存在目标，先删再改名。
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(path)
		if err2 := os.Rename(name, path); err2 != nil {
			_ = os.Remove(name)
			return err2
		}
	}
	return nil
}
