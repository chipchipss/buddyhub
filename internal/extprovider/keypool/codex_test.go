// keypool 此前没有测试文件；这里只钉住 codexHomes 的取值优先级——它是 codex
// 账号池的唯一凭据来源，选错目录等于扫错机器。
package keypool

import (
	"path/filepath"
	"testing"
)

// 显式 CODEX_HOME 优先，且只用它（官方 codex CLI 认这个变量，服务器部署常只
// 设它不设 HOME）。多号池的 b/c 目录是 Home 派生的约定名，显式指定时不再猜。
func TestCodexHomesHonorsExplicitEnv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "codex-explicit")
	t.Setenv("CODEX_HOME", dir)

	got := codexHomes()
	if len(got) != 1 {
		t.Fatalf("显式 CODEX_HOME 应只给一个候选，实际 %d 个", len(got))
	}
	if got[0].Key != "a" || got[0].Home != dir {
		t.Fatalf("候选 = %+v，期望 {a %s}", got[0], dir)
	}
}

// 未显式指定时，候选一律是绝对路径：曾经因为忽略 UserHomeDir 的错误，在拿不到
// 家目录时退化成相对路径 ".codex"，于是去扫当前工作目录里的同名文件夹。
func TestCodexHomesNeverRelative(t *testing.T) {
	t.Setenv("CODEX_HOME", "")
	for _, cand := range codexHomes() {
		if cand.Home != "" && !filepath.IsAbs(cand.Home) {
			t.Fatalf("候选目录 %s（%s）是相对路径，会随 CWD 变化", cand.Key, cand.Home)
		}
	}
}
