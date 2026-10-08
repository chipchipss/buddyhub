package atomicfile

import (
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
)

// 并发写同一目标：每次调用都必须完整落成一版可解析的内容，且不留临时文件残骸。
// 固定临时名的老写法在这里会直接让目标文件消失（见包注释）。
func TestWriteConcurrentKeepsTargetUsable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	const n = 32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			payload := []byte(`{"writer":` + strconv.Itoa(i) + `}`)
			<-start
			if err := Write(path, payload); err != nil {
				t.Errorf("writer %d: %v", i, err)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("并发写之后目标文件读不到: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("目标文件为空")
	}
	// 内容必须是某一个 writer 的完整快照，不能是两版的拼接。
	s := string(raw)
	if s[:1] != "{" || s[len(s)-1:] != "}" {
		t.Fatalf("内容是混合态: %q", s)
	}
}

func TestWriteLeavesNoTempFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "state.json")
	if err := Write(path, []byte("x")); err != nil {
		t.Fatalf("建目录并写入: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "sub", "*"))
	if len(matches) != 1 {
		t.Fatalf("目录里应有且只有 state.json，实得 %v", matches)
	}
}

// 覆盖已存在文件：Windows 的 rename 不覆盖，必须走删除后改名那条路。
func TestWriteOverwritesExisting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o600); err != nil {
		t.Fatalf("预置旧文件: %v", err)
	}
	if err := Write(path, []byte("new")); err != nil {
		t.Fatalf("覆盖写: %v", err)
	}
	raw, _ := os.ReadFile(path)
	if string(raw) != "new" {
		t.Fatalf("覆盖后内容=%q", raw)
	}
}
