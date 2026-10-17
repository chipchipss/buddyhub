package auth

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSaveAtomicWriteFailureKeepsOldFile refresh 成功但落盘失败时必须返回 error，
// 且**不能写坏原文件**。
//
// 守的契约：SaveAtomic 是 refresh 之后唯一的持久化出口（auth.go:290）。它若在
// 写盘失败时静默成功（或把原文件截断/半写），下次启动 LoadDir 读到的是坏凭证
// ——旧 token 至少还能用一阵子，坏文件让账号直接失效，表现为「全员掉线且要重登」。
// 反过来说：这里断言的两件事（返回 error + 原内容原样可读）正是 tmp+rename
// 这套原子写存在的理由，回归破坏任何一半都要红。
//
// 失败注入方式：把 FilePath+".tmp" 预先占成**目录**。os.WriteFile 以
// O_WRONLY|O_CREATE|O_TRUNC 打开目录得到 EISDIR——这条对 root 同样成立
// （区别于 permhint_test.go 的 0555 目录，root 不受权限位约束必须跳过）。
func TestSaveAtomicWriteFailureKeepsOldFile(t *testing.T) {
	dir := t.TempDir()
	fp := filepath.Join(dir, "workbuddy-u1.json")

	// 原文件：一份完整的嵌套形凭证（插件可读形状）。
	const oldDoc = `{
  "auth": {
    "accessToken": "at-old",
    "refreshToken": "rt-old",
    "expiresAt": 1753600000,
    "domain": "www.workbuddy.ai",
    "realm": "global"
  },
  "account": {
    "uid": "u1",
    "enterpriseId": "e1",
    "nickname": "n1"
  }
}`
	if err := os.WriteFile(fp, []byte(oldDoc), 0o600); err != nil {
		t.Fatalf("准备原文件: %v", err)
	}

	// 抢占 tmp 路径 → WriteFile 必然失败（EISDIR），且 FilePath 本体不动。
	if err := os.Mkdir(fp+".tmp", 0o755); err != nil {
		t.Fatalf("抢占 tmp 路径: %v", err)
	}

	// refresh 后的新 token：写不进去，但内存里已经是新的了。
	a := &Auth{
		AccessToken:  "at-new",
		RefreshToken: "rt-new",
		ExpiresAt:    1753999999,
		Domain:       "www.workbuddy.ai",
		realm:        "global",
		UID:          "u1",
		EnterpriseID: "e1",
		Nickname:     "n1",
		FilePath:     fp,
	}

	err := a.SaveAtomic()
	if err == nil {
		t.Fatal("写盘失败必须返回 error（静默成功 = 重启后拿坏凭证）")
	}
	// 报的必须是**写 tmp** 这一处失败，不是更早的空 token / 空 FilePath 守卫
	// （那两条会给出完全不同的误导性原因）。
	if !strings.Contains(err.Error(), fp+".tmp") {
		t.Errorf("错误应指向 tmp 写失败 %s，实际: %v", fp+".tmp", err)
	}

	// 原文件原样可读：内容一个字节都不能变。
	got, rerr := os.ReadFile(fp)
	if rerr != nil {
		t.Fatalf("失败后原文件读不出来: %v", rerr)
	}
	if string(got) != oldDoc {
		t.Errorf("失败后原文件被写坏:\n got: %s\nwant: %s", got, oldDoc)
	}
	// 且仍是可解析的旧凭证（重启后 LoadDir 至少能加载成功）。
	reparsed, perr := Parse(got)
	if perr != nil {
		t.Fatalf("失败后原文件不再可解析: %v", perr)
	}
	if reparsed.AccessToken != "at-old" || reparsed.RefreshToken != "rt-old" {
		t.Errorf("失败后原文件 token 变了: %+v", reparsed)
	}

	// 不留半成品：目录里除了原文件和被抢占的 tmp 目录，不该多出任何文件。
	entries, derr := os.ReadDir(dir)
	if derr != nil {
		t.Fatalf("读目录: %v", derr)
	}
	for _, e := range entries {
		name := e.Name()
		if name == filepath.Base(fp) || name == filepath.Base(fp)+".tmp" {
			continue
		}
		t.Errorf("失败后留下多余文件 %s（原子写只应碰 tmp）", name)
	}
}
