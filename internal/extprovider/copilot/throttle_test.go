package copilot

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// 实测踩过的坑：浏览器那边 GitHub 已显示「Congratulations, you're all set!」，
// 面板却一直停在等待授权。GitHub 被问得太勤会连回 slow_down，而只要还在按原速
// 问，它就继续只回 slow_down——授权早已完成，token 却永远换不出来，直到设备码过期。
// 退避必须做在服务端：不能指望前端的节奏感。

func slowDownHandler(t *testing.T, mu *sync.Mutex, hits *int, err string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			mu.Lock()
			*hits++
			mu.Unlock()
			writeJSONT(w, map[string]any{"error": err})
		default:
			t.Errorf("不该打到上游别处: %s", r.URL.Path)
		}
	}
}

func TestPollSlowDownBacksOffLocally(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	withMock(t, slowDownHandler(t, &mu, &hits, "slow_down"), nil)

	f := &DeviceFlow{DeviceCode: "dev-1", Interval: 5}
	ctx := context.Background()

	r1, err := f.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r1.Status != "slow_down" || r1.Done {
		t.Fatalf("首次应透传上游状态: %+v", r1)
	}

	// 上游刚说慢点问，紧接着的第二轮必须被本地挡下，且不许打上游
	r2, err := f.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if r2.RetryIn <= 0 || r2.Done {
		t.Errorf("slow_down 之后立即再轮询应本地节流: %+v", r2)
	}
	if hits != 1 {
		t.Errorf("被节流的轮询不该打上游: hits=%d", hits)
	}

	// 退避会累加：第二次真实发问又收到 slow_down → 间隔至少 15 秒
	f.nextPollAt = time.Time{}
	if _, err := f.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	f.nextPollAt = time.Time{}
	if _, err := f.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	if f.curInterval < 15 {
		t.Errorf("每次 slow_down 应再加 5 秒退避，curInterval=%d", f.curInterval)
	}
	if until := time.Until(f.nextPollAt); until < 14*time.Second {
		t.Errorf("下次允许时间没跟上退避: %v", until)
	}
}

func TestPollPendingKeepsCallerCadence(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	withMock(t, slowDownHandler(t, &mu, &hits, "authorization_pending"), nil)

	f := &DeviceFlow{DeviceCode: "dev-1", Interval: 5}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		res, err := f.Poll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.RetryIn != 0 {
			t.Errorf("authorization_pending 不该本地节流（第 %d 轮）: %+v", i+1, res)
		}
	}
	if hits != 3 {
		t.Errorf("待授权期间每轮都该问上游: hits=%d", hits)
	}
}

// 退避结束后上游会正常交出 token——节流不能把成功也一起挡掉。
func TestPollDeliversTokenAfterBackoff(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	withMock(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			mu.Lock()
			hits++
			n := hits
			mu.Unlock()
			if n == 1 {
				writeJSONT(w, map[string]any{"error": "slow_down"})
				return
			}
			writeJSONT(w, map[string]any{"access_token": "gho_abcdef123456", "token_type": "bearer"})
		case "/copilot_internal/v2/token":
			writeJSONT(w, map[string]any{"token": "cop_tok_1", "expires_at": time.Now().Add(25 * time.Minute).Unix()})
		case "/user":
			writeJSONT(w, map[string]any{"login": "octocat", "plan": map[string]any{"name": "free"}})
		default:
			t.Errorf("未预期路径: %s", r.URL.Path)
		}
	}, nil)

	f := &DeviceFlow{DeviceCode: "dev-1", Interval: 5}
	ctx := context.Background()
	if r, err := f.Poll(ctx); err != nil || r.Done {
		t.Fatalf("首轮应是 slow_down 未完成: %+v %v", r, err)
	}
	if r, err := f.Poll(ctx); err != nil || r.Done || r.RetryIn <= 0 {
		t.Fatalf("第二轮应被本地节流: %+v %v", r, err)
	}
	f.nextPollAt = time.Time{}
	r, err := f.Poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Done || r.Cred == nil || r.Cred.CopilotToken != "cop_tok_1" {
		t.Fatalf("退避放行后应换到凭据: %+v", r)
	}
	if !strings.HasPrefix(r.Cred.GitHubToken, "gho_") {
		t.Errorf("GitHub token 丢失: %q", r.Cred.GitHubToken)
	}
}
