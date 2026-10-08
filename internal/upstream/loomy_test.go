package upstream

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// 面板 Loomy 凭据的两条 Linux-only 隐患：写死 ./data（换 WorkingDirectory 就
// 与 state_file 分家）和 0644（Windows 不认权限位，本机永远看不出问题）。
func TestLoomySessionFollowsDirAndIsPrivate(t *testing.T) {
	prev := loomySessionDir
	defer SetLoomySessionDir(prev)

	dir := filepath.Join(t.TempDir(), "state")
	SetLoomySessionDir(dir)
	if err := SaveLoomySession(&LoomySession{Session: "sess-1", UserID: "u-1"}); err != nil {
		t.Fatalf("落盘失败: %v", err)
	}

	path := filepath.Join(dir, "loomy-session.json")
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("没有写进指定目录（%v），凭据跟着 CWD 走就是这次要修的", err)
	}
	if runtime.GOOS != "windows" {
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Fatalf("登录态文件权限 = %o, want 600（同机其他用户可读）", perm)
		}
	}

	// 读侧必须走同一个目录，否则写完自己找不到。
	got, err := FindLoomySession()
	if err != nil {
		t.Fatalf("FindLoomySession 读不回: %v", err)
	}
	if got.Session != "sess-1" || got.UserID != "u-1" {
		t.Fatalf("读回 %+v，期望 sess-1/u-1", got)
	}
}

// 空串与 "." 一律忽略：一次误传就把凭据写到当前目录，正是上面要避免的事。
func TestSetLoomySessionDirIgnoresEmptyAndDot(t *testing.T) {
	prev := loomySessionDir
	defer SetLoomySessionDir(prev)

	dir := filepath.Join(t.TempDir(), "data")
	SetLoomySessionDir(dir)
	SetLoomySessionDir(".")
	SetLoomySessionDir("")
	if got := loomySessionPath(); got != filepath.Join(dir, "loomy-session.json") {
		t.Fatalf("空参数把目录改成了 %s，应当保持 %s", got, filepath.Join(dir, "loomy-session.json"))
	}
}

func TestMaskPhone(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"13812345678", "138****5678"},
		{"123456", "123456"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := MaskPhone(tc.in); got != tc.want {
			t.Errorf("MaskPhone(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestLoomyGetTasks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/onboarding/tasks" {
			t.Errorf("path = %s, want /api/v1/onboarding/tasks", r.URL.Path)
		}
		if r.Header.Get("token") != "test-token" {
			t.Errorf("token = %s, want test-token", r.Header.Get("token"))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "000000",
			"desc": "成功",
			"data": map[string]any{
				"tasks": map[string]bool{
					"first_message": true,
					"pick_skill":    true,
				},
				"earned": 1500,
				"total":  10000,
			},
		})
	}))
	defer srv.Close()

	client := NewLoomyClient(srv.URL)
	tasks, earned, total, err := client.GetTasks("test-token")
	if err != nil {
		t.Fatalf("GetTasks failed: %v", err)
	}
	if !tasks["first_message"] || !tasks["pick_skill"] {
		t.Errorf("expected first_message and pick_skill to be true")
	}
	if tasks["generate_ppt"] {
		t.Errorf("expected generate_ppt to be false")
	}
	if earned != 1500 || total != 10000 {
		t.Errorf("earned/total = %d/%d, want 1500/10000", earned, total)
	}
}

func TestLoomyCompleteTask(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1/onboarding/tasks/complete" {
			t.Errorf("path = %s, want /api/v1/onboarding/tasks/complete", r.URL.Path)
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["key"] != "generate_ppt" {
			t.Errorf("key = %s, want generate_ppt", body["key"])
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "000000",
			"desc": "成功",
			"data": map[string]any{
				"alreadyCompleted": false,
				"balance":          4000,
			},
		})
	}))
	defer srv.Close()

	client := NewLoomyClient(srv.URL)
	already, bal, err := client.CompleteTask("test-token", "generate_ppt")
	if err != nil {
		t.Fatalf("CompleteTask failed: %v", err)
	}
	if already {
		t.Errorf("alreadyCompleted = true, want false")
	}
	if bal != 4000 {
		t.Errorf("balance = %d, want 4000", bal)
	}
}

func TestLoomyCompleteAll(t *testing.T) {
	tmpDir := t.TempDir()
	completedSet := make(map[string]bool)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": "000000",
				"desc": "成功",
				"data": map[string]any{
					"tasks":  completedSet,
					"earned": 0,
					"total":  10000,
				},
			})
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		completedSet[body["key"]] = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "000000",
			"desc": "成功",
			"data": map[string]any{
				"alreadyCompleted": false,
				"balance":          10000,
			},
		})
	}))
	defer srv.Close()

	session := &LoomySession{
		Session:     "test-token",
		UserID:      "uid123",
		Phone:       "13800138000",
		UserDataDir: tmpDir,
	}

	client := NewLoomyClient(srv.URL)
	keys, status, err := client.CompleteAll(session)
	if err != nil {
		t.Fatalf("CompleteAll failed: %v", err)
	}
	if len(keys) != len(LoomyRegistry) {
		t.Errorf("newly completed count = %d, want %d", len(keys), len(LoomyRegistry))
	}
	if status.Earned != 10000 {
		t.Errorf("status.Earned = %d, want 10000", status.Earned)
	}

	// Verify local cache file was written
	cacheFile := filepath.Join(tmpDir, "onboarding-tasks.json")
	if _, err := os.Stat(cacheFile); os.IsNotExist(err) {
		t.Fatalf("expected cache file %s to exist", cacheFile)
	}
}
